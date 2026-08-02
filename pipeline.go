package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Job is the live view of one file moving through the pipeline. It exists only
// to drive the status page and is deliberately not durable.
type Job struct {
	Path    string
	Name    string
	ID      int
	Stage   string
	Started time.Time
}

type Pipeline struct {
	app  *App
	jobs chan string

	wg sync.WaitGroup

	mu     sync.Mutex
	active map[string]*Job
	queued map[string]bool
	// retries counts how often a file has been put back for the same reason,
	// so contention cannot become a permanent loop.
	retries map[string]int
}

func NewPipeline(app *App) *Pipeline {
	return &Pipeline{
		app:     app,
		jobs:    make(chan string, 1024),
		active:  map[string]*Job{},
		queued:  map[string]bool{},
		retries: map[string]int{},
	}
}

func (p *Pipeline) Start(ctx context.Context) error {
	for i := 0; i < p.app.cfg.Workers; i++ {
		p.wg.Add(1)
		go p.worker(ctx)
	}
	if err := p.watch(ctx); err != nil {
		return err
	}
	go p.scanConsume()
	return nil
}

// Enqueue submits a path, ignoring anything already in flight so a duplicate
// filesystem event can't start the same work twice.
func (p *Pipeline) Enqueue(path string) {
	p.mu.Lock()
	if p.queued[path] {
		p.mu.Unlock()
		return
	}
	p.queued[path] = true
	p.mu.Unlock()

	select {
	case p.jobs <- path:
	default:
		logf("ingest queue full, dropping %s (it will be picked up on restart)", path)
		p.mu.Lock()
		delete(p.queued, path)
		p.mu.Unlock()
	}
}

// EnqueueDoc re-runs an existing document from its stored original, which is
// what both the retry button and startup recovery need.
func (p *Pipeline) EnqueueDoc(id int) error {
	orig, err := p.app.store.OriginalGlob(id)
	if err != nil {
		return err
	}
	p.Enqueue(orig)
	return nil
}

func (p *Pipeline) scanConsume() {
	entries, err := os.ReadDir(p.app.store.ConsumeDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p.Enqueue(filepath.Join(p.app.store.ConsumeDir(), e.Name()))
	}
}

func (p *Pipeline) watch(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(p.app.store.ConsumeDir()); err != nil {
		w.Close()
		return err
	}
	go func() {
		defer w.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
					continue
				}
				if strings.HasPrefix(filepath.Base(ev.Name), ".") {
					continue
				}
				go func(path string) {
					if waitForStableSize(path) {
						p.Enqueue(path)
					}
				}(ev.Name)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				logf("watcher: %v", err)
			}
		}
	}()
	return nil
}

// waitForStableSize blocks until the file stops growing, so a slow copy or a
// scanner writing directly into the inbox isn't ingested half-written.
func waitForStableSize(path string) bool {
	var last int64 = -1
	stable := 0
	for i := 0; i < 240; i++ {
		fi, err := os.Stat(path)
		if err != nil {
			return false
		}
		if fi.Size() == last && fi.Size() > 0 {
			stable++
			if stable >= 2 {
				return true
			}
		} else {
			stable = 0
		}
		last = fi.Size()
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// retryLater re-queues a file after a pause, for the one case that is worth
// waiting on rather than failing: an identical file already being ingested.
// Attempts are counted so a pathological loop cannot outlive the process.
func (p *Pipeline) retryLater(ctx context.Context, path, name string) {
	const (
		delay = 5 * time.Second
		max   = 12
	)
	p.mu.Lock()
	n := p.retries[path] + 1
	p.retries[path] = n
	p.mu.Unlock()

	if n > max {
		// Give up by leaving the file where it is. Deleting would be a guess:
		// unlike the dedup hit, this path has never seen the competing ingest
		// finish, and that ingest may yet fail and leave nothing behind.
		logf("%s: still contended after %d attempts, leaving it in the inbox (it will be picked up on restart)", name, max)
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		p.Enqueue(path)
	}()
}

func (p *Pipeline) worker(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case path := <-p.jobs:
			p.mu.Lock()
			before := p.retries[path]
			p.mu.Unlock()

			p.process(ctx, path)

			p.mu.Lock()
			delete(p.queued, path)
			// An unchanged count means process did not put this file back, so
			// it is resolved and its count has nothing left to guard; dropping
			// it stops the map growing by one entry per contended file that
			// later succeeded. A count that did change belongs to a retry still
			// to come and must survive, or the give-up limit would never be
			// reached.
			if p.retries[path] == before {
				delete(p.retries, path)
			}
			p.mu.Unlock()
		}
	}
}

// setStage takes the job rather than a job and its own path: the two were
// passed separately at every call site and had to agree at all of them.
func (p *Pipeline) setStage(j *Job, stage string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j.Stage = stage
	p.active[j.Path] = j
}

func (p *Pipeline) clearJob(j *Job) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.active, j.Path)
}

// Drain waits for workers to finish the document they are on. A document
// killed mid-OCR is recoverable — its sidecar still says processing, so the
// next boot re-enqueues it — but finishing costs a few seconds and avoids
// throwing away work that was nearly done.
func (p *Pipeline) Drain(limit time.Duration) {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		logf("shutdown: %d document(s) still in flight, leaving them for the next start", len(p.ActiveJobs()))
	}
}

// ActiveJobs returns a snapshot for the status page.
func (p *Pipeline) ActiveJobs() []Job {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Job, 0, len(p.active))
	for _, j := range p.active {
		out = append(out, *j)
	}
	slices.SortFunc(out, func(a, b Job) int { return a.Started.Compare(b.Started) })
	return out
}

// process runs one file through every stage. Stages are idempotent and skip
// when their output already exists, so a retry costs only the unfinished work.
func (p *Pipeline) process(ctx context.Context, path string) {
	store, search := p.app.store, p.app.search
	name := filepath.Base(path)
	job := &Job{Path: path, Name: name, Started: time.Now()}
	defer p.clearJob(job)

	fromConsume := filepath.Dir(path) == store.ConsumeDir()

	p.setStage(job, "hash")
	sum, size, err := hashFile(path)
	if err != nil {
		// A file that has gone is not a failure: another pass may have ingested
		// or deleted it as a duplicate between the queue and here, which is the
		// ordinary outcome of a retry that raced with the real resolution.
		if !os.IsNotExist(err) {
			logf("hash %s: %v", name, err)
		}
		return
	}

	ext := strings.ToLower(filepath.Ext(path))
	if !isSupportedExt(ext) {
		// An unsupported file never becomes a document: no sidecar, no id. The
		// upload path (acceptUpload in web.go) already refuses these with a
		// visible flash before they ever reach the inbox, so this branch only
		// fires for consume-folder drops — and since the file is then deleted,
		// the journal line is all that says the archive ever saw that name.
		logf("rejecting %s: unsupported type %q, deleting — the archive keeps only supported documents", name, ext)
		p.app.journal("rejected", 0, name, fmt.Sprintf("unsupported type %q", ext))
		// The fromConsume guard is sacred: reprocessing hands this function
		// paths inside originals/, and the delete must never be able to reach
		// outside the inbox.
		if fromConsume {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				logf("deleting unsupported %s: %v", name, err)
			}
		}
		return
	}

	// Reuse the existing document when reprocessing something we already own;
	// only files arriving through the inbox are dedup candidates.
	var doc *Doc
	if !fromConsume {
		if id, err := idFromOriginalPath(path); err == nil {
			if d, err := store.Load(id); err == nil {
				doc = d
			}
		}
	}

	if doc == nil {
		p.setStage(job, "dedup")
		release, ok := store.ClaimHash(sum)
		if !ok {
			// Identical bytes are mid-ingest in another worker. The document
			// they will become does not exist yet, so there is nothing for a
			// duplicate record to point at — and dropping the file here left
			// it stranded in the inbox, neither ingested nor reported. Come
			// back once the other one has landed, when the ordinary check can
			// name it. Terminates either way: if that ingest succeeds the
			// retry is recorded as a duplicate of it, and if it fails the
			// hash is released and the retry ingests normally.
			logf("%s: identical bytes already being ingested, retrying shortly", name)
			p.retryLater(ctx, path, name)
			return
		}
		defer release()

		if existing, found, err := search.FindByHash(ctx, sum); err != nil {
			logf("dedup lookup for %s: %v", name, err)
		} else if found {
			// Deleting is safe by construction here: FindByHash has just proved
			// byte-identical content is already sitting in originals/, so the
			// bytes are not lost. The name→id trace survives in the journal.
			logf("%s duplicates document %d, deleting", name, existing)
			p.app.journal("duplicate", existing, name, "already in the archive")
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				logf("deleting duplicate %s: %v", name, err)
			}
			return
		}

		p.setStage(job, "intake")
		doc, err = p.intake(ctx, path, name, ext, sum, size)
		if err != nil {
			logf("intake %s: %v", name, err)
			return
		}
	}

	job.ID = doc.ID
	if err := p.stages(ctx, job, doc); err != nil {
		doc.Status = StatusFailed
		doc.Error = err.Error()
		doc.FailedStage = job.Stage
		logf("doc %d failed at %s: %v", doc.ID, job.Stage, err)
		p.save(ctx, doc)
		p.app.journal("failed", doc.ID, "", fmt.Sprintf("%s: %v", job.Stage, err))
		return
	}

	doc.Status = StatusReady
	doc.Error = ""
	doc.FailedStage = ""
	p.save(ctx, doc)
	logf("doc %d ready: %q (%s)", doc.ID, doc.Title, doc.OCRSource)
	p.app.journal("ingested", doc.ID, doc.OriginalName, ingestDetail(doc))

	// The document is complete and usable now. Metadata is a separate concern
	// that depends on a remote service with its own budget, so it is queued
	// rather than waited on.
	if !doc.Enriched && p.app.enrichq != nil {
		p.app.enrichq.Add(doc.ID)
	}
}

// ingestDetail is what the journal records about a finished document: how much
// of it there is, and where its text came from. Those are the two facts a later
// reader needs to judge whether the document was read well, and the second one
// is not visible from the file itself.
func ingestDetail(doc *Doc) string {
	if doc.PageCount <= 0 {
		return doc.OCRSource
	}
	return fmt.Sprintf("%d page%s, %s", doc.PageCount, plural(doc.PageCount), doc.OCRSource)
}

func (p *Pipeline) intake(ctx context.Context, path, name, ext, sum string, size int64) (*Doc, error) {
	store := p.app.store
	doc := &Doc{
		ID:           store.AllocID(),
		SHA256:       sum,
		OriginalName: name,
		OriginalExt:  ext,
		Status:       StatusProcessing,
		AddedTS:      time.Now().Unix(),
		FileSize:     size,
		Title:        name,
		Tags:         []string{},
	}
	if err := copyFile(path, store.OriginalPath(doc.ID, ext)); err != nil {
		return nil, err
	}
	p.save(ctx, doc)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logf("removing consumed file %s: %v", name, err)
	}
	return doc, nil
}

// stages runs the conversion and extraction work. Errors here mark the
// document failed; the LLM stage is deliberately allowed to fail softly.
func (p *Pipeline) stages(ctx context.Context, job *Job, doc *Doc) error {
	store := p.app.store
	orig := store.OriginalPath(doc.ID, doc.OriginalExt)
	archive := store.ArchivePath(doc.ID)

	if _, err := os.Stat(archive); err != nil {
		p.setStage(job, "normalize")
		normalized := archive + ".norm.pdf"
		defer os.Remove(normalized)
		if err := ToPDF(ctx, orig, doc.OriginalExt, normalized); err != nil {
			return fmt.Errorf("normalize: %w", err)
		}

		// Whether the input already carried a text layer decides both how the
		// result is labelled and whether a vision rescue is ever allowed:
		// ocrmypdf --skip-text succeeds either way, so asking it afterwards
		// can't distinguish "OCR'd" from "left alone".
		hadText := false
		if doc.OriginalExt == ".pdf" {
			if pre, err := ExtractText(ctx, normalized); err == nil {
				hadText = HasTextLayer(pre)
			}
		}
		doc.NativeText = hadText

		p.setStage(job, "ocr")
		doc.Signed = doc.OriginalExt == ".pdf" && IsSignedPDF(orig)
		source, err := OCR(ctx, normalized, archive, doc.Signed)
		if err != nil {
			return fmt.Errorf("ocr: %w", err)
		}
		if source == OCRTesseract && hadText {
			source = OCRNone
		}
		doc.OCRSource = source
		p.save(ctx, doc)
	}

	p.setStage(job, "inspect")
	doc.PageCount = PageCount(ctx, archive)

	p.setStage(job, "thumb")
	if _, err := os.Stat(store.ThumbPath(doc.ID)); err != nil {
		if err := Thumbnail(ctx, archive, store.ThumbPath(doc.ID)); err != nil {
			logf("doc %d thumbnail: %v", doc.ID, err)
		}
	}

	p.setStage(job, "extract")
	text, err := ExtractText(ctx, archive)
	if err != nil {
		logf("doc %d text extraction: %v", doc.ID, err)
	}
	doc.Content = strings.TrimSpace(text)
	doc.TextTS = time.Now().Unix()
	if doc.OCRSource == "" {
		doc.OCRSource = OCRNone
	}

	// A rescue is only ever considered when there was no native text layer to
	// trust; cost is not the gate, correctness is.
	if !doc.NativeText && NeedsVisionRescue(doc.Content, doc.PageCount) {
		doc.NeedsRescue = true
	}

	if doc.Title == "" {
		doc.Title = doc.OriginalName
	}
	if doc.Tags == nil {
		doc.Tags = []string{}
	}

	return nil
}

// save persists a document. persist retries the sidecar write and the index
// update until both take, so a worker here will sit on a document for as long
// as the disk or Typesense is refusing it — deliberately: the queue backing up
// is what makes the outage visible, and persist's own log lines say why.
//
// That leaves cancellation as the only error, which means the process is
// stopping. The document keeps whatever state it already has on disk and the
// next start picks it up, so this is not a fault and does not get a line.
func (p *Pipeline) save(ctx context.Context, doc *Doc) {
	if err := p.app.persist(ctx, doc); err != nil && !errors.Is(err, context.Canceled) {
		logf("doc %d: %v", doc.ID, err)
	}
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func idFromOriginalPath(path string) (int, error) {
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	var id int
	_, err := fmt.Sscanf(stem, "%d", &id)
	return id, err
}
