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
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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

// The container mounts its key at the system path and passes no flag, so a
// candidate that is absent — or present but useless — has to fall through to
// the next one rather than end the search.
func TestOpenAIKeyFallsThroughToTheNextPath(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	dir := t.TempDir()
	system := filepath.Join(dir, "system")
	if err := os.WriteFile(system, []byte("sk-system\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("# no key here\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		first string
	}{
		{"absent", filepath.Join(dir, "absent")},
		{"empty", empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, source := openAIKey(tc.first, system)
			if key != "sk-system" || source != system {
				t.Errorf("got (%q, %q), want (%q, %q)", key, source, "sk-system", system)
			}
		})
	}
}

// A key in the home file is the one to use even when the machine also has a
// system-wide one, and naming a file explicitly has to replace the search
// rather than sit at the front of it.
func TestKeyFilesOrder(t *testing.T) {
	t.Setenv("HOME", "/home/somebody")
	want := []string{"/home/somebody/.openai.secret", systemKeyFile}
	if got := keyFiles(""); !slices.Equal(got, want) {
		t.Errorf("keyFiles(\"\") = %v, want %v", got, want)
	}
	if got := keyFiles("/tmp/mine"); !slices.Equal(got, []string{"/tmp/mine"}) {
		t.Errorf("keyFiles(%q) = %v, want just it", "/tmp/mine", got)
	}
}

// Only a PDF arrives already made; everything else normalize renders itself,
// and that is what decides whether text density is evidence. Getting this
// wrong is silent: the document still ingests and is still searchable, it is
// just recorded as having been OCR'd when nothing was recognised.
func TestRenderedHere(t *testing.T) {
	for _, tc := range []struct {
		ext  string
		want bool
	}{
		{".pdf", false}, // arrives as a PDF; density is the only evidence
		{".PDF", false},
		{".txt", true},
		{".md", true},
		{".doc", true},
		{".docx", true},
		{".xlsx", true},
		{".odp", true},
		{".RTF", true}, // casing comes off a filename someone else chose
		{".jpg", true}, // rendered by magick, though it renders no text
		{".exe", false},
	} {
		if got := renderedHere(tc.ext); got != tc.want {
			t.Errorf("renderedHere(%q) = %v, want %v", tc.ext, got, tc.want)
		}
	}
}

// The office formats have to be reachable through the same table everything
// else goes through, or the upload picker and the intake check disagree about
// what may be sent.
func TestOfficeExtensionsAreAccepted(t *testing.T) {
	accepted := acceptedExts()
	for _, ext := range []string{".doc", ".docx", ".odt", ".rtf", ".xls", ".xlsx", ".ods", ".ppt", ".pptx", ".odp"} {
		if !isSupportedExt(ext) {
			t.Errorf("%s is not supported", ext)
		}
		if !strings.Contains(accepted, ext) {
			t.Errorf("%s missing from the upload accept list", ext)
		}
		if isTextExt(ext) {
			t.Errorf("%s should not take the plain-text path", ext)
		}
	}
}

// Converting through LibreOffice, on the real thing. RTF is the fixture
// because it is the one office format that is plain text, so the test needs no
// binary blob checked in. Skipped where soffice is not installed, which is
// every machine that is not the container.
func TestOfficeToPDF(t *testing.T) {
	if _, err := exec.LookPath("soffice"); err != nil {
		t.Skip("soffice not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "memo.rtf")
	if err := os.WriteFile(src, []byte(`{\rtf1\ansi Quarterly memo about the Acme contract.\par}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.pdf")
	if err := officeToPDF(t.Context(), src, dst); err != nil {
		t.Fatal(err)
	}
	// A PDF that exists but has no text layer would defeat the point, so the
	// check is that the words came through, not that a file appeared.
	text, err := ExtractText(t.Context(), dst)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Acme contract") {
		t.Errorf("converted PDF text = %q, want it to contain the memo's words", text)
	}
}

// The reason officeToPDF makes a profile per conversion, which is otherwise
// just an odd-looking flag. Measured on this machine with six at once: the
// default profile converted three, one shared profile converted four, a
// private profile each converted six. Workers default to half the core count,
// so a batch of office documents hits this immediately — and it degrades by
// losing documents rather than by being slow.
func TestOfficeToPDFConcurrent(t *testing.T) {
	if _, err := exec.LookPath("soffice"); err != nil {
		t.Skip("soffice not installed")
	}
	if testing.Short() {
		t.Skip("runs a handful of real LibreOffice conversions")
	}
	dir := t.TempDir()
	const n = 6

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		src := filepath.Join(dir, fmt.Sprintf("in%d.rtf", i))
		body := fmt.Sprintf(`{\rtf1\ansi Document number %d.\par}`, i)
		if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = officeToPDF(t.Context(), src, filepath.Join(dir, fmt.Sprintf("out%d.pdf", i)))
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("conversion %d: %v", i, err)
		}
	}
}

// A broken file has to fail rather than leave an empty archive entry behind.
//
// It takes a real container header to get there. LibreOffice identifies by
// content rather than by extension and falls back to its text filter for
// anything it cannot place, so plain text named .docx converts happily, and
// even 4KB of /dev/urandom comes out as a PDF full of mojibake — the extension
// allowlist is the only thing standing between junk and an archived document.
// What does fail is a file that claims a format it then is not: the PK header
// below commits it to being a zip, and it is not one.
func TestOfficeToPDFRejectsBrokenContainer(t *testing.T) {
	if _, err := exec.LookPath("soffice"); err != nil {
		t.Skip("soffice not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "broken.docx")
	if err := os.WriteFile(src, []byte("PK\x03\x04truncated-and-not-a-zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.pdf")
	if err := officeToPDF(t.Context(), src, dst); err == nil {
		t.Error("converting a broken container succeeded, want an error")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a destination file was left behind by a failed conversion")
	}
}

// Current tags and title are offered back with their preservation rules only
// when they exist — a document with neither should not be told to preserve
// anything.
func TestEnrichPromptCurrentState(t *testing.T) {
	p := enrichPrompt(EnrichInput{
		Filename:     "scan.pdf",
		KnownTags:    []string{"tax", "medical"},
		CurrentTags:  []string{"visa", "sunandita"},
		CurrentTitle: "Citizenship Application",
	})
	for _, want := range []string{"tax, medical", "visa, sunandita", `currently titled "Citizenship Application"`, "Start from those"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}

	bare := enrichPrompt(EnrichInput{Filename: "scan.pdf"})
	for _, absent := range []string{"already tagged", "currently titled", "Reuse these existing tags"} {
		if strings.Contains(bare, absent) {
			t.Errorf("bare prompt should not contain %q", absent)
		}
	}
}

// signaturePDF writes a PDF carrying the markers the named case would carry.
// Both shapes are taken from real documents in the archive: a PAN card
// application whose signature box nobody ever signed, and a DocuSign envelope
// whose signature dictionary omits /Type entirely, the spec having made it
// optional.
func signaturePDF(t *testing.T, kind string) string {
	t.Helper()
	var body string
	switch kind {
	case "unsigned-field":
		// A field to sign, and no signature: no /ByteRange, because that is
		// written when someone signs.
		body = "1 0 obj<</FT /Sig /T (Signature1) /Type /Annot>>endobj\n"
	case "signed":
		body = "1 0 obj<</FT /Sig /ByteRange [0 345803 353763 567] " +
			"/SubFilter /adbe.pkcs7.detached /Contents <308206>>>endobj\n"
	case "plain":
		body = "1 0 obj<</Type /Page>>endobj\n"
	default:
		t.Fatalf("unknown kind %q", kind)
	}
	path := filepath.Join(t.TempDir(), kind+".pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.7\n"+body+"%%EOF\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The byte-scan half of IsSignedPDF, which is what runs wherever pdfsig is not
// installed. It used to require /Type/Sig and so called a DocuSign envelope
// unsigned — which would have handed it to ocrmypdf, which refuses signed PDFs
// and would have failed the ingest outright.
func TestIsSignedPDFByteScan(t *testing.T) {
	if _, err := exec.LookPath("pdfsig"); err == nil {
		t.Skip("pdfsig is installed, so the fallback under test does not run")
	}
	for _, tc := range []struct {
		kind string
		want bool
	}{
		{"signed", true},
		{"unsigned-field", false},
		{"plain", false},
	} {
		if got := IsSignedPDF(signaturePDF(t, tc.kind)); got != tc.want {
			t.Errorf("IsSignedPDF(%s) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

// The same question asked of pdfsig, which is what actually runs in the
// container. A field is not a signature: pdfsig prints the field's name either
// way, and only a signed one gets a time and a validation line.
func TestIsSignedPDFViaPdfsig(t *testing.T) {
	if _, err := exec.LookPath("pdfsig"); err != nil {
		t.Skip("pdfsig not installed")
	}
	// An empty signature field is the case that matters: hand-built PDFs are
	// too thin for pdfsig to parse, so this is asserted on the shape of its
	// output rather than on a fixture it would reject.
	for _, tc := range []struct {
		name string
		out  string
		want bool
	}{
		{"empty field", "Signature #1:\n  - Signature Field Name: Signature1\n  The signature form field is not signed.\n", false},
		{"real signature", "Signature #1:\n  - Signature Field Name: ENVELOPEID_834B\n  - Signing Time: Mar 03 2016 00:36:04\n  - Signature Validation: Signature is Valid.\n", true},
		{"no fields at all", "Digital Signature Info of: x.pdf\n", false},
		{"one of each", "Signature #1:\n  The signature form field is not signed.\nSignature #2:\n  - Signature Validation: Signature is Valid.\n", true},
	} {
		got := strings.Contains(tc.out, "Signature Validation:") || strings.Contains(tc.out, "Signing Time:")
		if got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A signed document earns a line in Naturalization saying nothing was done to
// it, which is the point — it used to be a banner announcing a negative over a
// document that had been read perfectly well.
func TestIntakeStageSigned(t *testing.T) {
	for _, tc := range []struct {
		name    string
		doc     Doc
		present bool
		want    string
	}{
		{"signed only", Doc{Signed: true}, true, "Digitally signed — kept exactly as it arrived"},
		{"encrypted only", Doc{Encrypted: true}, true, "Unlocked — password removed from original"},
		{"both", Doc{Encrypted: true, Signed: true}, true, "Unlocked — archive copy decrypted"},
		{"neither", Doc{}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ok := intakeStage(&tc.doc)
			if ok != tc.present {
				t.Fatalf("present = %v, want %v", ok, tc.present)
			}
			if ok && s.Name != tc.want {
				t.Errorf("Name = %q, want %q", s.Name, tc.want)
			}
			if ok && s.State != "done" {
				t.Errorf("State = %q, want done", s.State)
			}
		})
	}
}

// "Queued" and "Working…" are different claims, and the model call is the one
// step slow enough for the difference to be visible. The timeline already had a
// working state, with its own pulse and its own place in the selector that
// starts the page polling — nothing in Go ever set it.
func TestTaggingStageDistinguishesQueuedFromRunning(t *testing.T) {
	app := &App{cfg: Config{LLMModel: "gpt-5.6-luna"}, enricher: &OpenAIEnricher{}}
	app.enrichq = NewEnrichQueue(app)
	doc := &Doc{ID: 7, Status: StatusReady}

	// Waiting its turn.
	app.enrichq.Add(doc.ID)
	if s := app.taggingStage(doc); s.State != "pending" || s.Detail != "Queued" {
		t.Errorf("queued: got (%q, %q), want (pending, Queued)", s.State, s.Detail)
	}

	// next() claims it, which is what the worker does before the call goes out.
	if id, ok := app.enrichq.next(); !ok || id != doc.ID {
		t.Fatalf("next() = %d, %v", id, ok)
	}
	s := app.taggingStage(doc)
	if s.State != "working" || s.Detail != "Working…" {
		t.Errorf("in flight: got (%q, %q), want (working, Working…)", s.State, s.Detail)
	}

	// Still outstanding as far as the watching page is concerned, or it would
	// stop polling with the call still out.
	if !app.enrichq.Has(doc.ID) {
		t.Error("Has() went false while the call was in flight")
	}

	app.enrichq.clearActive(doc.ID)
	if app.enrichq.Active(doc.ID) {
		t.Error("Active() still true after the call finished")
	}
}

// The OCR stage was making the same claim: pending, while ocrmypdf was running.
func TestReadStageWorkingWhileProcessing(t *testing.T) {
	if s := readStage(&Doc{Status: StatusProcessing}); s.State != "working" {
		t.Errorf("State = %q, want working", s.State)
	}
}

// A row goes quiet when the pipeline finishes, but the title, summary and tags
// are all still to come — so the index has to ask the queue, which is the only
// thing that knows. The index itself cannot: a document is written to Typesense
// as ready the moment the local tools are done.
func TestEnrichingIDs(t *testing.T) {
	app := &App{}
	hits := []Hit{{Doc: &Doc{ID: 1}}, {Doc: &Doc{ID: 2}}, {Doc: &Doc{ID: 3}}}

	// No queue at all — no model configured — must not panic or claim work.
	if got := app.enrichingIDs(hits); got != nil {
		t.Errorf("with no queue: got %v, want nil", got)
	}

	app.enrichq = NewEnrichQueue(app)
	app.enrichq.Add(2)
	got := app.enrichingIDs(hits)
	if !got[2] || got[1] || got[3] {
		t.Errorf("queued: got %v, want only 2", got)
	}

	// In flight counts too, or the badge would blink off for exactly the
	// seconds the model is working and live.js would stop watching.
	app.enrichq.next()
	if got := app.enrichingIDs(hits); !got[2] {
		t.Errorf("in flight: got %v, want 2 still marked", got)
	}

	app.enrichq.clearActive(2)
	if got := app.enrichingIDs(hits); len(got) != 0 {
		t.Errorf("finished: got %v, want empty", got)
	}
}

// Several documents can be at the model at once now, which the queue could not
// say before: active was one id, so with a pool it would have named whichever
// worker claimed last and called the rest of them queued.
func TestEnrichQueueTracksSeveralInFlight(t *testing.T) {
	app := &App{cfg: Config{LLMModel: "gpt-5.6-luna"}, enricher: &OpenAIEnricher{}}
	app.enrichq = NewEnrichQueue(app)
	for _, id := range []int{1, 2, 3, 4, 5} {
		app.enrichq.Add(id)
	}

	// Four workers each claim one, as Run's pool would.
	var claimed []int
	for range 4 {
		id, ok := app.enrichq.next()
		if !ok {
			t.Fatal("next() ran dry with documents still pending")
		}
		claimed = append(claimed, id)
	}
	if pending, _, _ := app.enrichq.Stats(); pending != 1 {
		t.Errorf("pending = %d, want 1", pending)
	}

	for _, id := range claimed {
		if !app.enrichq.Active(id) {
			t.Errorf("doc %d claimed but not active", id)
		}
		if s := app.taggingStage(&Doc{ID: id, Status: StatusReady}); s.State != "working" {
			t.Errorf("doc %d stage = %q, want working", id, s.State)
		}
	}
	// Whichever one nobody took is queued, not working. Which one that is, is
	// deliberately not knowable: the queue is a set and hands out an arbitrary
	// member, so the test asks who is left rather than assuming.
	left := 0
	for _, id := range []int{1, 2, 3, 4, 5} {
		if !slices.Contains(claimed, id) {
			left = id
		}
	}
	if left == 0 {
		t.Fatal("all five were claimed by four workers")
	}
	if s := app.taggingStage(&Doc{ID: left, Status: StatusReady}); s.State != "pending" {
		t.Errorf("unclaimed doc %d stage = %q, want pending", left, s.State)
	}

	// Finishing one must not release the others.
	app.enrichq.clearActive(claimed[0])
	if app.enrichq.Active(claimed[0]) {
		t.Error("finished document still active")
	}
	for _, id := range claimed[1:] {
		if !app.enrichq.Active(id) {
			t.Errorf("doc %d stopped being active when a sibling finished", id)
		}
	}
}

// next() is the one place four workers touch the same slice, and handing the
// same document to two of them would pay for it twice.
func TestEnrichQueueNextIsExclusive(t *testing.T) {
	app := &App{}
	q := NewEnrichQueue(app)
	const n = 200
	for id := 1; id <= n; id++ {
		q.Add(id)
	}

	var mu sync.Mutex
	seen := map[int]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				id, ok := q.next()
				if !ok {
					return
				}
				mu.Lock()
				seen[id]++
				mu.Unlock()
				q.clearActive(id)
			}
		}()
	}
	wg.Wait()

	if len(seen) != n {
		t.Errorf("saw %d distinct documents, want %d", len(seen), n)
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("doc %d handed out %d times, want 1", id, times)
		}
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
		{".docx", true, kindOffice},
		{".DOC", true, kindOffice},
		{".xlsx", true, kindOffice},
		{".exe", false, 0},
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
	const want = ".doc,.docx,.jpeg,.jpg,.md,.odp,.ods,.odt,.pdf,.png,.ppt,.pptx,.rtf,.tif,.tiff,.txt,.webp,.xls,.xlsx"
	if got := acceptedExts(); got != want {
		t.Errorf("acceptedExts() = %q, want %q", got, want)
	}
}

// pageStamps is the entire text layer of a real eight-page document: a scanned
// zoo ticket with a page number printed over each image. Thirteen characters a
// page passed for "this PDF has its own text", so tesseract was told to skip
// every page that had any and the model was told the document was already read.
const pageStamps = "Page 1 of 8\n\nPage 2 of 8\n\nPage 3 of 8\n\nPage 4 of 8\n\n" +
	"Page 5 of 8\n\nPage 6 of 8\n\nPage 7 of 8\n\nPage 8 of 8\n\n"

// An authenticating proxy answers who is asking and never whether they meant
// to ask, so the archive still has to refuse a request some other site made on
// a signed-in reader's behalf.
func TestCrossSiteWritesAreRefused(t *testing.T) {
	const public = "https://docs.example.com"
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		allow   bool
	}{
		{"a form on our own page", "POST", map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		{"typed in, or a bookmark", "POST", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"a form on someone else's page", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		// One host. A neighbouring subdomain is somebody else as far as this is
		// concerned, and saying so costs nothing here.
		{"a neighbouring subdomain", "POST", map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		// Reading is not refused: the proxy has already decided who may read,
		// and a cross-site GET carries no authority a page did not already have.
		{"reading, from anywhere", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		// Older browsers, falling back to Origin.
		{"old browser, our origin", "POST", map[string]string{"Origin": public}, true},
		{"old browser, elsewhere", "POST", map[string]string{"Origin": "https://evil.example"}, false},
		{"old browser, trailing slash", "POST", map[string]string{"Origin": public + "/"}, true},
		// curl, a script, a health check. Allowed deliberately: the attack is a
		// request forged inside somebody's browser, and no browser can be made
		// to send neither header.
		{"not a browser at all", "POST", nil, true},
	}
	for _, c := range cases {
		reached := false
		h := guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }), public)
		req := httptest.NewRequest(c.method, "/doc/5/delete", nil)
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if reached != c.allow {
			t.Errorf("%s: handler reached = %v, want %v", c.name, reached, c.allow)
		}
		if !c.allow && rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, http.StatusForbidden)
		}
	}
}

// With no -public-origin there is nothing to compare an Origin against, and
// guessing from the Host header would reject every real request the moment a
// proxy is in front. Sec-Fetch-Site still does the work.
func TestWithoutAPublicOriginTheHeaderStillDecides(t *testing.T) {
	for _, c := range []struct {
		name  string
		hdr   map[string]string
		allow bool
	}{
		{"still refused on the modern header", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"an origin alone cannot be judged", map[string]string{"Origin": "https://evil.example"}, true},
	} {
		reached := false
		h := guard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }), "")
		req := httptest.NewRequest("POST", "/upload", nil)
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if reached != c.allow {
			t.Errorf("%s: handler reached = %v, want %v", c.name, reached, c.allow)
		}
	}
}

// A title the model is asked to preserve has to be one somebody meant. The
// pipeline titles an untitled document after its own filename so nothing shows
// up blank, and defending that would preserve the very thing the model is being
// asked to fix.
func TestOnlyARealTitleIsWorthDefending(t *testing.T) {
	cases := []struct {
		name string
		doc  *Doc
		want string
	}{
		{"never titled, carrying its filename",
			&Doc{Title: "Scan 2026-05-02 11.14.38.pdf", OriginalName: "Scan 2026-05-02 11.14.38.pdf"}, ""},
		{"titled by an earlier pass",
			&Doc{Title: "Taronga Zoo Sydney Entry Tickets", OriginalName: "zoo.pdf"},
			"Taronga Zoo Sydney Entry Tickets"},
		{"titled by hand, and happens to resemble the filename",
			&Doc{Title: "Zoo entry", OriginalName: "zoo entry.pdf"}, "Zoo entry"},
		{"no title at all",
			&Doc{Title: "", OriginalName: "zoo.pdf"}, ""},
	}
	for _, c := range cases {
		if got := realTitle(c.doc); got != c.want {
			t.Errorf("%s: realTitle = %q, want %q", c.name, got, c.want)
		}
	}
}

// The two questions — is this text the document, and does the model need to
// look again — are one question, so they are tested as one. Anything that
// answers yes to the first must answer no to the second.
func TestTextDensityIsOneThreshold(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		pages int
		want  bool
	}{
		{"page furniture", pageStamps, 8, false},
		{"the same furniture on one page", "Page 1 of 1", 1, false},
		{"a real text layer", strings.Repeat("Invoice line item 42.00 ", 60), 3, true},
		{"empty", "", 1, false},
		{"whitespace only", "\n \n\t\n", 1, false},
		// Real text, and far too little of it to be a page of a document — the
		// shape of a scanned page whose only extractable text is a caption. It
		// is below the bar and so gets read again, which costs a slow pass and,
		// if that pass finds nothing more, nothing else: OCR keeps whichever
		// read more.
		{"one page, thirty good characters", "Received with thanks, $42.00.", 1, false},
		// A receipt is short because it is a receipt, not because anything went
		// unread. This is the case the bar must stay below.
		{"a one-page receipt", strings.Repeat("Item 4.20 VAT 0.84 ", 30), 1, true},
		// A scanner watermark: the old floor on total characters existed to
		// catch this, and the density bar now does it unaided.
		{"scanner watermark", "Scanned by CamScanner", 1, false},
		{"mojibake", strings.Repeat("�", 400), 1, false},
		// pdfinfo failed. The question cannot be asked, so it is not answered
		// either way: no claim that the text is the document, and no rescue.
		{"no page count", strings.Repeat("real text ", 100), 0, false},
	}
	for _, c := range cases {
		if got := HasTextLayer(c.text, c.pages); got != c.want {
			t.Errorf("%s: HasTextLayer(%d chars, %d pages) = %v, want %v",
				c.name, utf8.RuneCountInString(strings.TrimSpace(c.text)), c.pages, got, c.want)
		}
		// The rescue is the same threshold read the other way round, except
		// where there is no page count and neither side claims anything.
		wantRescue := !c.want && c.pages > 0
		if got := NeedsVisionRescue(c.text, c.pages); got != wantRescue {
			t.Errorf("%s: NeedsVisionRescue = %v, want %v", c.name, got, wantRescue)
		}
	}
}

// Which mode OCR runs in is the whole fix: fixing the threshold alone changes
// nothing, because --skip-text skips any page carrying any text and every page
// of the stamped scan carries its number.
func TestOCRModeForPicksThreePaths(t *testing.T) {
	dense := strings.Repeat("Invoice line item 42.00 ", 60)
	cases := []struct {
		name   string
		signed bool
		text   string
		pages  int
		want   ocrMode
	}{
		{"signed", true, dense, 3, ocrSkipSigned},
		// Signed wins over everything: the signature is worth more than the
		// text a second pass would add, and ocrmypdf refuses these anyway.
		{"signed and thin", true, pageStamps, 8, ocrSkipSigned},
		{"digital PDF", false, dense, 3, ocrSkipText},
		// No text at all is the branch that keeps --deskew, and it is exactly
		// the crooked scan that needs it.
		{"image-only scan", false, "", 8, ocrSkipText},
		{"whitespace is not text", false, "\n\n \n", 8, ocrSkipText},
		{"page-stamped scan", false, pageStamps, 8, ocrRedoText},
	}
	for _, c := range cases {
		if got := ocrModeFor(c.signed, c.text, c.pages); got != c.want {
			t.Errorf("%s: ocrModeFor = %v, want %v", c.name, got, c.want)
		}
	}

	// --deskew is incompatible with --redo-ocr; ocrmypdf errors out rather than
	// ignoring it, so the two must never be built into the same command line.
	if slices.Contains(ocrFlags(ocrRedoText), "--deskew") {
		t.Error("--redo-ocr pass carries --deskew, which ocrmypdf refuses")
	}
	if !slices.Contains(ocrFlags(ocrSkipText), "--deskew") {
		t.Error("--skip-text pass lost --deskew, which is the only pass that can keep it")
	}
}

// A form that re-posts the current filters has to re-post all of them. The
// custom-range form used to leave out dir, so choosing a date range silently
// flipped the sort back to newest-first. Round-tripping through parseQuery is
// the check that cannot be fooled by adding a field to one list and not the
// other.
func TestFilterFieldsRoundTrip(t *testing.T) {
	want := Query{
		// The reserved tag is in the list rather than in a field of its own,
		// which is the point: it survives the round trip because it is a tag,
		// and nothing had to remember to carry it.
		Q: "oceanside", Tags: []string{"alta", "escrow", TagTrash}, Status: StatusReady,
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

// The trash tag has to travel with the filters for the same reason the sort
// direction does, but the consequence is worse: a form that dropped it would
// answer "sort by document date" by taking you out of the trash, which reads as
// the documents having been restored. parseQuery keeps it —
// TestFilterFieldsRoundTrip covers that — so what is left to check is that the
// forms actually put it on the page.
func TestIndexFormsCarryTheTrashTag(t *testing.T) {
	a := &App{}
	tpl, err := a.templates("index.html")
	if err != nil {
		t.Fatal(err)
	}
	data := page{
		Query: Query{
			Tags: []string{TagTrash}, Range: "custom", From: "2025-03", To: "2026-01",
		},
		Result: &Result{Facets: map[string][]FacetValue{}},
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), `name="tag" value="trash"`); n != 3 {
		t.Errorf("trash hidden input appears %d time(s), want 3 (custom-range form, picker and search box)", n)
	}
	// And it is not offered a second time among the ordinary tag pills, where
	// its count would be of these results rather than of the archive.
	if n := strings.Count(buf.String(), `class="pill-tag`); n != 1 {
		t.Errorf("%d tag pills for one selected reserved tag, want just the reserved one", n)
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
		`<li class="row" data-doc-id=`,     // the result itself, numbered for "@47"
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
		Query:  Query{Q: "oceanside", Tags: []string{TagTrash}},
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

// The year-only input is the one worth pinning. The schema asks for YYYY-12 on a
// document that names only a year and the model does answer in that form, but
// nothing holds it to that, and a bare "2022" used to be discarded here — a
// failure indistinguishable from the model having found no date at all.
func TestNormalizeMonth(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"2022", "2022-12"},
		{"2022-03", "2022-03"},
		{"2022-03-14", "2022-03"},
		{" 2022 ", "2022-12"},
		{"2022-13", ""},
		{"22", ""},
		// Four characters that are not a year, and a year with a partial month:
		// both are junk rather than something to round off to December.
		{"abcd", ""},
		{"2022-3", ""},
		{"", ""},
	} {
		if got := normalizeMonth(tc.in); got != tc.want {
			t.Errorf("normalizeMonth(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

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

// The reserved tags are derived rather than stored, so this is where they can
// be wrong: a document whose state says one thing and whose index entry says
// another. Locked and failed have to partition the failures — a document that
// carried both would be counted twice and would sit in both piles of work —
// and trash has to ride along with either, since a failed document can be
// thrown away like any other.
func TestReservedTagsDerivedFromState(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  Doc
		want []string
	}{
		{"a ready document has none", Doc{Status: StatusReady}, nil},
		{"a failure", Doc{Status: StatusFailed, FailedStage: "normalize"}, []string{TagFailed}},
		{"a failure with no stage is still a failure", Doc{Status: StatusFailed}, []string{TagFailed}},
		{"waiting for a password is locked and not failed",
			Doc{Status: StatusFailed, FailedStage: stageDecrypt}, []string{TagLocked}},
		{"trashed", Doc{Status: StatusReady, DeleteAfterTS: 1}, []string{TagTrash}},
		{"trashed and failed carries both",
			Doc{Status: StatusFailed, FailedStage: "ocr", DeleteAfterTS: 1}, []string{TagTrash, TagFailed}},
		{"still processing is neither", Doc{Status: StatusProcessing}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reservedTags(&tc.doc); !slices.Equal(got, tc.want) {
				t.Errorf("reservedTags = %q, want %q", got, tc.want)
			}
		})
	}
}

// The index carries the derived tags and the sidecar never does, which is what
// makes "filtering works just like tags" free. The stripping matters as much as
// the appending: a sidecar written before any of this existed can hold a
// literal "failed", and a document listing it twice would be one document
// counted twice on the pill.
func TestIndexCarriesReservedTagsAndOnlyOnce(t *testing.T) {
	doc := &Doc{
		Status: StatusFailed, FailedStage: stageDecrypt,
		Tags: []string{"statement", TagFailed, TagTrash},
	}
	got, _ := tsDoc(doc)["tags"].([]string)
	want := []string{"statement", TagLocked}
	if !slices.Equal(got, want) {
		t.Errorf("indexed tags = %q, want %q", got, want)
	}
	// And the document itself is untouched: this is a rendering of it, not an
	// edit to it, and the sidecar is written from the same value.
	if len(doc.Tags) != 3 {
		t.Errorf("tsDoc rewrote the document's own tags to %q", doc.Tags)
	}
	// A Doc read back out of the index has to mean what its sidecar means, or
	// the tag box on the document page would offer to save what it was shown.
	back := docFromMap(map[string]any{"id": "1", "tags": []any{"statement", TagLocked}})
	if !slices.Equal(back.Tags, []string{"statement"}) {
		t.Errorf("tags read back from the index = %q, want the sidecar's own", back.Tags)
	}
}

// Trash is the one reserved tag that is more than a tag: it is excluded unless
// it is asked for, and that rule lives in the filter every caller's request is
// built through rather than in the callers.
func TestTrashIsHiddenUnlessItsTagIsSelected(t *testing.T) {
	var asked []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Query().Get("filter_by"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"found":0,"page":1,"hits":[]}`)
	}))
	defer ts.Close()

	s := NewSearch(ts.URL, "test-key", "documents")
	for _, q := range []Query{{}, {Tags: []string{"alta"}}, {Tags: []string{TagTrash}}, {Status: StatusFailed}} {
		if _, err := s.Query(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		`delete_after_ts:=0`,
		`delete_after_ts:=0 && tags:="alta"`,
		`delete_after_ts:>0 && tags:="trash"`,
		`delete_after_ts:=0 && status:="failed"`,
	}
	if !slices.Equal(asked, want) {
		t.Errorf("filters sent:\n got %q\nwant %q", asked, want)
	}
}

// Nobody outside the derivation may allocate one of these names, so every list
// arriving from outside is stripped. The model's is the interesting one: it is
// shown the tags already in use, that list is a facet count of the index, and
// the index is now full of reserved names.
func TestReservedTagsCannotBeSelfAllocated(t *testing.T) {
	if got := withoutReserved([]string{"trash", "statement", "locked", "failed", "needs-review"}); !slices.Equal(got, []string{"statement", "needs-review"}) {
		t.Errorf("withoutReserved kept %q", got)
	}
	// The caller's slice is left alone: these lists belong to documents.
	in := []string{"trash", "statement"}
	withoutReserved(in)
	if !slices.Equal(in, []string{"trash", "statement"}) {
		t.Errorf("withoutReserved edited the list it was given: %q", in)
	}
}

// The pills lead the tag row with counts of the archive, and the ordinary pills
// and the browse panel must not offer the same names a second time with counts
// of the results.
func TestReservedPillsAreSeparateFromTheTagPills(t *testing.T) {
	p := page{
		Query:    Query{Tags: []string{TagTrash, "alta"}},
		Reserved: ReservedCounts{Locked: 2, Trash: 9},
		Result: &Result{Facets: map[string][]FacetValue{
			"tags": {{Value: "alta", Count: 4}, {Value: TagTrash, Count: 9}, {Value: TagFailed, Count: 1}},
		}},
	}

	var got []string
	for _, r := range p.ReservedPills() {
		got = append(got, fmt.Sprintf("%s/%d/%t", r.Tag, r.Count, r.On))
	}
	// Failed has no documents and is not selected, so it is not offered; trash
	// is selected and locked has documents, so both are.
	if want := []string{"locked/2/false", "trash/9/true"}; !slices.Equal(got, want) {
		t.Errorf("pills = %q, want %q", got, want)
	}

	for _, f := range p.TagFacets() {
		if isReserved(f.Value) {
			t.Errorf("the tag browser offers %q, which has its own pill", f.Value)
		}
	}
	for _, f := range p.TopTags() {
		if isReserved(f.Value) {
			t.Errorf("the tag pills offer %q, which has its own pill", f.Value)
		}
	}
	// Called twice on purpose: the facets are filtered by a delete that writes
	// through to the slice it is given, so a second render must see the same
	// list rather than the wreckage of the first.
	if a, b := len(p.TagFacets()), len(p.TagFacets()); a != b || a != 1 {
		t.Errorf("TagFacets gave %d values then %d, want 1 both times", a, b)
	}
}

// Clear takes away the tags it stands among — reserved ones included, since
// they are tags — and leaves the date window alone. That last part was a
// deliberate fix: reaching for the tags you had and losing the months you had
// chosen with them is a worse surprise than an extra click.
func TestClearTagsDropsReservedTagsButNotTheDate(t *testing.T) {
	u, err := url.Parse("/?q=oceanside&tag=trash&tag=alta&range=custom&from_y=2025&from_m=03&to_y=2026&to_m=01&page=3")
	if err != nil {
		t.Fatal(err)
	}
	got := parseQuery(mustParseQuery(t, (page{URL: u}).ClearTags()))
	want := Query{Q: "oceanside", Range: "custom", From: "2025-03", To: "2026-01"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after Clear:\n got %+v\nwant %+v", got, want)
	}
}

func mustParseQuery(t *testing.T, rawurl string) url.Values {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatalf("parsing %q: %v", rawurl, err)
	}
	return u.Query()
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
			Q: "oceanside", Tags: []string{"alta", TagTrash}, Status: StatusReady,
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
		`name="tag" value="` + TagTrash + `"`,
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
		Q: "oceanside", Tags: []string{"alta", "escrow", TagTrash}, Status: StatusReady,
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
		`id="results"`,                 // the rows, as before
		`<li class="row" data-doc-id=`, //
		`id="tag-pills"`,               // the pills, their counts and the state of each
		`class="pill-tag on"`,
		`>escrow<`,
		`id="tag-browse"`, // the vocabulary, and the foot that counts it
		`data-tag="escrow"`,
		"2 tags in these results",
		// The bulk tag controls and the vocabulary behind them. They live in
		// the swapped region because their datalist is a list of the tags in
		// these results, and because the popovers are drawn from the same
		// count of them that everything else here is.
		`formaction="/docs/tags/add"`,
		`formaction="/docs/tags/remove"`,
		`<datalist id="tag-vocab">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the fragment is missing %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{
		"<html", "<head", "<body", "topbar", "searchbox", "filterbar", "<form",
		// The two things the reader sets by hand, which a swap must not touch.
		`id="tags-open"`, "tag-filter",
		// The reserved pills, whose counts are of the archive and do not move
		// with the search text — a swap that carried them would put them back
		// with no counts at all.
		"pill-reserved",
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
	// Encrypted starts true to prove the stage leaves it alone. This is what a
	// reprocess of an already-replaced document looks like: its original is
	// plain now, and clearing the flag would erase the only record that a
	// password was ever needed.
	doc := &Doc{ID: 1, Encrypted: true}
	dst := filepath.Join(dir, "decrypted.pdf")
	src, err := p.decrypt(ctx, doc, plain, dst)
	if err != nil {
		t.Fatalf("decrypt on a plain PDF: %v", err)
	}
	if src != plain {
		t.Errorf("decrypt returned %q, want the original %q", src, plain)
	}
	if !doc.Encrypted {
		t.Error("a reprocess cleared Encrypted, so the document has forgotten that it arrived password-protected")
	}
	// A document that was never encrypted must not acquire the flag either.
	fresh := &Doc{ID: 2}
	if _, err := p.decrypt(ctx, fresh, plain, dst); err != nil {
		t.Fatalf("decrypt on a plain PDF: %v", err)
	}
	if fresh.Encrypted {
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
	// The sentinel itself, so a caller can tell "nobody has the password" apart
	// from "qpdf is broken" — which is the distinction that decides whether the
	// document offers a password box or reports a fault. It deliberately carries
	// no instruction and no path: the document page says this in its timeline
	// with the box to type into right below, and the path this used to name went
	// stale the moment the password file moved into the archive.
	if !errors.Is(err, errNoPassword) {
		t.Errorf("the failure is %q, which no caller can tell apart from qpdf failing", err)
	}
	if strings.Contains(err.Error(), pwFile) {
		t.Errorf("the failure names %s, sending the reader to edit a file by hand", pwFile)
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
	// This stage never rewrites the original, whatever becomes of it later.
	// Replacing it with the decrypted copy is adoptDecrypted's decision, taken
	// once the document is known not to be signed, and nothing here has asked
	// that question yet.
	if PDFEncryption(ctx, locked) != pdfLocked {
		t.Error("decrypt modified the original — this stage only ever writes its copy")
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

// restrictPDF builds the other encrypted shape: no user password, so it opens
// for anybody, and an owner password that forbids printing and copying. This is
// what most statements that are "password-protected" actually are, and the
// restrictions are real ones rather than the flags qpdf leaves permissive by
// default — otherwise the test that they are gone afterwards would pass on a
// file that never had any.
func restrictPDF(t *testing.T, src, dst string) string {
	t.Helper()
	_, err := runCmd(context.Background(), 30*time.Second, "qpdf", "--encrypt",
		"--user-password=", "--owner-password=no-printing-please", "--bits=256",
		"--print=none", "--modify=none", "--extract=n", "--", src, dst)
	if err != nil {
		t.Fatalf("building the restricted fixture: %v", err)
	}
	return dst
}

// decryptAndAdopt runs the two stages that between them decide what becomes of
// a document's original: the decryption, and the replacement that follows it.
// The pipeline runs them a few lines apart with the signature check in between,
// because whether the original may be replaced depends on the answer — so a
// test that wants the signed branch sets doc.Signed before calling this, which
// is exactly what the pipeline does with what IsSignedPDF told it.
func decryptAndAdopt(t *testing.T, p *Pipeline, doc *Doc, orig string) error {
	t.Helper()
	dec := filepath.Join(t.TempDir(), "decrypted.pdf")
	src, err := p.decrypt(context.Background(), doc, orig, dec)
	if err != nil {
		return err
	}
	return p.adoptDecrypted(doc, orig, src)
}

// lockedFixture puts a prepared file where a document's original lives and
// returns the path along with the hash of the bytes as they arrived, which is
// what the sidecar records and what every one of these tests checks did not
// move.
func lockedFixture(t *testing.T, s *Store, id int, src string) (orig, arrived string) {
	t.Helper()
	orig = s.OriginalPath(id, ".pdf")
	if err := copyFile(src, orig); err != nil {
		t.Fatalf("placing the original: %v", err)
	}
	sum, _, err := hashFile(orig)
	if err != nil {
		t.Fatalf("hashing the original: %v", err)
	}
	return orig, sum
}

// originalsDir is what is actually sitting in originals/ afterwards, including
// dotfiles. The atomic write leaves its temp file there if it fails part way,
// and a stray .tmp-* beside a document is how a half-written original would
// first show itself.
func originalsDir(t *testing.T, s *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(s.OriginalPath(1, ".pdf")))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// The point of the whole thing: once a document can be opened, nothing left on
// disk still demands a password. The decrypted copy takes the original's place,
// which deliberately overrides the rule that the original is the untouched
// preservation copy — an owner's judgement that a file nobody can open without
// first finding a password is worth less than the same file openable. The text
// has to come through intact, and the recorded hash has to stay as it was: it
// is the dedup identity, and the same locked file sent again is still the same
// document.
func TestSuccessfulDecryptionReplacesTheOriginal(t *testing.T) {
	const (
		text = "Interest certificate FY 2025-26, account 4471"
		pw   = "MRJN-0412-open"
	)
	fixtures := t.TempDir()
	locked := encryptPDF(t, testPDF(t, fixtures, "plain", text),
		filepath.Join(fixtures, "locked.pdf"), pw, "owner-too")

	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orig, arrived := lockedFixture(t, s, 7, locked)

	p := &Pipeline{app: &App{store: s, cfg: Config{PasswordFile: s.PasswordsPath()}, pdfPasswords: []string{pw}}}
	doc := &Doc{ID: 7, SHA256: arrived, OriginalExt: ".pdf"}
	if err := decryptAndAdopt(t, p, doc, orig); err != nil {
		t.Fatalf("decrypting a document whose password is known: %v", err)
	}

	ctx := context.Background()
	if got := PDFEncryption(ctx, orig); got != pdfPlain {
		t.Errorf("the original is %v, want pdfPlain — it was replaced precisely so it would open with no password", got)
	}
	got, err := ExtractText(ctx, orig)
	if err != nil {
		t.Fatalf("pdftotext on the replaced original: %v", err)
	}
	if !strings.Contains(got, text) {
		t.Errorf("the replaced original reads %q, want it to contain %q — the document itself has to survive losing its lock", got, text)
	}

	// The stored hash is of the bytes that arrived, and it now describes no file
	// on disk. That is the trade, not an oversight: FindByHash has to recognise
	// the same encrypted file if it is dropped in again, and the only hash that
	// can match what the sender will send is the one taken before we changed it.
	if doc.SHA256 != arrived {
		t.Errorf("SHA256 is %s, want the arriving %s — the same file sent again would be ingested a second time", doc.SHA256, arrived)
	}
	onDisk, _, err := hashFile(orig)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk == arrived {
		t.Error("the original still hashes to the encrypted bytes, so nothing was replaced")
	}
	if !doc.Encrypted {
		t.Error("the document does not record that it arrived encrypted, so its page cannot explain why its checksum names bytes that are gone")
	}
	// The replacement goes through a temp file in originals/ and a rename. Only
	// the document may be left behind.
	if names := originalsDir(t, s); len(names) != 1 || names[0] != "7.pdf" {
		t.Errorf("originals/ holds %v, want only 7.pdf", names)
	}
}

// A file that opens with the empty password but refuses to be printed or copied
// gets the same treatment, because it is the same annoyance and removing the
// restrictions costs nothing: qpdf rewrites it losslessly and nobody had to
// know a password for it in the first place.
func TestRestrictedOriginalIsReplacedAndLosesItsRestrictions(t *testing.T) {
	const text = "Form 1040, page 1 of 48"
	fixtures := t.TempDir()
	restricted := restrictPDF(t, testPDF(t, fixtures, "plain", text),
		filepath.Join(fixtures, "restricted.pdf"))

	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orig, arrived := lockedFixture(t, s, 3, restricted)

	ctx := context.Background()
	if got := PDFEncryption(ctx, orig); got != pdfRestricted {
		t.Fatalf("the fixture is %v, want pdfRestricted — this test is about the file that opens but is locked down", got)
	}

	// No passwords at all are configured, because none are needed: the empty
	// password DecryptPDF always tries first is the whole of what this takes.
	p := &Pipeline{app: &App{store: s, cfg: Config{PasswordFile: s.PasswordsPath()}}}
	doc := &Doc{ID: 3, SHA256: arrived, OriginalExt: ".pdf"}
	if err := decryptAndAdopt(t, p, doc, orig); err != nil {
		t.Fatalf("decrypting a restricted document: %v", err)
	}

	if got := PDFEncryption(ctx, orig); got != pdfPlain {
		t.Errorf("the original is %v, want pdfPlain", got)
	}
	// Asked of qpdf rather than inferred: restrictions cannot outlive the
	// encryption that carries them, so "not encrypted" is the strongest form of
	// "prints and copies like any other document".
	out, err := runCmd(ctx, 30*time.Second, "qpdf", "--show-encryption", orig)
	if err != nil {
		t.Fatalf("qpdf --show-encryption on the replaced original: %v", err)
	}
	if !strings.Contains(out, "File is not encrypted") {
		t.Errorf("qpdf says the replaced original is still encrypted:\n%s", out)
	}
	got, err := ExtractText(ctx, orig)
	if err != nil {
		t.Fatalf("pdftotext on the replaced original: %v", err)
	}
	if !strings.Contains(got, text) {
		t.Errorf("the replaced original reads %q, want it to contain %q", got, text)
	}
	if doc.SHA256 != arrived {
		t.Errorf("SHA256 is %s, want the arriving %s", doc.SHA256, arrived)
	}
}

// The one document that keeps its encrypted original. qpdf --decrypt rewrites
// the file, and a rewritten PDF no longer matches the bytes its signature was
// computed over — so replacing this one would trade an annoyance for the
// destruction of the only thing that makes the document evidence. It stays
// locked; the archive copy is decrypted like everybody else's.
//
// On the fixture: this drives the decision directly with doc.Signed set, rather
// than signing a PDF. Signing needs a certificate and a signer — pyhanko,
// PDFBox — and neither is a dependency of this program, so a test that required
// one would not run on a machine that can run the pipeline. Building a
// signature dictionary by hand instead would prove only that the detector can
// be fooled by a forgery. The flag is what the pipeline itself passes here,
// from IsSignedPDF on the decrypted copy, and the branch under test is what is
// done with it.
func TestSignedAndEncryptedOriginalIsKept(t *testing.T) {
	const pw = "notary-2026"
	fixtures := t.TempDir()
	locked := encryptPDF(t, testPDF(t, fixtures, "plain", "Deed of conveyance, executed 12 March 2026"),
		filepath.Join(fixtures, "signed-locked.pdf"), pw, "owner-too")

	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orig, arrived := lockedFixture(t, s, 11, locked)

	p := &Pipeline{app: &App{store: s, cfg: Config{PasswordFile: s.PasswordsPath()}, pdfPasswords: []string{pw}}}
	doc := &Doc{ID: 11, SHA256: arrived, OriginalExt: ".pdf", Signed: true}
	if err := decryptAndAdopt(t, p, doc, orig); err != nil {
		t.Fatalf("decrypting a signed document: %v", err)
	}

	ctx := context.Background()
	if got := PDFEncryption(ctx, orig); got != pdfLocked {
		t.Errorf("the original is %v, want pdfLocked — the signed copy must keep its encryption, since that is the only version whose signature still verifies", got)
	}
	onDisk, _, err := hashFile(orig)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk != arrived {
		t.Error("the signed original was rewritten, which invalidates the signature it was kept for")
	}
	// It is still an encrypted document, and its page still has to say so — the
	// banner reads Signed as well, and says the opposite thing for this one.
	if !doc.Encrypted {
		t.Error("the document does not record that it arrived encrypted")
	}
}

// The ordinary document pays nothing for any of this. A plain PDF is not
// rewritten — not re-encoded, not even touched — because there is nothing to
// take off it, and a reprocess of a document whose original was already
// replaced arrives here looking exactly like this one.
func TestPlainOriginalIsNeverRewritten(t *testing.T) {
	fixtures := t.TempDir()
	plain := testPDF(t, fixtures, "plain", "Northwind Utilities, March 2026")

	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	orig, arrived := lockedFixture(t, s, 5, plain)
	before, err := os.Stat(orig)
	if err != nil {
		t.Fatal(err)
	}

	p := &Pipeline{app: &App{store: s, cfg: Config{PasswordFile: s.PasswordsPath()}}}
	// Encrypted set, as it is on a reprocess of a document that arrived locked
	// and has already had its original replaced: the flag stays, and this stage
	// still has nothing to do.
	doc := &Doc{ID: 5, SHA256: arrived, OriginalExt: ".pdf", Encrypted: true}
	if err := decryptAndAdopt(t, p, doc, orig); err != nil {
		t.Fatalf("reprocessing a document whose original is already plain: %v", err)
	}

	after, err := os.Stat(orig)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the original's mtime moved from %s to %s, so something wrote it", before.ModTime(), after.ModTime())
	}
	onDisk, _, err := hashFile(orig)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk != arrived {
		t.Error("the original's bytes changed, and nothing here had any reason to change them")
	}
	if !doc.Encrypted {
		t.Error("a reprocess cleared Encrypted, and the banner explaining the document's checksum would vanish with it")
	}
	if names := originalsDir(t, s); len(names) != 1 || names[0] != "5.pdf" {
		t.Errorf("originals/ holds %v, want only 5.pdf", names)
	}
}

// Where the passwords live. The default belongs in the archive, so that a
// backup of the documents carries the means to open them and a restore onto
// another machine is not one missing file away from a folder of documents
// nobody can read. The flag still wins, for anyone who keeps them elsewhere.
//
// It cannot be the flag's default value, which is the whole reason this
// function exists: the default is derived from -data, and nothing knows -data
// until flag.Parse has run. An empty flag therefore means "not given".
func TestPasswordFileDefaultsIntoTheArchive(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := s.PasswordsPath(), filepath.Join(dir, "passwords"); got != want {
		t.Errorf("PasswordsPath = %q, want %q", got, want)
	}
	if got := resolvePasswordFile("", s); got != s.PasswordsPath() {
		t.Errorf("with no -pdf-passwords the file is %q, want the archive's %q", got, s.PasswordsPath())
	}
	elsewhere := filepath.Join(t.TempDir(), ".docovia-passwords")
	if got := resolvePasswordFile(elsewhere, s); got != elsewhere {
		t.Errorf("with -pdf-passwords %q the file is %q, want the flag to win", elsewhere, got)
	}
}

// A password proved against a real document is written into the file that
// exists to hold it, and the file is not ours alone: someone maintains it by
// hand, with comments saying which password belongs to which bank. So it is
// appended to and never rewritten, a password already in it is not filed a
// second time, and a file left ending mid-line does not have the new password
// spliced onto the end of the old one — which would take both out of service
// and be all but invisible in a file nobody wants to open and read.
func TestRememberPasswordAppendsWithoutDisturbingTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "passwords")
	a := &App{cfg: Config{PasswordFile: path}}

	// A file that does not exist yet is the ordinary case on a machine that has
	// only just met an encrypted document.
	if err := a.rememberPassword("first-one"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("the password file was created mode %#o, readable beyond this user", mode)
	}

	// Hand-edited afterwards, comment and all, and left without a final
	// newline the way an editor that does not add one leaves it.
	if err := os.WriteFile(path, []byte("first-one\n# the second bank\nsecond-one"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Already there: nothing to do, and in particular nothing to write.
	if err := a.rememberPassword("second-one"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "first-one\n# the second bank\nsecond-one" {
		t.Errorf("a password already in the file changed it to %q", got)
	}
	if err := a.rememberPassword("third-one"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "first-one\n# the second bank\nsecond-one\nthird-one\n"; string(got) != want {
		t.Errorf("password file is\n%q\nwant\n%q", got, want)
	}
	// The comment is still there: this is somebody's file.
	if !strings.Contains(string(got), "# the second bank") {
		t.Error("the append rewrote the file and lost what was written in it by hand")
	}

	// And the pipeline can see all of it without a restart, which is the point
	// of writing it to memory as well.
	if want := []string{"first-one", "second-one", "third-one"}; !slices.Equal(a.passwords(), want) {
		t.Errorf("in memory: %q, want %q", a.passwords(), want)
	}
	// The copy handed out is the caller's own: appending to it must not reach
	// back into the list a pipeline worker is about to read.
	a.passwords()[0] = "not-a-password"
	if want := []string{"first-one", "second-one", "third-one"}; !slices.Equal(a.passwords(), want) {
		t.Errorf("a caller writing to what passwords() returned changed the list itself: %q", a.passwords())
	}
}

// The unlock form's two refusals, which are the whole of what it does when it
// does not work. Neither reaches Typesense: a request that is not going to
// change anything must not need the index to say so.
//
// The wrong password is the one to get right. Nothing is written — not the
// password file, not the sidecar — and the answer carries no trace of what was
// typed, because the reader's next act is to try again and the one before it
// may have been to paste the right password into the wrong document.
func TestUnlockRefusesWhatItCannotUse(t *testing.T) {
	const pw = "MRJN0412-open"
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	locked := encryptPDF(t, testPDF(t, dir, "plain", "Interest certificate"),
		s.OriginalPath(1, ".pdf"), pw, "owner-only")

	a := &App{cfg: Config{PasswordFile: s.PasswordsPath()}, store: s}
	mux := http.NewServeMux()
	a.routes(mux)

	post := func(id int, password string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", fmt.Sprintf("/doc/%d/unlock", id),
			strings.NewReader(url.Values{"password": {password}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		mux.ServeHTTP(rec, req)
		return rec
	}

	// A document that is not waiting for a password has no use for one, so the
	// form has no meaning on it — a stale tab, or a hand-made request.
	if err := s.Save(&Doc{ID: 1, Status: StatusFailed, FailedStage: "ocr", OriginalExt: ".pdf"}); err != nil {
		t.Fatal(err)
	}
	if got := post(1, pw).Code; got != http.StatusBadRequest {
		t.Errorf("unlocking a document that failed at ocr = %d, want %d", got, http.StatusBadRequest)
	}

	if err := s.Save(&Doc{ID: 1, Status: StatusFailed, FailedStage: stageDecrypt, OriginalExt: ".pdf"}); err != nil {
		t.Fatal(err)
	}
	for _, guess := range []string{"", "not-the-one"} {
		rec := post(1, guess)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("a wrong password answered %d, want a redirect back to the form:\n%s", rec.Code, rec.Body)
		}
		if got, want := rec.Header().Get("Location"), "/doc/1?unlock=bad"; got != want {
			t.Errorf("redirected to %q, want %q", got, want)
		}
		if guess != "" && strings.Contains(rec.Header().Get("Location")+rec.Body.String(), guess) {
			t.Error("the answer to a wrong password contains the password")
		}
		if _, err := os.Stat(s.PasswordsPath()); !os.IsNotExist(err) {
			t.Fatalf("a wrong password wrote to the password file")
		}
		if doc, err := s.Load(1); err != nil || doc.Status != StatusFailed {
			t.Errorf("a wrong password changed the document to %+v", doc)
		}
	}

	// Nothing left behind in the archive directory either: the copy qpdf writes
	// while the password is being tried is the answer, not a file.
	entries, err := os.ReadDir(s.ArchiveDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("trying passwords left %d file(s) in the archive directory", len(entries))
	}
	// And the original is untouched, which is what makes trying again free.
	if PDFEncryption(context.Background(), locked) != pdfLocked {
		t.Error("trying a password rewrote the original")
	}
}

// The rig the bulk tag routes need: a real store to write sidecars into, and a
// stand-in for Typesense that accepts whatever it is sent. The stand-in has to
// succeed — persist retries an indexing failure until the request's context
// ends, so a server that answered anything else would hang this file rather
// than fail it.
func newTagApp(t *testing.T) (*Store, *http.ServeMux) {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 201 with the document echoed back is what an index write answers,
		// and the client reads the status: anything else is an error to it.
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(ts.Close)

	a := &App{store: s, search: NewSearch(ts.URL, "test-key", "documents")}
	mux := http.NewServeMux()
	a.routes(mux)
	return s, mux
}

func postTags(t *testing.T, mux *http.ServeMux, op string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/docs/tags/"+op, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(rec, req)
	return rec
}

func tagsOf(t *testing.T, s *Store, id int) []string {
	t.Helper()
	doc, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return doc.Tags
}

// Filing a stack of documents under one name in one act. The tags already on
// them are the thing to be careful with: this is an addition, not the tag box
// on the document page, which replaces the whole list.
//
// Both fields are posted, because both always are — the two popovers live
// inside the index's one form, so every submit carries whatever is sitting in
// the other one. The add route must read the box it is named for and no other,
// or "Add tax" would quietly remove whatever was left in the Remove box.
func TestBulkTagAddsToTheSelectionAndLeavesOtherTagsAlone(t *testing.T) {
	s, mux := newTagApp(t)
	for _, doc := range []*Doc{
		{ID: 1, Status: StatusReady, Title: "Water bill", Tags: []string{"utility"}},
		{ID: 2, Status: StatusReady, Title: "Dentist receipt", Tags: []string{"medical", "receipt"}},
		{ID: 3, Status: StatusReady, Title: "Not selected", Tags: []string{"utility"}},
	} {
		if err := s.Save(doc); err != nil {
			t.Fatal(err)
		}
	}

	rec := postTags(t, mux, "add", url.Values{
		"id":         {"1", "2"},
		"tag-add":    {"tax"},
		"tag-remove": {"utility"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /docs/tags/add = %d, want a redirect back to the listing:\n%s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "done=addtag") || !strings.Contains(loc, "n=2") {
		t.Errorf("redirected to %q, want the outcome as done=addtag and n=2", loc)
	}

	if got, want := tagsOf(t, s, 1), []string{"utility", "tax"}; !slices.Equal(got, want) {
		t.Errorf("DOC-1 tags = %q, want %q", got, want)
	}
	if got, want := tagsOf(t, s, 2), []string{"medical", "receipt", "tax"}; !slices.Equal(got, want) {
		t.Errorf("DOC-2 tags = %q, want %q", got, want)
	}
	// The other field rode along and was ignored, which is the whole reason
	// the two boxes are not both called "tag".
	if got := tagsOf(t, s, 1); !slices.Contains(got, "utility") {
		t.Errorf("the add route read the Remove box as well: DOC-1 tags = %q", got)
	}
	// A document nobody selected is a document nothing happened to.
	if got, want := tagsOf(t, s, 3), []string{"utility"}; !slices.Equal(got, want) {
		t.Errorf("DOC-3 was not selected but its tags are now %q, want %q", got, want)
	}
}

// Removing is the same act backwards, with one difference that shows up in the
// notice: a selection is made by ticking rows, not by knowing which of them
// carry the tag, so most of these requests reach documents that never had it.
// Those are not changes and must not be written, journalled or counted — "Tag
// removed from 40 documents" when three of them had it is a report of work
// that did not happen.
func TestBulkTagRemoveOnlyTouchesTheDocumentsThatHadIt(t *testing.T) {
	s, mux := newTagApp(t)
	for _, doc := range []*Doc{
		{ID: 1, Status: StatusReady, Title: "Water bill", Tags: []string{"utility", "tax"}},
		{ID: 2, Status: StatusReady, Title: "Dentist receipt", Tags: []string{"medical"}},
		{ID: 3, Status: StatusReady, Title: "Council rates", Tags: []string{"tax"}},
	} {
		if err := s.Save(doc); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.Stat(s.DocPath(2))
	if err != nil {
		t.Fatal(err)
	}

	rec := postTags(t, mux, "remove", url.Values{
		"id":         {"1", "2", "3"},
		"tag-remove": {"tax"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /docs/tags/remove = %d, want a redirect:\n%s", rec.Code, rec.Body)
	}
	// Three were selected, two of them changed.
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "done=removetag") || !strings.Contains(loc, "n=2") {
		t.Errorf("redirected to %q, want done=removetag and n=2 — the count is of documents that changed", loc)
	}

	if got, want := tagsOf(t, s, 1), []string{"utility"}; !slices.Equal(got, want) {
		t.Errorf("DOC-1 tags = %q, want %q", got, want)
	}
	if got, want := tagsOf(t, s, 3), []string{}; !slices.Equal(got, want) {
		t.Errorf("DOC-3 tags = %q, want the tag gone and nothing else with it", got)
	}
	if got, want := tagsOf(t, s, 2), []string{"medical"}; !slices.Equal(got, want) {
		t.Errorf("DOC-2 never had the tag but its tags are now %q, want %q", got, want)
	}
	// Not merely unchanged in content: not rewritten at all, which is what
	// keeps a bulk remove over a filtered view from touching every sidecar in
	// the archive.
	if after, err := os.Stat(s.DocPath(2)); err != nil {
		t.Fatal(err)
	} else if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a document that did not carry the tag was written anyway")
	}
	// And the notice says so in words, from the count alone.
	got := actionNotice(url.Values{"done": {actionRemoveTag}, "n": {"2"}})
	if len(got) != 1 || got[0].Text != "Tag removed from 2 documents." {
		t.Errorf("notice = %+v, want one line reading \"Tag removed from 2 documents.\"", got)
	}
	if one := actionNotice(url.Values{"done": {actionAddTag}, "n": {"1"}}); len(one) != 1 || one[0].Text != "Tag added to 1 document." {
		t.Errorf("notice for one document = %+v", one)
	}
	// The tag itself is deliberately not in either sentence: it would have to
	// travel in the URL to get there, and a URL is not a place to put text that
	// will be shown to whoever opens it.
	if strings.Contains(rec.Header().Get("Location"), "tax") {
		t.Errorf("the redirect carries the tag text: %q", rec.Header().Get("Location"))
	}
}

// What the two routes refuse, and the state of the archive afterwards, which
// is the part that matters: every one of these is a request that reached a
// selection of documents and must leave all of them exactly as they were.
func TestBulkTagRefusesWhatItCannotFileUnder(t *testing.T) {
	s, mux := newTagApp(t)
	if err := s.Save(&Doc{ID: 1, Status: StatusReady, Title: "Water bill", Tags: []string{"utility"}}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		op   string
		form url.Values
	}{
		// The names the index derives from a document's own state. One written
		// onto a sidecar would either be ignored or, worse, be believed.
		{"a reserved tag", "add", url.Values{"id": {"1"}, "tag-add": {"trash"}}},
		{"a reserved tag, removed", "remove", url.Values{"id": {"1"}, "tag-remove": {TagLocked}}},
		// An empty box is a button pressed by accident, not an instruction.
		{"an empty tag", "add", url.Values{"id": {"1"}, "tag-add": {""}}},
		{"a tag of whitespace", "add", url.Values{"id": {"1"}, "tag-add": {"   "}}},
		{"a tag of separators", "remove", url.Values{"id": {"1"}, "tag-remove": {" , "}}},
		// Nothing ticked. Without this the same request would mean the archive.
		{"nothing selected", "add", url.Values{"tag-add": {"tax"}}},
		// A hand-made request, or a route that grew a third verb somewhere and
		// not here.
		{"an unknown op", "frobnicate", url.Values{"id": {"1"}, "tag-add": {"tax"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postTags(t, mux, tc.op, tc.form)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /docs/tags/%s with %v = %d, want %d\n%s",
					tc.op, tc.form, rec.Code, http.StatusBadRequest, rec.Body)
			}
			if got, want := tagsOf(t, s, 1), []string{"utility"}; !slices.Equal(got, want) {
				t.Errorf("the refusal still wrote to the document: tags = %q, want %q", got, want)
			}
		})
	}
}

// One tag, normalised the way the document page normalises what is typed into
// its tag box — lowered and trimmed — because "Tax" and "tax" filed as two tags
// is a vocabulary nobody can search. splitTags is the shared answer, so the two
// ways into the archive cannot come to disagree about what a tag name is.
func TestBulkTagNormalisesWhatWasTyped(t *testing.T) {
	s, mux := newTagApp(t)
	if err := s.Save(&Doc{ID: 1, Status: StatusReady, Title: "Council rates"}); err != nil {
		t.Fatal(err)
	}

	if rec := postTags(t, mux, "add", url.Values{"id": {"1"}, "tag-add": {"  Tax "}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /docs/tags/add = %d\n%s", rec.Code, rec.Body)
	}
	if got, want := tagsOf(t, s, 1), []string{"tax"}; !slices.Equal(got, want) {
		t.Errorf("tags = %q, want %q", got, want)
	}
	// And the same spelling takes it off again, which is the point of
	// normalising on the way in rather than at the point of comparison.
	if rec := postTags(t, mux, "remove", url.Values{"id": {"1"}, "tag-remove": {"TAX"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /docs/tags/remove = %d\n%s", rec.Code, rec.Body)
	}
	if got := tagsOf(t, s, 1); len(got) != 0 {
		t.Errorf("tags = %q, want the tag gone", got)
	}
}

// The escalation banner, which is how a bulk action comes to mean more than the
// page it was started from. It replaced a button that only Download read, so
// the checkbox is the thing to check: it is what every bulk route now reads,
// it posts by being present rather than by being clicked, and it carries
// pick-scope rather than pick so that the select-all, the running count and the
// stylesheet's "is anything selected" all keep ignoring it.
//
// One page and it is not offered at all — "all of these" and "all of them" are
// the same sentence there, and asking would be a question with one answer.
func TestScopeBannerIsOfferedOnlyWhenThereIsMoreThanOnePage(t *testing.T) {
	a := &App{}
	tpl, err := a.templates("index.html")
	if err != nil {
		t.Fatal(err)
	}
	render := func(pages int) string {
		t.Helper()
		var buf bytes.Buffer
		data := page{Result: &Result{
			Found: 4231, Page: 1, Pages: pages,
			Hits:   []Hit{{Doc: &Doc{ID: 1}}, {Doc: &Doc{ID: 2}}},
			Facets: map[string][]FacetValue{},
		}}
		if err := tpl.ExecuteTemplate(&buf, "results", data); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	body := render(2)
	for _, want := range []string{
		`class="pick-scope" name="scope" value="filtered"`, // the whole mechanism
		"Select all 4,231 matching",                        // the offer
		"Select only this page",                            // and the way back from it
		"<strong>2</strong> on this page selected",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the banner is missing %q:\n%s", want, body)
		}
	}
	// The button this replaced posted scope only to Download, so trashing after
	// pressing it moved one page. Nothing else may post the field.
	if n := strings.Count(body, `name="scope"`); n != 1 {
		t.Errorf("the form posts scope from %d places, want exactly one", n)
	}

	if one := render(1); strings.Contains(one, "scope-bar") {
		t.Errorf("a single page offers to select every other page too:\n%s", one)
	}
}
