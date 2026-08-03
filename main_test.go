package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// The key file is hand-written, usually once, often by pasting. These are the
// shapes that paste produces; getting one of them wrong would send a mangled
// key to the API and report an authentication failure that looks like a bad
// key rather than a bad parse.
func TestOpenAIKeyFromFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"bare", "sk-bare\n", "sk-bare"},
		{"no trailing newline", "sk-bare", "sk-bare"},
		{"padded", "  sk-padded  \n", "sk-padded"},
		{"dotenv", "OPENAI_API_KEY=sk-dotenv\n", "sk-dotenv"},
		{"dotenv spaced", "OPENAI_API_KEY = sk-dotenv\n", "sk-dotenv"},
		{"export quoted", "export OPENAI_API_KEY=\"sk-export\"\n", "sk-export"},
		{"single quoted", "'sk-quoted'\n", "sk-quoted"},
		{"comment then key", "# personal key\n\nsk-after-comment\n", "sk-after-comment"},
		{"trailing lines ignored", "sk-first\nsk-second\n", "sk-first"},
		{"empty", "\n\n  \n", ""},
		{"comments only", "# nothing here\n", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, source := openAIKey(path)
			if got != tc.want {
				t.Errorf("openAIKey(%q) = %q, want %q", tc.content, got, tc.want)
			}
			if tc.want != "" && source != path {
				t.Errorf("source = %q, want %q", source, path)
			}
		})
	}
}

// A key on the command line or in a unit file should be able to override the
// one on disk without moving the file out of the way.
func TestOpenAIKeyEnvWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("sk-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "sk-from-env")

	key, source := openAIKey(path)
	if key != "sk-from-env" {
		t.Errorf("key = %q, want the environment's", key)
	}
	if source != "OPENAI_API_KEY" {
		t.Errorf("source = %q, want OPENAI_API_KEY", source)
	}
}

// A blank environment variable is the same as an unset one — otherwise
// `OPENAI_API_KEY= ./docovia` would silently disable the file fallback.
func TestOpenAIKeyBlankEnvFallsThrough(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("sk-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "  ")

	if key, _ := openAIKey(path); key != "sk-from-file" {
		t.Errorf("key = %q, want the file's", key)
	}
}

func TestOpenAIKeyMissingFile(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if key, source := openAIKey(filepath.Join(t.TempDir(), "absent")); key != "" || source != "" {
		t.Errorf("got (%q, %q), want empty", key, source)
	}
}

// A title is free text from a model or a person, and it becomes a filename on
// someone's disk. These are the shapes that break that.
func TestDownloadName(t *testing.T) {
	cases := []struct {
		name string
		doc  Doc
		ext  string
		want string
	}{
		{"plain", Doc{ID: 3, Title: "Northwind Electricity Statement"}, ".pdf",
			"Northwind Electricity Statement.pdf"},
		{"slashes and colons", Doc{ID: 3, Title: "Invoice 12/2026: Acme \"Ltd\""}, ".pdf",
			"Invoice 12 2026 Acme Ltd.pdf"},
		{"collapses whitespace", Doc{ID: 3, Title: "  Spaced   out \n title "}, ".pdf",
			"Spaced out title.pdf"},
		{"non-ascii survives", Doc{ID: 3, Title: "Rechnung Müller — März"}, ".pdf",
			"Rechnung Müller — März.pdf"},
		{"trailing dot trimmed", Doc{ID: 3, Title: "Report v2."}, ".pdf", "Report v2.pdf"},
		{"falls back to the original", Doc{ID: 3, Title: "", OriginalName: "scan_001.pdf"}, ".pdf",
			"scan_001.pdf"},
		{"falls back to the number", Doc{ID: 3}, ".pdf", "DOC-3.pdf"},
		{"control chars dropped", Doc{ID: 3, Title: "Bad\x00title\x07"}, ".pdf", "Badtitle.pdf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := downloadName(&tc.doc, tc.ext); got != tc.want {
				t.Errorf("downloadName = %q, want %q", got, tc.want)
			}
		})
	}
}

// A long title must not produce a name the filesystem rejects, and must not be
// cut through the middle of a character.
func TestDownloadNameLength(t *testing.T) {
	doc := Doc{ID: 3, Title: strings.Repeat("ü", 200)}
	got := downloadName(&doc, ".pdf")
	if len(got) > 130 {
		t.Errorf("name is %d bytes, too long: %q", len(got), got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("name is not valid UTF-8: %q", got)
	}
}

// Content-Disposition has to carry a non-ASCII name in a form browsers read.
func TestDispositionEncodesNonASCII(t *testing.T) {
	v := disposition("attachment", "Rechnung Müller.pdf")
	if !strings.Contains(v, "filename*=") {
		t.Errorf("no RFC 2231 encoded form in %q", v)
	}
	plain := disposition("attachment", "Simple.pdf")
	if plain != `attachment; filename=Simple.pdf` && plain != `attachment; filename="Simple.pdf"` {
		t.Errorf("unexpected plain form: %q", plain)
	}
}

// Every page must parse against the real function map. A template calling a
// helper that is not registered fails when someone opens the page, not when
// the binary is built, so this is the cheapest place to catch it.
func TestTemplatesParse(t *testing.T) {
	for _, name := range []string{"index.html", "doc.html", "upload.html", "status.html"} {
		if _, err := parsePage(assets, name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The parsed template is now shared by every request instead of being rebuilt
// per render, so it has to survive concurrent execution — that property is the
// whole reason caching it is safe. Run under -race to mean anything.
func TestTemplateCacheConcurrentExecute(t *testing.T) {
	a := &App{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				tpl, err := a.templates("index.html")
				if err != nil {
					t.Error(err)
					return
				}
				data := page{Query: Query{}, Result: &Result{Facets: map[string][]FacetValue{}}}
				if err := tpl.ExecuteTemplate(io.Discard, "layout", data); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// LoadMeta exists to avoid reading a document's whole text to work out what to
// call the file. It must still return the fields a download name is built from.
func TestLoadMetaSkipsContentButKeepsNames(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := &Doc{
		ID: 7, Title: "Northwind Statement", OriginalName: "scan_007.pdf",
		OriginalExt: ".pdf", Status: StatusReady, AddedTS: 1785400000,
		Content: strings.Repeat("x", 5000),
	}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadMeta(7)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "" {
		t.Errorf("LoadMeta returned %d bytes of content, want none", len(got.Content))
	}
	for _, c := range []struct{ name, got, want string }{
		{"title", got.Title, want.Title},
		{"original name", got.OriginalName, want.OriginalName},
		{"status", got.Status, want.Status},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if got.ID != want.ID || got.AddedTS != want.AddedTS {
		t.Errorf("id/addedTS = %d/%d, want %d/%d", got.ID, got.AddedTS, want.ID, want.AddedTS)
	}
	if name := downloadName(got, ".pdf"); name != "Northwind Statement.pdf" {
		t.Errorf("downloadName from meta = %q", name)
	}
}

// One table now answers "do we accept this" and "how is it converted", and it
// has to keep accepting the casing a scanner or a mail client actually emits.
func TestExtensionTaxonomy(t *testing.T) {
	cases := []struct {
		ext       string
		supported bool
		kind      fileKind
	}{
		{".pdf", true, kindPDF},
		{".PDF", true, kindPDF},
		{".jpeg", true, kindImage},
		{".TIFF", true, kindImage},
		{".md", true, kindText},
		{".txt", true, kindText},
		{".docx", false, 0},
		{"", false, 0},
	}
	for _, c := range cases {
		if got := isSupportedExt(c.ext); got != c.supported {
			t.Errorf("isSupportedExt(%q) = %v, want %v", c.ext, got, c.supported)
		}
		if !c.supported {
			continue
		}
		if k, _ := kindOf(c.ext); k != c.kind {
			t.Errorf("kindOf(%q) = %v, want %v", c.ext, k, c.kind)
		}
		if want := c.kind == kindText; isTextExt(c.ext) != want {
			t.Errorf("isTextExt(%q) = %v, want %v", c.ext, !want, want)
		}
	}
	// The picker offers exactly what the server accepts.
	if got := acceptedExts(); got != ".jpeg,.jpg,.md,.pdf,.png,.tif,.tiff,.txt,.webp" {
		t.Errorf("acceptedExts() = %q", got)
	}
}

// A form that re-posts the current filters has to re-post all of them. The
// custom-range form used to leave out dir, so choosing a date range silently
// flipped the sort back to newest-first. Round-tripping through parseQuery is
// the check that cannot be fooled by adding a field to one list and not the
// other.
func TestFilterFieldsRoundTrip(t *testing.T) {
	want := Query{
		Q: "oceanside", Tags: []string{"alta", "escrow"}, Status: StatusReady,
		View: ViewTrash,
		Sort: "created", Dir: "asc", Range: "custom", From: "2025-03", To: "2026-01",
	}
	p := page{Query: want}

	v := url.Values{}
	for _, f := range p.FilterFields(true) {
		v.Add(f.Name, f.Value)
	}
	if got := parseQuery(v); !reflect.DeepEqual(got, want) {
		t.Errorf("round trip lost something:\n got %+v\nwant %+v", got, want)
	}

	// The form that edits the dates carries everything except them.
	names := map[string]bool{}
	for _, f := range p.FilterFields(false) {
		names[f.Name] = true
	}
	for _, n := range []string{"q", "tag", "status", "view", "sort", "dir"} {
		if !names[n] {
			t.Errorf("FilterFields(false) dropped %q", n)
		}
	}
	for _, n := range []string{"range", "from_y", "from_m", "to_y", "to_m"} {
		if names[n] {
			t.Errorf("FilterFields(false) carried %q, which that form sets itself", n)
		}
	}
}

// The omission was in the template rather than in any helper, so this renders
// the real page and looks for the input on every form that posts the filters.
func TestIndexFormsCarrySortDirection(t *testing.T) {
	a := &App{}
	tpl, err := a.templates("index.html")
	if err != nil {
		t.Fatal(err)
	}
	data := page{
		Query: Query{
			Range: "custom", From: "2025-03", To: "2026-01",
			Sort: "created", Dir: "asc",
		},
		Result: &Result{Facets: map[string][]FacetValue{}},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatal(err)
	}
	// Once in the custom-range form, once in the download picker, once in the
	// search box.
	if n := strings.Count(buf.String(), `name="dir" value="asc"`); n != 3 {
		t.Errorf("dir hidden input appears %d time(s), want 3 (custom-range form, picker and search box)", n)
	}
}

// The view has to travel with the filters for the same reason the sort
// direction does, but the consequence is worse: a form that dropped it would
// answer "sort by document date" by moving you out of the trash and back to
// All, which reads as the documents having been restored. parseQuery keeps it —
// TestFilterFieldsRoundTrip covers that — so what is left to check is that the
// forms actually put it on the page.
func TestIndexFormsCarryTheView(t *testing.T) {
	a := &App{}
	tpl, err := a.templates("index.html")
	if err != nil {
		t.Fatal(err)
	}
	data := page{
		Query: Query{
			View: ViewTrash, Range: "custom", From: "2025-03", To: "2026-01",
		},
		Result: &Result{Facets: map[string][]FacetValue{}},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), `name="view" value="trash"`); n != 3 {
		t.Errorf("view hidden input appears %d time(s), want 3 (custom-range form, picker and search box)", n)
	}
	// And the trash offers the two ways out of itself rather than the way in.
	for _, want := range []string{`value="restore"`, `value="purge"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the trash view does not offer %s", want)
		}
	}
	if strings.Contains(buf.String(), ">Move to trash<") {
		t.Error("the trash view offers Move to trash, which is where you already are")
	}
}

// Search as you type swaps what /results returns into the picker form, so the
// response has to be the results region and nothing that surrounds it: a
// fragment carrying the layout would put a second <html>, a second search box
// and a second filter bar inside the form it was swapped into, and the nested
// form would take the Download button with it.
//
// Typesense is stubbed rather than run. What is under test is which template
// the handler executes and what it reads out of the URL, not searching — and a
// test that needs a server running is a test that gets skipped.
func TestResultsRendersTheFragmentAndNotThePage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "oceanside" {
			t.Errorf("searched for %q, want the term from the fragment URL", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"found":1,"page":1,"hits":[{"document":{"id":"7","title":"Settlement","status":"ready","tags":["alta"],"created_date":"2025-03"},"highlights":[{"field":"content","snippet":"an %soceanside%s statement"}]}],"facet_counts":[]}`,
			hlStart, hlEnd)
	}))
	defer ts.Close()

	a := &App{search: NewSearch(ts.URL, "test-key", "documents")}
	mux := http.NewServeMux()
	a.routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/results?q=oceanside&tag=alta", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /results: %d\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type %q, want html — the browser inserts this as markup", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{
		`<li class="row">`,                 // the result itself
		"result for \u201coceanside\u201d", // the count, which is what changes as you type
		"<mark>oceanside</mark>",           // marked up here, where the text was escaped first
		`name="q" value="oceanside"`,       // the filters, so Download still means these results
		`name="tag" value="alta"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment is missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{"<html", "<head", "<body", "topbar", "searchbox", "filterbar", "<form"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("fragment contains %q, which belongs to the page around it:\n%s", unwanted, body)
		}
	}
}

// The two renderings of the results have to be the same markup — that is the
// point of there being one template — so the block must also execute on its
// own, against nothing but a page value.
func TestResultsBlockExecutesStandalone(t *testing.T) {
	a := &App{}
	tpl, err := a.templates("index.html")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	data := page{
		Query:  Query{Q: "oceanside", View: ViewTrash},
		Result: &Result{Found: 0, Facets: map[string][]FacetValue{}},
	}
	if err := tpl.ExecuteTemplate(&buf, "results", data); err != nil {
		t.Fatal(err)
	}
	// The empty state belongs to the results, not to the page: typing on to a
	// term that matches nothing has to say so.
	if !strings.Contains(buf.String(), "Nothing here.") {
		t.Errorf("the fragment has no empty state:\n%s", buf.String())
	}
}

// The sidecar is the source of truth, so it must not carry a second copy of
// the date in timestamp form. Every write site used to maintain that copy by
// hand, and any one of them forgetting would leave the file disagreeing with
// itself. Reading the raw bytes is the only check that a field is really gone:
// a struct-level assertion would pass just as well if the field were merely
// renamed or tagged omitempty and still written whenever the date was set.
func TestSidecarStoresDateNotTimestamp(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(&Doc{ID: 3, Status: StatusReady, CreatedDate: "2026-03"}); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(s.DocPath(3))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"created_ts"`)) {
		t.Errorf("sidecar still writes created_ts:\n%s", b)
	}
	if !bytes.Contains(b, []byte(`"created_date": "2026-03"`)) {
		t.Errorf("sidecar lost the date it was given:\n%s", b)
	}
}

// The two figures on a document mean different things and this is the only
// place that keeps them apart: the tokens describe the run whose title and tags
// are currently on display, so a re-tag replaces them, while the cents are the
// bill, so a re-tag adds to them — the money really was spent twice. A call
// that never reached the model reports nothing and must leave both alone,
// otherwise a network blip would blank the tokens of a document that was
// tagged perfectly well an hour ago.
func TestApplyUsage(t *testing.T) {
	const model = "gpt-5.6-luna"
	doc := &Doc{}

	applyUsage(doc, model, Usage{In: 1000, Out: 200})
	first := doc.LLMCents
	if doc.LLMIn != 1000 || doc.LLMOut != 200 {
		t.Fatalf("first run recorded %d/%d tokens, want 1000/200", doc.LLMIn, doc.LLMOut)
	}
	if first <= 0 {
		t.Fatalf("first run recorded %v cents, want a real cost", first)
	}

	// A second, cheaper run: the tokens are now the second run's alone, but the
	// bill covers both.
	applyUsage(doc, model, Usage{In: 500, Out: 100})
	if doc.LLMIn != 500 || doc.LLMOut != 100 {
		t.Errorf("second run left %d/%d tokens, want the latest 500/100", doc.LLMIn, doc.LLMOut)
	}
	if want := first + llmCents(model, Usage{In: 500, Out: 100}); !closeEnough(doc.LLMCents, want) {
		t.Errorf("cents = %v, want both runs summed (%v)", doc.LLMCents, want)
	}

	// A rate limit or a dropped connection bills nothing and must erase nothing.
	before := *doc
	applyUsage(doc, model, Usage{})
	if doc.LLMIn != before.LLMIn || doc.LLMOut != before.LLMOut || doc.LLMCents != before.LLMCents {
		t.Errorf("a call that never reached the model changed the document: %+v, was %+v",
			*doc, before)
	}

	// An unpriced model still tagged the document, so the tokens are real; only
	// the money is unknown, and unknown is zero rather than a guess.
	unknown := &Doc{LLMIn: 10, LLMOut: 20, LLMCents: 5}
	applyUsage(unknown, "gpt-9-imaginary", Usage{In: 700, Out: 300})
	if unknown.LLMIn != 700 || unknown.LLMOut != 300 {
		t.Errorf("unpriced model left %d/%d tokens, want 700/300", unknown.LLMIn, unknown.LLMOut)
	}
	if unknown.LLMCents != 5 {
		t.Errorf("unpriced model changed the recorded cents to %v, want the earlier 5", unknown.LLMCents)
	}
}

// The price table is the only place tokens become money now, so the arithmetic
// is pinned to a concrete case rather than restated in the test.
func TestLLMCents(t *testing.T) {
	// gpt-5.6-luna is $0.20 and $1.20 per million tokens, so this document
	// costs 9058*0.20/1e6 + 1386*1.20/1e6 dollars, in cents.
	got := llmCents("gpt-5.6-luna", Usage{In: 9058, Out: 1386})
	if want := 0.34748; !closeEnough(got, want) {
		t.Errorf("llmCents = %v, want %v", got, want)
	}
	if got := centsStr(got); got != "0.35¢" {
		t.Errorf("that cost renders as %q, want %q", got, "0.35¢")
	}
	// A model the table has never heard of costs nothing we can name. Wrong
	// prices are worse than none, so this must stay zero rather than fall back
	// to some other model's rate.
	if got := llmCents("gpt-9-imaginary", Usage{In: 9058, Out: 1386}); got != 0 {
		t.Errorf("unpriced model = %v cents, want 0", got)
	}
	if modelPriced("gpt-9-imaginary") {
		t.Error("modelPriced says an unlisted model is priced")
	}
	if !modelPriced("gpt-5.6-luna") {
		t.Error("modelPriced does not recognise a model in its own table")
	}
}

// One document costs a fraction of a cent and a backfill costs a few hundred,
// and the same line has to stay readable across that whole range — without
// rounding a real cost away to nothing, and without printing a cost at all when
// there is none to print.
func TestCentsStr(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		// Nothing at all, so an unpriced model shows tokens and no money
		// rather than claiming the call was free.
		{0, ""},
		{0.005, "0.005¢"},
		{0.00049, "0.000¢"}, // still not "", so the line does not vanish
		{0.35, "0.35¢"},
		{9.999, "10.00¢"},
		{99.994, "99.99¢"},
		{100, "100¢"},
		{150, "150¢"},
	}
	for _, c := range cases {
		if got := centsStr(c.in); got != c.want {
			t.Errorf("centsStr(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The cost is now recorded rather than derived at display time, which only
// works if it survives the trip to disk and back. Checking the raw bytes as
// well pins the field name: the index reads and sums that key, so a rename here
// would silently zero the archive total.
func TestSidecarKeepsCost(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := &Doc{ID: 5, Status: StatusReady, Enriched: true,
		LLMIn: 9058, LLMOut: 1386, LLMCents: 0.34748}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load(5)
	if err != nil {
		t.Fatal(err)
	}
	if got.LLMCents != want.LLMCents {
		t.Errorf("cents came back as %v, want %v", got.LLMCents, want.LLMCents)
	}
	if got.LLMIn != want.LLMIn || got.LLMOut != want.LLMOut {
		t.Errorf("tokens came back as %d/%d, want %d/%d",
			got.LLMIn, got.LLMOut, want.LLMIn, want.LLMOut)
	}

	b, err := os.ReadFile(s.DocPath(5))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"llm_cents"`)) {
		t.Errorf("sidecar does not record the cost:\n%s", b)
	}
}

// Trashing is a deadline rather than a flag: what goes on the document is when
// it may be destroyed, so the thirty days survive a restart with no timer to
// rebuild and can be read straight off the sidecar. The period is restated here
// rather than taken from trashRetention, because a test that reads the constant
// it is checking would agree with any value that constant ever had.
//
// Restoring has to clear the deadline completely. A document left carrying a
// stale one would be swept away weeks after somebody rescued it, which is the
// one failure this feature must not have.
func TestTrashSetsAPurgeDeadlineAndRestoreClearsIt(t *testing.T) {
	now := time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC)
	d := &Doc{ID: 7, Status: StatusReady}
	if d.Trashed() {
		t.Fatal("a document that was never trashed reports itself as trashed")
	}

	d.Trash(now)
	if !d.Trashed() {
		t.Error("Trash left the document out of the trash")
	}
	if want := now.AddDate(0, 0, 30).Unix(); d.DeleteAfterTS != want {
		t.Errorf("purge deadline is %s, want %s",
			time.Unix(d.DeleteAfterTS, 0).UTC(), time.Unix(want, 0).UTC())
	}

	d.Restore()
	if d.Trashed() || d.DeleteAfterTS != 0 {
		t.Errorf("Restore left delete_after_ts at %d, want 0", d.DeleteAfterTS)
	}
}

// The sweeper deletes files and cannot be undone, so what counts as due is
// worth stating once and checking directly rather than inferring from a live
// sweep. Zero is the case that matters most: every document in the archive
// carries it, so a predicate that called zero due would empty the archive on
// the first pass.
func TestDuePurge(t *testing.T) {
	const now int64 = 1_800_000_000
	cases := []struct {
		what          string
		deleteAfterTS int64
		want          bool
	}{
		{"deadline passed", now - 1, true},
		{"deadline is exactly now", now, true},
		{"deadline still ahead", now + 1, false},
		{"a day short", now + 86400, false},
		{"not in the trash at all", 0, false},
	}
	for _, c := range cases {
		if got := duePurge(c.deleteAfterTS, now); got != c.want {
			t.Errorf("duePurge(%d, %d) = %v (%s), want %v", c.deleteAfterTS, now, got, c.what, c.want)
		}
	}
}

// The deadline is the only record that a document is in the trash, so it has to
// survive the trip to disk and back — under the name the index filters on,
// since a rename would leave every trashed document visible in the listing and
// never swept. LoadMeta reads it too: the sweeper re-checks the sidecar before
// destroying anything, and reading whole documents to ask one question would
// make the check cost the archive's entire text.
//
// A document that is not in the trash must carry no key at all, which is what
// keeps the field from appearing in thousands of sidecars to say nothing.
func TestSidecarKeepsPurgeDeadline(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	trashed := &Doc{ID: 3, Status: StatusReady, Title: "Water bill"}
	trashed.Trash(time.Now())
	if err := s.Save(trashed); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load(3)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeleteAfterTS != trashed.DeleteAfterTS || !got.Trashed() {
		t.Errorf("deadline came back as %d, want %d", got.DeleteAfterTS, trashed.DeleteAfterTS)
	}
	meta, err := s.LoadMeta(3)
	if err != nil {
		t.Fatal(err)
	}
	if meta.DeleteAfterTS != trashed.DeleteAfterTS {
		t.Errorf("LoadMeta reported deadline %d, want %d", meta.DeleteAfterTS, trashed.DeleteAfterTS)
	}

	b, err := os.ReadFile(s.DocPath(3))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"delete_after_ts"`)) {
		t.Errorf("sidecar does not record the purge deadline:\n%s", b)
	}

	if err := s.Save(&Doc{ID: 4, Status: StatusReady, Title: "Gas bill"}); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(s.DocPath(4))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"delete_after_ts"`)) {
		t.Errorf("a document that is not in the trash still records a deadline:\n%s", b)
	}
}

// The data directory is the archive's whole shape, and the store creating a
// subdirectory is what declares that shape. Rejected files are deleted now, so
// there is no duplicates directory to hold them and creating one would leave an
// empty folder implying files are being kept somewhere. Reading the entries
// rather than probing for names means a subdirectory added later fails here
// too, which is the point: the list is worth a second look every time it grows.
func TestNewStoreCreatesOnlyTheDirectoriesItUses(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewStore(dir); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		if e.IsDir() {
			got = append(got, e.Name())
		}
	}
	sort.Strings(got)

	want := []string{"archive", "consume", "docs", "originals", "thumbs"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NewStore created %v, want exactly %v", got, want)
	}
}

// The journal is the only record that a thing was done — the file a duplicate
// arrived as is deleted, and an edit leaves nothing behind but its result — so
// what goes in has to come back out intact. Newest first, because the status
// page shows the top of the list and the most recent event is the one being
// waited on.
func TestJournalRoundTrip(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &App{store: s}

	a.journal("ingested", 1, "scan_001.pdf", "3 pages, tesseract")
	a.journal("rejected", 0, "notes.docx", `unsupported type ".docx"`)
	a.journal("enriched", 1, "", "397 in · 107 out · 0.02¢")

	got := readJournalTail(s.JournalPath(), journalTailBytes, 50)
	if len(got) != 3 {
		t.Fatalf("read %d events, want the 3 that were written", len(got))
	}
	for i, want := range []string{"enriched", "rejected", "ingested"} {
		if got[i].Event != want {
			t.Errorf("event %d is %q, want %q — the list is not newest first", i, got[i].Event, want)
		}
	}

	// The rejected file never became a document, so its line carries a name and
	// no id; that pairing is what the status page reads to decide what to link.
	if rejected := got[1]; rejected.DocID != 0 || rejected.Name != "notes.docx" ||
		rejected.Detail != `unsupported type ".docx"` {
		t.Errorf("rejected event came back as %+v", rejected)
	}
	ingested := got[2]
	if ingested.DocID != 1 || ingested.Name != "scan_001.pdf" || ingested.Detail != "3 pages, tesseract" {
		t.Errorf("ingested event came back as %+v", ingested)
	}
	if ingested.TS <= 0 {
		t.Errorf("ingested event has no timestamp: %+v", ingested)
	}
}

// The status page is polled every few seconds while anything is processing, so
// the reader takes the end of the file rather than the file — which means it
// almost always lands in the middle of a line. That half line is not an event
// and must not be reported as one.
func TestJournalTailBounds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")

	var buf bytes.Buffer
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&buf, `{"ts":%d,"event":"ingested","doc_id":%d}`+"\n", 1785400000+i, i)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// Every line here is the same length, so a budget of two and a half lines
	// puts the seek squarely inside the third from the end.
	line := int64(buf.Len() / 5)
	partial := line*2 + line/2

	got := readJournalTail(path, partial, 50)
	if len(got) != 2 {
		t.Fatalf("read %d events from a two-and-a-half line budget, want the 2 whole ones: %+v", len(got), got)
	}
	if got[0].DocID != 5 || got[1].DocID != 4 {
		t.Errorf("got documents %d and %d, want 5 then 4", got[0].DocID, got[1].DocID)
	}

	// n bounds the answer independently of the byte budget.
	if got := readJournalTail(path, partial, 1); len(got) != 1 || got[0].DocID != 5 {
		t.Errorf("n=1 returned %+v, want only the newest event", got)
	}
	// A budget larger than the file starts at the beginning, where there is no
	// partial line to drop and nothing may be discarded.
	if got := readJournalTail(path, journalTailBytes, 50); len(got) != 5 {
		t.Errorf("read %d events with the whole file in budget, want 5", len(got))
	}

	// Nothing has happened yet is the ordinary state of a fresh archive: the
	// file does not exist until the first event, and the page still renders.
	if got := readJournalTail(filepath.Join(dir, "absent.jsonl"), journalTailBytes, 50); len(got) != 0 {
		t.Errorf("a missing journal returned %+v, want no events", got)
	}
}

// Every event used to be stated twice — a logf for whoever was watching and a
// journal call for the durable record — written by hand at each site, in two
// formats, which is exactly how the two came to say slightly different things.
// One call now has to produce both halves and they have to agree: the log line
// has to name the document and carry the same detail the entry keeps, and the
// call has to leave exactly one entry behind rather than a line and no record,
// or a record and a silent terminal.
func TestRecordLogsAndJournalsTheSameEvent(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &App{store: s}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const detail = `"Northwind Utilities Statement" · [statement utilities] · 2026-02 · 397 in · 107 out · 0.02¢`
	a.record("enriched", 4, "", detail)

	line := buf.String()
	if strings.Count(strings.TrimSpace(line), "\n") != 0 {
		t.Errorf("one call wrote more than one log line:\n%s", line)
	}
	for _, want := range []string{docCode(4), "enriched", detail} {
		if !strings.Contains(line, want) {
			t.Errorf("the log line does not mention %q, so the terminal says less than the journal:\n%s", want, line)
		}
	}

	got := readJournalTail(s.JournalPath(), journalTailBytes, 50)
	if len(got) != 1 {
		t.Fatalf("one call left %d journal entries, want exactly 1: %+v", len(got), got)
	}
	if ev := got[0]; ev.Event != "enriched" || ev.DocID != 4 || ev.Name != "" || ev.Detail != detail {
		t.Errorf("the entry is %+v, which is not what the log line said", got[0])
	}
	if got[0].TS <= 0 {
		t.Errorf("the entry has no timestamp: %+v", got[0])
	}
}

// The log line is derived from the four values the event already carries, so
// the shapes those values come in are what it has to survive. A rejected file
// never became a document and has only a name; a restore has nothing to add and
// must not produce a line ending in a colon with nothing after it; an ingest
// has both an id and a filename, and both are worth reading.
func TestRecordLine(t *testing.T) {
	cases := []struct {
		what   string
		event  string
		docID  int
		name   string
		detail string
		want   string
	}{
		{
			"a document with something to say",
			"enriched", 4, "", "397 in · 107 out · 0.02¢",
			"DOC-4 enriched: 397 in · 107 out · 0.02¢",
		},
		{
			"a file that never became a document, which only its name identifies",
			"rejected", 0, "notes.docx", `unsupported type ".docx", deleted from the inbox`,
			`notes.docx rejected: unsupported type ".docx", deleted from the inbox`,
		},
		{
			"an arrival, where the id says which document and the name says which file",
			"duplicate", 12, "scan_001.pdf", "already in the archive",
			"DOC-12 duplicate scan_001.pdf: already in the archive",
		},
		{
			"an event that speaks for itself",
			"restored", 7, "", "",
			"DOC-7 restored",
		},
	}
	for _, c := range cases {
		if got := recordLine(c.event, c.docID, c.name, c.detail); got != c.want {
			t.Errorf("%s reads as %q, want %q", c.what, got, c.want)
		}
	}
}

// The log line used to be the only place the model's decisions were written
// down and the journal the only place the cost was, so neither could answer
// what the model had actually done to the archive. Both live in the detail now,
// which is what the two halves share. The cost keeps the rule it always had:
// a model the price table does not know says nothing rather than claiming the
// run was free.
func TestEnrichDetail(t *testing.T) {
	cases := []struct {
		what  string
		doc   Doc
		used  Usage
		cents float64
		want  string
	}{
		{
			"a priced run, which says what it decided and what that cost",
			Doc{Title: "Northwind Utilities Statement", Tags: []string{"statement", "utilities"}, CreatedDate: "2026-02"},
			Usage{In: 397, Out: 107}, 0.02,
			`"Northwind Utilities Statement" · [statement utilities] · 2026-02 · 397 in · 107 out · 0.02¢`,
		},
		{
			"an unpriced model on a document the run gave no tags or date",
			Doc{Title: "scan_001.pdf"},
			Usage{In: 1203, Out: 88}, 0,
			`"scan_001.pdf" · 1,203 in · 88 out`,
		},
	}
	for _, c := range cases {
		if got := enrichDetail(&c.doc, c.used, c.cents); got != c.want {
			t.Errorf("%s reads as %q, want %q", c.what, got, c.want)
		}
	}
}

// The title moved into the ingest detail when the log line that used to carry
// it went away, but only where it says something the event does not already:
// a freshly ingested document is titled with the filename the event names, and
// printing that twice would be noise. A reprocess of a document the model has
// since titled is the case worth the room.
func TestIngestDetail(t *testing.T) {
	cases := []struct {
		what string
		doc  Doc
		want string
	}{
		{
			"a new document, whose title is still the filename",
			Doc{OriginalName: "scan_001.pdf", Title: "scan_001.pdf", PageCount: 3, OCRSource: OCRTesseract},
			"3 pages, tesseract",
		},
		{
			"a reprocess of a document that has been titled since",
			Doc{OriginalName: "scan_001.pdf", Title: "Northwind Utilities Statement", PageCount: 1, OCRSource: OCRNone},
			`"Northwind Utilities Statement" · 1 page, none`,
		},
		{
			"a document nothing could count the pages of",
			Doc{OriginalName: "scan_001.pdf", Title: "scan_001.pdf", OCRSource: OCRTesseract},
			"tesseract",
		},
	}
	for _, c := range cases {
		if got := ingestDetail(&c.doc); got != c.want {
			t.Errorf("%s reads as %q, want %q", c.what, got, c.want)
		}
	}
}

// An edit event has to say what changed, not that a save happened: the document
// page autosaves, so a field that was focused and left alone posts exactly like
// a real edit. Nothing changed has to produce nothing at all, or the journal
// fills with lines that record no information.
func TestEditDetail(t *testing.T) {
	cases := []struct {
		name          string
		before, after Doc
		want          string
	}{
		{
			"title only",
			Doc{Title: "scan_001", Tags: []string{"car"}, CreatedDate: "2026-01"},
			Doc{Title: "Northwind Statement", Tags: []string{"car"}, CreatedDate: "2026-01"},
			`title: "scan_001" → "Northwind Statement"`,
		},
		{
			"one tag in, one tag out",
			Doc{Title: "Northwind Statement", Tags: []string{"medical", "statement"}},
			Doc{Title: "Northwind Statement", Tags: []string{"statement", "car"}},
			"tags: +car −medical",
		},
		{
			// Reordering the same tags is not an edit anyone made.
			"tags reordered",
			Doc{Tags: []string{"car", "statement"}},
			Doc{Tags: []string{"statement", "car"}},
			"",
		},
		{
			"a date where there was none",
			Doc{Title: "Northwind Statement"},
			Doc{Title: "Northwind Statement", CreatedDate: "2026-02"},
			"date: none → 2026-02",
		},
		{
			"everything at once",
			Doc{Title: "scan_001", Tags: []string{"medical"}, CreatedDate: "2026-01"},
			Doc{Title: "Northwind Statement", Tags: []string{"car"}, CreatedDate: "2026-02"},
			`title: "scan_001" → "Northwind Statement"; tags: +car −medical; date: 2026-01 → 2026-02`,
		},
		{
			"nothing changed",
			Doc{Title: "Northwind Statement", Tags: []string{"car"}, CreatedDate: "2026-01"},
			Doc{Title: "Northwind Statement", Tags: []string{"car"}, CreatedDate: "2026-01"},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := editDetail(&tc.before, &tc.after); got != tc.want {
				t.Errorf("editDetail = %q, want %q", got, tc.want)
			}
		})
	}
}

// closeEnough compares money that has been through a division, where the last
// bit is not worth failing a test over.
func closeEnough(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Dropping the field from the document only works if the index still receives
// the number it sorts and filters on, so this pins the derivation at the one
// place that now performs it.
func TestIndexDerivesCreatedTimestamp(t *testing.T) {
	want := parseDateTS("2026-03")
	if want <= 0 {
		t.Fatalf("parseDateTS(%q) = %d, want a real timestamp", "2026-03", want)
	}
	if got := tsDoc(&Doc{CreatedDate: "2026-03"})["created_ts"]; got != want {
		t.Errorf("created_ts = %v, want %d", got, want)
	}
	// An undated document sorts and filters as zero, which is what the range
	// filters are written to exclude.
	if got := tsDoc(&Doc{})["created_ts"]; got != int64(0) {
		t.Errorf("created_ts for an undated document = %v, want 0", got)
	}
}

// The payload caps are measured in bytes on purpose, but a byte cut through a
// multi-byte character produces a string that is not valid UTF-8, and everything
// downstream of these caps quietly rewrites the broken bytes to U+FFFD rather
// than rejecting them. So the property to pin is that whatever comes back is
// still valid text and still within the byte limit it was given.
func TestTruncateBytes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than the limit is returned as it stands", "hello", 10, "hello"},
		{"a length exactly at the limit is not a truncation", "hello", 5, "hello"},
		{"a cut that already lands between characters keeps them all", "héllo", 3, "hé"},
		{"a cut inside a two-byte character drops it", "héllo", 2, "h"},
		{"a cut one byte into a three-byte character drops it", "a€b", 2, "a"},
		{"a cut two bytes into a three-byte character drops it", "a€b", 3, "a"},
		{"a three-byte character that fits is kept whole", "a€b", 4, "a€"},
		{"a cut inside a four-byte character drops it", "a😀b", 3, "a"},
		{"a four-byte character that fits is kept whole", "a😀b", 5, "a😀"},
		{"a limit of zero leaves nothing", "héllo", 0, ""},
		// Text that is nothing but wide characters is the case the byte cut got
		// wrong most often: only one offset in three is a legal place to stop.
		{"text of only multi-byte characters stops at the last whole one",
			strings.Repeat("日", 5), 7, "日日"},
		// U+FFFD is a rune real documents carry — this app counts them on purpose
		// in garbageRatio — so a complete one at the end of the cut must survive.
		// Only an incomplete encoding, which decodes as U+FFFD of size one, is
		// evidence that the cut landed mid-character.
		{"a replacement character the text really contains is kept", "ab�cd", 5, "ab�"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateBytes(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("truncateBytes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncateBytes(%q, %d) = % x, which is not valid UTF-8", tc.in, tc.max, got)
			}
			if len(got) > tc.max {
				t.Errorf("truncateBytes(%q, %d) returned %d bytes, over the limit", tc.in, tc.max, len(got))
			}
		})
	}
}

// The tail cut is not the head cut mirrored. Keeping the last N bytes leaves the
// damaged character at the *start* of what is kept, so the boundary has to be
// found by moving forward, and reusing the backward walk here would return a
// string that still begins with orphaned continuation bytes.
func TestTailBytes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than the limit is returned as it stands", "hello", 10, "hello"},
		{"a length exactly at the limit is not a truncation", "hello", 5, "hello"},
		{"a suffix that already starts between characters keeps them all",
			"日本語", 6, "本語"},
		{"a suffix starting one byte inside a character skips the remains of it",
			"日本語", 4, "語"},
		{"a suffix starting two bytes inside a character skips the remains of it",
			"日本語", 5, "語"},
		{"a suffix too small for a whole character is empty rather than broken",
			"abc€", 2, ""},
		{"a limit of zero leaves nothing", "日本語", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tailBytes(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("tailBytes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("tailBytes(%q, %d) = % x, which is not valid UTF-8", tc.in, tc.max, got)
			}
			if len(got) > tc.max {
				t.Errorf("tailBytes(%q, %d) returned %d bytes, over the limit", tc.in, tc.max, len(got))
			}
			if !strings.HasSuffix(tc.in, got) {
				t.Errorf("tailBytes(%q, %d) = %q, which is not a suffix of the input", tc.in, tc.max, got)
			}
		})
	}
}

// This is the assertion that would have caught the original bug: a long document
// in a script that spends three bytes a character lands the index cap inside a
// character, and the JSON encoder on the way to Typesense replaces the split
// rune with U+FFFD instead of reporting anything. A 400KB scan in Japanese is an
// ordinary document here, not a contrived input.
func TestIndexedContentIsValidUTF8(t *testing.T) {
	text := strings.Repeat("日", 140000) // 420000 bytes, past contentIndexLimit
	if len(text) <= contentIndexLimit {
		t.Fatalf("fixture is only %d bytes, under the %d-byte cap", len(text), contentIndexLimit)
	}
	// Without this the test would pass on a cap that happened to fall between
	// characters, and prove nothing about the cut.
	if utf8.ValidString(text[:contentIndexLimit]) {
		t.Fatalf("fixture does not exercise the bug: a byte cut at %d is already on a boundary", contentIndexLimit)
	}

	content, ok := tsDoc(&Doc{Content: text})["content"].(string)
	if !ok {
		t.Fatal("the indexed content is not a string")
	}
	if !utf8.ValidString(content) {
		t.Error("the indexed content is not valid UTF-8, so Typesense stores a replacement character at the cut")
	}
	if len(content) > contentIndexLimit {
		t.Errorf("the indexed content is %d bytes, over the %d-byte cap", len(content), contentIndexLimit)
	}
	if !strings.HasPrefix(text, content) {
		t.Error("the indexed content is not a prefix of the document text")
	}
}

// Enrich sends a long document head-and-tail, and both cuts used to be byte
// cuts, so a German or Japanese document reached the model with a replacement
// character at each seam — one of them right where the dates and reference
// numbers are. The truncation lives outside Enrich so this can be checked
// without a call to the model.
func TestCapTextCutsBothEndsOnCharacterBoundaries(t *testing.T) {
	if got := capText("Rechnung Müller — März"); got != "Rechnung Müller — März" {
		t.Errorf("a short document was altered: %q", got)
	}

	text := strings.Repeat("日", 20000) // 60000 bytes, past textCap
	if len(text) <= textCap {
		t.Fatalf("fixture is only %d bytes, under the %d-byte cap", len(text), textCap)
	}
	// Both naive cuts have to be broken for this fixture to mean anything.
	if utf8.ValidString(text[:headCap]) {
		t.Fatalf("fixture does not exercise the head cut: byte %d is already a boundary", headCap)
	}
	if utf8.ValidString(text[len(text)-tailCap:]) {
		t.Fatalf("fixture does not exercise the tail cut: byte %d is already a boundary", len(text)-tailCap)
	}

	got := capText(text)
	if !utf8.ValidString(got) {
		t.Error("the text sent to the model is not valid UTF-8")
	}
	const marker = "\n\n…[middle omitted]…\n\n"
	if !strings.Contains(got, marker) {
		t.Error("the middle was dropped without saying so")
	}
	head, tail, _ := strings.Cut(got, marker)
	if len(head) > headCap {
		t.Errorf("the head is %d bytes, over the %d-byte cap", len(head), headCap)
	}
	if len(tail) > tailCap {
		t.Errorf("the tail is %d bytes, over the %d-byte cap", len(tail), tailCap)
	}
	if !strings.HasPrefix(text, head) {
		t.Error("the head is not the start of the document")
	}
	if !strings.HasSuffix(text, tail) {
		t.Error("the tail is not the end of the document")
	}
}

// A write that cannot reach the index is not allowed to be skipped or handed to
// a later reconciliation, so the helper under persist has to keep going through
// failures — and stop the instant one attempt works, rather than paying for a
// backoff nobody is waiting on.
func TestRetryUntilSucceedsAfterFailures(t *testing.T) {
	calls := 0
	err := retryUntil(context.Background(), time.Microsecond, 10*time.Microsecond, "test write", func() error {
		calls++
		if calls < 4 {
			return errors.New("typesense is down")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryUntil = %v, want nil once the write went through", err)
	}
	if calls != 4 {
		t.Errorf("fn ran %d times, want 4: three failures and the one that worked", calls)
	}
}

// The context is the only way out other than success, and it has to be a fast
// one. A worker parked on a dead index must come back on SIGTERM well inside
// Drain's thirty seconds, or a clean shutdown becomes a kill and whatever was
// mid-flight is left for the next start to redo.
func TestRetryUntilStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	done := make(chan error, 1)
	go func() {
		done <- retryUntil(ctx, time.Millisecond, 5*time.Millisecond, "test write", func() error {
			calls++
			// Stand in for the signal arriving while a write is in progress.
			if calls == 3 {
				cancel()
			}
			return errors.New("typesense is down")
		})
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("retryUntil = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("retryUntil was still retrying ten seconds after its context was canceled")
	}
	// Generous, because the point is that it stopped rather than exactly when:
	// an unbounded loop would be in the thousands by now.
	if calls > 20 {
		t.Errorf("fn ran %d times, want it to give up within an attempt or two of the cancel", calls)
	}
}

// The ordinary write succeeds first time, and that path must cost nothing: no
// sleep, no second attempt, and — though only the log can show this — no line
// claiming a retry that never happened.
func TestRetryUntilFirstTrySucceeds(t *testing.T) {
	calls := 0
	err := retryUntil(context.Background(), time.Hour, time.Hour, "test write", func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("retryUntil = %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("fn ran %d times, want 1", calls)
	}
}

// The backoff is what keeps an hour-long outage from becoming an hour of
// hammering, so the delay has to actually grow — and stop growing at the cap,
// or a service that comes back after a long silence would not be noticed for
// minutes. The timing here is deliberately loose: Go's timers never fire early,
// which is the only guarantee this leans on, and everything else is given room
// for a busy machine.
func TestRetryUntilBackoffGrowsUpToTheCap(t *testing.T) {
	const (
		initial  = time.Millisecond
		maxDelay = 4 * time.Millisecond
		attempts = 8
	)

	var gaps []time.Duration
	calls := 0
	last := time.Now()
	err := retryUntil(context.Background(), initial, maxDelay, "test write", func() error {
		now := time.Now()
		calls++
		if calls > 1 {
			gaps = append(gaps, now.Sub(last))
		}
		last = now
		if calls < attempts {
			return errors.New("typesense is down")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryUntil = %v, want nil", err)
	}
	if len(gaps) != attempts-1 {
		t.Fatalf("recorded %d gaps over %d attempts, want %d", len(gaps), calls, attempts-1)
	}

	// Growth: by the last gap the delay has doubled at least twice, and a timer
	// cannot go off before its deadline, so this cannot be a scheduling fluke.
	if gaps[len(gaps)-1] < 2*initial {
		t.Errorf("last gap was %s, no longer than the first delay of %s: the backoff never grew", gaps[len(gaps)-1], initial)
	}
	// The cap holds: doubling without one would have put the seventh gap a
	// minute out. The slack absorbs a loaded machine, not a missing cap.
	const slack = 500 * time.Millisecond
	for i, gap := range gaps {
		if gap > maxDelay+slack {
			t.Errorf("gap %d was %s, past the %s cap by more than %s", i+1, gap, maxDelay, slack)
		}
	}
}

// A failed dedup lookup is not evidence that the document is absent. Reporting
// one as "no duplicate" is exactly how an outage let byte-identical files become
// two documents sharing a sha256, so a lookup that stumbles on its way to an
// answer has to end at the answer the index would have given all along.
func TestFindByHashWaitOutlastsTransientErrors(t *testing.T) {
	calls := 0
	id, found, err := findByHashWait(context.Background(), time.Microsecond, 10*time.Microsecond,
		"dedup lookup for second.pdf", func() (int, bool, error) {
			calls++
			if calls < 3 {
				return 0, false, errors.New("typesense is down")
			}
			return 7, true, nil
		})
	if err != nil {
		t.Fatalf("findByHashWait = %v, want nil once the lookup answered", err)
	}
	if !found || id != 7 {
		t.Errorf("findByHashWait = (%d, %v), want (7, true): two failures on the way must not turn into 'not a duplicate'", id, found)
	}
	if calls != 3 {
		t.Errorf("lookup ran %d times, want 3: two failures and the one that answered", calls)
	}
}

// The only outcome other than an answer is the context ending, and it has to be
// told apart from "no duplicate" — the zero tuple that comes with it means "not
// asked", not "not present". Both callers therefore read the error first: the
// pipeline leaves the file in the inbox, the upload refuses it. A caller that
// looked only at found would be reading a lookup that never happened, which is
// the bug this whole path exists to prevent.
func TestFindByHashWaitReportsCancellationRatherThanAbsence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	id, found, err := findByHashWait(ctx, time.Microsecond, 10*time.Microsecond,
		"dedup lookup for second.pdf", func() (int, bool, error) {
			calls++
			// Stand in for the upload being abandoned, or shutdown arriving,
			// while the index is still unreachable.
			if calls == 3 {
				cancel()
			}
			return 0, false, errors.New("typesense is down")
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("findByHashWait = %v, want context.Canceled: a lookup that never answered must say so", err)
	}
	if id != 0 || found {
		t.Errorf("findByHashWait returned (%d, %v) alongside its error, want the zero tuple", id, found)
	}
}

// removeFromInbox is the only thing standing between the dedup branch and
// permanent data loss, so it is exercised directly rather than through process:
// reaching it for real takes a sidecar that breaks between enqueue and the
// worker running, which is rare enough that no test would catch a regression
// there. The case that matters is originals/1.pdf — a reprocess whose sidecar
// will not load falls through to the dedup check, the index truthfully reports
// the document as a duplicate of itself, and deleting on that report destroys
// the one copy that cannot be rebuilt, since the archive PDF is made from the
// original and the original is made from nothing. The other two fence the same
// rule from either side.
func TestRemoveFromInboxOnlyEverDeletesFromTheInbox(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &Pipeline{app: &App{store: s}}

	cases := []struct {
		what     string
		path     string
		survives bool
	}{
		{"a consume-folder drop, which is what deletion is for", filepath.Join(s.ConsumeDir(), "scan_001.pdf"), false},
		{"originals/1.pdf, the copy that cannot be regenerated", s.OriginalPath(1, ".pdf"), true},
		{"a path outside the data directory entirely", filepath.Join(t.TempDir(), "elsewhere.pdf"), true},
	}
	for _, c := range cases {
		if err := os.WriteFile(c.path, []byte("%PDF-1.4\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		p.removeFromInbox(c.path, "duplicate")
		_, err := os.Stat(c.path)
		switch survived := err == nil; {
		case survived && !c.survives:
			t.Errorf("%s was left behind, want it deleted", c.what)
		case !survived && c.survives:
			t.Errorf("%s was deleted (%v)", c.what, err)
		}
	}
}

// Refusing has to say so. The file is then sitting somewhere nothing will
// reconsider it, and the only reader who can act on that — or on the damaged
// sidecar behind it — is whoever is watching the log, so the line has to name
// the path. A file that has already gone is the opposite case: that is the
// ordinary outcome of a retry racing the resolution it was waiting for, and
// reporting it would put a failure in the log for every contended ingest that
// ended correctly.
func TestRemoveFromInboxIsLoudOnlyWhenSomethingIsWrong(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &Pipeline{app: &App{store: s}}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	orig := s.OriginalPath(1, ".pdf")
	if err := os.WriteFile(orig, []byte("%PDF-1.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.removeFromInbox(orig, "duplicate")
	if !strings.Contains(buf.String(), orig) {
		t.Errorf("the refusal does not name the file it refused to delete:\n%s", buf.String())
	}

	buf.Reset()
	p.removeFromInbox(filepath.Join(s.ConsumeDir(), "already-gone.pdf"), "duplicate")
	if buf.Len() != 0 {
		t.Errorf("a file that was already gone was reported as a problem:\n%s", buf.String())
	}
}

// persist is the single write path, so "a permanently deleted document is not
// in the index" has to be true there rather than in each of its callers, and
// the part of persist that decides it is this function — a document in, one of
// two operations out, nothing else consulted. Checking it directly is what lets
// the rule be tested at all: a test that had to watch a live upsert could only
// run where a search server is running, which is not where a rule like this
// gets broken.
//
// The trashed case is the one that would be easy to get wrong from the other
// side. A document in the trash still has every one of its files and the trash
// view is a filter over the index, so removing it there would empty the trash
// rather than fill it.
func TestIndexOpForRemovesOnlyTombstones(t *testing.T) {
	trashed := &Doc{ID: 4, Status: StatusReady, Title: "Council tax"}
	trashed.Trash(time.Now())

	name := func(op indexOp) string {
		if op == indexRemove {
			return "be removed from the index"
		}
		return "be written to the index"
	}
	cases := []struct {
		what string
		doc  *Doc
		want indexOp
	}{
		{"still processing", &Doc{ID: 1, Status: StatusProcessing}, indexUpsert},
		{"ready", &Doc{ID: 2, Status: StatusReady}, indexUpsert},
		{"that failed, which is still a document you can find", &Doc{ID: 3, Status: StatusFailed}, indexUpsert},
		{"in the trash, which is a view of the index", trashed, indexUpsert},
		{"that was deleted for good", &Doc{ID: 5, Status: StatusDeleted}, indexRemove},
	}
	for _, c := range cases {
		if got := indexOpFor(c.doc); got != c.want {
			t.Errorf("a document %s should %s, but persist would make it %s", c.what, name(c.want), name(got))
		}
	}
}

// The sequence that put purged documents back on the site: a document is
// destroyed, so its files are gone and the index has forgotten it, but the
// sidecar stays behind to hold the id — and every route that changes a document
// begins by reading exactly that file. Trashing one, then restoring it, brought
// it back into the listing pointing at files that no longer exist.
//
// Both defences are checked against a tombstone made by the real Store.Delete
// rather than one written out by hand here, since a hand-made tombstone only
// proves this test agrees with itself. LoadMeta is included because the delete
// route reads the status from there, alongside the title it needs for the
// journal, instead of reading the sidecar a second time.
func TestPurgedDocumentCannotComeBack(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	doc := &Doc{
		ID: 1, SHA256: "0f1e2d", OriginalName: "scan_001.pdf", OriginalExt: ".pdf",
		Status: StatusReady, Title: "Water bill", Content: "the whole text of it",
	}
	for _, p := range []string{s.OriginalPath(1, ".pdf"), s.ArchivePath(1), s.ThumbPath(1)} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Save(doc); err != nil {
		t.Fatal(err)
	}
	if doc.Gone() {
		t.Fatal("a ready document reports itself as a tombstone")
	}

	if err := s.Delete(1); err != nil {
		t.Fatal(err)
	}
	tomb, err := s.Load(1)
	if err != nil {
		t.Fatalf("the tombstone must stay readable, or the id it reserves is lost: %v", err)
	}
	if !tomb.Gone() {
		t.Errorf("a deleted document loads with status %q, which the guard in loadDoc lets through as an ordinary document", tomb.Status)
	}
	if got := indexOpFor(tomb); got != indexRemove {
		t.Error("persist would write the tombstone into the index, which is how a deleted document reappears in the listing")
	}
	meta, err := s.LoadMeta(1)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Gone() {
		t.Errorf("LoadMeta reports status %q for a tombstone, so the delete route cannot tell it has already run", meta.Status)
	}
	// Why the resurrection was worse than a stale row: the files are really
	// gone, so a document brought back is one whose every link is a 404.
	for _, p := range []string{s.OriginalPath(1, ".pdf"), s.ArchivePath(1), s.ThumbPath(1)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the delete (%v)", filepath.Base(p), err)
		}
	}
}

// What a tombstone keeps is a deliberate list. The id is the whole reason the
// file stays — ids get written on paper, so one must never be handed to a
// second document — and the hash, the original name and the title are what let
// someone reading the archive later say which document this was. The text and
// the tags are the bulk, and keeping them would make deletion a rename.
//
// The status is checked in the file rather than only on the struct, because the
// replay that rebuilds the index at every boot reads that key to decide what
// not to index: a value that did not survive the round trip would resurrect
// every deleted document in the archive on the next restart.
func TestDeleteLeavesATombstoneThatKeepsTheIdentity(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	doc := &Doc{
		ID: 7, SHA256: "9a8b7c", OriginalName: "scan_007.pdf", OriginalExt: ".pdf",
		Status: StatusReady, Title: "Dentist receipt", Summary: "one visit",
		Content: "the whole text of it", Tags: []string{"health", "receipt"},
		AddedTS: 1_700_000_000, PageCount: 3, FileSize: 40 << 10,
	}
	doc.Trash(time.Now())
	if err := s.Save(doc); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(7); err != nil {
		t.Fatal(err)
	}

	tomb, err := s.Load(7)
	if err != nil {
		t.Fatal(err)
	}
	if tomb.Status != StatusDeleted {
		t.Errorf("tombstone status is %q, want %q", tomb.Status, StatusDeleted)
	}
	if tomb.ID != doc.ID || tomb.SHA256 != doc.SHA256 || tomb.OriginalName != doc.OriginalName || tomb.Title != doc.Title {
		t.Errorf("the tombstone lost the identity it exists to keep: %+v", tomb)
	}
	if tomb.AddedTS != doc.AddedTS {
		t.Errorf("added_ts is %d, want %d: when it arrived is part of the record", tomb.AddedTS, doc.AddedTS)
	}
	if tomb.DeletedTS == 0 {
		t.Error("the tombstone does not say when the document was deleted")
	}
	if tomb.Content != "" || tomb.Summary != "" || len(tomb.Tags) != 0 {
		t.Errorf("the bulk survived the delete: %d bytes of text, summary %q, tags %v",
			len(tomb.Content), tomb.Summary, tomb.Tags)
	}
	if tomb.Trashed() {
		t.Errorf("the tombstone still carries a purge deadline (%d), so the sweeper would come back for a document that has already gone", tomb.DeleteAfterTS)
	}

	b, err := os.ReadFile(s.DocPath(7))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"status": "`+StatusDeleted+`"`)) {
		t.Errorf("the sidecar on disk does not record the deletion:\n%s", b)
	}
}

// The search box posts a form, and a form posts what is inside it — which was
// the term and nothing else. So searching from a filtered page searched the
// whole archive instead: /?tag=statement narrowed to three documents, and
// typing a word into the box turned that into every document in the archive
// containing it. The fix is hidden inputs, and it fixes search-as-you-type at
// the same time, since search.js builds its request out of this form's own
// FormData rather than out of a list of its own.
//
// q is the trap. The visible input carries it — it is the one field this form
// exists to edit — so a hidden input for it as well would post the name twice
// and send the word being typed alongside the word that was already there,
// where parseQuery reads the first of the two and the reader's keystrokes go
// nowhere.
func TestSearchBoxCarriesTheFilters(t *testing.T) {
	a := &App{}
	tpl, err := a.templates("index.html")
	if err != nil {
		t.Fatal(err)
	}
	data := page{
		Query: Query{
			Q: "oceanside", Tags: []string{"alta"}, Status: StatusReady, View: ViewTrash,
			Sort: "created", Dir: "asc", Range: "custom", From: "2025-03", To: "2026-01",
		},
		Result: &Result{Facets: map[string][]FacetValue{}},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatal(err)
	}
	// The search form alone. The picker and the custom-range form post these
	// same names, so a match found anywhere on the page would say nothing about
	// the form under test.
	body := buf.String()
	i := strings.Index(body, `<form class="searchbox"`)
	if i < 0 {
		t.Fatal("there is no search form on the index")
	}
	j := strings.Index(body[i:], "</form>")
	if j < 0 {
		t.Fatal("the search form is never closed")
	}
	box := body[i : i+j]

	for _, want := range []string{
		`name="tag" value="alta"`,
		`name="status" value="` + StatusReady + `"`,
		`name="view" value="` + ViewTrash + `"`,
		`name="sort" value="created"`,
		`name="dir" value="asc"`,
		`name="range" value="custom"`,
		`name="from_y" value="2025"`,
		`name="from_m" value="03"`,
		`name="to_y" value="2026"`,
		`name="to_m" value="01"`,
	} {
		if !strings.Contains(box, want) {
			t.Errorf("the search box drops %s, so a search from a filtered page is a search of the archive:\n%s", want, box)
		}
	}
	if n := strings.Count(box, `name="q"`); n != 1 {
		t.Errorf("the search box has %d inputs named q, want exactly the visible one:\n%s", n, box)
	}
}

// The round trip TestFilterFieldsRoundTrip makes for the other two forms, made
// for this one: what the box posts, plus the word typed into it, has to parse
// back into the search that was on screen. Rendering proves the inputs are on
// the page; only this proves they still mean the same thing once the server has
// read them back.
func TestSearchBoxFieldsRoundTrip(t *testing.T) {
	want := Query{
		Q: "oceanside", Tags: []string{"alta", "escrow"}, Status: StatusReady,
		View: ViewTrash,
		Sort: "created", Dir: "asc", Range: "custom", From: "2025-03", To: "2026-01",
	}

	v := url.Values{}
	for _, f := range (page{Query: want}).SearchBoxFields() {
		v.Add(f.Name, f.Value)
	}
	// What the browser adds from the visible input, which is the one field the
	// hidden ones have to leave alone.
	v.Add("q", want.Q)

	if got := v["q"]; len(got) != 1 {
		t.Fatalf("the box would post q %d times (%q); the hidden fields must not repeat it", len(got), got)
	}
	if got := parseQuery(v); !reflect.DeepEqual(got, want) {
		t.Errorf("the search box does not post the search it is sitting in:\n got %+v\nwant %+v", got, want)
	}
}

// The tag facets narrow with the result set and arrive on the same search
// response the rows do, so leaving them out of /results left counts on screen
// describing results that had already been replaced — a pill reading 12 beside
// three rows. They come back with the fragment now.
//
// What must not come back with it is anything holding state the reader set:
// #tags-open is whether the tag popover is open and .tag-filter is what they
// typed into it, and both sit between the two swapped regions rather than
// inside either. Finding one of them here would mean the swap resets it, which
// on screen is the popover shutting under the reader's hands.
//
// Typesense is stubbed the way TestResultsRendersTheFragmentAndNotThePage
// stubs it, with facet counts added — the counts are the point here.
func TestResultsCarriesTheTagRegions(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"found":1,"page":1,`+
			`"hits":[{"document":{"id":"7","title":"Settlement","status":"ready","tags":["alta"],"created_date":"2025-03"}}],`+
			`"facet_counts":[{"field_name":"tags","counts":[{"value":"alta","count":3},{"value":"escrow","count":1}]}]}`)
	}))
	defer ts.Close()

	a := &App{search: NewSearch(ts.URL, "test-key", "documents")}
	mux := http.NewServeMux()
	a.routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/results?q=set&tag=alta", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /results: %d\n%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	for _, want := range []string{
		`id="results"`,     // the rows, as before
		`<li class="row">`, //
		`id="tag-pills"`,   // the pills, their counts and the state of each
		`class="pill-tag on"`,
		`>escrow<`,
		`id="tag-browse"`, // the vocabulary, and the foot that counts it
		`data-tag="escrow"`,
		"2 tags in these results",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the fragment is missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{
		"<html", "<head", "<body", "topbar", "searchbox", "filterbar", "<form",
		// The two things the reader sets by hand, which a swap must not touch.
		`id="tags-open"`, "tag-filter",
		// The archive-wide counts, which do not move with the search text.
		"seg views",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("the fragment contains %q, which belongs to the page around it:\n%s", unwanted, body)
		}
	}
}

// The fixtures for the encryption tests are built here rather than checked in,
// because a binary PDF in the repository is a file nobody can read in a diff
// and nobody dares regenerate. testPDF renders one with the program's own text
// renderer, so the fixture has real extractable text — which is the thing the
// decryption tests have to prove came through intact.
func testPDF(t *testing.T, dir, name, text string) string {
	t.Helper()
	txt := filepath.Join(dir, name+".txt")
	if err := os.WriteFile(txt, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	pdf := filepath.Join(dir, name+".pdf")
	if err := textToPDF(txt, pdf); err != nil {
		t.Fatalf("rendering the %s fixture: %v", name, err)
	}
	return pdf
}

// encryptPDF locks a fixture the way the documents that started this arrived:
// a user password is one nobody can open without, an owner password on its own
// only restricts what may be done with a file that opens fine.
//
// qpdf is required rather than skipped around. It is already a runtime
// dependency — the code under test shells out to it for both the detection and
// the decryption — so a machine that cannot build these fixtures cannot run the
// program either, and skipping would report that as a pass.
func encryptPDF(t *testing.T, src, dst, userPW, ownerPW string) string {
	t.Helper()
	_, err := runCmd(context.Background(), 30*time.Second, "qpdf", "--encrypt",
		"--user-password="+userPW, "--owner-password="+ownerPW, "--bits=256", "--", src, dst)
	if err != nil {
		t.Fatalf("building the encrypted fixture: %v (qpdf 11.7+ is needed for these flags, "+
			"and qpdf itself is a dependency of the pipeline)", err)
	}
	return dst
}

// A password-protected PDF has to be recognised as one before anything is done
// to it, because everything downstream mistakes it for a document that is
// merely empty: ocrmypdf refuses it, Ghostscript writes a blank page instead,
// and pdftotext then finds nothing to find. The right password must produce a
// copy that reads exactly like the original, and the wrong one must produce
// nothing at all — not a partial file that a later stage would archive.
func TestEncryptedPDFIsDetectedAndDecryptsWithItsPassword(t *testing.T) {
	const text = "Statement of account 4471, closing balance 12,940.55"
	dir := t.TempDir()
	locked := encryptPDF(t, testPDF(t, dir, "plain", text),
		filepath.Join(dir, "locked.pdf"), "s3cret-open", "s3cret-owner")

	ctx := context.Background()
	if got := PDFEncryption(ctx, locked); got != pdfLocked {
		t.Errorf("PDFEncryption = %v, want pdfLocked", got)
	}
	if !PDFNeedsPassword(ctx, locked) {
		t.Error("PDFNeedsPassword = false for a PDF that cannot be opened without one")
	}

	// A wrong password is a wrong answer, not a broken machine: the caller has
	// to be able to tell the two apart, so this is the sentinel and not some
	// qpdf exit status.
	out := filepath.Join(dir, "out.pdf")
	if which, err := DecryptPDF(ctx, locked, out, []string{"not-the-one"}); !errors.Is(err, errNoPassword) {
		t.Errorf("DecryptPDF with the wrong password = (%d, %v), want errNoPassword", which, err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("a failed decryption left %s behind, which a later stage would archive as the document", out)
	}

	// The position identifies which line of the password file was the right
	// one — 0 is the empty password, so the second candidate is 2 — without the
	// caller ever having to hold the password to find that out.
	which, err := DecryptPDF(ctx, locked, out, []string{"not-the-one", "s3cret-open"})
	if err != nil {
		t.Fatalf("DecryptPDF with the right password: %v", err)
	}
	if which != 2 {
		t.Errorf("DecryptPDF reported position %d, want 2 (the second candidate)", which)
	}
	if got := PDFEncryption(ctx, out); got != pdfPlain {
		t.Errorf("the decrypted copy is still %v, so nothing downstream could read it either", got)
	}
	got, err := ExtractText(ctx, out)
	if err != nil {
		t.Fatalf("pdftotext on the decrypted copy: %v", err)
	}
	if !strings.Contains(got, text) {
		t.Errorf("the decrypted copy reads %q, want it to contain %q", got, text)
	}
}

// Plenty of statements carry only an owner password: nothing stops them being
// opened, they just say they would rather not be printed. Those must never
// become failures a person has to find a password for — the empty password is
// tried first precisely so a file like this costs one qpdf run and then behaves
// like any other document.
func TestOwnerPasswordOnlyPDFOpensWithTheEmptyPassword(t *testing.T) {
	dir := t.TempDir()
	restricted := encryptPDF(t, testPDF(t, dir, "plain", "Form 1040 page 1 of 48"),
		filepath.Join(dir, "restricted.pdf"), "", "no-printing-please")

	ctx := context.Background()
	if got := PDFEncryption(ctx, restricted); got != pdfRestricted {
		t.Errorf("PDFEncryption = %v, want pdfRestricted", got)
	}
	// The narrow question is the one the OCR guard asks, and the answer has to
	// be no: this file needs nothing from anybody.
	if PDFNeedsPassword(ctx, restricted) {
		t.Error("PDFNeedsPassword = true for a file that opens with no password at all")
	}

	// Nothing is supplied — no password file, no candidates — and it still
	// works, which is the whole point.
	out := filepath.Join(dir, "out.pdf")
	which, err := DecryptPDF(ctx, restricted, out, nil)
	if err != nil {
		t.Fatalf("DecryptPDF on a restricted-only PDF with no candidates: %v", err)
	}
	if which != 0 {
		t.Errorf("DecryptPDF reported position %d, want 0 (the empty password)", which)
	}
	if got := PDFEncryption(ctx, out); got != pdfPlain {
		t.Errorf("the copy is still %v, so the restrictions came along with it", got)
	}
}

// The ordinary document must not pay for any of this. An unencrypted PDF is
// reported as unencrypted and left exactly where it is — the pipeline goes on
// working from the original, and no decrypted copy is written for the archive
// to be built from.
func TestPlainPDFIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	plain := testPDF(t, dir, "plain", "Northwind Utilities, March 2026")

	ctx := context.Background()
	if got := PDFEncryption(ctx, plain); got != pdfPlain {
		t.Errorf("PDFEncryption = %v, want pdfPlain", got)
	}
	if PDFNeedsPassword(ctx, plain) {
		t.Error("PDFNeedsPassword = true for a plain PDF")
	}

	p := &Pipeline{app: &App{cfg: Config{PasswordFile: filepath.Join(dir, "passwords")}}}
	// Encrypted starts true to prove the stage clears it. A document whose
	// original was replaced by an unencrypted one and then reprocessed would
	// otherwise keep claiming an encryption that is no longer there.
	doc := &Doc{ID: 1, Encrypted: true}
	dst := filepath.Join(dir, "decrypted.pdf")
	src, err := p.decrypt(ctx, doc, plain, dst)
	if err != nil {
		t.Fatalf("decrypt on a plain PDF: %v", err)
	}
	if src != plain {
		t.Errorf("decrypt returned %q, want the original %q", src, plain)
	}
	if doc.Encrypted {
		t.Error("a plain PDF was recorded as encrypted")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("decrypt wrote %s for a file that needed nothing done to it", dst)
	}
}

// This is the bug, at the stage that now catches it. A document nobody can open
// must fail, visibly, with something the reader can act on — that failure is
// what puts it in the Failed view, keeps the original for a Retry, and stops a
// blank Ghostscript page being written as the archive and filed as ready. And
// when the password is there, the same stage has to produce a readable copy and
// say so on the document, without the password appearing anywhere.
func TestLockedDocumentFailsAtDecryptUntilItsPasswordIsKnown(t *testing.T) {
	const (
		text = "Interest certificate for FY 2025-26"
		pw   = "MRJN0412-open"
	)
	dir := t.TempDir()
	locked := encryptPDF(t, testPDF(t, dir, "plain", text),
		filepath.Join(dir, "locked.pdf"), pw, "owner-only")

	ctx := context.Background()
	pwFile := filepath.Join(dir, "passwords")
	// A wrong password is loaded rather than none, so the failure below is the
	// one that had a real secret in hand and still did not write it down.
	p := &Pipeline{app: &App{cfg: Config{PasswordFile: pwFile}, pdfPasswords: []string{"wrong-" + pw}}}
	doc := &Doc{ID: 17}
	dst := filepath.Join(dir, "decrypted.pdf")

	_, err := p.decrypt(ctx, doc, locked, dst)
	if err == nil {
		t.Fatal("decrypt succeeded on a document whose password nobody has")
	}
	for _, want := range []string{"password-protected", pwFile, "reprocess"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure reads %q, which does not tell the reader %q", err, want)
		}
	}
	if strings.Contains(err.Error(), pw) {
		t.Error("the failure carries a password, and this one is written to the sidecar and the journal")
	}
	if doc.Encrypted {
		t.Error("a document that was never decrypted is claiming a decrypted copy in the archive")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a failed decryption left %s behind for the archive to be built from", dst)
	}

	// Now the password is known, which is exactly what the Retry button does
	// after someone adds it to the file.
	p.app.pdfPasswords = []string{"wrong-" + pw, pw}
	src, err := p.decrypt(ctx, doc, locked, dst)
	if err != nil {
		t.Fatalf("decrypt with the password in hand: %v", err)
	}
	if src != dst {
		t.Errorf("decrypt returned %q, want the decrypted copy %q — the archive must not be built from the encrypted original", src, dst)
	}
	if !doc.Encrypted {
		t.Error("the document does not record that it arrived encrypted, so its page cannot say the archive copy differs from the original")
	}
	got, err := ExtractText(ctx, src)
	if err != nil {
		t.Fatalf("pdftotext on the decrypted copy: %v", err)
	}
	if !strings.Contains(got, text) {
		t.Errorf("the decrypted copy reads %q, want it to contain %q", got, text)
	}
	// The original is the preservation copy and stays exactly as it arrived,
	// encryption and all: re-encrypting it later could not reproduce it.
	if PDFEncryption(ctx, locked) != pdfLocked {
		t.Error("the original was modified — it must be left encrypted")
	}
}

// The password file is hand-written, next to the key file and read the same
// way. These are the shapes a person writing one produces. Nothing else is
// interpreted: a password may contain quotes, spaces or an equals sign, and
// unwrapping any of those the way the key file does would silently corrupt it.
func TestPDFPasswordFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"one per line", "alpha\nbravo\n", []string{"alpha", "bravo"}},
		{"no trailing newline", "alpha", []string{"alpha"}},
		{"blank lines", "\n\nalpha\n\n\nbravo\n", []string{"alpha", "bravo"}},
		{"comments", "# hdfc statement\nalpha\n# tax return\nbravo\n", []string{"alpha", "bravo"}},
		{"indented comment", "  # noted\nalpha\n", []string{"alpha"}},
		{"trailing whitespace", "alpha   \nbravo\t\n", []string{"alpha", "bravo"}},
		{"carriage returns", "alpha\r\nbravo\r\n", []string{"alpha", "bravo"}},
		{"spaces inside are part of it", "my pass phrase\n", []string{"my pass phrase"}},
		{"quotes and equals are part of it", "\"quoted\"\nPW=abc\n", []string{"\"quoted\"", "PW=abc"}},
		{"hash inside is part of it", "a#1b\n", []string{"a#1b"}},
		{"empty", "\n\n  \n", nil},
		{"comments only", "# nothing here yet\n", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "passwords")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := pdfPasswords(path)
			if err != nil {
				t.Fatalf("pdfPasswords: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("pdfPasswords(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}

	// The ordinary state of a machine that has never met an encrypted document.
	// It is not a failure, and it must not be logged as one every boot.
	t.Run("missing file", func(t *testing.T) {
		got, err := pdfPasswords(filepath.Join(t.TempDir(), "absent"))
		if err != nil || got != nil {
			t.Errorf("pdfPasswords on a missing file = (%q, %v), want (nil, nil)", got, err)
		}
	})

	// The one thing this function must never do. Errors get logged, and a log
	// line with a bank password in it is precisely what the whole feature is
	// arranged to avoid — so the only failure it can have is a read that never
	// reached the contents, and this is what proves it.
	t.Run("no password can reach an error", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a mode-000 file regardless, so the failure cannot be set up here")
		}
		const secret = "0412-mrjn-hdfc"
		path := filepath.Join(t.TempDir(), "passwords")
		if err := os.WriteFile(path, []byte(secret+"\n"), 0o000); err != nil {
			t.Fatal(err)
		}
		got, err := pdfPasswords(path)
		if err == nil {
			t.Fatalf("pdfPasswords on an unreadable file = %q, want an error", got)
		}
		if len(got) != 0 {
			t.Errorf("pdfPasswords returned %q alongside its error", got)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error carries the password: %v", err)
		}
	})
}
