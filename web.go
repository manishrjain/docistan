package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Flash is a one-off message shown after an upload. DocID, when set, links to
// the document a duplicate collided with.
type Flash struct {
	Text  string `json:"text"`
	DocID int    `json:"doc_id,omitempty"`
	Bad   bool   `json:"bad,omitempty"`
}

//go:embed templates static
var assets embed.FS

// templates parses the layout together with exactly one page. Parsing every
// page into a single set would not work: they all define "content", and in a
// shared namespace the last one parsed silently wins.
//
// Reads from the embedded copy normally, and from disk under -dev so the UI
// can be iterated on without rebuilding.
func (a *App) templates(page string) (*template.Template, error) {
	var src fs.FS = assets
	if a.cfg.Dev {
		src = os.DirFS(".")
	}
	return template.New("").Funcs(templateFuncs()).
		ParseFS(src, "templates/layout.html", "templates/"+page)
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"humanSize": humanSize,
		"shortDate": func(ts int64) string {
			if ts <= 0 {
				return ""
			}
			return time.Unix(ts, 0).Format("2 Jan 2006")
		},
		"joinTags": func(tags []string) string { return strings.Join(tags, ", ") },
		// Document dates are month precision: the day on a statement is rarely
		// meaningful and rarely unambiguous.
		"monthLabel": func(s string) string {
			t, err := time.Parse("2006-01", s)
			if err != nil {
				return s
			}
			return t.Format("Jan 2006")
		},
		// Go's built-in "slice" slices an existing value; it cannot build a
		// literal list, which is what the templates actually want.
		"list": func(items ...string) []string { return items },
		"add":  func(a, b int) int { return a + b },
		"facetLabel": func(field string) string {
			switch field {
			case "tags":
				return "Tags"
			case "status":
				return "Status"
			}
			return field
		},
	}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	case n > 0:
		return fmt.Sprintf("%d B", n)
	}
	return ""
}

func (a *App) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /doc/{id}", a.handleDoc)
	mux.HandleFunc("POST /doc/{id}", a.handleDocUpdate)
	mux.HandleFunc("POST /doc/{id}/delete", a.handleDocDelete)
	mux.HandleFunc("POST /doc/{id}/retry", a.handleDocRetry)
	mux.HandleFunc("POST /doc/{id}/enrich", a.handleDocEnrich)
	mux.HandleFunc("GET /doc/{id}/pdf", a.handleDocPDF)
	mux.HandleFunc("GET /doc/{id}/original", a.handleDocOriginal)
	mux.HandleFunc("GET /doc/{id}/thumb", a.handleDocThumb)
	mux.HandleFunc("GET /upload", a.handleUploadForm)
	mux.HandleFunc("POST /upload", a.handleUpload)
	mux.HandleFunc("GET /status", a.handleStatus)
	mux.HandleFunc("GET /healthz", a.handleHealthz)

	static, _ := fs.Sub(assets, "static")
	if a.cfg.Dev {
		static = os.DirFS("static")
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
}

// Stage is one step of processing as shown on a document page. The point is
// to make it obvious what ran, what did not, and crucially which steps were
// local and which involved the model.
type Stage struct {
	Name   string
	State  string // done | pending | skipped | failed
	Detail string
}

func (s Stage) Symbol() string {
	switch s.State {
	case "done":
		return "✓"
	case "pending":
		return "◷"
	case "failed":
		return "✗"
	}
	return "–"
}

// stagesFor reconstructs what happened to a document from what is on disk and
// in the sidecar, rather than from a stored log, so it cannot drift.
func (a *App) stagesFor(doc *Doc) []Stage {
	var out []Stage

	orig := Stage{Name: "Original stored", State: "done", Detail: doc.OriginalName}
	if doc.FileSize > 0 {
		orig.Detail += " · " + humanSize(doc.FileSize)
	}
	out = append(out, orig)

	archive := Stage{Name: "Converted to archival PDF", State: "failed", Detail: "not produced"}
	if _, err := os.Stat(a.store.ArchivePath(doc.ID)); err == nil {
		archive.State, archive.Detail = "done", "PDF/A"
		if doc.PageCount > 0 {
			archive.Detail = fmt.Sprintf("PDF/A · %d page%s", doc.PageCount, plural(doc.PageCount))
		}
	}
	out = append(out, archive)

	// Success here is whether text exists, not which tool produced it. Driving
	// this off the OCR source instead would report a document that came
	// through the Ghostscript fallback as failed despite having good text.
	// The source is worth stating plainly though, since "did AI read this?"
	// is not otherwise answerable from the page.
	text := Stage{Name: "Text extracted"}
	if n := len(strings.TrimSpace(doc.Content)); n == 0 {
		text.State = "failed"
		text.Detail = "no text could be extracted"
	} else {
		text.State = "done"
		switch {
		case doc.NativeText:
			text.Detail = "local · the PDF already had a text layer"
		case doc.OCRSource == OCRTesseract:
			text.Detail = "local · OCR by tesseract"
		case doc.OCRSource == OCRSkippedSigned:
			text.Detail = "local · digitally signed, left as-is"
		case doc.OCRSource == OCRLLM:
			text.Detail = "model · transcribed from page images"
		default:
			text.Detail = "local"
		}
		text.Detail += fmt.Sprintf(" · %d characters", n)
	}
	out = append(out, text)

	thumb := Stage{Name: "Thumbnail", State: "skipped", Detail: "not generated"}
	if _, err := os.Stat(a.store.ThumbPath(doc.ID)); err == nil {
		thumb.State, thumb.Detail = "done", ""
	}
	out = append(out, thumb)

	index := Stage{Name: "Indexed for search", State: "pending", Detail: doc.Status}
	if doc.Status == StatusReady {
		index.State, index.Detail = "done", "full text searchable"
	}
	out = append(out, index)

	out = append(out, a.taggingStage(doc))
	return out
}

func (a *App) taggingStage(doc *Doc) Stage {
	s := Stage{Name: "Titled and tagged by AI"}
	switch {
	case doc.Enriched:
		s.State = "done"
		s.Detail = "model · " + a.cfg.LLMModel
	case a.enricher == nil:
		s.State = "skipped"
		s.Detail = "no model configured — set OPENAI_API_KEY"
	case a.enrichq != nil && a.enrichq.Has(doc.ID):
		s.State = "pending"
		s.Detail = "queued"
		if _, resetIn, stopped := a.enricher.Budget(); stopped != "" {
			s.Detail = "stopped: " + stopped
		} else if resetIn > 0 {
			if remaining, _, _ := a.enricher.Budget(); remaining == 0 {
				s.Detail = "waiting for rate limit to reset in " + resetIn.Round(time.Second).String()
			}
		}
	case hasTag(doc.Tags, "needs-review"):
		s.State = "failed"
		s.Detail = "the model call did not succeed — use Re-tag with AI"
	default:
		s.State = "skipped"
		s.Detail = "not run"
	}
	return s
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// page is the data every template receives.
type page struct {
	Title     string
	Query     Query
	Result    *Result
	Doc       *Doc
	Stages    []Stage
	KnownTags []string
	Jobs      []Job
	Dupes     []DupeEvent
	Failed    []Hit
	Flash     []Flash
	Spend     *SpendSummary
	URL       *url.URL
}

// SpendSummary reports actual model usage rather than an estimate, alongside
// the state of the enrichment queue and the remaining request budget.
type SpendSummary struct {
	Calls   int64
	In      int64
	Out     int64
	USD     float64
	PerDoc  float64
	Pending int
	Done    int
	Failed  int
	Budget  int
	ResetIn string
	Stopped string
}

func (a *App) render(w http.ResponseWriter, name string, data page) {
	tpl, err := a.templates(name)
	if err != nil {
		http.Error(w, "template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "layout", data); err != nil {
		logf("render %s: %v", name, err)
	}
}

// WithParam rebuilds the current URL with one parameter replaced, which is how
// facet links and pagination preserve everything else the user selected.
func (p page) WithParam(key, value string) string {
	q := url.Values{}
	if p.URL != nil {
		q = p.URL.Query()
	}
	if value == "" {
		q.Del(key)
	} else {
		q.Set(key, value)
	}
	if key != "page" {
		q.Del("page")
	}
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

// Active reports whether a facet value is the one currently filtered on.
func (p page) Active(field, value string) bool {
	switch field {
	case "tags":
		return p.Query.Tag == value
	case "status":
		return p.Query.Status == value
	}
	return false
}

func (p page) ParamFor(field string) string {
	if field == "tags" {
		return "tag"
	}
	return field
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	q := Query{
		Q:      v.Get("q"),
		Tag:    v.Get("tag"),
		Status: v.Get("status"),
		Sort:   v.Get("sort"),
	}
	q.Page, _ = strconv.Atoi(v.Get("page"))

	res, err := a.search.Query(r.Context(), q)
	if err != nil {
		http.Error(w, "search unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	a.render(w, "index.html", page{
		Title:  "Documents",
		Query:  q,
		Result: res,
		URL:    r.URL,
	})
}

func docID(r *http.Request) (int, error) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		return 0, errors.New("bad document id")
	}
	return id, nil
}

func (a *App) handleDoc(w http.ResponseWriter, r *http.Request) {
	id, err := docID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	doc, err := a.search.Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The tag vocabulary drives autocomplete, so adding a tag that already
	// exists is a pick rather than a retype — which is what keeps the
	// vocabulary from filling with near-duplicates.
	known, err := a.search.Vocabulary(r.Context(), "tags", 200)
	if err != nil {
		logf("doc %d: reading tag vocabulary: %v", id, err)
	}
	a.render(w, "doc.html", page{
		Title: doc.Title, Doc: doc, Stages: a.stagesFor(doc),
		KnownTags: known, URL: r.URL,
	})
}

func (a *App) handleDocUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := docID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The sidecar is authoritative, so edits are applied to it rather than to
	// the indexed copy.
	doc, err := a.store.Load(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	doc.Title = strings.TrimSpace(r.FormValue("title"))
	doc.CreatedDate = normalizeMonth(r.FormValue("created_date"))
	doc.CreatedTS = parseDateTS(doc.CreatedDate)
	doc.Tags = splitTags(r.FormValue("tags"))

	// Write durably first, then index: a 200 must mean both.
	if err := a.store.Save(doc); err != nil {
		http.Error(w, "saving document: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.search.Upsert(r.Context(), doc); err != nil {
		http.Error(w, "saved to disk, but indexing failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Autosave posts in the background and stays on the page.
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":           true,
			"title":        doc.Title,
			"tags":         doc.Tags,
			"created_date": doc.CreatedDate,
		})
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/doc/%d", id), http.StatusSeeOther)
}

func (a *App) handleDocDelete(w http.ResponseWriter, r *http.Request) {
	id, err := docID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.store.Delete(id); err != nil {
		http.Error(w, "deleting document: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.search.Delete(r.Context(), id); err != nil {
		logf("doc %d: removing from index: %v", id, err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleDocRetry rebuilds a document from its original: OCR, thumbnail, text
// and metadata are all redone. Unlike startup recovery, which deliberately
// skips finished stages, this clears them first so the work actually happens.
func (a *App) handleDocRetry(w http.ResponseWriter, r *http.Request) {
	id, err := docID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	doc, err := a.store.Load(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.store.ClearDerived(id); err != nil {
		http.Error(w, "clearing derived files: "+err.Error(), http.StatusInternalServerError)
		return
	}
	doc.Status = StatusProcessing
	doc.Enriched = false
	if err := a.store.Save(doc); err != nil {
		http.Error(w, "saving document: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.pipeline.EnqueueDoc(id); err != nil {
		http.Error(w, "cannot reprocess: "+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}

// handleDocEnrich re-runs just the metadata call, leaving OCR alone. Useful
// when a document was ingested while the model was unavailable or rate
// limited, without paying to redo the OCR.
func (a *App) handleDocEnrich(w http.ResponseWriter, r *http.Request) {
	id, err := docID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if a.enricher == nil {
		http.Error(w, "no model configured: set OPENAI_API_KEY", http.StatusPreconditionFailed)
		return
	}
	doc, err := a.store.Load(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	doc.Enriched = false
	if err := a.store.Save(doc); err != nil {
		http.Error(w, "saving document: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Queued rather than run inline: the request should not hang waiting on a
	// rate limit that may be hours from resetting.
	a.enrichq.Add(id)
	http.Redirect(w, r, fmt.Sprintf("/doc/%d", id), http.StatusSeeOther)
}

func (a *App) handleDocPDF(w http.ResponseWriter, r *http.Request) {
	id, err := docID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", "inline")
	http.ServeFile(w, r, a.store.ArchivePath(id))
}

func (a *App) handleDocOriginal(w http.ResponseWriter, r *http.Request) {
	id, err := docID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path, err := a.store.OriginalGlob(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := filepath.Base(path)
	if doc, err := a.store.Load(id); err == nil && doc.OriginalName != "" {
		name = doc.OriginalName
	}
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", name))
	http.ServeFile(w, r, path)
}

func (a *App) handleDocThumb(w http.ResponseWriter, r *http.Request) {
	id, err := docID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path := a.store.ThumbPath(id)
	if _, err := os.Stat(path); err != nil {
		http.ServeFileFS(w, r, assets, "static/placeholder.svg")
		return
	}
	http.ServeFile(w, r, path)
}

func (a *App) handleUploadForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, "upload.html", page{Title: "Upload", URL: r.URL})
}

// handleUpload streams each file into the inbox while hashing it, so an exact
// duplicate is reported immediately instead of being discovered later by the
// watcher.
func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "bad upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File["files"]
	var flash []Flash
	if len(files) == 0 {
		flash = []Flash{{Text: "No files selected.", Bad: true}}
	}
	for _, fh := range files {
		flash = append(flash, a.acceptUpload(r.Context(), fh))
	}

	// Drag-and-drop posts from any page and stays where it is, so it asks for
	// JSON rather than a whole rendered upload page.
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"flash": flash})
		return
	}
	a.render(w, "upload.html", page{Title: "Upload", Flash: flash, URL: r.URL})
}

// acceptUpload writes the file into the inbox while hashing it in the same
// pass, so an exact duplicate is reported to the user right away rather than
// being discovered asynchronously by the watcher.
func (a *App) acceptUpload(ctx context.Context, fh *multipart.FileHeader) Flash {
	name := filepath.Base(fh.Filename)
	fail := func(format string, args ...any) Flash {
		return Flash{Text: name + ": " + fmt.Sprintf(format, args...), Bad: true}
	}

	ext := strings.ToLower(filepath.Ext(name))
	if !SupportedExts[ext] {
		return fail("unsupported file type %q", ext)
	}

	src, err := fh.Open()
	if err != nil {
		return fail("%v", err)
	}
	defer src.Close()

	// The dot prefix keeps the watcher off it until it is fully written.
	tmp, err := os.CreateTemp(a.store.ConsumeDir(), ".upload-*")
	if err != nil {
		return fail("%v", err)
	}
	tmpName := tmp.Name()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fail("%v", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fail("%v", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))

	if id, found, err := a.search.FindByHash(ctx, sum); err != nil {
		logf("upload dedup lookup: %v", err)
	} else if found {
		os.Remove(tmpName)
		a.pipeline.recordDupe(name, id)
		return Flash{Text: fmt.Sprintf("%s is already in the archive", name), DocID: id}
	}

	dst := uniquePath(a.store.ConsumeDir(), name)
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return fail("%v", err)
	}
	// The rename fires a watcher event, so uploads and dropped files share one
	// ingest path from here on.
	return Flash{Text: fmt.Sprintf("%s queued for processing", name)}
}

// uniquePath avoids clobbering a file of the same name already waiting in the
// inbox by suffixing until the name is free.
func uniquePath(dir, name string) string {
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); err != nil {
		return candidate
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	failed, err := a.search.Query(r.Context(), Query{Status: StatusFailed})
	if err != nil {
		http.Error(w, "search unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	var spend *SpendSummary
	if a.enricher != nil {
		calls, in, out, usd := a.enricher.Spend()
		spend = &SpendSummary{Calls: calls, In: in, Out: out, USD: usd}
		if calls > 0 {
			spend.PerDoc = usd / float64(calls)
		}
		spend.Pending, spend.Done, spend.Failed = a.enrichq.Stats()
		remaining, resetIn, stopped := a.enricher.Budget()
		spend.Budget, spend.Stopped = remaining, stopped
		if resetIn > 0 {
			spend.ResetIn = resetIn.Round(time.Second).String()
		}
	}
	a.render(w, "status.html", page{
		Title:  "Status",
		Jobs:   a.pipeline.ActiveJobs(),
		Dupes:  a.pipeline.RecentDupes(20),
		Failed: failed.Hits,
		Spend:  spend,
		URL:    r.URL,
	})
}

func (a *App) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := a.search.Health(r.Context()); err != nil {
		http.Error(w, "typesense: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

func splitTags(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		t := strings.ToLower(strings.TrimSpace(part))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// parseDateTS turns a YYYY-MM document month into a sortable timestamp. A
// full YYYY-MM-DD is accepted and truncated so older sidecars still load.
func parseDateTS(s string) int64 {
	if s == "" {
		return 0
	}
	if len(s) >= 7 {
		if t, err := time.Parse("2006-01", s[:7]); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// normalizeMonth clamps any date-ish string to YYYY-MM.
func normalizeMonth(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 7 {
		if _, err := time.Parse("2006-01", s[:7]); err == nil {
			return s[:7]
		}
	}
	return ""
}
