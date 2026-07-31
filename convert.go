package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

// SupportedExts are the extensions we accept. Anything else is rejected at
// intake with a failed sidecar rather than silently ignored.
var SupportedExts = map[string]bool{
	".pdf": true,
	".jpg": true, ".jpeg": true, ".png": true, ".tif": true, ".tiff": true, ".webp": true,
	".txt": true, ".md": true,
}

func isImageExt(ext string) bool {
	switch ext {
	case ".jpg", ".jpeg", ".png", ".tif", ".tiff", ".webp":
		return true
	}
	return false
}

func isTextExt(ext string) bool { return ext == ".txt" || ext == ".md" }

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

// ToPDF normalizes any supported input into a PDF at dst.
func ToPDF(ctx context.Context, src, ext, dst string) error {
	switch {
	case ext == ".pdf":
		return copyFile(src, dst)
	case isImageExt(ext):
		_, err := runCmd(ctx, cmdTimeout, "magick", src, "-auto-orient", "-strip", "-quality", "92", dst)
		return err
	case isTextExt(ext):
		return textToPDF(src, dst)
	}
	return fmt.Errorf("unsupported extension %q", ext)
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

// OCR produces the archival PDF. It returns which engine supplied the text so
// the UI can explain the result.
func OCR(ctx context.Context, src, dst string, signed bool) (string, error) {
	if signed {
		// Preserve the signature; these are digitally generated and already
		// carry a text layer.
		return OCRSkippedSigned, copyFile(src, dst)
	}

	tmp := dst + ".tmp.pdf"
	defer os.Remove(tmp)

	_, err := runCmd(ctx, ocrTimeout, "ocrmypdf",
		"--skip-text", "--output-type", "pdfa", "--optimize", "1",
		"--language", "eng", "--rotate-pages", "--deskew", "--jobs", "2",
		"--quiet", src, tmp)
	if err == nil {
		return OCRTesseract, os.Rename(tmp, dst)
	}
	logf("ocrmypdf failed, falling back to plain normalize: %v", err)

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

// HasTextLayer reports whether a PDF carries its own extractable text. The bar
// is deliberately low — this asks "is there a text layer at all", not "is the
// text any good" — because a native text layer is exact by construction. A
// one-line receipt has a real text layer just as much as a long statement.
func HasTextLayer(text string) bool {
	trimmed := strings.TrimSpace(text)
	return utf8.RuneCountInString(trimmed) >= 25 && garbageRatio(trimmed) <= 5
}

// NeedsVisionRescue decides whether to re-read a document with the model.
//
// It applies only to documents that had no native text layer, i.e. ones OCR
// actually ran on. Length is not used as a cost proxy: a short document is a
// small document, and rasterizing one page costs on the order of $0.00015, so
// withholding a rescue to save money makes no sense. What the rescue must
// never do is overwrite an exact native text layer with an approximation of a
// picture of it — which is why that case is excluded by the caller instead.
func NeedsVisionRescue(text string, pages int) bool {
	trimmed := strings.TrimSpace(text)
	runes := utf8.RuneCountInString(trimmed)
	if pages <= 0 {
		return false
	}
	return runes < 25 || runes/pages < 20 || garbageRatio(trimmed) > 5
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, b)
}
