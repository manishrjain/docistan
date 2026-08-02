package main

import (
	"bytes"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
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
// `OPENAI_API_KEY= ./docistan` would silently disable the file fallback.
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
	for _, n := range []string{"q", "tag", "status", "sort", "dir"} {
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
// the real page and looks for the input on both forms.
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
	// Once in the custom-range form, once in the download picker.
	if n := strings.Count(buf.String(), `name="dir" value="asc"`); n != 2 {
		t.Errorf("dir hidden input appears %d time(s), want 2 (custom-range form and picker)", n)
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
