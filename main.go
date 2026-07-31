package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

type Config struct {
	DataDir      string
	Listen       string
	TypesenseURL string
	TypesenseKey string
	Workers      int
	LLMModel     string
	LLMEnabled   bool
	Dev          bool
}

// App holds everything the handlers and the pipeline need.
type App struct {
	cfg      Config
	store    *Store
	search   *Search
	pipeline *Pipeline
	enricher *OpenAIEnricher
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.DataDir, "data", "./data", "data directory")
	flag.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "listen address")
	flag.StringVar(&cfg.TypesenseURL, "typesense-url", "http://localhost:8108", "Typesense URL")
	flag.StringVar(&cfg.TypesenseKey, "typesense-key", envOr("TYPESENSE_API_KEY", "docistan-dev-key"), "Typesense API key")
	flag.IntVar(&cfg.Workers, "workers", 2, "ingest workers")
	flag.StringVar(&cfg.LLMModel, "llm-model", "gpt-5.6-luna", "LLM model id")
	flag.BoolVar(&cfg.LLMEnabled, "llm", true, "use the model to title, tag and date documents")
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
	search := NewSearch(cfg.TypesenseURL, cfg.TypesenseKey)
	app := &App{cfg: cfg, store: store, search: search}
	if cfg.LLMEnabled {
		if key := os.Getenv("OPENAI_API_KEY"); key != "" {
			app.enricher = NewOpenAIEnricher(cfg.LLMModel, key)
			logf("metadata enrichment on, model %s", cfg.LLMModel)
		} else {
			logf("OPENAI_API_KEY not set: documents will keep filename-derived titles and no tags")
		}
	}

	ctx := context.Background()
	// Typesense answers every read, so refuse to start rather than serve a
	// site whose pages are all mysteriously empty.
	if err := waitForSearch(ctx, search, 30*time.Second); err != nil {
		return fmt.Errorf("typesense unreachable at %s: %w", cfg.TypesenseURL, err)
	}
	if err := search.EnsureFreshCollection(ctx); err != nil {
		return fmt.Errorf("create collection: %w", err)
	}
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

	mux := http.NewServeMux()
	app.routes(mux)
	logf("listening on http://%s", cfg.Listen)
	return http.ListenAndServe(cfg.Listen, mux)
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func logf(format string, args ...any) {
	log.Printf(format, args...)
}
