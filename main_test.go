package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
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

// The view has to travel with the filters for the same reason the sort
// direction does, but the consequence is worse: a form that dropped it would
// answer "sort by document date" by moving you out of the trash and back to
// All, which reads as the documents having been restored. parseQuery keeps it —
// TestFilterFieldsRoundTrip covers that — so what is left to check is that both
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
	if n := strings.Count(buf.String(), `name="view" value="trash"`); n != 2 {
		t.Errorf("view hidden input appears %d time(s), want 2 (custom-range form and picker)", n)
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
