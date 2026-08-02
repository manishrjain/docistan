package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
// Each page is parsed once and kept. html/template redoes its escape analysis
// on every parse — measured at 209us and 154KB per render on this repo — and
// the index re-fetches itself every few seconds while anything is processing,
// so this was being paid over and over for an identical result. A parsed
// template may be executed in parallel, which is what makes sharing one safe.
// Under -dev nothing is cached, so the UI can be edited without rebuilding.
func (a *App) templates(page string) (*template.Template, error) {
	if a.cfg.Dev {
		return parsePage(os.DirFS("."), page)
	}
	a.tplMu.Lock()
	defer a.tplMu.Unlock()
	if t, ok := a.tpl[page]; ok {
		return t, nil
	}
	t, err := parsePage(assets, page)
	if err != nil {
		return nil, err
	}
	if a.tpl == nil {
		a.tpl = map[string]*template.Template{}
	}
	a.tpl[page] = t
	return t, nil
}

func parsePage(src fs.FS, page string) (*template.Template, error) {
	return template.New("").Funcs(templateFuncs).
		ParseFS(src, "templates/layout.html", "templates/"+page)
}

var templateFuncs = template.FuncMap{
	"humanSize": humanSize,
	"shortDate": func(ts int64) string {
		if ts <= 0 {
			return ""
		}
		return time.Unix(ts, 0).Format("2 Jan 2006")
	},
	"joinTags": func(tags []string) string { return strings.Join(tags, ", ") },
	// When something happened, to the minute — the journal is a sequence, and
	// the order of two events within an hour is the point of reading it.
	"stamp": stamp,
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
	// The number as it is written and spoken: on the paper original, in
	// a filing box, out loud. Rendered in one place so it cannot drift.
	"docCode": docCode,
	// Counts in the chrome read as quantities, not identifiers, so they
	// get separators. Document ids deliberately do not.
	"commaNum":     commaNum,
	"usd":          usd,
	"monthOptions": monthOptions,
	"yearOptions":  yearOptions,
	"ymYear":       ymYear,
	"ymMonth":      ymMonth,
	// The file picker's accept list comes from the same table the server
	// validates against, so the two cannot disagree about what may be sent.
	"acceptedExts": acceptedExts,
}

// docCode renders a document's number for display. The id itself stays a bare
// integer everywhere it is used as an identifier — in URLs, in filenames, in
// the sidecar — because that is what makes it easy to type and to sort.
func docCode(id int) string { return "DOC-" + strconv.Itoa(id) }

// docPath is the same number as a URL. The id is a bare integer in a link for
// the same reason it is bare in a filename: it is an identifier there, not a
// label.
func docPath(id int) string { return "/doc/" + strconv.Itoa(id) }

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

// ymYear and ymMonth split a YYYY-MM back into the two selects that produced
// it, for rendering which option is currently chosen.
func ymYear(s string) string {
	if len(s) >= 4 {
		return s[:4]
	}
	return ""
}

func ymMonth(s string) string {
	if len(s) >= 7 {
		return s[5:7]
	}
	return ""
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

// centsStr formats a per-document cost. Cents rather than dollars because one
// document is a fraction of a cent, and "$0.00035" is a number nobody can read
// at a glance. Zero renders as nothing at all: a cost we cannot name — an
// unpriced model — must not be shown as free.
func centsStr(v float64) string {
	switch {
	case v == 0:
		return ""
	case v < 0.01:
		return fmt.Sprintf("%.3f¢", v)
	case v < 100:
		return fmt.Sprintf("%.2f¢", v)
	}
	return fmt.Sprintf("%.0f¢", v)
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
	mux.HandleFunc("POST /download", a.handleDownload)
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

// readStage covers getting text out of the document.
func readStage(doc *Doc) Stage {
	s := Stage{Key: "text", Name: "OCR"}
	chars := len(strings.TrimSpace(doc.Content))

	// Status wins over the content: a reprocess leaves the previous text in
	// the sidecar until the new pass replaces it, so counting characters
	// would report the step finished while it is still running.
	if doc.Status == StatusProcessing {
		s.State, s.Detail = "pending", "Working…"
		return s
	}
	if chars == 0 {
		s.Name, s.State = "OCR — no text found", "failed"
		s.Detail = "nothing to search or summarise"
		return s
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
		// Tokens from the last run, cost from every run — that is what the
		// document records, and the line says both without explaining itself.
		if doc.LLMIn > 0 {
			s.Cost = fmt.Sprintf("%s in · %s out", commaNum(doc.LLMIn), commaNum(doc.LLMOut))
			if c := centsStr(a.docCents(doc)); c != "" {
				s.Cost += " · " + c
			}
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
		if stopped, reason := a.budgetReason(); reason != "" {
			if stopped {
				s.Detail = "Stopped: " + reason
			} else {
				s.Detail = "Waiting — " + reason
			}
		}
	case slices.Contains(doc.Tags, TagNeedsReview):
		s.State = "failed"
		s.Detail = "the model call did not succeed — use Re-tag with AI"
	default:
		s.State = "skipped"
		s.Detail = "not run"
	}
	return s
}

// docCents is the cost to show beside a document. Sidecars written before the
// cost was recorded carry tokens but no cents, and a timeline that suddenly
// went silent about money on every older document would look like a bug; for
// those the current price table is the best answer available. Display only —
// it is never written back, because a price table read today is a guess at
// what a run months ago actually paid, and a guess must not become a record.
func (a *App) docCents(doc *Doc) float64 {
	if doc.LLMCents == 0 && doc.LLMIn > 0 {
		return llmCents(a.cfg.LLMModel, Usage{In: doc.LLMIn, Out: doc.LLMOut})
	}
	return doc.LLMCents
}

// budgetReason says why the model is not working right now, or "" when nothing
// is in the way. Three routes asked this and answered it three different ways,
// so the same waiting document could explain itself on one and stay silent on
// another. stopped separates "will not resume" from "not yet".
func (a *App) budgetReason() (stopped bool, reason string) {
	if a.enricher == nil {
		return false, ""
	}
	remaining, resetIn, halted := a.enricher.Budget()
	switch {
	case halted != "":
		return true, halted
	case remaining == 0 && resetIn > 0:
		return false, "rate limit resets in " + resetIn.Round(time.Second).String()
	}
	return false, ""
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
	Search  string
	Jobs    []Job
	Journal []JournalEvent
	Failed  []Hit
	Flash   []Flash
	Spend   *SpendSummary
	URL     *url.URL
}

// SpendSummary reports actual model usage rather than an estimate, alongside
// the state of the enrichment queue and the remaining request budget.
//
// Two totals, because they answer different questions. The session figures are
// process counters and reset on restart; the archive figures are summed from
// the indexed documents, so they cover every run and are what a backfill
// should be judged on.
type SpendSummary struct {
	Calls int64
	In    int64
	Out   int64
	// Priced is false when the configured model is missing from the price
	// table. The session tokens are still real and still worth showing; the
	// dollars would be a fiction, so the page shows the counts without them.
	Priced bool
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

// wantsJSON reports whether the caller is our own JavaScript rather than a
// browser following a form. Spelled once so every background action agrees on
// what counts as an asynchronous request.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logf("writing json response: %v", err)
	}
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

func (p page) TagOn(tag string) bool { return slices.Contains(p.Query.Tags, tag) }

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

func (p page) RangeOn(r string) bool {
	if r == "" {
		// All time is the default, so an unrecognised or absent value reads as
		// all time rather than leaving no segment selected.
		return !p.Query.HasDateFilter()
	}
	return p.Query.Range == r
}

func (p page) HasFilters() bool { return p.Query.HasFilters() }

// ClearFilters keeps the search term — clearing filters and clearing the
// query are separate actions, and the header has its own Clear for the query.
func (p page) ClearFilters() string {
	return p.urlWith(func(q url.Values) {
		for _, k := range []string{"tag", "status", "range", "from_y", "from_m", "to_y", "to_m", "page"} {
			q.Del(k)
		}
	})
}

// RangeLabel names the selected date window in words, for the collapsed
// control on a narrow screen.
func (p page) RangeLabel() string {
	switch {
	case p.RangeOn("month"):
		return "Last month"
	case p.RangeOn("quarter"):
		return "Last quarter"
	case p.RangeOn("year"):
		return "Last year"
	case p.RangeOn("custom"):
		return "Custom"
	}
	return "All time"
}

// FilterSummary is the whole filter bar in one line. A narrow screen has no
// room for a row of tag pills beside a five-segment date control, so both fold
// into this and open as a sheet.
func (p page) FilterSummary() string {
	tags := "All tags"
	switch n := len(p.Query.Tags); {
	case n == 1:
		tags = p.Query.Tags[0]
	case n > 1:
		tags = fmt.Sprintf("%d tags", n)
	}
	return tags + " · " + p.RangeLabel()
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
	out := slices.Clone(top)
	for _, t := range p.Query.Tags {
		if !slices.ContainsFunc(out, func(f FacetValue) bool { return f.Value == t }) {
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

// Field is one hidden input, so that submitting a form does not throw away the
// filters that form is not itself editing.
type Field struct{ Name, Value string }

// FilterFields is the current view's filters, ready to be carried through a
// submit. Both forms on the index have to re-post what they do not edit, and
// writing that list out once per form is precisely how the custom-range form
// came to omit dir: choosing a date range quietly reset the sort back to
// newest-first. One list, rendered twice, cannot drift.
//
// withDates is false for the form that edits the dates, since it posts its own.
func (p page) FilterFields(withDates bool) []Field {
	q := p.Query
	var out []Field
	add := func(name, value string) {
		if value != "" {
			out = append(out, Field{Name: name, Value: value})
		}
	}
	add("q", q.Q)
	for _, t := range q.Tags {
		add("tag", t)
	}
	add("status", q.Status)
	// The raw values, not SortField(): resolving "" to "added" here would turn
	// a relevance-ordered search into an arrival-ordered one on submit.
	add("sort", q.Sort)
	add("dir", q.Dir)
	if withDates {
		add("range", q.Range)
		add("from_y", ymYear(q.From))
		add("from_m", ymMonth(q.From))
		add("to_y", ymYear(q.To))
		add("to_m", ymMonth(q.To))
	}
	return out
}

// parseQuery reads a search out of URL or form values. The download posts the
// same names the index reads, so one parser serves both and a filter cannot
// mean one thing on screen and another in the zip.
func parseQuery(v url.Values) Query {
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
	return q
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	q := parseQuery(r.URL.Query())

	res, err := a.search.Query(r.Context(), q)
	if err != nil {
		http.Error(w, "search unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	// The placeholder says how many documents are searchable, which is only
	// the same as the result count when nothing is filtered.
	total := res.Found
	if q.Q != "" || q.HasFilters() {
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

// pathID parses the document number out of the route and answers the request
// itself when it cannot. With integer ids the parse is the validation, so a
// malformed id and a missing document are the same 404 to the caller.
func pathID(w http.ResponseWriter, r *http.Request) (int, bool) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

// loadDoc goes one step further and reads the sidecar, which is what every
// handler that intends to change a document needs. Edits apply to the sidecar
// because it, not the indexed copy, is authoritative.
func (a *App) loadDoc(w http.ResponseWriter, r *http.Request) (*Doc, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return nil, false
	}
	doc, err := a.store.Load(id)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	return doc, true
}

func (a *App) handleDoc(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
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
	doc, ok := a.loadDoc(w, r)
	if !ok {
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

	// Kept whole so the journal can say what this save changed rather than
	// only that a save happened. Doc has no reference fields other than Tags,
	// which the update replaces rather than edits in place.
	before := *doc

	// Apply only the fields this request actually carries. Assigning every
	// field from the form meant a request containing just one of them silently
	// blanked the rest — a partial save has to be a partial update, not a
	// whole-record overwrite.
	if r.PostForm.Has("title") {
		doc.Title = strings.TrimSpace(r.PostFormValue("title"))
	}
	if r.PostForm.Has("created_date") {
		doc.CreatedDate = normalizeMonth(r.PostFormValue("created_date"))
	}
	if r.PostForm.Has("tags") {
		doc.Tags = splitTags(r.PostFormValue("tags"))
	}

	// A 200 has to mean durable and queryable, and persist now delivers exactly
	// that: it writes the sidecar, then indexes, retrying each until it lands.
	// The one error it can still return is this request's context ending, so
	// there is usually nobody left to read the answer — send one anyway.
	if err := a.persist(r.Context(), doc); err != nil {
		http.Error(w, "saving document: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Only after the edit is durable, and only when it was an edit at all: the
	// page autosaves, so most of these requests change nothing.
	if detail := editDetail(&before, doc); detail != "" {
		a.journal("edited", doc.ID, "", detail)
	}

	// Autosave posts in the background and stays on the page.
	if wantsJSON(r) {
		writeJSON(w, map[string]any{
			"ok":           true,
			"title":        doc.Title,
			"tags":         doc.Tags,
			"created_date": doc.CreatedDate,
		})
		return
	}
	http.Redirect(w, r, docPath(doc.ID), http.StatusSeeOther)
}

func (a *App) handleDocDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// Read the title before the document goes, so the journal can name what was
	// deleted. The tombstone keeps it too, but a line that says only "DOC-12
	// deleted" makes the reader go looking.
	var title string
	if doc, err := a.store.LoadMeta(id); err == nil {
		title = doc.Title
	}
	if err := a.store.Delete(id); err != nil {
		http.Error(w, "deleting document: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.journal("deleted", id, "", title)
	// Removal gets the same contract as a write: the index is what every page
	// is drawn from, so a document deleted from disk but left in it is a search
	// result that 404s. Retried until Typesense accepts it, holding the request
	// the way persist does.
	//
	// The accepted edge is the client hanging up mid-retry: the ghost then
	// survives until the next start, whose replay rebuilds the index from the
	// sidecars and skips tombstones — so it goes then, and no earlier.
	if err := retryUntil(r.Context(), retryInitial, retryMax,
		fmt.Sprintf("doc %d: removing from index", id),
		func() error { return a.search.Delete(r.Context(), id) },
	); err != nil {
		logf("doc %d: gave up removing from index (%v); it stays visible until the next restart", id, err)
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleDocRetry rebuilds a document from its original: OCR, thumbnail, text
// and metadata are all redone. Unlike startup recovery, which deliberately
// skips finished stages, this clears them first so the work actually happens.
func (a *App) handleDocRetry(w http.ResponseWriter, r *http.Request) {
	doc, ok := a.loadDoc(w, r)
	if !ok {
		return
	}
	if err := a.store.ClearDerived(doc.ID); err != nil {
		http.Error(w, "clearing derived files: "+err.Error(), http.StatusInternalServerError)
		return
	}
	doc.Status = StatusProcessing
	doc.Enriched = false
	// Indexed as well as saved, in that order: the document page reads its copy
	// out of the index, so writing only the sidecar left it reporting the state
	// it was in before the reprocess was asked for. An error here means this
	// request's context ended, nothing else.
	if err := a.persist(r.Context(), doc); err != nil {
		http.Error(w, "saving document: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.pipeline.EnqueueDoc(doc.ID); err != nil {
		http.Error(w, "cannot reprocess: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.journal("reprocessed", doc.ID, "", "")
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"status": "processing"})
		return
	}
	// Without JavaScript, back to the document: the timeline there already
	// reports the work, and the status page is about the queue as a whole.
	http.Redirect(w, r, docPath(doc.ID), http.StatusSeeOther)
}

// handleDocEnrich re-runs just the metadata call, leaving OCR alone. Useful
// when a document was ingested while the model was unavailable or rate
// limited, without paying to redo the OCR.
func (a *App) handleDocEnrich(w http.ResponseWriter, r *http.Request) {
	doc, ok := a.loadDoc(w, r)
	if !ok {
		return
	}
	if a.enricher == nil {
		http.Error(w, "no model configured: set OPENAI_API_KEY", http.StatusPreconditionFailed)
		return
	}
	doc.Enriched = false
	// Durable first, then indexed, both retried until they take; only a dead
	// request context comes back as an error.
	if err := a.persist(r.Context(), doc); err != nil {
		http.Error(w, "saving document: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Queued rather than run inline: the request should not hang waiting on a
	// rate limit that may be hours from resetting.
	a.enrichq.Add(doc.ID)

	if wantsJSON(r) {
		// Say which of the three states this is, so the page can report
		// "working" versus "waiting on a limit" rather than a blank pause.
		out := map[string]any{"status": "queued"}
		if stopped, reason := a.budgetReason(); reason != "" {
			out["status"], out["reason"] = "waiting", reason
			if stopped {
				out["status"] = "blocked"
			}
		}
		writeJSON(w, out)
		return
	}
	http.Redirect(w, r, docPath(doc.ID), http.StatusSeeOther)
}

// handleDocMeta reports a document's current metadata, so a page waiting on
// background tagging can show the result without a reload.
func (a *App) handleDocMeta(w http.ResponseWriter, r *http.Request) {
	doc, ok := a.loadDoc(w, r)
	if !ok {
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
	if _, reason := a.budgetReason(); reason != "" {
		out["reason"] = reason
	}
	writeJSON(w, out)
}

// downloadName is what a browser should save this document as. The title is
// what the archive calls the document, so it is what belongs on disk; the
// original name is kept in the record but is usually a scanner serial or a
// mail attachment's throwaway name.
func downloadName(doc *Doc, ext string) string {
	name := sanitizeFilename(doc.Title)
	if name == "" {
		name = sanitizeFilename(strings.TrimSuffix(doc.OriginalName, filepath.Ext(doc.OriginalName)))
	}
	if name == "" {
		name = docCode(doc.ID)
	}
	return name + ext
}

// sanitizeFilename makes free text safe to hand a filesystem. Titles come
// from a model or from a person and contain slashes, colons and quotes; a
// long one would also exceed the 255-byte name limit on ext4.
func sanitizeFilename(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case strings.ContainsRune(`/\:*?"<>|`, r):
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	// Cut on a rune boundary, not a byte one, or a multi-byte character at
	// the limit becomes mojibake.
	const maxBytes = 120
	if len(s) > maxBytes {
		s = s[:maxBytes]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	// Windows refuses a name ending in a dot or a space.
	return strings.TrimRight(s, " .")
}

// disposition builds a Content-Disposition that survives a non-ASCII title:
// FormatMediaType emits the RFC 2231 encoded form when it needs to, which a
// hand-rolled %q never would.
func disposition(kind, name string) string {
	if v := mime.FormatMediaType(kind, map[string]string{"filename": name}); v != "" {
		return v
	}
	return kind
}

func (a *App) handleDocPDF(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// Inline, but named: the viewer's own download button and "save as" both
	// take the name from here.
	name := docCode(id) + ".pdf"
	if doc, err := a.store.LoadMeta(id); err == nil {
		name = downloadName(doc, ".pdf")
	}
	w.Header().Set("Content-Disposition", disposition("inline", name))
	http.ServeFile(w, r, a.store.ArchivePath(id))
}

func (a *App) handleDocOriginal(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(w, r); ok {
		a.serveOriginal(w, r, id)
	}
}

// serveOriginal is shared with the download route, which reaches it with an id
// it has already chosen rather than one from the path.
func (a *App) serveOriginal(w http.ResponseWriter, r *http.Request, id int) {
	path, err := a.store.OriginalGlob(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := filepath.Base(path)
	if doc, err := a.store.LoadMeta(id); err == nil {
		name = downloadName(doc, filepath.Ext(path))
	}
	w.Header().Set("Content-Disposition", disposition("attachment", name))
	http.ServeFile(w, r, path)
}

func (a *App) handleDocThumb(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	path := a.store.ThumbPath(id)
	if _, err := os.Stat(path); err != nil {
		http.ServeFileFS(w, r, assets, "static/placeholder.svg")
		return
	}
	http.ServeFile(w, r, path)
}

// maxBulkDownload bounds "download everything matching" so a stray click on an
// unfiltered archive does not start an eight-gigabyte stream. Well above any
// deliberate selection.
const maxBulkDownload = 2000

// handleDownload streams the selected documents as a zip. Either the ids the
// form posted, or — when the whole filtered set was asked for — every document
// the same filter matches, which is more than the page can show.
func (a *App) handleDownload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}

	var ids []int
	if r.PostFormValue("scope") == "filtered" {
		var err error
		if ids, err = a.search.AllIDs(r.Context(), parseQuery(r.PostForm), maxBulkDownload); err != nil {
			http.Error(w, "search unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
	} else {
		for _, v := range r.PostForm["id"] {
			if id, err := strconv.Atoi(v); err == nil && id > 0 {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		http.Error(w, "nothing selected", http.StatusBadRequest)
		return
	}

	// A single document goes out as itself. Making someone unzip one file to
	// reach one file is a rude way to answer a click.
	if len(ids) == 1 {
		a.serveOriginal(w, r, ids[0])
		return
	}
	a.streamZip(w, r, ids)
}

// streamZip writes the archive straight to the connection. Nothing is
// buffered, so the size of the selection does not become the size of a
// temporary file — but it also means the headers are gone before the first
// document is read, and a failure part way through can only be logged.
func (a *App) streamZip(w http.ResponseWriter, r *http.Request, ids []int) {
	name := fmt.Sprintf("docistan-%s.zip", time.Now().Format("2006-01-02"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", disposition("attachment", name))

	zw := zip.NewWriter(w)
	defer zw.Close()

	used := map[string]bool{}
	var written, skipped int
	for _, id := range ids {
		doc, err := a.store.LoadMeta(id)
		if err != nil || doc.Status == StatusDeleted {
			skipped++
			continue
		}
		path, err := a.store.OriginalGlob(id)
		if err != nil {
			skipped++
			continue
		}
		if err := a.addToZip(zw, doc, path, used); err != nil {
			logf("download: %s: %v", docCode(id), err)
			skipped++
			continue
		}
		written++
	}
	logf("download: %d document(s) zipped, %d skipped", written, skipped)
}

func (a *App) addToZip(zw *zip.Writer, doc *Doc, path string, used map[string]bool) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	hdr := &zip.FileHeader{
		Name: uniqueName(used, downloadName(doc, filepath.Ext(path)), doc),
		// PDFs, JPEGs and PNGs are already compressed; deflating them again
		// costs time and saves nothing. Text is worth compressing.
		Method: zip.Store,
	}
	if isTextExt(strings.ToLower(filepath.Ext(path))) {
		hdr.Method = zip.Deflate
	}
	if doc.AddedTS > 0 {
		hdr.Modified = time.Unix(doc.AddedTS, 0)
	}
	f, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, src)
	return err
}

// uniqueName keeps two documents the model gave the same title from colliding
// inside one archive, by falling back to the number that is unique by design.
func uniqueName(used map[string]bool, name string, doc *Doc) string {
	if !used[name] {
		used[name] = true
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s (%s).%s", base, docCode(doc.ID), strings.TrimPrefix(ext, "."))
		if n > 2 {
			candidate = fmt.Sprintf("%s (%s-%d).%s", base, docCode(doc.ID), n, strings.TrimPrefix(ext, "."))
		}
		if !used[candidate] {
			used[candidate] = true
			return candidate
		}
	}
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
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"flash": flash})
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
	if !isSupportedExt(ext) {
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
		a.journal("duplicate", id, name, "already in the archive")
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
		// The session figures are process counters, so they have to be priced
		// here from the model in use; the page stays in dollars because a whole
		// session's spend is a dollar-sized number.
		spend = &SpendSummary{
			Calls: calls, In: in, Out: out,
			Priced: modelPriced(a.cfg.LLMModel),
			USD:    llmCents(a.cfg.LLMModel, Usage{In: in, Out: out}) / 100,
		}
		if calls > 0 {
			spend.PerDoc = spend.USD / float64(calls)
		}
		// Summed from the documents themselves, so this covers every run the
		// archive has ever seen rather than only this one — and because each
		// document banked its own cost when it was tagged, this is recorded
		// spend rather than today's prices reapplied to old tokens.
		if allIn, allOut, allCents, docs, err := a.search.TokenTotals(r.Context()); err != nil {
			logf("token totals: %v", err)
		} else {
			spend.AllIn, spend.AllOut, spend.AllDocs = allIn, allOut, docs
			spend.AllCost = allCents / 100
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
		Title: "Status",
		Jobs:  a.pipeline.ActiveJobs(),
		// Bounded at both ends: this page is polled every few seconds while
		// anything is processing, so it reads the tail of the file rather than
		// the file.
		Journal: readJournalTail(a.store.JournalPath(), journalTailBytes, 50),
		Failed:  failed.Hits,
		Spend:   spend,
		URL:     r.URL,
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
