package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	DataDir      string
	Listen       string
	TypesenseURL string
	TypesenseKey string
	Collection   string
	Workers      int
	LLMModel     string
	LLMEnabled   bool
	// Per-million-token prices, so the cost shown next to a document stays
	// true when the model changes. Wrong prices are worse than none: the
	// number still looks authoritative.
	PriceIn  float64
	PriceOut float64
	// KeyFile is read when OPENAI_API_KEY is unset. A file keeps the key out
	// of shell history, out of the process listing, and out of any unit file
	// or script that might get committed.
	KeyFile string
	Dev     bool
}

// App holds everything the handlers and the pipeline need.
type App struct {
	cfg      Config
	store    *Store
	search   *Search
	pipeline *Pipeline
	enricher *OpenAIEnricher
	enrichq  *EnrichQueue
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.DataDir, "data", "./data", "data directory")
	flag.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "listen address")
	flag.StringVar(&cfg.TypesenseURL, "typesense-url", "http://localhost:8108", "Typesense URL")
	flag.StringVar(&cfg.TypesenseKey, "typesense-key", envOr("TYPESENSE_API_KEY", "docistan-dev-key"), "Typesense API key")
	flag.StringVar(&cfg.Collection, "collection", "documents", "Typesense collection name; give a second instance its own")
	flag.IntVar(&cfg.Workers, "workers", 2, "ingest workers")
	flag.StringVar(&cfg.LLMModel, "llm-model", "gpt-5.6-luna", "LLM model id")
	flag.BoolVar(&cfg.LLMEnabled, "llm", true, "use the model to title, tag and date documents")
	flag.Float64Var(&cfg.PriceIn, "llm-price-in", 0.20, "USD per million input tokens")
	flag.Float64Var(&cfg.PriceOut, "llm-price-out", 1.20, "USD per million output tokens")
	flag.StringVar(&cfg.KeyFile, "openai-key-file", defaultKeyFile(),
		"file holding the OpenAI API key, read when OPENAI_API_KEY is unset")
	flag.BoolVar(&cfg.Dev, "dev", false, "reload templates from disk on each request")
	flag.Parse()

	if err := run(cfg); err != nil {
		log.Fatalf("docistan: %v", err)
	}
}

func run(cfg Config) error {
	store, err := NewStore(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	search := NewSearch(cfg.TypesenseURL, cfg.TypesenseKey, cfg.Collection)
	app := &App{cfg: cfg, store: store, search: search}
	if cfg.LLMEnabled {
		if key, source := openAIKey(cfg.KeyFile); key != "" {
			app.enricher = NewOpenAIEnricher(cfg.LLMModel, key)
			logf("metadata enrichment on, model %s (key from %s)", cfg.LLMModel, source)
		} else {
			logf("no OpenAI key in OPENAI_API_KEY or %s: documents will keep filename-derived titles and no tags", cfg.KeyFile)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Typesense answers every read, so refuse to start rather than serve a
	// site whose pages are all mysteriously empty.
	if err := waitForSearch(ctx, search, 30*time.Second); err != nil {
		return fmt.Errorf("typesense unreachable at %s: %w", cfg.TypesenseURL, err)
	}
	if err := search.EnsureFreshCollection(ctx); err != nil {
		return fmt.Errorf("create collection: %w", err)
	}
	app.enrichq = NewEnrichQueue(app)

	n, unfinished, err := app.replaySidecars(ctx)
	if err != nil {
		return fmt.Errorf("replay sidecars: %w", err)
	}
	logf("indexed %d documents from %s", n, store.path("docs"))

	app.pipeline = NewPipeline(app)
	if err := app.pipeline.Start(ctx); err != nil {
		return fmt.Errorf("start pipeline: %w", err)
	}
	// Anything that was mid-flight when we last stopped resumes here. Stages
	// skip completed work, so this only costs what was actually unfinished.
	for _, id := range unfinished {
		if err := app.pipeline.EnqueueDoc(id); err != nil {
			logf("cannot resume doc %d: %v", id, err)
		}
	}
	if len(unfinished) > 0 {
		logf("resuming %d unfinished documents", len(unfinished))
	}

	if pending, _, _ := app.enrichq.Stats(); pending > 0 {
		logf("%d document(s) awaiting metadata", pending)
	}
	go app.enrichq.Run(ctx)

	mux := http.NewServeMux()
	app.routes(mux)
	srv := &http.Server{Addr: cfg.Listen, Handler: mux}

	go func() {
		<-ctx.Done()
		logf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logf("http shutdown: %v", err)
		}
	}()

	logf("listening on http://%s", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	app.pipeline.Drain(30 * time.Second)
	logf("stopped cleanly")
	return nil
}

// replaySidecars rebuilds the whole index from disk. Documents are streamed in
// batches and released immediately, so memory does not scale with corpus size.
func (a *App) replaySidecars(ctx context.Context) (indexed int, unfinished []int, err error) {
	const batchSize = 200
	batch := make([]*Doc, 0, batchSize)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := a.search.Import(ctx, batch); err != nil {
			return err
		}
		indexed += len(batch)
		batch = batch[:0]
		return nil
	}

	err = a.store.Each(func(d *Doc) error {
		// Tombstones stay on disk to keep their ids reserved, but must not
		// come back into the index.
		if d.Status == StatusDeleted {
			return nil
		}
		if d.Status == StatusProcessing {
			unfinished = append(unfinished, d.ID)
		}
		// Anything ready but untagged joins the queue, so a restart picks up
		// whatever the budget did not cover last time.
		if d.Status == StatusReady && !d.Enriched && d.Content != "" {
			a.enrichq.Add(d.ID)
		}
		batch = append(batch, d)
		if len(batch) >= batchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return indexed, unfinished, err
	}
	return indexed, unfinished, flush()
}

func waitForSearch(ctx context.Context, s *Search, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	var err error
	for time.Now().Before(deadline) {
		if err = s.Health(ctx); err == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

// LLMCost is the only place tokens become money, so the per-document figure
// and the archive total can never disagree.
func (c Config) LLMCost(in, out int64) float64 {
	return float64(in)*c.PriceIn/1e6 + float64(out)*c.PriceOut/1e6
}

func defaultKeyFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".openai.secret")
}

// openAIKey finds the key, preferring the environment so a one-off run can
// override the file. It returns where the key came from as well, because
// "which key is this actually using" is otherwise guesswork.
func openAIKey(path string) (key, source string) {
	if v := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); v != "" {
		return v, "OPENAI_API_KEY"
	}
	if path == "" {
		return "", ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		// A missing file is the ordinary case for someone using the
		// environment; anything else is worth saying.
		if !os.IsNotExist(err) {
			logf("reading %s: %v", path, err)
		}
		return "", ""
	}

	// Usually a bare key on one line, but a dotenv-style assignment, an
	// "export" prefix, wrapping quotes and comment lines are all common
	// enough to accept rather than fail on.
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		if name, value, ok := strings.Cut(line, "="); ok && strings.TrimSpace(name) == "OPENAI_API_KEY" {
			line = strings.TrimSpace(value)
		}
		if key = strings.Trim(line, `"'`); key != "" {
			warnKeyPerms(path)
			return key, path
		}
	}
	return "", ""
}

// warnKeyPerms says so once when the key file is readable by anyone else on
// the machine. Refusing to start would be worse than the risk; saying nothing
// would be worse than saying it.
func warnKeyPerms(path string) {
	st, err := os.Stat(path)
	if err != nil {
		return
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		logf("warning: %s is mode %#o, readable beyond your user — chmod 600 %s", path, mode, path)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func logf(format string, args ...any) {
	log.Printf(format, args...)
}
