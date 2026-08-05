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

// jobState is where a file is: waiting for a worker, or with one.
type jobState uint8

const (
	jobWaiting jobState = iota + 1
	jobActive
)

const (
	// pollInterval is how long a worker with nothing to do waits before looking
	// again. A worker that just finished something does not wait at all, so
	// this is only ever the latency on an idle queue.
	pollInterval = time.Second
	// retryDelay and maxRetries bound the one case worth waiting on rather than
	// failing: an identical file already being ingested.
	retryDelay = 5 * time.Second
	maxRetries = 12
)

// Job is one file moving through the pipeline: where it is, how long it has
// been there, and how many times it has been put back. Not durable — a
// document interrupted mid-flight is found again by its own sidecar, which
// still says processing, so this is only ever the live view.
type Job struct {
	Path    string
	Name    string
	ID      int
	Stage   string
	Started time.Time

	State   jobState
	Retries int
	// NotBefore holds a contended file back without a goroutine and a timer for
	// each one: a worker simply does not claim it yet.
	NotBefore time.Time
}

// Pipeline is the work still to do, keyed by path.
//
// One map, where there used to be a buffered channel of paths and three maps
// beside it — one for what was in flight, one for what was queued, one counting
// retries — which between them held the state of a single file in four places
// and had to agree. The channel is what made that necessary and is also what
// made it lossy: it held 1024 paths, and Enqueue dropped anything that did not
// fit, so eight thousand files in the inbox became about a thousand documents
// and seven thousand log lines. A map has no capacity to exceed.
type Pipeline struct {
	app *App
	wg  sync.WaitGroup

	mu   sync.Mutex
	jobs map[string]*Job
}

func NewPipeline(app *App) *Pipeline {
	return &Pipeline{app: app, jobs: map[string]*Job{}}
}

func (p *Pipeline) Start(ctx context.Context) error {
	for i := 0; i < p.app.cfg.Workers; i++ {
		p.wg.Add(1)
		go p.worker(ctx)
	}
	if err := p.watch(ctx); err != nil {
		return err
	}
	go p.scanIngest()
	return nil
}

// Enqueue submits a path, ignoring anything already known so a duplicate
// filesystem event can't start the same work twice. Nothing is refused: the
// queue is however many files somebody put in the inbox.
func (p *Pipeline) Enqueue(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.jobs[path] != nil {
		return
	}
	p.jobs[path] = &Job{Path: path, Name: filepath.Base(path), State: jobWaiting}
}

// claim hands a worker the next file that is ready to be worked on, and marks
// it so no other worker takes it too. Which file that is does not matter —
// they are all going to be done.
func (p *Pipeline) claim() (*Job, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, j := range p.jobs {
		if j.State != jobWaiting || now.Before(j.NotBefore) {
			continue
		}
		j.State = jobActive
		j.Started = now
		j.Stage = ""
		return j, true
	}
	return nil, false
}

// release drops a job its worker has finished with — unless retryLater has
// already put it back to waiting, in which case it is no longer the worker's
// to drop and deleting it would throw the retry away.
func (p *Pipeline) release(j *Job) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.jobs[j.Path] == j && j.State == jobActive {
		delete(p.jobs, j.Path)
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

func (p *Pipeline) scanIngest() {
	entries, err := os.ReadDir(p.app.store.IngestDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p.Enqueue(filepath.Join(p.app.store.IngestDir(), e.Name()))
	}
}

func (p *Pipeline) watch(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := w.Add(p.app.store.IngestDir()); err != nil {
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

// retryLater puts a file back to waiting after a pause, for the one case worth
// waiting on rather than failing: an identical file already being ingested.
// Attempts are counted so a pathological loop cannot outlive the process.
//
// A timestamp rather than a goroutine and a timer per retry — the worker is
// already looking at the clock every time it claims.
func (p *Pipeline) retryLater(j *Job) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j.Retries++
	if j.Retries > maxRetries {
		// Give up by leaving the file where it is, and by leaving the job
		// active so release removes it. Deleting the file would be a guess:
		// unlike the dedup hit, this path has never seen the competing ingest
		// finish, and that ingest may yet fail and leave nothing behind.
		logf("%s: still contended after %d attempts, leaving it in the inbox (it will be picked up on restart)", j.Name, maxRetries)
		return
	}
	j.State = jobWaiting
	j.NotBefore = time.Now().Add(retryDelay)
}

func (p *Pipeline) worker(ctx context.Context) {
	defer p.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		job, ok := p.claim()
		if !ok {
			// Nothing to do. Only an idle worker waits; one that just finished
			// something comes straight back round for the next file.
			if !sleepCtx(ctx, pollInterval) {
				return
			}
			continue
		}
		p.process(ctx, job)
		p.release(job)
	}
}

// setStage records where a job has got to, for the status page. Under the lock
// because that page reads it from another goroutine.
func (p *Pipeline) setStage(j *Job, stage string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j.Stage = stage
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

// ActiveJobs returns a snapshot of what is being worked on for the status page.
// Only the active ones: the waiting are in the same map now, and on a large
// import there are thousands of them with nothing to say beyond their names.
// Backlog is how many files have been seen but not yet picked up. The status
// page listed only the jobs being worked on, which is a fixed handful however
// much is queued behind them — during an import of eight thousand documents it
// looked exactly the same as an import of twenty, and the only way to tell
// them apart was to count the ingest directory by hand.
func (p *Pipeline) Backlog() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int
	for _, j := range p.jobs {
		if j.State == jobWaiting {
			n++
		}
	}
	return n
}

func (p *Pipeline) ActiveJobs() []Job {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Job, 0, len(p.jobs))
	for _, j := range p.jobs {
		if j.State != jobActive {
			continue
		}
		out = append(out, *j)
	}
	slices.SortFunc(out, func(a, b Job) int { return a.Started.Compare(b.Started) })
	return out
}

// process runs one file through every stage. Stages are idempotent and skip
// when their output already exists, so a retry costs only the unfinished work.
func (p *Pipeline) process(ctx context.Context, job *Job) {
	store, search := p.app.store, p.app.search
	path, name := job.Path, job.Name

	fromIngest := filepath.Dir(path) == store.IngestDir()

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
		// fires for ingest-folder drops — and since the file is then deleted,
		// this line is all that says the archive ever saw that name. That is
		// why the detail carries the deletion too: whoever finds the file gone
		// has only this to read, whether they read it in the log now or in the
		// journal a month from now.
		p.app.record("rejected", 0, name, fmt.Sprintf("unsupported type %q, deleted from the inbox", ext))
		p.removeFromInbox(path, "unsupported")
		return
	}

	// Reuse the existing document when reprocessing something we already own;
	// only files arriving through the inbox are dedup candidates.
	var doc *Doc
	if !fromIngest {
		if id, err := idFromOriginalPath(path); err == nil {
			if d, err := store.Load(id); err == nil {
				doc = d
			} else {
				// The document exists but cannot be read, so this pass has to
				// carry on as if the file were new — and a file already living
				// in originals/ then walks into the dedup check below and
				// matches its own index entry. removeFromInbox is what keeps
				// that from being fatal; this line is what makes it visible,
				// because a sidecar that broke between enqueue and here leaves
				// no other trace.
				logf("reprocess %s: sidecar for doc %d is unreadable (%v), cannot reuse the document", name, id, err)
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
			p.retryLater(job)
			return
		}
		defer release()

		existing, found, err := search.FindByHashWait(ctx, sum, name)
		if err != nil {
			// The lookup is retried until Typesense answers, so the only way out
			// here is the context ending: shutdown, or a worker being let go.
			// Leave the file in the inbox rather than ingest one we never checked
			// — the next start finds it and asks again. Same reasoning as
			// retryLater's give-up path: an unanswered question is not a licence
			// to guess, and the bytes are safe where they are.
			logf("%s: dedup lookup abandoned (%v), leaving it in the inbox (it will be picked up on restart)", name, err)
			return
		}
		if found {
			// Deleting looks safe by construction here: the lookup has just
			// proved byte-identical content is already sitting in originals/, so
			// the bytes are not lost, and the name→id trace survives in the
			// journal. That argument holds for every file that came in through
			// the inbox — but it collapses for the one that did not, a reprocess
			// whose sidecar would not load and which matched itself. Hence
			// removeFromInbox rather than a bare os.Remove.
			//
			// The detail names which copy went, because "duplicate" on its own
			// reads like something was lost: the archive's copy is exactly
			// these bytes and stays.
			p.app.record("duplicate", existing, name, "already in the archive, deleted from the inbox")
			p.removeFromInbox(path, "duplicate")
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
		p.save(ctx, doc)
		// The stage leads the detail, so a pipeline failure is told apart from
		// the other thing that fails against a document — enrichment, whose
		// line names itself the same way.
		p.app.record("failed", doc.ID, "", fmt.Sprintf("%s: %v", job.Stage, err))
		return
	}

	doc.Status = StatusReady
	doc.Error = ""
	doc.FailedStage = ""
	p.save(ctx, doc)

	// The document is complete and usable now. Metadata is a separate concern
	// that depends on a remote service with its own budget, so it is queued
	// rather than waited on.
	//
	// Immediately after the save and before the journal line, because a page
	// watching this document reloads itself the moment it is neither processing
	// nor queued, and between those two states there must be nothing for it to
	// observe. A journal append here — a mutex and a write — was a window in
	// which the document had gone ready and had not yet been queued, and a poll
	// landing in it would refresh the page onto the metadata this is about to
	// replace.
	if !doc.Enriched && p.app.enrichq != nil {
		p.app.enrichq.Add(doc.ID)
	}
	p.app.record("ingested", doc.ID, doc.OriginalName, ingestDetail(doc))
}

// removeFromInbox deletes a file only if it is actually sitting in the inbox.
// Both delete paths in the pipeline are reached with a path that is usually a
// ingest-folder drop but can, on a reprocess whose sidecar failed to load, be
// originals/<id>.<ext> — the one copy of a document that cannot be regenerated,
// because the archive PDF is derived from the original and the original is
// derived from nothing. The dedup lookup will match such a file against its own
// index entry and report it as a duplicate, which is a true statement and a
// fatal instruction.
//
// The guard is therefore stated here, once, where the damage would be done,
// rather than as a boolean at each call site that a later edit can forget. It is
// sacred: nothing in this file may delete a document's original. Refusing is
// loud, because getting here means a damaged sidecar or a bug, and it leaves the
// file exactly where it was either way.
func (p *Pipeline) removeFromInbox(path, why string) {
	if filepath.Dir(path) != p.app.store.IngestDir() {
		logf("refusing to delete %s as %s: it is not in the inbox, so it may be a document's only original — this means a damaged sidecar or a bug; the file has been left alone", path, why)
		return
	}
	// A file that has already gone is the ordinary outcome of a retry that
	// raced with the real resolution, not a failure.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logf("deleting %s %s: %v", why, filepath.Base(path), err)
	}
}

// ingestDetail is what is recorded about a finished document: how much of it
// there is, and where its text came from. Those are the two facts a later
// reader needs to judge whether the document was read well, and the second one
// is not visible from the file itself.
//
// The title leads when it is not simply the filename, which is the case for a
// document that has been through the model and is now being reprocessed: the
// event already carries the original name, so repeating it as a title would
// say nothing, while "Northwind Utilities Statement" is the only word for that
// document a person would recognise.
func ingestDetail(doc *Doc) string {
	var parts []string
	if doc.Title != "" && doc.Title != doc.OriginalName {
		parts = append(parts, fmt.Sprintf("%q", doc.Title))
	}
	if doc.PageCount <= 0 {
		parts = append(parts, doc.OCRSource)
	} else {
		parts = append(parts, fmt.Sprintf("%d page%s, %s", doc.PageCount, plural(doc.PageCount), doc.OCRSource))
	}
	return strings.Join(parts, " · ")
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
		// Decryption comes before everything derived, because everything
		// derived is made from what it returns.
		src := orig
		if doc.OriginalExt == ".pdf" {
			p.setStage(job, stageDecrypt)
			decrypted := archive + ".dec.pdf"
			defer os.Remove(decrypted)
			src, err = p.decrypt(ctx, doc, orig, decrypted)
			if err != nil {
				return err
			}
		}

		p.setStage(job, "normalize")
		normalized := archive + ".norm.pdf"
		defer os.Remove(normalized)
		if err := ToPDF(ctx, src, doc.OriginalExt, normalized); err != nil {
			return fmt.Errorf("normalize: %w", err)
		}

		// Whether the input already carried a text layer decides three things:
		// how the result is labelled, whether a vision rescue is ever allowed —
		// ocrmypdf succeeds either way, so asking it afterwards can't
		// distinguish "OCR'd" from "left alone" — and which mode OCR runs in.
		//
		// The page count is what makes that a question about density rather
		// than presence, and it is needed here, before OCR. doc.PageCount is
		// set in inspect, after; this is a second pdfinfo on the file about to
		// be OCR'd, which is cheap, and the archive's own count is still the
		// one worth recording.
		var pre string
		if text, err := ExtractText(ctx, normalized); err == nil {
			pre = text
		}
		prePages := PageCount(ctx, normalized)
		// Density answers this for a PDF that arrived from outside, where a
		// thin text layer is the signature of a scan. It cannot answer it for
		// one we rendered, where the text is there because we put it there —
		// so for those, having any at all settles it.
		hadText := HasTextLayer(pre, prePages)
		if renderedHere(doc.OriginalExt) && strings.TrimSpace(pre) != "" {
			hadText = true
		}
		doc.NativeText = hadText

		p.setStage(job, "ocr")
		// Asked of src rather than orig: on an encrypted original pdfsig cannot
		// read the file and the byte-scan fallback is looking at ciphertext, so
		// the question is only answerable on the copy we can open.
		doc.Signed = doc.OriginalExt == ".pdf" && IsSignedPDF(src)
		// Here rather than in decrypt, because whether the original may be
		// replaced depends on doc.Signed and this is the first moment it is
		// known: the question can only be asked of a copy that opens.
		if err := p.adoptDecrypted(doc, orig, src); err != nil {
			// Not fatal. writeFileAtomicFrom renames or fails, so the original
			// is whole either way, and every derived file is already being made
			// from the decrypted copy — the document is complete and readable
			// with an encrypted original, which is what it had before this
			// existed. Failing it here would hide a good document behind a
			// convenience that did not come off.
			logf("doc %d: %v", doc.ID, err)
		}
		source, err := OCR(ctx, normalized, archive, ocrModeFor(doc.Signed, pre, prePages), pre)
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

// decrypt resolves a password-protected original into something the rest of the
// pipeline can read, and returns the file to work from — the original itself
// when there was nothing to do.
//
// This stage does not touch the original: it writes the decrypted copy beside
// the archive and returns it, and everything derived — the archival PDF, its
// text and its thumbnail — is made from that, which is the whole reason the
// document can be read and searched at all. Replacing the original with the
// decrypted copy is adoptDecrypted's job, later, once it is known whether the
// document is signed.
//
// A file no password opens fails here, deliberately, because the alternative is
// the bug this replaces: ocrmypdf refuses the file, Ghostscript "succeeds" by
// writing a blank page, and a forty-eight page tax return is filed as a
// one-page document with no text, marked ready, and invisible in the Failed
// view. Failing keeps the original, puts the document on the Failed list with a
// reason someone can act on, and leaves Retry as the way to finish the job.
func (p *Pipeline) decrypt(ctx context.Context, doc *Doc, orig, dst string) (string, error) {
	if PDFEncryption(ctx, orig) == pdfPlain {
		// doc.Encrypted is deliberately left alone rather than cleared. A
		// document whose original has already been replaced by its decrypted
		// copy arrives here plain on every reprocess, and clearing the flag
		// would erase the one record that a password was ever needed — the
		// banner would vanish and the sidecar would stop explaining why its
		// sha256 does not match the file beside it. The flag says the document
		// arrived encrypted, which no later pass can make untrue.
		return orig, nil
	}

	which, err := DecryptPDF(ctx, orig, dst, p.app.passwords())
	switch {
	case errors.Is(err, errNoPassword):
		// Short, and without the path it used to name. The document page says
		// this in the timeline now, with the box to type the password into
		// directly underneath — an instruction to go and edit a file by hand is
		// worse advice than the form already on screen, and the path it named
		// went stale the moment the password file moved into the archive.
		return "", errNoPassword
	case err != nil:
		return "", fmt.Errorf("decrypt: %w", err)
	}

	// Set only now, so the flag means what the document page says it means:
	// this document was encrypted and the copy in the archive is readable. A
	// document that failed above is not "decrypted in the archive" — it has no
	// archive at all.
	doc.Encrypted = true
	// Which password worked is worth a line. What it was is never written down
	// anywhere: not here, not in the journal, not in the sidecar. Position 0 is
	// the empty password, which means the file only carried restrictions and
	// nobody had to know anything to open it.
	if which == 0 {
		logf("doc %d: encrypted with restrictions only, opened with the empty password", doc.ID)
	} else {
		logf("doc %d: password-protected, decrypted with a password from %s", doc.ID, p.passwordFileName())
	}
	return dst, nil
}

// adoptDecrypted puts the decrypted copy in the original's place, so that once
// a document can be opened, nothing left on disk still demands a password.
//
// This overrides the archive's usual rule that originals/<id>.<ext> is the
// preservation copy, untouched from the moment it arrived. That is deliberate,
// and it is the owner's decision rather than an oversight: a file nobody can
// open without first finding a password is worth less than the same file
// openable, and the encrypted form has no value that survives losing the
// password. The archival PDF is a re-render, so the original is still the only
// copy of exactly what the bank produced — minus its lock.
//
// Both encrypted cases get this. A file that needs a real password and one that
// opens with the empty password but forbids printing or copying are equally
// annoying to deal with later, and removing restrictions is lossless.
//
// decrypted == orig means this pass decrypted nothing — a plain PDF, or a
// document whose original was replaced on an earlier pass and is plain now — so
// there is nothing to do and the original is not rewritten.
func (p *Pipeline) adoptDecrypted(doc *Doc, orig, decrypted string) error {
	if !doc.Encrypted || decrypted == orig {
		return nil
	}
	if doc.Signed {
		// The one document that keeps its encrypted original. qpdf --decrypt
		// rewrites the file, and a rewritten PDF no longer matches the bytes its
		// signature was computed over, so replacing this original would destroy
		// the only thing that makes it evidence. The lock is the lesser
		// annoyance; the archive copy is decrypted as usual, so the document is
		// still readable and searchable.
		logf("doc %d: signed as well as encrypted, so the original stays encrypted — decrypting it would invalidate the signature; the archive copy is the decrypted one", doc.ID)
		return nil
	}
	// Atomic, via a temp file in originals/ and a rename: a half-written
	// original is the one failure this system cannot recover from, since the
	// archive PDF is derived from the original and the original is derived from
	// nothing. Either the encrypted file or the decrypted one is there, never
	// part of both.
	if err := copyFile(decrypted, orig); err != nil {
		return fmt.Errorf("replacing the original with its decrypted copy: %w", err)
	}
	// doc.SHA256 stays as it was: it is the hash of the bytes that arrived, and
	// it no longer describes the file on disk. That is the point. It is the
	// dedup identity — dropping the same locked file into the inbox again must
	// still be recognised as a duplicate by FindByHash, and the only hash that
	// can match is the one of the encrypted bytes the sender will send again.
	logf("doc %d: the original now holds the decrypted copy; its recorded sha256 still names the encrypted bytes that arrived", doc.ID)
	return nil
}

// passwordFileName is what to call the password file in the log line that says
// a document was opened with one. The configured path is the useful answer — it
// is right both for the default in the archive and for a -pdf-passwords
// elsewhere — but a Config that never went through resolvePasswordFile has
// none, and a log line naming an empty path names nothing.
func (p *Pipeline) passwordFileName() string {
	if path := p.app.cfg.PasswordFile; path != "" {
		return path
	}
	return "the -pdf-passwords file"
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
