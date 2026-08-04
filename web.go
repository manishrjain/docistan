package main

import (
	"archive/zip"
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
	mux.HandleFunc("GET /results", a.handleResults)
	mux.HandleFunc("GET /doc/{id}", a.handleDoc)
	mux.HandleFunc("POST /doc/{id}", a.handleDocUpdate)
	mux.HandleFunc("POST /doc/{id}/delete", a.handleDocDelete)
	mux.HandleFunc("POST /doc/{id}/trash", a.handleDocTrash)
	mux.HandleFunc("POST /doc/{id}/restore", a.handleDocRestore)
	mux.HandleFunc("POST /docs/action", a.handleDocsAction)
	mux.HandleFunc("POST /doc/{id}/retry", a.handleDocRetry)
	mux.HandleFunc("POST /doc/{id}/unlock", a.handleDocUnlock)
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

// sameOrigin reports whether a state-changing request came from this site.
//
// An authenticating proxy answers who is making a request and never whether
// they meant to make it. Once a browser holds a session for this archive, any
// page anywhere can post to /doc/5/delete and the browser will attach that
// session — the proxy sees a signed-in reader and waves it through. That was
// not a risk while nothing here had any authority to forge; it arrives with the
// login, not before it, which is why it belongs in the same change.
//
// Sec-Fetch-Site is the answer where it exists, and it needs no configuration:
// the browser states the relationship itself and a page cannot lie about it.
// same-site is refused along with cross-site — this archive is one host, so a
// neighbouring subdomain has no business posting to it.
//
// Origin is the fallback for browsers too old to send it, and is only usable
// with -public-origin: behind a proxy the request arrives with Host
// 127.0.0.1:8080 while Origin names the real site, so comparing the two would
// reject every genuine request.
//
// Neither header means this is not a browser — curl, a script, a health check.
// That is allowed on purpose, and it is not the hole it looks like: forging a
// request in someone else's browser is the whole attack, and no browser can be
// made to leave both headers off.
func sameOrigin(r *http.Request, public string) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "cross-site", "same-site":
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" || public == "" {
		return true
	}
	return strings.EqualFold(strings.TrimSuffix(origin, "/"), strings.TrimSuffix(public, "/"))
}

// guard wraps the whole mux rather than the handlers that change things. The
// list of those is twelve long today and every future one would have to
// remember to join it; wrapping the router means a new route is covered by
// existing code rather than by whoever writes it.
func guard(next http.Handler, public string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if !sameOrigin(r, public) {
				// No detail: the page that sent this is not one of ours, and
				// what it learns from the reply should be nothing.
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
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
	out := []Stage{landedStage(doc)}
	// Only documents that arrived locked have anything to say here, which is
	// why this is a step rather than a fourth fixed row: on every other
	// document there was no lock and nothing happened.
	if s, ok := unlockStage(doc); ok {
		out = append(out, s)
	}
	return append(out, readStage(doc), a.taggingStage(doc))
}

// unlockStage is what happened to a password. It sits between arriving and
// being read because that is where it happened — nothing could be read until
// the file opened — and because a timeline is where someone looks to find out
// why a document is not what they sent.
//
// This used to be a banner at the top of the page, which put a paragraph about
// checksums above a document whose owner wanted to read it. It is the same two
// facts either way; a step states them where they belong, in the order they
// occurred.
func unlockStage(doc *Doc) (Stage, bool) {
	// A step is a line in a list, not a paragraph. Each of these says what
	// happened and then the one fact that outlives it; anything more belongs on
	// whatever page explains the archive, not on top of a document someone has
	// just unlocked in order to read.
	switch {
	case doc.Locked():
		// Still locked: the step names where the pipeline stopped. Without it
		// the timeline claims the text step failed, when the truth is that it
		// never ran — nothing had opened the file for it to read.
		return Stage{
			Key: "unlock", Name: "Locked — no password opened this file", State: "failed",
			Detail: "Add the password above",
		}, true
	case doc.Encrypted && doc.Signed:
		// The one document that keeps its lock. qpdf --decrypt rewrites the
		// file, and a rewritten PDF no longer matches the signature computed
		// over it, so here the archive copy is the decrypted one.
		return Stage{
			Key: "unlock", Name: "Unlocked — archive copy decrypted", State: "done",
			Detail: "Original kept encrypted to preserve its signature",
		}, true
	case doc.Encrypted:
		return Stage{
			Key: "unlock", Name: "Unlocked — password removed from original", State: "done",
			Detail: "Original checksum recorded",
		}, true
	}
	return Stage{}, false
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
//
// The step is named after whatever actually produced the text. Calling it OCR
// regardless was a small lie on every document that carried its own text
// layer, and a flat contradiction on a signed one: the page says the signature
// was preserved and the document therefore not OCR'd, directly above a step
// headed OCR reporting three thousand characters. Those characters are real —
// pdftotext read them out of the PDF — but no OCR ran to find them.
func readStage(doc *Doc) Stage {
	s := Stage{Key: "text", Name: readVerb(doc)}
	chars := len(strings.TrimSpace(doc.Content))

	// Status wins over the content: a reprocess leaves the previous text in
	// the sidecar until the new pass replaces it, so counting characters
	// would report the step finished while it is still running.
	if doc.Status == StatusProcessing {
		s.State, s.Detail = "pending", "Working…"
		return s
	}
	if chars == 0 {
		s.Name, s.State = s.Name+" — no text found", "failed"
		s.Detail = "nothing to search or summarise"
		return s
	}

	s.State = "done"
	var parts []string
	if doc.PageCount > 0 {
		parts = append(parts, fmt.Sprintf("%d page%s", doc.PageCount, plural(doc.PageCount)))
	}
	parts = append(parts, commaNum(chars)+" characters")
	s.Name += " — " + strings.Join(parts, ", ")

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

// readVerb names the step after the tool that did the work. Only tesseract is
// OCR; a PDF that arrived with a text layer — including every signed one,
// which is deliberately left untouched — was simply read.
func readVerb(doc *Doc) string {
	if doc.OCRSource == OCRTesseract && !doc.NativeText {
		return "OCR"
	}
	if doc.OCRSource == OCRLLM {
		return "Read by the model"
	}
	return "Text"
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
	// Reserved is how big each reserved slice of the archive is, for the counts
	// on the three pills that lead the tag row. A value rather than a pointer: a
	// lookup that failed leaves zeros, which is a row of tags that renders
	// rather than a page that does not.
	Reserved ReservedCounts
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
		return p.Title + " · Docovia"
	}
	// A search gets its terms in the tab. The count belongs to the archive,
	// not to the page, and reading "4 docs" above three results is a small
	// lie that costs nothing to avoid.
	if q := strings.TrimSpace(p.Query.Q); q != "" {
		return fmt.Sprintf("\u201c%s\u201d · Docovia", q)
	}
	if p.Total > 0 {
		return fmt.Sprintf("Docovia — %s doc%s", commaNum(p.Total), plural(p.Total))
	}
	return "Docovia"
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

// InTrash decides which bulk actions the selection offers and how an empty
// listing explains itself. Asked of the page rather than of the query, because
// every template that needs it has the page in hand.
func (p page) InTrash() bool { return p.TagOn(TagTrash) }

// ReservedPill is one of the three system tags as the tag row draws it: the
// same pill a tag gets, with a count that is of the archive rather than of the
// results and a flag to say it was not written by a person.
type ReservedPill struct {
	Tag   string
	Label string
	Count int
	On    bool
}

// ReservedPills is what leads the tag row. A pill appears when it has documents
// behind it, or when it is the filter currently on — otherwise fixing the last
// locked document would take the pill away under the reader while they are
// still looking at it, and leave them no way back.
func (p page) ReservedPills() []ReservedPill {
	out := make([]ReservedPill, 0, 3)
	for _, r := range []ReservedPill{
		{Tag: TagLocked, Label: "Locked", Count: p.Reserved.Locked},
		{Tag: TagFailed, Label: "Failed", Count: p.Reserved.Failed},
		{Tag: TagTrash, Label: "Trash", Count: p.Reserved.Trash},
	} {
		r.On = p.TagOn(r.Tag)
		if r.Count == 0 && !r.On {
			continue
		}
		out = append(out, r)
	}
	return out
}

// UnlockFailed reports whether the reader has just been told the wrong
// password. It travels in the URL because the answer is a redirect — the
// password was posted, and a page rendered straight from that POST is a page
// whose reload offers to send it again.
//
// A flag and not the guess itself: nothing that was typed into that field ever
// comes back to the browser.
func (p page) UnlockFailed() bool {
	return p.URL != nil && p.URL.Query().Get("unlock") == "bad"
}

// BackLink returns to the listing that led here, so "All results" means the
// results you actually came from rather than an unfiltered list.
//
// Built from FilterFields, the same list the forms post and the same one
// indexURL uses, because the way back and the way in have to agree. Carrying
// only q — which is what this did — was not a smaller version of that: it
// dropped the tags, the date range and the sort, so following a tag into a
// document and pressing back landed on the whole archive.
//
// The page number is added here and is deliberately not in FilterFields: a form
// that changes a filter should return to the first page of the new results,
// while this should return to the page you were actually reading.
func (p page) BackLink() string {
	v := url.Values{}
	for _, f := range p.FilterFields(true) {
		v.Add(f.Name, f.Value)
	}
	if p.Query.Page > 1 {
		v.Set("page", strconv.Itoa(p.Query.Page))
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

// DocLink is the way in: a result row carrying the listing it was on, so the
// document page can offer the way back. Same list again — a row that carried
// less than BackLink reads would make the return trip lossy no matter how
// carefully BackLink was written.
func (p page) DocLink(id int) string {
	v := url.Values{}
	for _, f := range p.FilterFields(true) {
		v.Add(f.Name, f.Value)
	}
	if p.Query.Page > 1 {
		v.Set("page", strconv.Itoa(p.Query.Page))
	}
	if len(v) == 0 {
		return "/doc/" + strconv.Itoa(id)
	}
	return "/doc/" + strconv.Itoa(id) + "?" + v.Encode()
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

// ClearTags drops the tag selection alone. It backs the Clear that sits among
// the tag pills, so it clears what it stands beside; the date range is reached
// from its own control, and having this take that away too meant a reader who
// wanted their tags back lost the window they had chosen as well.
func (p page) ClearTags() string {
	return p.urlWith(func(q url.Values) {
		q.Del("tag")
		q.Del("page")
	})
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
//
// The reserved names come back in this facet like any other tag and are taken
// out here, at the one place the browser, the pills and the "+ N more" count
// all read from — they have their own pills at the head of the row, with
// archive-wide counts, and offering them twice would be offering two different
// numbers for the same thing. Cloned because DeleteFunc writes through to the
// slice it is given, and this is called several times per render.
func (p page) TagFacets() []FacetValue {
	if p.Result == nil {
		return nil
	}
	return slices.DeleteFunc(slices.Clone(p.Result.Facets["tags"]),
		func(f FacetValue) bool { return isReserved(f.Value) })
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
		if isReserved(t) {
			// Selected, but its pill is already at the head of the row.
			continue
		}
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
	// The reserved tags are in here with the rest, which is what keeps a change
	// of sort order from quietly moving you out of the trash: they are carried
	// because they are tags, not because anything remembered to carry them.
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

// SearchBoxFields is the same list for the search box, which owns q: the
// visible input carries the term, so a hidden input for it as well would post
// the name twice and leave the word being typed arguing with the word that was
// already there. Everything else rides along, dates included — the box edits
// none of it — and search.js builds its request out of this form's own
// FormData, so typing and pressing Enter cannot come to mean different
// searches.
//
// A method of its own rather than a second boolean on FilterFields: three forms
// on the index post the filters now, and FilterFields(true, false) at a call
// site says nothing about which of them is which.
func (p page) SearchBoxFields() []Field {
	return slices.DeleteFunc(p.FilterFields(true), func(f Field) bool { return f.Name == "q" })
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
	// The counts are the archive's, not this page's, so they are asked for
	// separately. A failure here leaves zeros rather than a broken page: the
	// results themselves are already rendered by the time it matters.
	reserved, err := a.search.ReservedCounts(r.Context())
	if err != nil {
		logf("reserved tag counts: %v", err)
	}
	// The placeholder says how many documents are searchable, which is only the
	// same as the result count when nothing is filtered. res.Found is the
	// fallback if that lookup failed.
	total := res.Found
	if reserved.Archive > 0 {
		total = reserved.Archive
	}

	a.render(w, "index.html", page{
		Query:    q,
		Result:   res,
		Total:    total,
		Reserved: reserved,
		Flash:    actionNotice(r.URL.Query()),
		URL:      r.URL,
	})
}

// handleResults renders the regions that move with the search text and nothing
// else, for search as you type. The rows carry marked-up snippets, tag pills
// that are filter links and selection state; rebuilding those in JavaScript
// would mean a second copy of the rendering and — worse — a second copy of the
// highlight escaping, which is what keeps document text out of the page's
// markup. So the server renders the same blocks it renders inside the page, and
// the browser swaps them in.
//
// The tag facets are among those regions. They narrow with the result set and
// arrive on this very response — Typesense counts them alongside the hits — so
// leaving them out was leaving counts on screen for a result set that had been
// replaced, at the cost of nothing saved.
//
// Everything about the query is read by parseQuery and built into a page
// exactly as handleIndex does it, so the two cannot come to disagree about what
// a filter means.
func (a *App) handleResults(w http.ResponseWriter, r *http.Request) {
	q := parseQuery(r.URL.Query())

	res, err := a.search.Query(r.Context(), q)
	if err != nil {
		http.Error(w, "search unavailable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}

	tpl, err := a.templates("index.html")
	if err != nil {
		http.Error(w, "template: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// No ReservedCounts: those counts are of the whole archive and do not move
	// with the search text, so asking for them on every keystroke would be four
	// round trips spent to render the numbers already on the pills — and those
	// pills deliberately sit outside the regions below, which is what lets this
	// leave them alone.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "regions", page{
		Query:  q,
		Result: res,
		URL:    r.URL,
	}); err != nil {
		logf("render regions: %v", err)
	}
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
//
// A tombstone is not a document and is answered as missing. It is a record that
// something was destroyed: its files are gone, so a deleted document is not
// there to be edited, retried, enriched, trashed or restored, and every one of
// those handlers reaches its document through here. This is what makes them
// agree with the document page, which already 404s because it reads the index
// via search.Get and tombstones are not in it. Without it a stale tab could
// post to any of them and put a purged document back into the listing —
// findable, restorable, and 404 on its own PDF.
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
	if doc.Gone() {
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
	// vocabulary from filling with near-duplicates. It is a facet count of the
	// index, so the reserved names are in it and have to come out: the box below
	// would offer them, and the save would then silently drop what it suggested.
	known, err := a.search.Vocabulary(r.Context(), "tags", 200)
	if err != nil {
		logf("doc %d: reading tag vocabulary: %v", id, err)
	}
	known = withoutReserved(known)
	// The listing this document was opened from, parsed back out of the URL the
	// row carried. Only BackLink reads it — the document itself is the same
	// document however you arrived at it — but without it "All results" has
	// nothing to return to but the whole archive.
	a.render(w, "doc.html", page{
		Title: doc.Title, Doc: doc, Stages: a.stagesFor(doc),
		Query:     parseQuery(r.URL.Query()),
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
		// Typing "trash" into the tag box must not fake a reserved tag. The
		// index derives those from the document's own state, so a sidecar
		// carrying one would either be ignored or, worse, be believed.
		doc.Tags = withoutReserved(splitTags(r.PostFormValue("tags")))
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
		a.record("edited", doc.ID, "", detail)
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

// Why a document was destroyed, for the journal line that outlives it. The two
// are not the same event to whoever reads that line later: one is a person
// having decided, the other is a deadline having passed.
const (
	purgeByHand    = "delete forever"
	purgeBySweeper = "trash expired"
)

// errGone is purge refusing to destroy what is already destroyed. A sentinel
// rather than a log line, because the two callers answer it differently: the
// single-document route turns it into the 404 the rest of the site gives for a
// tombstone, and the bulk action counts it as one id of a selection that did
// not move.
var errGone = errors.New("document is already deleted")

// purge is permanent deletion, shared by the three ways to reach it: the
// document page, the bulk action, and the sweeper. Files go, the sidecar
// becomes a tombstone so the id stays spoken for, and the index entry is
// removed.
//
// The event name is the caller's because these are not one act: a person
// deleting one document by hand is "deleted", the way it always was, while the
// trash emptying itself is "purged". why says which flavour of purge it was, or
// is empty for the plain delete that needs no explanation.
func (a *App) purge(ctx context.Context, id int, event, why string) error {
	// Read the title before the document goes, so the journal can name what was
	// deleted. The tombstone keeps it too, but a line that says only "DOC-12
	// deleted" makes the reader go looking.
	var title string
	if doc, err := a.store.LoadMeta(id); err == nil {
		// Already a tombstone, so there is nothing left to destroy. Running the
		// delete again would rewrite it — losing the time of the real deletion —
		// and journal a second deletion of a document that has been gone for
		// weeks. The read that fetches the title carries the status, so asking
		// costs nothing, and this is the only way into Store.Delete.
		if doc.Gone() {
			return errGone
		}
		title = doc.Title
	}
	if err := a.store.Delete(id); err != nil {
		return err
	}
	detail := title
	if why != "" {
		detail = strings.TrimPrefix(title+" — "+why, " — ")
	}
	a.record(event, id, "", detail)

	// Removal gets the same contract as a write: the index is what every page
	// is drawn from, so a document deleted from disk but left in it is a search
	// result that 404s. Retried until Typesense accepts it, holding the caller
	// the way persist does.
	//
	// The accepted edge is the client hanging up mid-retry: the ghost then
	// survives until the next start, whose replay rebuilds the index from the
	// sidecars and skips tombstones — so it goes then, and no earlier.
	if err := retryUntil(ctx, retryInitial, retryMax,
		fmt.Sprintf("doc %d: removing from index", id),
		func() error { return a.search.Delete(ctx, id) },
	); err != nil {
		logf("doc %d: gave up removing from index (%v); it stays visible until the next restart", id, err)
	}
	return nil
}

// handleDocDelete destroys one document. This route has always been the
// permanent one; what changed is that the button which used to point at it now
// points at the trash, so reaching it means someone asked for "delete forever".
func (a *App) handleDocDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.purge(r.Context(), id, "deleted", ""); err != nil {
		// Already deleted is not an error to report, it is a document that is
		// not there — the same answer its page and every other route that
		// changes a document give for a tombstone. This route reads the id
		// straight out of the path rather than through loadDoc, because purge
		// needs the title for its journal line before the document goes.
		if errors.Is(err, errGone) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "deleting document: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleDocTrash and handleDocRestore move one document in and out of the
// trash. Neither touches a file: trashing writes a purge deadline into the
// sidecar and restoring clears it, which is what makes a restore lossless
// rather than a reconstruction.
func (a *App) handleDocTrash(w http.ResponseWriter, r *http.Request)   { a.setTrashed(w, r, true) }
func (a *App) handleDocRestore(w http.ResponseWriter, r *http.Request) { a.setTrashed(w, r, false) }

func (a *App) setTrashed(w http.ResponseWriter, r *http.Request, trash bool) {
	doc, ok := a.loadDoc(w, r)
	if !ok {
		return
	}
	if trash {
		doc.Trash(time.Now())
	} else {
		doc.Restore()
	}
	// Sidecar first, then the index, both retried until they land — the same
	// contract every other write has. Only a dead request context comes back.
	if err := a.persist(r.Context(), doc); err != nil {
		http.Error(w, "saving document: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.recordTrash(doc, trash)

	if wantsJSON(r) {
		writeJSON(w, map[string]any{"trashed": doc.Trashed(), "delete_after_ts": doc.DeleteAfterTS})
		return
	}
	http.Redirect(w, r, docPath(doc.ID), http.StatusSeeOther)
}

// recordTrash records the move. The trashing line carries the date rather than
// the retention period, because "purges 2026-09-01" is a fact about this
// document and "30 days" is a fact about the configuration.
func (a *App) recordTrash(doc *Doc, trash bool) {
	if !trash {
		a.record("restored", doc.ID, "", "")
		return
	}
	a.record("trashed", doc.ID, "", "purges "+time.Unix(doc.DeleteAfterTS, 0).Format("2006-01-02"))
}

// The bulk actions the index offers, as the form posts them.
const (
	actionTrash   = "trash"
	actionRestore = "restore"
	actionPurge   = "purge"
)

// handleDocsAction applies one action to a posted selection. It shares the
// index's existing form with Download — the buttons carry formaction and their
// own action name — so a selection can be trashed, restored or destroyed
// without JavaScript and without a second set of checkboxes.
func (a *App) handleDocsAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	action := r.PostFormValue("action")
	switch action {
	case actionTrash, actionRestore, actionPurge:
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	var done int
	for _, id := range postedIDs(r.PostForm) {
		var err error
		if action == actionPurge {
			err = a.purge(r.Context(), id, "purged", purgeByHand)
		} else {
			err = a.setTrashedByID(r.Context(), id, action == actionTrash)
		}
		if err != nil {
			// One document that could not be moved does not cancel the rest of
			// the selection; the count reports what actually happened.
			logf("%s %s: %v", action, docCode(id), err)
			continue
		}
		done++
	}
	// Back to the listing the selection was made in, filters and view intact,
	// with the outcome to report. Page one, because the documents that were on
	// the page are the ones that just moved.
	http.Redirect(w, r, indexURL(parseQuery(r.PostForm), action, done), http.StatusSeeOther)
}

func (a *App) setTrashedByID(ctx context.Context, id int, trash bool) error {
	doc, err := a.store.Load(id)
	if err != nil {
		return err
	}
	// The bulk route's own version of what loadDoc does for the single-document
	// ones: a selection made before a purge — or before the sweeper ran — still
	// posts the ids it saw, and a tombstone is not a document to move in or out
	// of the trash. persist would keep it out of the index either way; refusing
	// here is what also keeps the tombstone unedited and the journal honest.
	if doc.Gone() {
		return errGone
	}
	if trash {
		doc.Trash(time.Now())
	} else {
		doc.Restore()
	}
	if err := a.persist(ctx, doc); err != nil {
		return err
	}
	a.recordTrash(doc, trash)
	return nil
}

// postedIDs reads a selection out of a form. Download and the bulk actions post
// the same checkboxes, so they read them the same way.
func postedIDs(v url.Values) []int {
	var ids []int
	for _, raw := range v["id"] {
		if id, err := strconv.Atoi(raw); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

// indexURL is the way back to a listing after a bulk action, built from
// FilterFields — the same list the forms post — so the way back cannot drift
// from the way in. The outcome travels as an action and a count rather than as
// the sentence itself: a URL is not a place to put text that will be shown to
// whoever opens it.
func indexURL(q Query, action string, n int) string {
	v := url.Values{}
	for _, f := range (page{Query: q}).FilterFields(true) {
		v.Add(f.Name, f.Value)
	}
	if n > 0 {
		v.Set("done", action)
		v.Set("n", strconv.Itoa(n))
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + v.Encode()
}

// actionNotice turns that back into the line the index shows. The wording lives
// here, in one place, so the three notices are written once and a hand-edited
// URL can only ever produce one of them.
func actionNotice(v url.Values) []Flash {
	n, _ := strconv.Atoi(v.Get("n"))
	if n <= 0 {
		return nil
	}
	switch v.Get("done") {
	case actionTrash:
		// The retention is named in days rather than spelled out, so the notice
		// cannot come to disagree with the deadline actually written.
		return []Flash{{Text: fmt.Sprintf("%d moved to trash — purged after %d days.", n, int(trashRetention/(24*time.Hour)))}}
	case actionRestore:
		return []Flash{{Text: fmt.Sprintf("%d restored.", n)}}
	case actionPurge:
		return []Flash{{Text: fmt.Sprintf("%d deleted permanently.", n)}}
	}
	return nil
}

// handleDocRetry rebuilds a document from its original: OCR, thumbnail, text
// and metadata are all redone. Unlike startup recovery, which deliberately
// skips finished stages, this clears them first so the work actually happens.
func (a *App) handleDocRetry(w http.ResponseWriter, r *http.Request) {
	doc, ok := a.loadDoc(w, r)
	if !ok {
		return
	}
	if err := a.reprocess(r.Context(), doc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.pipeline.EnqueueDoc(doc.ID); err != nil {
		http.Error(w, "cannot reprocess: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.record("reprocessed", doc.ID, "", "")
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"status": "processing"})
		return
	}
	// Without JavaScript, back to the document: the timeline there already
	// reports the work, and the status page is about the queue as a whole.
	http.Redirect(w, r, docPath(doc.ID), http.StatusSeeOther)
}

// reprocess throws away everything derived and puts the document back into the
// processing state, which is what makes a re-run actually run: stages skip work
// that is already done, so without this the queue would look at the archive PDF
// sitting on disk and decide there was nothing to do.
//
// The queueing itself is the caller's, because the two callers reach it for
// different reasons — the reader thinks the result is wrong, or the machine has
// just been handed the password it was missing — and answer a queue that will
// not take the work differently.
func (a *App) reprocess(ctx context.Context, doc *Doc) error {
	if err := a.store.ClearDerived(doc.ID); err != nil {
		return fmt.Errorf("clearing derived files: %w", err)
	}
	doc.Status = StatusProcessing
	doc.Enriched = false
	// Indexed as well as saved, in that order: the document page reads its copy
	// out of the index, so writing only the sidecar left it reporting the state
	// it was in before the reprocess was asked for. An error here means the
	// request's context ended, nothing else.
	if err := a.persist(ctx, doc); err != nil {
		return fmt.Errorf("saving document: %w", err)
	}
	return nil
}

// handleDocUnlock takes the password for a document nobody could open, tries it
// against that document's own original, and on success keeps it and reprocesses.
//
// The password must not survive this request anywhere it can be read again. It
// is not logged, not journalled, not written to a sidecar, not put into an error
// message and not echoed back into the form — the redirect after a wrong guess
// carries a flag rather than the guess. The one place it is written is the
// password file, which exists for exactly that.
func (a *App) handleDocUnlock(w http.ResponseWriter, r *http.Request) {
	doc, ok := a.loadDoc(w, r)
	if !ok {
		return
	}
	// The form is only ever drawn on a locked document, so arriving here any
	// other way is a stale tab or a hand-made request. There is nothing to
	// unlock and therefore nothing sensible to do with a password.
	if !doc.Locked() {
		http.Error(w, "this document is not waiting for a password", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Trimmed the way the password file is read, so that a password which works
	// typed in here is the same string that works pasted in there.
	pw := strings.TrimSpace(r.PostFormValue("password"))

	// A wrong password changes nothing at all: no file is written, no event is
	// recorded, and the form comes back empty with a line above it.
	wrong := func() { http.Redirect(w, r, docPath(doc.ID)+"?unlock=bad", http.StatusSeeOther) }

	// Nothing typed. Not worth a qpdf run: the empty password is the one
	// DecryptPDF always tries first, and this document is locked precisely
	// because that already failed.
	if pw == "" {
		wrong()
		return
	}

	orig, err := a.store.OriginalGlob(doc.ID)
	if err != nil {
		http.Error(w, "reading the original: "+err.Error(), http.StatusInternalServerError)
		return
	}
	which, err := a.tryPassword(r.Context(), orig, pw)
	switch {
	case errors.Is(err, errNoPassword):
		wrong()
		return
	case err != nil:
		// Not a wrong guess: qpdf missing, an unreadable original, a timeout.
		// Those are the machine's problem and say so as themselves.
		http.Error(w, "trying the password: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Position 0 is the empty password having opened the file, which means what
	// the reader typed was never actually tested and must not be filed against
	// this document. That cannot happen to a document that is locked — failing
	// the empty password is how it became locked — but it is qpdf's answer that
	// is believed here rather than the reasoning about it.
	if which > 0 {
		if err := a.rememberPassword(pw); err != nil {
			http.Error(w, "saving the password: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if err := a.reprocess(r.Context(), doc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := a.pipeline.EnqueueDoc(doc.ID); err != nil {
		http.Error(w, "cannot reprocess: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// That it was unlocked, and nothing about what with. The document page and
	// the journal both stop there.
	a.record("unlocked", doc.ID, "", "")
	a.retryLocked(r.Context(), doc.ID)
	http.Redirect(w, r, docPath(doc.ID), http.StatusSeeOther)
}

// retryLocked puts every other locked document back through the pipeline, now
// that there is one more password to try.
//
// A password is rarely one document's own: a bank sends the same locked
// statement every month under the same password, so the one just proved against
// this document is the likeliest thing to open the others — and without this,
// unlocking four statements means typing the same password four times and
// wondering why the archive did not notice.
//
// Best effort by design. The unlock itself has already succeeded and been
// redirected on; this is opportunistic work on other documents, so a failure
// here is logged and dropped rather than turned into an error about a document
// the reader did not ask about. The ones that do not open simply stay locked,
// exactly as they were.
func (a *App) retryLocked(ctx context.Context, except int) {
	ids, err := a.search.LockedIDs(ctx)
	if err != nil {
		logf("looking for other locked documents: %v", err)
		return
	}
	var queued int
	for _, id := range ids {
		if id == except {
			continue
		}
		doc, err := a.store.Load(id)
		if err != nil {
			logf("doc %d: %v", id, err)
			continue
		}
		if err := a.reprocess(ctx, doc); err != nil {
			logf("doc %d: %v", id, err)
			continue
		}
		if err := a.pipeline.EnqueueDoc(id); err != nil {
			logf("doc %d: %v", id, err)
			continue
		}
		queued++
	}
	if queued > 0 {
		logf("a new password may open other documents: requeued %d still locked", queued)
	}
}

// tryPassword answers whether pw opens this document and nothing else. It runs
// the very decryption the pipeline runs, against the same file, rather than
// asking qpdf its own question in its own way: two ways of asking is two ways of
// being wrong, and being wrong here files a password against a document it does
// not open.
//
// The decrypted copy goes to a temp file beside the archive and is removed
// immediately. What is wanted is the answer, not the file — the pipeline makes
// that copy properly a moment later, from the original, with the password now in
// hand.
func (a *App) tryPassword(ctx context.Context, orig, pw string) (int, error) {
	f, err := os.CreateTemp(a.store.ArchiveDir(), ".unlock-*.pdf")
	if err != nil {
		return -1, err
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)
	return DecryptPDF(ctx, orig, tmp, []string{pw})
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
		ids = postedIDs(r.PostForm)
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
	name := fmt.Sprintf("docovia-%s.zip", time.Now().Format("2006-01-02"))
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

	// Retried until the index answers, so this blocks while Typesense is down
	// rather than accepting a file it could not check — an unchecked upload is
	// how the same bytes became two documents. The error is a canceled request:
	// the file is dropped and the user is told, because a silent accept would
	// put it in the inbox as if it had passed.
	id, found, err := a.search.FindByHashWait(ctx, sum, name)
	if err != nil {
		os.Remove(tmpName)
		return fail("could not check the archive for duplicates (%v), so it was not accepted", err)
	}
	if found {
		os.Remove(tmpName)
		// No "deleted from the inbox" here, unlike the pipeline's duplicate:
		// this file was refused on the way in and never reached the inbox, so
		// there is nothing to say went.
		a.record("duplicate", id, name, "already in the archive")
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

// normalizeMonth clamps any date-ish string to YYYY-MM. A bare year expands to
// December of it, which is the same rule the model is given for a document that
// names a year and no month: filed at the end of the year it belongs to beats
// filed nowhere. Without this the model answering "2022" would be discarded here
// and look exactly like a document it had failed to date.
func normalizeMonth(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 7 {
		if _, err := time.Parse("2006-01", s[:7]); err == nil {
			return s[:7]
		}
	}
	if len(s) == 4 {
		if _, err := time.Parse("2006", s); err == nil {
			return s + "-12"
		}
	}
	return ""
}
