package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
	// KeyFile is read when OPENAI_API_KEY is unset. A file keeps the key out
	// of shell history, out of the process listing, and out of any unit file
	// or script that might get committed.
	KeyFile string
	// PasswordFile holds the passwords for encrypted PDFs, one per line. Same
	// reasoning as KeyFile and more of it: these are bank passwords, so a flag
	// or an environment variable would put them in the process listing for
	// everyone on the machine to read.
	PasswordFile string
	Dev          bool
}

// App holds everything the handlers and the pipeline need.
type App struct {
	cfg      Config
	store    *Store
	search   *Search
	pipeline *Pipeline
	enricher *OpenAIEnricher
	enrichq  *EnrichQueue

	// pdfPasswords are the candidates tried against an encrypted document, read
	// once at startup. Re-reading the file per document would put a secret
	// through the disk on every ingest to answer a question that is almost
	// always "this one is not encrypted".
	pdfPasswords []string

	// Pages are parsed on first use and kept, so a render costs an execute
	// rather than a parse. Empty under -dev, which never caches.
	tplMu sync.Mutex
	tpl   map[string]*template.Template

	// journalMu serialises appends to journal.jsonl. Ingest workers, the
	// enrichment queue and request handlers all write to that one file, and
	// two lines interleaved are two lines lost.
	journalMu sync.Mutex
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.DataDir, "data", defaultDataDir(), "document archive directory")
	flag.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "listen address")
	flag.StringVar(&cfg.TypesenseURL, "typesense-url", "http://localhost:8108", "Typesense URL")
	flag.StringVar(&cfg.TypesenseKey, "typesense-key", cmp.Or(os.Getenv("TYPESENSE_API_KEY"), "docovia-dev-key"), "Typesense API key")
	flag.StringVar(&cfg.Collection, "collection", "documents", "Typesense collection name; give a second instance its own")
	flag.IntVar(&cfg.Workers, "workers", 2, "ingest workers")
	flag.StringVar(&cfg.LLMModel, "llm-model", "gpt-5.6-luna", "LLM model id")
	flag.BoolVar(&cfg.LLMEnabled, "llm", true, "use the model to title, tag and date documents")
	flag.StringVar(&cfg.KeyFile, "openai-key-file", defaultKeyFile(),
		"file holding the OpenAI API key, read when OPENAI_API_KEY is unset")
	flag.StringVar(&cfg.PasswordFile, "pdf-passwords", defaultPasswordFile(),
		"file holding passwords for encrypted PDFs, one per line")
	flag.BoolVar(&cfg.Dev, "dev", false, "reload templates from disk on each request")
	flag.Parse()

	if err := run(cfg); err != nil {
		log.Fatalf("docovia: %v", err)
	}
}

func run(cfg Config) error {
	store, err := NewStore(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("data dir: %w", err)
	}
	search := NewSearch(cfg.TypesenseURL, cfg.TypesenseKey, cfg.Collection)
	app := &App{cfg: cfg, store: store, search: search}

	passwords, err := pdfPasswords(cfg.PasswordFile)
	if err != nil {
		// Not fatal. Everything unencrypted still ingests, and a document that
		// does need a password says so on its own row in the Failed view rather
		// than taking the whole archive down with it.
		logf("reading %s: %v — encrypted PDFs will fail until it can be read", cfg.PasswordFile, err)
	}
	app.pdfPasswords = passwords
	if len(passwords) > 0 {
		// The count, and never anything else. This is the one line in the
		// program tempted to print what it just read.
		logf("%d PDF password(s) loaded from %s", len(passwords), cfg.PasswordFile)
	}

	if cfg.LLMEnabled {
		if key, source := openAIKey(cfg.KeyFile); key != "" {
			app.enricher = NewOpenAIEnricher(cfg.LLMModel, key)
			logf("metadata enrichment on, model %s (key from %s)", cfg.LLMModel, source)
			// Prices live in a table in code, so a model it has never heard of
			// still gets tagged but cannot have its spend named. Say so once
			// here rather than leave a status page quietly reading zero.
			if !modelPriced(cfg.LLMModel) {
				logf("no price known for model %s: documents will still be tagged, but costs will not be shown", cfg.LLMModel)
			}
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
	go app.sweepTrash(ctx)

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

const (
	// What a write waits between attempts when the disk or the index is not
	// taking it. Fifteen seconds is short enough that a service coming back is
	// noticed almost immediately and long enough that a long outage costs a few
	// requests a minute rather than a hammering.
	retryInitial = 500 * time.Millisecond
	retryMax     = 15 * time.Second
	// retryLogEvery bounds how often one stuck write repeats itself. An outage
	// lasting an hour should leave sixty lines, not sixty thousand.
	retryLogEvery = time.Minute
)

// retryUntil runs fn until it succeeds or ctx is canceled, backing off from
// `initial` doubling to `max` between attempts. what names the work in the log,
// as the subject of "<what>: <what went wrong>".
//
// It returns nil once fn succeeds and ctx.Err() if the context is canceled,
// including part-way through a backoff — there is no third outcome, and no
// attempt limit: the caller has decided the work must happen.
//
// The bounds are parameters rather than constants so a test can drive this in
// microseconds; everything in the running system passes retryInitial/retryMax.
func retryUntil(ctx context.Context, initial, max time.Duration, what string, fn func() error) error {
	start := time.Now()
	var lastLog time.Time
	delay := initial

	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil {
			// Silent on the ordinary path: only a write that had to wait is
			// worth a line, and then it says how long the wait was so the
			// outage has a measured length rather than a remembered one.
			if attempt > 1 {
				logf("%s: succeeded after %d attempts over %s", what, attempt, time.Since(start).Round(time.Millisecond))
			}
			return nil
		}
		if ctx.Err() != nil {
			// Shutting down, or whoever was waiting has gone. Announcing a
			// retry we are not going to make would be a lie, so say nothing
			// and let the caller decide what an abandoned write means.
			return ctx.Err()
		}
		// The first failure goes out at once, because a stall with no
		// explanation is the thing this is meant to prevent. After that the
		// rate limit takes over.
		if attempt == 1 {
			logf("%s: %v — retrying until it succeeds", what, err)
			lastLog = time.Now()
		} else if time.Since(lastLog) >= retryLogEvery {
			logf("%s: still failing after %d attempts over %s: %v", what, attempt, time.Since(start).Round(time.Second), err)
			lastLog = time.Now()
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, max)
	}
}

// persist writes the sidecar and only then updates the index, so the durable
// copy is never behind what a reader can see. Every write goes through here:
// the ordering is a property of the code rather than a rule restated in a
// comment beside each copy of it, and no path can save without indexing.
//
// Both halves are retried until they succeed. The index is not a cache to be
// caught up later — every list, search and document page is served out of it,
// so a document that is on disk but not in it is a document nobody can find.
// A write that cannot reach the index therefore holds its caller, pipeline
// worker or HTTP request, until it can. Ingestion stalling while Typesense is
// down is the intended, visible behaviour: the queue backs up and the log says
// why, which is what brings someone to fix it. The alternatives — skipping
// ahead, or handing the gap to a reconciler that has to be right about which
// copy is newer — are how an archive quietly loses documents.
//
// So the only way out other than success is the context: shutdown, or a client
// that has given up. Callers get nil or ctx.Err(), nothing else.
//
// A tombstone is the one document that is written but not indexed. Its sidecar
// still goes to disk — that record is the whole reason one is kept, and it is
// what holds the id — while the index is told to forget the id instead of being
// handed the document. Deciding that here rather than at each call site is what
// makes "a permanently deleted document is not in the index" true for callers
// that do not know the rule, including the ones not written yet.
func (a *App) persist(ctx context.Context, doc *Doc) error {
	err := retryUntil(ctx, retryInitial, retryMax, fmt.Sprintf("doc %d: writing sidecar", doc.ID), func() error {
		return a.store.Save(doc)
	})
	if err != nil {
		return err
	}
	if indexOpFor(doc) == indexRemove {
		return retryUntil(ctx, retryInitial, retryMax, fmt.Sprintf("doc %d: removing from index", doc.ID), func() error {
			return a.search.Delete(ctx, doc.ID)
		})
	}
	return retryUntil(ctx, retryInitial, retryMax, fmt.Sprintf("doc %d: indexing", doc.ID), func() error {
		return a.search.Upsert(ctx, doc)
	})
}

// What persist should tell the index about a document.
type indexOp int

const (
	indexUpsert indexOp = iota // store the document, the ordinary case
	indexRemove                // take the id out: this is a tombstone
)

// indexOpFor is the whole of that decision, and it is a function of the
// document alone — nothing to reach, nothing to stand in for — so the rule that
// must never break can be checked directly instead of being inferred from a
// live write to a search server.
func indexOpFor(d *Doc) indexOp {
	if d.Gone() {
		return indexRemove
	}
	return indexUpsert
}

// sweepInterval is how often the trash is looked at. The deadline is a date
// thirty days out, so the only thing the frequency decides is how long past it
// a document can linger — three times a day is comfortably inside the margin of
// error of "30 days" and costs one search each time.
const sweepInterval = 8 * time.Hour

// sweepTrash purges documents whose retention has run out: once at startup, and
// then every eight hours.
//
// The pass at startup is the one that matters. A laptop or a home server that
// is shut down every night never stays up for eight hours in a row, so an
// interval-only sweeper would tick on exactly the machines that do not need it
// and never on the ones that do, and the trash would grow forever.
func (a *App) sweepTrash(ctx context.Context) {
	for {
		a.sweepOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(sweepInterval):
		}
	}
}

// sweepOnce destroys everything past its deadline right now. Candidates come
// from the index, so the cost is one search rather than a walk of every sidecar
// in the archive — but the deadline is then re-read from the sidecar before
// anything is destroyed, because the sidecar is the source of truth and this is
// the one operation that cannot be undone.
func (a *App) sweepOnce(ctx context.Context) {
	now := time.Now().Unix()
	ids, err := a.search.ExpiredTrashIDs(ctx, now)
	if err != nil {
		// Nothing is lost by a sweep that could not run: the deadlines are on
		// disk and the next pass, or the next start, finds the same documents.
		if ctx.Err() == nil {
			logf("trash sweep: %v", err)
		}
		return
	}

	var purged int
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		doc, err := a.store.LoadMeta(id)
		if err != nil {
			logf("trash sweep: doc %d: %v", id, err)
			continue
		}
		if !duePurge(doc.DeleteAfterTS, now) {
			// The index said due and the sidecar does not — a restore whose
			// index write was lost, most likely. The sidecar wins, and saying so
			// is worth a line because the two are meant to agree.
			logf("trash sweep: doc %d is not due after all, leaving it (the index disagrees with its sidecar)", id)
			continue
		}
		if err := a.purge(ctx, id, "purged", purgeBySweeper); err != nil {
			logf("trash sweep: doc %d: %v", id, err)
			continue
		}
		purged++
	}
	// Silent when there was nothing to do, which is the ordinary outcome three
	// times a day; a log that says "purged 0" every eight hours teaches whoever
	// reads it to stop reading it.
	if purged > 0 {
		logf("trash sweep: purged %d document(s) whose %d days were up", purged, int(trashRetention/(24*time.Hour)))
	}
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
		if d.Gone() {
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

// defaultDataDir keeps the archive out of the source tree. Defaulting to
// ./data put a directory of personal documents inside a git working copy,
// one careless `git add -f` away from being published — and made a fresh
// clone write into the repo the moment it ran.
func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "./data"
	}
	return filepath.Join(home, "docovia-data")
}

func defaultKeyFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".openai.secret")
}

func defaultPasswordFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docovia-passwords")
}

// pdfPasswords reads the candidates for encrypted documents, one per line.
// Blank lines and # comments are skipped so the file can say which password
// belongs to which bank, and nothing else is interpreted: unlike the API key,
// which has a known shape, a password may contain quotes, spaces or an equals
// sign, and unwrapping any of those would corrupt it.
//
// Both ends are trimmed all the same. No institution issues a password that
// begins or ends in a space, and an invisible character that makes a correct
// password fail is a bug nobody can see — the file would look right and the
// document would stay locked.
//
// A missing file is not an error. It is the ordinary state of a machine that
// has never met an encrypted document, and every PDF that only carries
// restrictions still opens with the empty password DecryptPDF always tries.
//
// No error returned here can carry a password: the only thing that can fail is
// the read, which fails before there is anything read to put in it.
func pdfPasswords(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if len(out) > 0 {
		warnKeyPerms(path)
	}
	return out, nil
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

func logf(format string, args ...any) {
	log.Printf(format, args...)
}
