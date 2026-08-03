package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-pdf/fpdf"
)

const (
	cmdTimeout = 120 * time.Second
	ocrTimeout = 600 * time.Second
)

// fileKind is how an extension becomes a PDF. The taxonomy lives in one table
// because it was previously spelled four ways — a set of accepted extensions,
// two predicates, and a switch — and adding a format meant finding all of them.
type fileKind int

const (
	kindPDF fileKind = iota
	kindImage
	kindText
)

// supportedExts is everything we accept. Anything else is rejected at intake
// with a failed sidecar rather than silently ignored.
var supportedExts = map[string]fileKind{
	".pdf":  kindPDF,
	".jpg":  kindImage,
	".jpeg": kindImage,
	".png":  kindImage,
	".tif":  kindImage,
	".tiff": kindImage,
	".webp": kindImage,
	".txt":  kindText,
	".md":   kindText,
}

// kindOf accepts any casing, since an extension reaching here came off a
// filename someone else chose.
func kindOf(ext string) (fileKind, bool) {
	k, ok := supportedExts[strings.ToLower(ext)]
	return k, ok
}

func isSupportedExt(ext string) bool {
	_, ok := kindOf(ext)
	return ok
}

func isTextExt(ext string) bool {
	k, ok := kindOf(ext)
	return ok && k == kindText
}

// acceptedExts is the file picker's accept list, taken from the same table the
// server validates against so the two cannot disagree about what may be sent.
func acceptedExts() string {
	out := make([]string, 0, len(supportedExts))
	for ext := range supportedExts {
		out = append(out, ext)
	}
	slices.Sort(out)
	return strings.Join(out, ",")
}

// runCmd executes a command, returning stdout. Stderr is folded into the error so
// failures are diagnosable from the log without re-running by hand.
func runCmd(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		return stdout.String(), fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return stdout.String(), nil
}

// IsSignedPDF reports whether the file carries a digital signature. ocrmypdf
// refuses to process signed PDFs, and invalidating a signature to gain a text
// layer is a bad trade, so these skip OCR entirely.
func IsSignedPDF(path string) bool {
	out, err := runCmd(context.Background(), 30*time.Second, "pdfsig", path)
	if err == nil && strings.Contains(out, "Signature Field") {
		return true
	}
	// pdfsig is unavailable or unhappy: fall back to scanning for the
	// signature dictionary, which needs no external tool.
	b, rerr := os.ReadFile(path)
	if rerr != nil {
		return false
	}
	return bytes.Contains(b, []byte("/ByteRange")) &&
		(bytes.Contains(b, []byte("/Type/Sig")) || bytes.Contains(b, []byte("/Type /Sig")))
}

// pdfCrypt is what qpdf knows about a PDF's encryption. The three values are
// the three answers `qpdf --requires-password` gives, kept apart because they
// call for three different things: nothing, the empty password, and a password
// somebody has to tell us.
type pdfCrypt int

const (
	pdfPlain      pdfCrypt = iota // not encrypted at all
	pdfRestricted                 // encrypted, but the empty password opens it
	pdfLocked                     // needs a password we have not supplied
)

// PDFEncryption asks qpdf which of the three a file is.
//
// --requires-password is the right question. --is-encrypted answers yes for a
// file carrying only an owner password — one that forbids printing or copying
// but opens and reads perfectly well — so it cannot tell a bank statement
// nobody can open from a tax form that merely dislikes being printed.
//
// Anything qpdf cannot answer at all — a missing file, a truncated one — is
// reported as pdfPlain rather than guessed at. Whatever is wrong with such a
// file, a password will not fix it, and the stages that follow fail on it with
// a better message than this function could invent.
func PDFEncryption(ctx context.Context, path string) pdfCrypt {
	_, err := runCmd(ctx, 30*time.Second, "qpdf", "--requires-password", path)
	if err == nil {
		// Exit 0: a password other than the one supplied — and none was — is
		// required, so the file cannot be opened as it stands.
		return pdfLocked
	}
	if exitCode(err) == 3 {
		// Exit 3: encrypted, and the empty password qpdf tried is the correct
		// one. Exit 2 is "not encrypted"; anything else is qpdf failing.
		return pdfRestricted
	}
	return pdfPlain
}

// PDFNeedsPassword reports whether the file cannot be opened without one. It is
// the narrow question: a merely restricted PDF answers false, because every
// tool here already reads it.
func PDFNeedsPassword(ctx context.Context, path string) bool {
	return PDFEncryption(ctx, path) == pdfLocked
}

// errNoPassword is DecryptPDF's answer when it ran out of candidates. It is a
// sentinel because the caller has to tell it apart from qpdf being broken: one
// means "ask the owner for the password", the other means "fix the machine",
// and those are not the same document status.
var errNoPassword = errors.New("no candidate password opened the file")

// DecryptPDF writes a decrypted copy of src at dst, trying the empty password
// first and then each candidate in turn. It returns the position of the one
// that worked — 0 for the empty password, otherwise the 1-based index into
// passwords — so a caller can say which line of the password file was the right
// one without ever handling what is on it.
//
// No password ever reaches the returned error. runCmd folds a command's stderr
// into its error but never its arguments, and qpdf's own message for a wrong
// guess is "invalid password" with the filename, so what comes back names the
// file and the failure and nothing else.
func DecryptPDF(ctx context.Context, src, dst string, passwords []string) (int, error) {
	// The empty password is always tried, and always first. A PDF carrying only
	// an owner password decrypts with it, which turns a file the rest of the
	// pipeline would refuse into an ordinary readable one for the cost of a
	// single qpdf run — before any real password is put at risk of being wrong.
	for i, pw := range append([]string{""}, passwords...) {
		_, err := runCmd(ctx, cmdTimeout, "qpdf", "--password="+pw, "--decrypt", src, dst)
		if err == nil {
			return i, nil
		}
		if !isBadPassword(err) {
			return -1, err
		}
	}
	return -1, errNoPassword
}

// isBadPassword separates a guess that was wrong from a run that went wrong.
// qpdf exits 2 and says "invalid password" for the first; a missing file, an
// unwritable destination or no qpdf at all must not be mistaken for it, or a
// broken machine would report every document as password-protected.
func isBadPassword(err error) bool {
	return exitCode(err) == 2 && strings.Contains(err.Error(), "invalid password")
}

// exitCode digs the process exit status out of what runCmd returns, which wraps
// it. qpdf answers questions with exit codes rather than output, so this is how
// its answers are read. -1 means the command never ran or was killed.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// ToPDF normalizes any supported input into a PDF at dst.
func ToPDF(ctx context.Context, src, ext, dst string) error {
	kind, ok := kindOf(ext)
	if !ok {
		return fmt.Errorf("unsupported extension %q", ext)
	}
	switch kind {
	case kindImage:
		_, err := runCmd(ctx, cmdTimeout, "magick", src, "-auto-orient", "-strip", "-quality", "92", dst)
		return err
	case kindText:
		return textToPDF(src, dst)
	}
	return copyFile(src, dst)
}

// textToPDF renders plain text so text files get the same viewer, thumbnail
// and archival treatment as everything else. Core PDF fonts are cp1252, so
// text is transliterated; unmappable characters degrade rather than fail.
func textToPDF(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	pdf := fpdf.New("P", "mm", "A4", "")
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	pdf.SetMargins(15, 15, 15)
	pdf.SetAutoPageBreak(true, 15)
	pdf.AddPage()
	pdf.SetFont("Courier", "", 10)

	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\t", "    ")
	for _, line := range strings.Split(text, "\n") {
		pdf.MultiCell(0, 4.5, tr(line), "", "L", false)
	}
	if err := pdf.OutputFileAndClose(dst); err != nil {
		return fmt.Errorf("render text to pdf: %w", err)
	}
	return nil
}

// ocrMode is which of three documents this is, as far as ocrmypdf is
// concerned: one that must not be touched, one whose own text is the document,
// and one carrying text that is not. The third used to be indistinguishable
// from the second, which is how eight scanned pages stamped "Page N of 8" were
// filed as their page numbers and nothing else.
type ocrMode int

const (
	ocrSkipSigned ocrMode = iota // signed: copy it, run nothing
	ocrSkipText                  // its text is the document, or it has none
	ocrRedoText                  // it has text, but too little to be the document
)

// ocrModeFor picks the treatment from what is known before OCR runs.
//
// The middle case covers two opposite documents on purpose. A digital PDF is
// skipped page by page and costs almost nothing, and a scan with no text at all
// has nothing to skip so tesseract reads every page of it — and that second one
// is where --deskew earns its place, because a genuinely crooked scan is
// precisely a scan with no text layer.
//
// The last case is the hybrid: any text on a page is enough to make --skip-text
// skip it, so "has some text and fails the density bar" is the real boundary of
// the trap. Judging that boundary wrongly is survivable, and ocrFlags says why.
func ocrModeFor(signed bool, text string, pages int) ocrMode {
	switch {
	case signed:
		return ocrSkipSigned
	case strings.TrimSpace(text) == "" || HasTextLayer(text, pages):
		return ocrSkipText
	default:
		return ocrRedoText
	}
}

// ocrFlags is what distinguishes the two passes that actually run tesseract.
//
// --redo-ocr keeps text that looks real and OCRs the image regions around it,
// which is exactly the hybrid document's shape. It is also the reason a
// misjudgement here is survivable: a fifty-page text PDF whose forty-five blank
// appendix pages drag its average under the density bar would land in this
// branch, and --redo-ocr costs it some CPU and changes nothing. --force-ocr
// would rasterize the same document and degrade it, which is why it is not the
// choice here despite reading the same amount of text.
//
// --deskew is dropped for that pass because ocrmypdf refuses the combination.
// Nothing is lost: deskew matters for a crooked scan, and a crooked scan has no
// text layer, so it goes down the --skip-text branch and keeps it.
func ocrFlags(mode ocrMode) []string {
	if mode == ocrRedoText {
		return []string{"--redo-ocr"}
	}
	return []string{"--skip-text", "--deskew"}
}

// ocrPass runs one pass. Only the mode flags vary between passes; the
// archival format the result has to be in does not.
func ocrPass(ctx context.Context, src, dst string, mode ocrMode) error {
	args := append(ocrFlags(mode),
		"--output-type", "pdfa", "--optimize", "1",
		"--language", "eng", "--rotate-pages", "--jobs", "2",
		"--quiet", src, dst)
	_, err := runCmd(ctx, ocrTimeout, "ocrmypdf", args...)
	return err
}

// OCR produces the archival PDF. It returns which engine supplied the text so
// the UI can explain the result.
func OCR(ctx context.Context, src, dst string, mode ocrMode) (string, error) {
	if mode == ocrSkipSigned {
		// Preserve the signature; these are digitally generated and already
		// carry a text layer.
		return OCRSkippedSigned, copyFile(src, dst)
	}

	tmp := dst + ".tmp.pdf"
	defer os.Remove(tmp)

	err := ocrPass(ctx, src, tmp, mode)
	if err != nil && mode == ocrRedoText {
		// Down a rung rather than out to Ghostscript: --skip-text reads at
		// least the pages that are wholly image, and a thinner text layer is a
		// better outcome than the chain below, which produces none at all.
		logf("ocrmypdf --redo-ocr failed, retrying with --skip-text: %v", err)
		err = ocrPass(ctx, src, tmp, ocrSkipText)
	}
	if err == nil {
		return OCRTesseract, os.Rename(tmp, dst)
	}
	logf("ocrmypdf failed, falling back to plain normalize: %v", err)

	// One thing must never reach the chain below, and this is where it would
	// arrive: an encrypted PDF. ocrmypdf refuses those outright, and Ghostscript
	// then "succeeds" on a file it cannot read by writing a blank single page —
	// which became the archive, made pdftotext find nothing and PageCount report
	// one page for a forty-eight page tax return, and left the document marked
	// ready and absent from the Failed view. The pipeline decrypts before it
	// gets here, so this is the second lock on that door rather than the first;
	// failing is the right outcome either way, because the original is untouched
	// and a password makes the document readable.
	if PDFNeedsPassword(ctx, src) {
		return "", errors.New("password-protected, so it can be neither OCR'd nor normalized")
	}

	// Fallback chain. A document is never lost to a conversion failure; the
	// worst case is an archive equal to the original with no text layer.
	if _, gsErr := runCmd(ctx, cmdTimeout, "gs", "-q", "-dBATCH", "-dNOPAUSE", "-dSAFER",
		"-sDEVICE=pdfwrite", "-dPDFA=2", "-dPDFACompatibilityPolicy=1",
		"-sColorConversionStrategy=RGB", "-dEmbedAllFonts=true",
		"-o", tmp, src); gsErr == nil {
		return OCRNone, os.Rename(tmp, dst)
	}
	if _, qErr := runCmd(ctx, cmdTimeout, "qpdf", "--linearize", src, tmp); qErr == nil {
		return OCRNone, os.Rename(tmp, dst)
	}
	return OCRNone, copyFile(src, dst)
}

var pagesRe = regexp.MustCompile(`(?m)^Pages:\s+(\d+)`)

func PageCount(ctx context.Context, pdf string) int {
	out, err := runCmd(ctx, cmdTimeout, "pdfinfo", pdf)
	if err != nil {
		return 0
	}
	if m := pagesRe.FindStringSubmatch(out); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// PDFInfo returns the raw pdfinfo key/value pairs, used by the heuristics for
// the embedded Title and CreationDate.
func PDFInfo(ctx context.Context, pdf string) map[string]string {
	out, err := runCmd(ctx, cmdTimeout, "pdfinfo", pdf)
	if err != nil {
		return nil
	}
	info := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			info[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return info
}

func Thumbnail(ctx context.Context, pdf, dst string) error {
	// pdftoppm appends the extension itself when -singlefile is used.
	base := strings.TrimSuffix(dst, filepath.Ext(dst))
	_, err := runCmd(ctx, cmdTimeout, "pdftoppm", "-jpeg", "-jpegopt", "quality=80",
		"-f", "1", "-l", "1", "-scale-to", "480", "-singlefile", pdf, base)
	return err
}

func ExtractText(ctx context.Context, pdf string) (string, error) {
	return runCmd(ctx, cmdTimeout, "pdftotext", "-q", "-enc", "UTF-8", "-eol", "unix", pdf, "-")
}

// RasterizePage renders one page for the vision rescue. Grayscale at 150dpi
// keeps the image-token count down without hurting legibility.
func RasterizePage(ctx context.Context, pdf string, page int, dstBase string) (string, error) {
	p := strconv.Itoa(page)
	_, err := runCmd(ctx, cmdTimeout, "pdftoppm", "-png", "-gray", "-r", "150",
		"-f", p, "-l", p, "-singlefile", pdf, dstBase)
	if err != nil {
		return "", err
	}
	return dstBase + ".png", nil
}

// garbageRatio is the percentage of runes that came out as replacement
// characters, which is what a failed embedded-font extraction looks like.
func garbageRatio(text string) int {
	runes := utf8.RuneCountInString(text)
	if runes == 0 {
		return 100
	}
	var bad int
	for _, r := range text {
		if r == utf8.RuneError || r == '�' {
			bad++
		}
	}
	return bad * 100 / runes
}

// HasTextLayer reports whether a PDF's own text is the document.
//
// It used to ask only whether any text was there at all, on the reasoning that
// a native text layer is exact by construction. It is — but page furniture is
// native text too, and eight scanned pages carrying nothing but "Page N of 8"
// answered yes to that question, were declared already-read, and had their
// contents thrown away twice over: ocrmypdf skipped every page because every
// page had something on it, and the vision rescue was withheld because the
// document was believed to have its own text.
//
// So the bar is density, and it is the same density the rescue asks about —
// one threshold with one meaning, rather than "has text" and "has enough text
// to be the document" drifting apart as two separate standards. Twenty
// characters a page is far below any real page and far above any stamp.
//
// Without a page count the question cannot be asked at all, and it answers no
// rather than guessing.
func HasTextLayer(text string, pages int) bool {
	if pages <= 0 {
		return false
	}
	trimmed := strings.TrimSpace(text)
	runes := utf8.RuneCountInString(trimmed)
	return runes >= 25 && runes/pages >= 20 && garbageRatio(trimmed) <= 5
}

// NeedsVisionRescue decides whether to re-read a document with the model. It is
// HasTextLayer's question over again on what came back: if OCR did not produce
// a document either, the model is the last reader left.
//
// It applies only to documents that had no native text layer, i.e. ones OCR
// actually ran on. Length is not used as a cost proxy: a short document is a
// small document, and rasterizing one page costs on the order of $0.00015, so
// withholding a rescue to save money makes no sense. What the rescue must
// never do is overwrite an exact native text layer with an approximation of a
// picture of it — which is why that case is excluded by the caller instead.
func NeedsVisionRescue(text string, pages int) bool {
	if pages <= 0 {
		return false
	}
	return !HasTextLayer(text, pages)
}

// copyFile streams rather than buffering: an ingested scan can be a hundred
// megabytes, and every worker was holding one entirely in memory.
func copyFile(src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeFileAtomicFrom(dst, f)
}
