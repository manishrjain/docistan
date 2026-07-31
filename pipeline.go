package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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

	mu     sync.Mutex
	active map[string]*Job
	queued map[string]bool
	dupes  []DupeEvent
}

type DupeEvent struct {
	TS    int64  `json:"ts"`
	Name  string `json:"name"`
	DupOf int    `json:"dup_of"`
}

func NewPipeline(app *App) *Pipeline {
	return &Pipeline{
		app:    app,
		jobs:   make(chan string, 1024),
		active: map[string]*Job{},
		queued: map[string]bool{},
	}
}

func (p *Pipeline) Start(ctx context.Context) error {
	for i := 0; i < p.app.cfg.Workers; i++ {
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

func (p *Pipeline) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case path := <-p.jobs:
			p.process(ctx, path)
			p.mu.Lock()
			delete(p.queued, path)
			p.mu.Unlock()
		}
	}
}

func (p *Pipeline) setStage(path string, j *Job, stage string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j.Stage = stage
	p.active[path] = j
}

func (p *Pipeline) clearJob(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.active, path)
}

// ActiveJobs returns a snapshot for the status page.
func (p *Pipeline) ActiveJobs() []Job {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Job, 0, len(p.active))
	for _, j := range p.active {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Started.Before(out[k].Started) })
	return out
}

func (p *Pipeline) RecentDupes(n int) []DupeEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.dupes) <= n {
		out := make([]DupeEvent, len(p.dupes))
		copy(out, p.dupes)
		return out
	}
	out := make([]DupeEvent, n)
	copy(out, p.dupes[len(p.dupes)-n:])
	return out
}

// process runs one file through every stage. Stages are idempotent and skip
// when their output already exists, so a retry costs only the unfinished work.
func (p *Pipeline) process(ctx context.Context, path string) {
	store, search := p.app.store, p.app.search
	name := filepath.Base(path)
	job := &Job{Path: path, Name: name, Started: time.Now()}
	defer p.clearJob(path)

	fromConsume := filepath.Dir(path) == store.ConsumeDir()

	p.setStage(path, job, "hash")
	sum, size, err := hashFile(path)
	if err != nil {
		logf("hash %s: %v", name, err)
		return
	}

	ext := strings.ToLower(filepath.Ext(path))
	if !SupportedExts[ext] {
		logf("rejecting %s: unsupported type %q", name, ext)
		p.rejectUnsupported(ctx, name, ext, sum, size)
		if fromConsume {
			p.moveAside(path, name)
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
		p.setStage(path, job, "dedup")
		if !store.ClaimHash(sum) {
			logf("%s is already being ingested, skipping", name)
			return
		}
		defer store.ReleaseHash(sum)

		if existing, found, err := search.FindByHash(ctx, sum); err != nil {
			logf("dedup lookup for %s: %v", name, err)
		} else if found {
			logf("%s duplicates document %d", name, existing)
			p.recordDupe(name, existing)
			p.moveAside(path, name)
			return
		}

		p.setStage(path, job, "intake")
		doc, err = p.intake(ctx, path, name, ext, sum, size)
		if err != nil {
			logf("intake %s: %v", name, err)
			return
		}
	}

	job.ID = doc.ID
	if err := p.stages(ctx, path, job, doc); err != nil {
		doc.Status = StatusFailed
		doc.Error = err.Error()
		doc.FailedStage = job.Stage
		logf("doc %d failed at %s: %v", doc.ID, job.Stage, err)
		p.persist(ctx, doc)
		return
	}

	doc.Status = StatusReady
	doc.Error = ""
	doc.FailedStage = ""
	p.persist(ctx, doc)
	logf("doc %d ready: %q (%s)", doc.ID, doc.Title, doc.OCRSource)
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
	p.persist(ctx, doc)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logf("removing consumed file %s: %v", name, err)
	}
	return doc, nil
}

// stages runs the conversion and extraction work. Errors here mark the
// document failed; the LLM stage is deliberately allowed to fail softly.
func (p *Pipeline) stages(ctx context.Context, path string, job *Job, doc *Doc) error {
	store := p.app.store
	orig := store.OriginalPath(doc.ID, doc.OriginalExt)
	archive := store.ArchivePath(doc.ID)

	if _, err := os.Stat(archive); err != nil {
		p.setStage(path, job, "normalize")
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

		p.setStage(path, job, "ocr")
		doc.Signed = doc.OriginalExt == ".pdf" && IsSignedPDF(orig)
		source, err := OCR(ctx, normalized, archive, doc.Signed)
		if err != nil {
			return fmt.Errorf("ocr: %w", err)
		}
		if source == OCRTesseract && hadText {
			source = OCRNone
		}
		doc.OCRSource = source
		p.persist(ctx, doc)
	}

	p.setStage(path, job, "inspect")
	doc.PageCount = PageCount(ctx, archive)

	p.setStage(path, job, "thumb")
	if _, err := os.Stat(store.ThumbPath(doc.ID)); err != nil {
		if err := Thumbnail(ctx, archive, store.ThumbPath(doc.ID)); err != nil {
			logf("doc %d thumbnail: %v", doc.ID, err)
		}
	}

	p.setStage(path, job, "extract")
	text, err := ExtractText(ctx, archive)
	if err != nil {
		logf("doc %d text extraction: %v", doc.ID, err)
	}
	doc.Content = strings.TrimSpace(text)
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

	p.setStage(path, job, "metadata")
	p.maybeEnrich(ctx, doc)
	return nil
}

func (p *Pipeline) rejectUnsupported(ctx context.Context, name, ext, sum string, size int64) {
	doc := &Doc{
		ID:           p.app.store.AllocID(),
		SHA256:       sum,
		OriginalName: name,
		OriginalExt:  ext,
		Status:       StatusFailed,
		FailedStage:  "hash",
		Error:        fmt.Sprintf("unsupported file type %q", ext),
		AddedTS:      time.Now().Unix(),
		FileSize:     size,
		Title:        name,
		Tags:         []string{},
	}
	p.persist(ctx, doc)
}

// persist writes the sidecar first and only then updates the index, so the
// durable copy is never behind what a reader can see.
func (p *Pipeline) persist(ctx context.Context, doc *Doc) {
	if err := p.app.store.Save(doc); err != nil {
		logf("doc %d: writing sidecar: %v", doc.ID, err)
		return
	}
	if err := p.app.search.Upsert(ctx, doc); err != nil {
		logf("doc %d: indexing (will self-heal on restart): %v", doc.ID, err)
	}
}

func (p *Pipeline) recordDupe(name string, dupOf int) {
	ev := DupeEvent{TS: time.Now().Unix(), Name: name, DupOf: dupOf}
	p.mu.Lock()
	p.dupes = append(p.dupes, ev)
	p.mu.Unlock()

	f, err := os.OpenFile(p.app.store.DuplicatesLog(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if b, err := json.Marshal(ev); err == nil {
		f.Write(append(b, '\n'))
	}
}

// moveAside preserves rejected files instead of deleting them; a scanner may
// hold the only copy.
func (p *Pipeline) moveAside(path, name string) {
	dst := filepath.Join(p.app.store.DuplicatesDir(),
		fmt.Sprintf("%d-%s", time.Now().Unix(), name))
	if err := os.Rename(path, dst); err != nil {
		logf("moving %s aside: %v", name, err)
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
