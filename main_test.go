package main

import (
	"io"
	"os"
	"path/filepath"
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
