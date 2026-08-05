package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
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
	// EnrichWorkers is how many model calls may be outstanding at once. It
	// defaults to the same figure as Workers; see the flag for why that is a
	// coincidence of size rather than a shared reason.
	EnrichWorkers int
	LLMModel      string
	LLMEnabled    bool
	// KeyFile is read when OPENAI_API_KEY is unset. A file keeps the key out
	// of shell history, out of the process listing, and out of any unit file
	// or script that might get committed. Empty means the usual places, which
	// keyFiles lists; a value here overrides them rather than adding to them.
	KeyFile string
	// PasswordFile holds the passwords for encrypted PDFs, one per line. Same
	// reasoning as KeyFile and more of it: these are bank passwords, so a flag
	// or an environment variable would put them in the process listing for
	// everyone on the machine to read.
	PasswordFile string
	// PublicOrigin is where this archive answers from as a browser sees it,
	// which is not what the process itself can observe once a proxy is in
	// front. Empty leaves the cross-site check resting on Sec-Fetch-Site alone.
	PublicOrigin string
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
	//
	// The list is no longer only read: the unlock form appends to it from a
	// request handler while pipeline workers are decrypting, so it is behind a
	// lock and reached through passwords() rather than touched directly. The one
	// exception is the assignment at startup, before anything else is running.
	pwMu         sync.Mutex
	pdfPasswords []string

	// Pages are parsed on first use and kept, so a render costs an execute
	// rather than a parse. Empty under -dev, which never caches.
	tplMu sync.Mutex
	tpl   map[string]*template.Template

	// journalMu serialises appends to journal.jsonl. Ingest workers, the
	// enrichment queue and request handlers all write to that one file, and
	// two lines interleaved are two lines lost.
	journalMu sync.Mutex

	// spend is what each document has cost the model, by id — the only
	// per-document map this process keeps, and it exists because the index
	// cannot answer the question.
	//
	// It used to: a facet on llm_cents, summed by Typesense. But facet stats
	// are computed over the facet values it kept rather than over the matching
	// documents, and with max_facet_values at 1 a thousand documents reported
	// the total of two of them — half a percent of the real figure, with no
	// error anywhere. Raising the cap only moves the cliff to however many
	// distinct values the next archive has.
	//
	// One entry per document holding absolutes, so a re-tag overwrites rather
	// than adds and nothing can drift. Eight thousand documents is a few
	// hundred kilobytes and a sum of eight thousand floats when somebody opens
	// the status page.
	spendMu sync.Mutex
	spend   map[int]docSpend
}

// docSpend is what one document has cost, over every call ever made for it.
// All three accumulate on the document, so summing them here gives the whole
// archive's spend rather than the sum of its most recent runs.
type docSpend struct {
	In, Out int64
	Cents   float64
}

// noteSpend records what a document has cost, replacing whatever was there.
func (a *App) noteSpend(d *Doc) {
	a.spendMu.Lock()
	defer a.spendMu.Unlock()
	if d.LLMIn == 0 && d.LLMOut == 0 && d.LLMCents == 0 {
		delete(a.spend, d.ID)
		return
	}
	a.spend[d.ID] = docSpend{In: d.LLMIn, Out: d.LLMOut, Cents: d.LLMCents}
}

// forgetSpend drops a document that has left the archive. The money was still
// spent, but the document is gone and counting it would be counting a ghost —
// the same reason the index drops it.
func (a *App) forgetSpend(id int) {
	a.spendMu.Lock()
	defer a.spendMu.Unlock()
	delete(a.spend, id)
}

// ArchiveSpend adds it all up, fresh each time rather than kept as a running
// total: the sum of a map cannot drift from the map.
func (a *App) ArchiveSpend() (in, out int64, cents float64, docs int) {
	a.spendMu.Lock()
	defer a.spendMu.Unlock()
	for _, s := range a.spend {
		in += s.In
		out += s.Out
		cents += s.Cents
	}
	return in, out, cents, len(a.spend)
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.DataDir, "data", defaultDataDir(), "document archive directory")
	flag.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "listen address")
	flag.StringVar(&cfg.TypesenseURL, "typesense-url", "http://localhost:8108", "Typesense URL")
	flag.StringVar(&cfg.TypesenseKey, "typesense-key", cmp.Or(os.Getenv("TYPESENSE_API_KEY"), "docovia-dev-key"), "Typesense API key")
	flag.StringVar(&cfg.Collection, "collection", "documents", "Typesense collection name; give a second instance its own")
	flag.IntVar(&cfg.Workers, "workers", defaultWorkers(), "ingest workers")
	// The same figure as -workers, though for the opposite reason: that one is
	// capped by the cores because every ingest worker spawns ocrmypdf, while a
	// model call is latency and almost no local work. What holds this down is
	// the API's request allowance, and at four calls in flight a backlog of
	// thousands drained far below it — the remaining-requests figure never
	// moved off its ceiling. Matching -workers spends more of the allowance
	// without needing a second number to reason about.
	flag.IntVar(&cfg.EnrichWorkers, "enrich-workers", defaultWorkers(), "concurrent model calls")
	flag.StringVar(&cfg.LLMModel, "llm-model", "gpt-5.6-luna", "LLM model id")
	flag.BoolVar(&cfg.LLMEnabled, "llm", true, "use the model to title, tag and date documents")
	// Empty rather than a default, because there is more than one default and
	// flag can only print a string. keyFiles holds the list; the help text has
	// to state it here.
	flag.StringVar(&cfg.KeyFile, "openai-key-file", "",
		"file holding the OpenAI API key, read when OPENAI_API_KEY is unset "+
			"(default ~/.openai.secret, then "+systemKeyFile+")")
	// Empty rather than a default, because the default is <data>/passwords and
	// -data is not parsed yet. resolvePasswordFile settles it once both are
	// known; the help text has to state the default itself, since there is no
	// value here for flag to print.
	flag.StringVar(&cfg.PasswordFile, "pdf-passwords", "",
		"file holding passwords for encrypted PDFs, one per line (default <data>/passwords)")
	// Only needed for browsers too old to send Sec-Fetch-Site, and only usable
	// when set: behind a proxy the Host header names 127.0.0.1 while Origin
	// names the site, so there is nothing here to compare against by default.
	flag.StringVar(&cfg.PublicOrigin, "public-origin", "",
		"the site's own origin, e.g. https://docs.example.com, for the cross-site check")
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
	// Before the config is handed to anything: App takes a copy of it, and the
	// pipeline reads cfg.PasswordFile out of that copy to tell a reader where to
	// put a password.
	cfg.PasswordFile = resolvePasswordFile(cfg.PasswordFile, store)

	search := NewSearch(cfg.TypesenseURL, cfg.TypesenseKey, cfg.Collection)
	app := &App{cfg: cfg, store: store, search: search, spend: map[int]docSpend{}}

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
		candidates := keyFiles(cfg.KeyFile)
		if key, source := openAIKey(candidates...); key != "" {
			app.enricher = NewOpenAIEnricher(cfg.LLMModel, key)
			logf("metadata enrichment on, model %s (key from %s)", cfg.LLMModel, source)
			// Prices live in a table in code, so a model it has never heard of
			// still gets tagged but cannot have its spend named. Say so once
			// here rather than leave a status page quietly reading zero.
			if !modelPriced(cfg.LLMModel) {
				logf("no price known for model %s: documents will still be tagged, but costs will not be shown", cfg.LLMModel)
			}
		} else {
			// Every place actually looked in, so this reads as instructions
			// rather than as a report that something is missing.
			logf("no OpenAI key in OPENAI_API_KEY or %s: documents will keep filename-derived titles and no tags",
				strings.Join(candidates, " or "))
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
	go app.enrichq.Run(ctx, cfg.EnrichWorkers)
	go app.sweepTrash(ctx)

	mux := http.NewServeMux()
	app.routes(mux)
	srv := &http.Server{Addr: cfg.Listen, Handler: guard(mux, cfg.PublicOrigin)}

	go func() {
		<-ctx.Done()
		logf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logf("http shutdown: %v", err)
		}
	}()

	warnIfReachable(cfg.Listen)
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
	// The spend map tracks the index: a document that belongs in one belongs in
	// the other, and this is the single path both go through, so they cannot
	// come to disagree.
	if indexOpFor(doc) == indexRemove {
		a.forgetSpend(doc.ID)
		return retryUntil(ctx, retryInitial, retryMax, fmt.Sprintf("doc %d: removing from index", doc.ID), func() error {
			return a.search.Delete(ctx, doc.ID)
		})
	}
	a.noteSpend(doc)
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
		// What this document has cost, on the one pass that already has every
		// sidecar open. Tombstones returned above, so they are not counted.
		a.noteSpend(d)
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

// defaultWorkers scales with the machine, because OCR is the slow stage and it
// is CPU-bound: two workers left a 32-core desktop idle through a backfill that
// takes hours.
//
// Half the cores rather than all of them, because a worker is not one thread.
// Each runs ocrmypdf with --jobs 2, and ocrmypdf holds that as a budget rather
// than a floor — it divides the jobs across concurrent pages and hands what is
// left to Tesseract as OMP_THREAD_LIMIT, clamped so that pages times threads
// stays inside it. So two is what a worker costs whether the document has one
// page or eighty, and half the cores is what makes the total come out at the
// core count instead of twice it.
//
// The floor of eight is for small machines, where the pipeline waits on
// Ghostscript and Tesseract subprocesses more than it runs anything of its own.
// It is a floor on parallelism, not on memory: eight concurrent ocrmypdf runs
// want a few GB between them, so a small box that is also short of RAM is the
// one case for setting -workers by hand.
func defaultWorkers() int {
	return max(8, runtime.NumCPU()/2)
}

// systemKeyFile is where the key lives when there is no home directory worth
// the name — which is the normal case in a container, where HOME points at an
// empty directory that no one ever puts anything in. Mounting the key here
// means the image needs no flag to find it.
const systemKeyFile = "/etc/docovia/openai.secret"

// keyFiles is where the key is looked for, in order. An explicit path replaces
// the list rather than extending it: someone who names a file is answering the
// question, and quietly reading a different one after theirs turned out to be
// empty would be the wrong kind of helpful.
//
// The home file wins over the system file for the same reason it does in every
// other tool that reads both — a machine-wide key is the fallback for whoever
// has not set their own, not an override of it.
func keyFiles(flagValue string) []string {
	if flagValue != "" {
		return []string{flagValue}
	}
	var paths []string
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".openai.secret"))
	}
	return append(paths, systemKeyFile)
}

// warnIfReachable says so when this process is listening somewhere other than
// the loopback address.
//
// There is no authentication in this program. Deployed as intended it sits
// behind a proxy that authenticates every request and forwards to 127.0.0.1,
// and the entire security of that arrangement is the fact that nothing else can
// reach this port — a listener on 0.0.0.0 does not weaken the proxy, it goes
// around it, and everything in the archive is then served to anyone who asks.
//
// A warning rather than a refusal: binding a LAN address is a legitimate thing
// to do on a trusted network, and this program should not decide that question
// for the person running it. But it should never be the quiet default nobody
// noticed.
func warnIfReachable(listen string) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return
	}
	// An empty host is the shorthand for every interface, which is the case
	// worth the loudest warning and the easiest to type by accident.
	if host != "" {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return
		}
		if strings.EqualFold(host, "localhost") {
			return
		}
	}
	logf("WARNING: listening on %s, which is reachable from outside this process.", listen)
	logf("WARNING: this program has no authentication of its own. Anything that can reach")
	logf("WARNING: this port can read, download, edit and delete every document.")
	if inContainer() {
		// Binding every interface is not a mistake in here — it is the only way
		// the port can be published at all — so the advice has to be about the
		// boundary that does exist rather than the one that does not.
		logf("WARNING: publish it to 127.0.0.1 on the host and put an authenticating")
		logf("WARNING: proxy in front, never straight to a public interface.")
		return
	}
	logf("WARNING: bind 127.0.0.1 and put an authenticating proxy in front of it.")
}

// inContainer reports whether this process is running inside a container, which
// changes what the warning above should tell someone to do rather than whether
// to warn at all. The port is still reachable by whatever can route to it; it is
// the place to fix that which moves, from the listen address to the published
// one.
//
// The docker-created marker file, and cgroup membership for runtimes that do not
// write one. Both are heuristics, and neither is load-bearing: guessing wrong
// only prints the less useful of two true warnings.
func inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	return err == nil && (strings.Contains(string(b), "docker") ||
		strings.Contains(string(b), "containerd") ||
		strings.Contains(string(b), "kubepods"))
}

// resolvePasswordFile settles where the passwords are read from. The flag wins
// when it was given; otherwise they live in the archive, next to the documents
// they open, so that one backup covers both.
//
// This cannot be the flag's default value: it is derived from -data, which is
// not known until flag.Parse has run and there is a store to ask. An empty flag
// is therefore "not given" rather than "no file", and the resolution happens
// here, once, before anything reads cfg.PasswordFile.
func resolvePasswordFile(flagValue string, store *Store) string {
	if flagValue != "" {
		return flagValue
	}
	return store.PasswordsPath()
}

// pdfPasswords reads the candidates for encrypted documents.
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
	out := parsePasswords(b)
	if len(out) > 0 {
		warnKeyPerms(path)
	}
	return out, nil
}

// parsePasswords is the file's format, one password per line. Blank lines and
// # comments are skipped so the file can say which password belongs to which
// bank, and nothing else is interpreted: unlike the API key, which has a known
// shape, a password may contain quotes, spaces or an equals sign, and
// unwrapping any of those would corrupt it.
//
// Both ends are trimmed all the same. No institution issues a password that
// begins or ends in a space, and an invisible character that makes a correct
// password fail is a bug nobody can see — the file would look right and the
// document would stay locked.
//
// Separate from the read because the unlock form has the bytes in hand and asks
// the same question of them — whether this password is already in the file —
// and two spellings of "what counts as a line" is how it would come to append a
// duplicate that only differs by a space.
func parsePasswords(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// passwords is the candidate list as the pipeline should see it right now.
// Copied out under the lock: a worker holding the slice while the unlock form
// appends to it is a race, and the cost of the copy is nothing beside the qpdf
// runs it is about to feed.
func (a *App) passwords() []string {
	a.pwMu.Lock()
	defer a.pwMu.Unlock()
	return slices.Clone(a.pdfPasswords)
}

// rememberPassword files a password that has just been proved against a real
// document: on disk so the next start still has it, and in memory so the
// document about to be requeued opens without waiting for one.
//
// Appended rather than rewritten. The file is not only ours — someone maintains
// it by hand, with comments saying which password belongs to which bank — and a
// rewrite that went wrong would take out passwords for documents nobody is even
// looking at today.
//
// The lock covers the file as well as the list, because they are two halves of
// one thing: two readers unlocking two documents with the same password at the
// same moment would otherwise each read a file without it and each append it.
func (a *App) rememberPassword(pw string) error {
	path := a.cfg.PasswordFile
	if path == "" {
		return errors.New("no password file is configured")
	}

	a.pwMu.Lock()
	defer a.pwMu.Unlock()

	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Compared after the reader's own parsing, so "already in the file" means
	// the same thing here as "already tried" at startup — a password that
	// differs only by surrounding space is not a second password.
	if !slices.Contains(parsePasswords(b), pw) {
		line := pw + "\n"
		if len(b) > 0 && b[len(b)-1] != '\n' {
			// The file ends mid-line. Appending straight onto it would splice the
			// two into one password that opens nothing, taking the existing one
			// out of service as well.
			line = "\n" + line
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(line); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		// 0600 above only applies to a file this created. One that was already
		// there keeps whatever mode it had, which is the case worth a warning.
		warnKeyPerms(path)
	}

	if !slices.Contains(a.pdfPasswords, pw) {
		// A fresh slice rather than an append in place, so a worker that is
		// already holding the old one is holding something nobody writes to.
		a.pdfPasswords = append(slices.Clone(a.pdfPasswords), pw)
	}
	return nil
}

// openAIKey finds the key, preferring the environment so a one-off run can
// override the file. It returns where the key came from as well, because
// "which key is this actually using" is otherwise guesswork.
func openAIKey(paths ...string) (key, source string) {
	if v := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); v != "" {
		return v, "OPENAI_API_KEY"
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if key, ok := keyFromFile(path); ok {
			return key, path
		}
	}
	return "", ""
}

// keyFromFile reads one candidate. A file that is absent, empty, or nothing but
// comments is not an answer, so it reports failure and lets the next one try.
func keyFromFile(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		// A missing file is the ordinary case when several places are tried;
		// anything else — a key sitting there unreadable, most of all — is
		// worth saying, because it looks identical from the outside.
		if !os.IsNotExist(err) {
			logf("reading %s: %v", path, err)
		}
		return "", false
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
		if key := strings.Trim(line, `"'`); key != "" {
			warnKeyPerms(path)
			return key, true
		}
	}
	return "", false
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
