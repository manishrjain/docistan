package main

import (
	"os"
	"path/filepath"
	"strings"
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
