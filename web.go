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
		"add": func(a, b int) int { return a + b },
		// Counts in the chrome read as quantities, not identifiers, so they
		// get separators. Document ids deliberately do not.
		"commaNum":     commaNum,
		"usd":          usd,
		"monthOptions": monthOptions,
		"yearOptions":  yearOptions,
		"ymYear": func(s string) string {
			if len(s) >= 4 {
				return s[:4]
			}
			return ""
		},
		"ymMonth": func(s string) string {
			if len(s) >= 7 {
				return s[5:7]
			}
			return ""
		},
	}
}

// stamp is the timeline's date format: precise to the minute, because the
// point of "Landed" is when this arrived relative to everything else that day.
func stamp(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).Format("2 Jan 2006 · 15:04")
}

// Option is one entry of a <select>, kept so the date-range selects can be
// built in the template without string surgery there.
type Option struct{ Value, Label string }

func monthOptions() []Option {
	out := make([]Option, 0, 12)
	for m := 1; m <= 12; m++ {
		t := time.Date(2000, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
		out = append(out, Option{Value: fmt.Sprintf("%02d", m), Label: t.Format("January")})
	}
	return out
}

// yearOptions offers a fixed window back from this year. Deriving it from the
// oldest document would cost a query on every page render for a list nobody
// scrolls to the end of.
func yearOptions() []string {
	const span = 15
	now := time.Now().Year()
	out := make([]string, 0, span+1)
	for y := now; y >= now-span; y-- {
		out = append(out, strconv.Itoa(y))
	}
	return out
}

// joinYM composes the YYYY-MM a date filter works in from the two selects the
// form actually posts. Either half missing means no bound.
func joinYM(year, month string) string {
	if len(year) != 4 || len(month) != 2 {
		return ""
	}
	return normalizeMonth(year + "-" + month)
}

// commaNum takes any so templates can pass either an int count or an int64
// token total without a conversion helper at every call site.
func commaNum(v any) string {
	var n int64
	switch t := v.(type) {
	case int:
		n = int64(t)
	case int64:
		n = t
	default:
		return fmt.Sprint(v)
	}
	if n < 0 {
		return "-" + commaNum(-n)
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// usd formats a cost that is usually a small fraction of a cent, without
// rounding it away to $0.00 and without trailing noise on larger totals.
func usd(v float64) string {
	switch {
	case v == 0:
		return "$0"
	case v < 0.01:
		return fmt.Sprintf("$%.5f", v)
	case v < 1:
		return fmt.Sprintf("$%.4f", v)
	}
	return fmt.Sprintf("$%.2f", v)
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
	mux.HandleFunc("GET /doc/{id}/meta", a.handleDocMeta)
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
	Key   string `json:"key"` // stable id so the page can update a row in place
	Name  string `json:"name"`
	State string `json:"state"` // done | pending | skipped | failed
	// Cost and File each get their own line under the label when set: the
	// tokens a step spent, and the filename a document arrived under. Detail
	// is the last line, carrying when the step finished.
	Cost   string `json:"cost,omitempty"`
	File   string `json:"file,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// stagesFor reconstructs what happened to a document from what is on disk and
// in the sidecar, rather than from a stored log, so it cannot drift.
//
// Three steps: the document arrives, it gets read, and the model describes it.
// The finer stages the pipeline actually runs — the archival conversion, the
// thumbnail, the index write — are either self-evident from the page or only
// interesting when they fail, and a failure names its own stage in the banner
// above this list.
func (a *App) stagesFor(doc *Doc) []Stage {
	return []Stage{
		landedStage(doc),
		readStage(doc),
		a.taggingStage(doc),
	}
}

func landedStage(doc *Doc) Stage {
	return Stage{
		Key: "landed", Name: "Landed", State: "done",
		// What the file was called before it became a number. Boxed rather
		// than run into the line, because scanner filenames are long and
		// unpunctuated and would otherwise read as prose.
		File:   doc.OriginalName,
		Detail: stamp(doc.AddedTS),
	}
}

// when puts the step's own timestamp first and what did the work second.
// Documents ingested before the timestamps were recorded simply have no time
// to show, rather than borrowing the arrival time and stating something false.
func when(ts int64, by string) string {
	t := stamp(ts)
	switch {
	case t == "":
		return by
	case by == "":
		return t
	}
	return t + " · " + by
}

// readStage covers getting text out of the document. The design labels this
// "OCR — 11 pages, 99.2% confidence"; we do not score OCR, so the second
// figure is the amount of text recovered, and the line beneath says which
// tool produced it — which is the question the confidence number stands in
// for: how far to trust this text.
func readStage(doc *Doc) Stage {
	s := Stage{Key: "text", Name: "OCR"}
	chars := len(strings.TrimSpace(doc.Content))

	// Status wins over the content: a reprocess leaves the previous text in
	// the sidecar until the new pass replaces it, so counting characters
	// would report the step finished while it is still running.
	if doc.Status == StatusProcessing {
		return Stage{Key: "text", Name: "OCR", State: "pending", Detail: "Working…"}
	}
	if chars == 0 {
		return Stage{Key: "text", Name: "OCR — no text found", State: "failed",
			Detail: "nothing to search or summarise"}
	}

	s.State = "done"
	var parts []string
	if doc.PageCount > 0 {
		parts = append(parts, fmt.Sprintf("%d page%s", doc.PageCount, plural(doc.PageCount)))
	}
	parts = append(parts, commaNum(chars)+" characters")
	s.Name = "OCR — " + strings.Join(parts, ", ")

	var by string
	switch {
	case doc.NativeText:
		by = "existing text layer"
	case doc.OCRSource == OCRTesseract:
		by = "tesseract"
	case doc.OCRSource == OCRSkippedSigned:
		by = "signed, left as-is"
	case doc.OCRSource == OCRLLM:
		by = "read by the model"
	}
	s.Detail = when(doc.TextTS, by)
	return s
}

// taggingStage keeps one fixed label and says what is happening underneath it,
// the way the design does — the step is the same step whether it is queued,
// running or finished.
func (a *App) taggingStage(doc *Doc) Stage {
	s := Stage{Key: "tagging", Name: "AI summary + tags"}
	switch {
	case doc.Enriched:
		s.State = "done"
		s.Name += " — " + a.cfg.LLMModel
		if doc.LLMIn > 0 {
			s.Cost = fmt.Sprintf("%s in · %s out · %s",
				commaNum(doc.LLMIn), commaNum(doc.LLMOut),
				usd(a.cfg.LLMCost(doc.LLMIn, doc.LLMOut)))
		}
		s.Detail = stamp(doc.EnrichedTS)
	case doc.Status == StatusProcessing:
		// Not queued yet — the pipeline enqueues it once the text is back.
		s.State = "pending"
		s.Detail = "Queued"
	case a.enricher == nil:
		s.State = "skipped"
		s.Detail = "no model configured — set OPENAI_API_KEY"
	case a.enrichq != nil && a.enrichq.Has(doc.ID):
		s.State = "pending"
		s.Detail = "Queued"
		if _, resetIn, stopped := a.enricher.Budget(); stopped != "" {
			s.Detail = "Stopped: " + stopped
		} else if resetIn > 0 {
			if remaining, _, _ := a.enricher.Budget(); remaining == 0 {
				s.Detail = "Waiting — rate limit resets in " + resetIn.Round(time.Second).String()
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
	Total     int // documents in the index, for the search placeholder
	Doc       *Doc
	Stages    []Stage
	KnownTags []string
	// Search carries the term that led here, so the PDF viewer can jump
	// straight to it instead of making the reader find it twice.
	Search string
	Jobs   []Job
	Dupes  []DupeEvent
	Failed []Hit
	Flash  []Flash
	Spend  *SpendSummary
	URL    *url.URL
}

// SpendSummary reports actual model usage rather than an estimate, alongside
// the state of the enrichment queue and the remaining request budget.
//
// Two totals, because they answer different questions. The session figures are
// process counters and reset on restart; the archive figures are summed from
// the indexed documents, so they cover every run and are what a backfill
// should be judged on.
type SpendSummary struct {
	Calls  int64
	In     int64
	Out    int64
	USD    float64
	PerDoc float64

	AllIn   int64
	AllOut  int64
	AllDocs int
	AllCost float64
	AllPer  float64

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

// BrowserTitle is what the tab says. The index carries the size of the
// archive, which is the one number worth seeing without looking at the page;
// everywhere else names the page and then the app, in that order, because a
// row of tabs truncates from the right.
func (p page) BrowserTitle() string {
	if p.Title != "" {
		return p.Title + " · Docistan"
	}
	// A search gets its terms in the tab. The count belongs to the archive,
	// not to the page, and reading "4 docs" above three results is a small
	// lie that costs nothing to avoid.
	if q := strings.TrimSpace(p.Query.Q); q != "" {
		return fmt.Sprintf("\u201c%s\u201d · Docistan", q)
	}
	if p.Total > 0 {
		return fmt.Sprintf("Docistan — %s doc%s", commaNum(p.Total), plural(p.Total))
	}
	return "Docistan"
}

// urlWith rebuilds the current index URL with the query string edited in
// place, which is how every filter link preserves what else was selected.
func (p page) urlWith(mutate func(url.Values)) string {
	q := url.Values{}
	if p.URL != nil {
		q = p.URL.Query()
	}
	mutate(q)
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

// WithParam replaces one parameter. Any change other than paging returns to
// page one, since the old page number rarely means anything afterwards.
func (p page) WithParam(key, value string) string {
	return p.urlWith(func(q url.Values) {
		if value == "" {
			q.Del(key)
		} else {
			q.Set(key, value)
		}
		if key != "page" {
			q.Del("page")
		}
	})
}

// ToggleTag adds or removes one tag from the filter. Tags accumulate rather
// than replace, so each click narrows further.
func (p page) ToggleTag(tag string) string {
	return p.urlWith(func(q url.Values) {
		var next []string
		found := false
		for _, t := range q["tag"] {
			if t == tag {
				found = true
				continue
			}
			next = append(next, t)
		}
		if !found {
			next = append(next, tag)
		}
		if len(next) == 0 {
			q.Del("tag")
		} else {
			q["tag"] = next
		}
		q.Del("page")
	})
}

func (p page) TagOn(tag string) bool { return hasTag(p.Query.Tags, tag) }

// BackLink returns to the search that led here, so "All results" means the
// results you actually came from rather than an unfiltered list.
func (p page) BackLink() string {
	if p.Search == "" {
		return "/"
	}
	return "/?" + url.Values{"q": {p.Search}}.Encode()
}

func (p page) WithRange(r string) string { return p.WithParam("range", r) }

// --- sorting -------------------------------------------------------------
// Two controls rather than one list: which field to order by, and which end
// to start from. Combining them into "newest / oldest / document date" made
// the two questions look like one, and left no way to ask for the oldest
// document date at all.

func (p page) SortOn(field string) bool { return p.Query.SortField() == field }

// WithSort keeps the current direction, so switching fields does not silently
// flip the order under you.
func (p page) WithSort(field string) string { return p.WithParam("sort", field) }

func (p page) ToggleDir() string {
	if p.Query.Descending() {
		return p.WithParam("dir", "asc")
	}
	return p.WithParam("dir", "")
}

// DirLabel names what you are looking at, not what the click would do — the
// arrow shows the current order the way a sorted table header does.
func (p page) DirLabel() string {
	if p.Query.Descending() {
		return "↓ Newest first"
	}
	return "↑ Oldest first"
}

// DirArrow is the same control with no room for words. Both are rendered and
// CSS picks one, so the choice follows the viewport rather than a guess made
// on the server.
func (p page) DirArrow() string {
	if p.Query.Descending() {
		return "↓"
	}
	return "↑"
}

// IsLatest reports the plain default view: everything, newest arrival first.
// It is the only state the design's "Latest additions" heading is true for.
func (p page) IsLatest() bool {
	return p.Query.Q == "" && !p.HasFilters() &&
		p.Query.SortField() == "added" && p.Query.Descending()
}

func (p page) RangeOn(r string) bool {
	if r == "" {
		// All time is the default, so an unrecognised or absent value reads as
		// all time rather than leaving no segment selected.
		return !p.Query.HasDateFilter()
	}
	return p.Query.Range == r
}

func (p page) HasFilters() bool {
	return len(p.Query.Tags) > 0 || p.Query.Status != "" || p.Query.HasDateFilter()
}

// ClearFilters keeps the search term — clearing filters and clearing the
// query are separate actions, and the header has its own Clear for the query.
func (p page) ClearFilters() string {
	return p.urlWith(func(q url.Values) {
		for _, k := range []string{"tag", "status", "range", "from_y", "from_m", "to_y", "to_m", "page"} {
			q.Del(k)
		}
	})
}

// topTagCount is how many tag pills sit in the filter bar before the rest
// move behind the browser.
const topTagCount = 6

// TagFacets is the full tag vocabulary of the current result set, ordered by
// how many documents carry each. Filtering narrows it, which is deliberate:
// every tag offered leads somewhere.
func (p page) TagFacets() []FacetValue {
	if p.Result == nil {
		return nil
	}
	return p.Result.Facets["tags"]
}

// TopTags is what the filter bar shows without opening the browser: the most
// common tags, plus any selected tag that would otherwise be hidden inside it.
func (p page) TopTags() []FacetValue {
	all := p.TagFacets()
	top := all
	if len(top) > topTagCount {
		top = top[:topTagCount]
	}
	out := append([]FacetValue(nil), top...)
	for _, t := range p.Query.Tags {
		shown := false
		for _, f := range out {
			if f.Value == t {
				shown = true
				break
			}
		}
		if !shown {
			out = append(out, FacetValue{Value: t})
		}
	}
	return out
}

// MoreTags counts what the browser adds beyond the pills already on screen.
func (p page) MoreTags() int {
	n := len(p.TagFacets()) - topTagCount
	if n < 0 {
		return 0
	}
	return n
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	q := Query{
		Q:      v.Get("q"),
		Tags:   v["tag"],
		Status: v.Get("status"),
		Sort:   v.Get("sort"),
		Dir:    v.Get("dir"),
		Range:  v.Get("range"),
		From:   joinYM(v.Get("from_y"), v.Get("from_m")),
		To:     joinYM(v.Get("to_y"), v.Get("to_m")),
	}
	q.Page, _ = strconv.Atoi(v.Get("page"))

	// Sorting used to be one list where "oldest" meant oldest-by-arrival.
	// Keep old links meaning what they meant.
	if q.Sort == "oldest" {
		q.Sort, q.Dir = "added", "asc"
	}

	// Picking "Custom" for the first time should open on a usable window
	// rather than on two empty selects that filter nothing.
	if q.Range == "custom" {
		now := time.Now()
		if q.From == "" {
			q.From = now.AddDate(-1, 0, 0).Format("2006-01")
		}
		if q.To == "" {
			q.To = now.Format("2006-01")
		}
	}

	res, err := a.search.Query(r.Context(), q)
	if err != nil {
		http.Error(w, "search unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	// The placeholder says how many documents are searchable, which is only
	// the same as the result count when nothing is filtered.
	total := res.Found
	if q.Q != "" || len(q.Tags) > 0 || q.Status != "" || q.HasDateFilter() {
		if n, err := a.search.Count(r.Context()); err == nil {
			total = n
		}
	}
	a.render(w, "index.html", page{
		Query:  q,
		Result: res,
		Total:  total,
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
		KnownTags: known, Search: r.URL.Query().Get("q"), URL: r.URL,
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
	// ParseForm ignores multipart bodies, leaving every value empty — which,
	// combined with assigning fields unconditionally, is how a save could wipe
	// a record. Pick the parser that matches the encoding.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Apply only the fields this request actually carries. Assigning every
	// field from the form meant a request containing just one of them silently
	// blanked the rest — a partial save has to be a partial update, not a
	// whole-record overwrite.
	if r.PostForm.Has("title") {
		doc.Title = strings.TrimSpace(r.PostFormValue("title"))
	}
	if r.PostForm.Has("created_date") {
		doc.CreatedDate = normalizeMonth(r.PostFormValue("created_date"))
		doc.CreatedTS = parseDateTS(doc.CreatedDate)
	}
	if r.PostForm.Has("tags") {
		doc.Tags = splitTags(r.PostFormValue("tags"))
	}

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
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "processing"})
		return
	}
	// Without JavaScript, back to the document: the timeline there already
	// reports the work, and the status page is about the queue as a whole.
	http.Redirect(w, r, fmt.Sprintf("/doc/%d", id), http.StatusSeeOther)
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

	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		// Say which of the three states this is, so the page can report
		// "working" versus "waiting on a limit" rather than a blank pause.
		out := map[string]any{"status": "queued"}
		remaining, resetIn, stopped := a.enricher.Budget()
		switch {
		case stopped != "":
			out["status"], out["reason"] = "blocked", stopped
		case remaining == 0 && resetIn > 0:
			out["status"] = "waiting"
			out["reason"] = "rate limit resets in " + resetIn.Round(time.Second).String()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/doc/%d", id), http.StatusSeeOther)
}

// handleDocMeta reports a document's current metadata, so a page waiting on
// background tagging can show the result without a reload.
func (a *App) handleDocMeta(w http.ResponseWriter, r *http.Request) {
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
	out := map[string]any{
		"id":           doc.ID,
		"title":        doc.Title,
		"tags":         doc.Tags,
		"created_date": doc.CreatedDate,
		"summary":      doc.Summary,
		"status":       doc.Status,
		"enriched":     doc.Enriched,
		"queued":       a.enrichq != nil && a.enrichq.Has(doc.ID),
		// The rendered steps, so a watching page shows exactly what a reload
		// would rather than a second implementation of the same labels.
		"stages": a.stagesFor(doc),
	}
	if a.enricher != nil {
		if _, resetIn, stopped := a.enricher.Budget(); stopped != "" {
			out["reason"] = stopped
		} else if resetIn > 0 {
			out["reason"] = "rate limit resets in " + resetIn.Round(time.Second).String()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
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
		calls, in, out := a.enricher.Spend()
		spend = &SpendSummary{Calls: calls, In: in, Out: out, USD: a.cfg.LLMCost(in, out)}
		if calls > 0 {
			spend.PerDoc = spend.USD / float64(calls)
		}
		// Summed from the documents themselves, so this covers every run the
		// archive has ever seen rather than only this one.
		if allIn, allOut, docs, err := a.search.TokenTotals(r.Context()); err != nil {
			logf("token totals: %v", err)
		} else {
			spend.AllIn, spend.AllOut, spend.AllDocs = allIn, allOut, docs
			spend.AllCost = a.cfg.LLMCost(allIn, allOut)
			if docs > 0 {
				spend.AllPer = spend.AllCost / float64(docs)
			}
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
