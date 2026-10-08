// Package convert handles turning uploaded project files into a
// Claude-consumable canonical form.
//
// The contract is simple:
//   - Text formats (markdown, plain text) are their own canonical — the
//     caller inlines them into the cached system prompt.
//   - PDFs are their own canonical and are extracted to text via
//     pdftotext on demand.
//   - Office formats (docx, pptx, xlsx, odt, odp, ods) are extracted to
//     text natively in pure Go — no external binary required. Legacy
//     binary formats (.doc, .ppt, .xls, .rtf) soft-fail: the upload
//     still succeeds; the file is stored but has no canonical form,
//     and agents see only its name.
package convert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// PdfToTextBin is the executable used for PDF → text extraction.
var PdfToTextBin = "pdftotext"

// Kind classifies an uploaded file for the conversion pipeline. The
// design goal is "everything becomes text agents can read inline."
type Kind int

const (
	// Text: markdown / plain text / csv / json / code. Use as-is.
	KindText Kind = iota
	// PDF: extract text via pdftotext.
	KindPDF
	// Office: libreoffice → PDF → pdftotext.
	KindOffice
	// Image: no text content; skip. (Rendered-image support deferred.)
	KindImage
	// Unknown: no canonical form.
	KindUnknown
)

// Classify returns how a file should be handled based on its extension.
// Any file type that's reasonably text gets KindText so agents see the
// content inline; binary containers of text (PDF, office) get converted.
func Classify(ext string) Kind {
	switch strings.ToLower(ext) {
	case ".md", ".markdown", ".txt",
		".csv", ".tsv",
		".json", ".yaml", ".yml", ".toml", ".xml", ".html", ".htm",
		".js", ".ts", ".go", ".py", ".rb", ".rs", ".java", ".c", ".cpp", ".h",
		".sh", ".bash", ".sql",
		".log":
		return KindText
	case ".pdf":
		return KindPDF
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return KindImage
	case ".docx", ".message", ".pptx", ".ppt", ".xlsx", ".xls", ".odt", ".odp", ".ods", ".rtf":
		return KindOffice
	}
	return KindUnknown
}

// MIMEFor returns the MIME type of a file's ORIGINAL form (not the
// canonical). Canonical form is always text/plain for non-text sources
// after conversion.
func MIMEFor(ext string) string {
	switch strings.ToLower(ext) {
	case ".md", ".markdown":
		return "text/markdown"
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".docx", ".message":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".odt":
		return "application/vnd.oasis.opendocument.text"
	case ".odp":
		return "application/vnd.oasis.opendocument.presentation"
	case ".ods":
		return "application/vnd.oasis.opendocument.spreadsheet"
	case ".doc":
		return "application/msword"
	case ".ppt":
		return "application/vnd.ms-powerpoint"
	case ".xls":
		return "application/vnd.ms-excel"
	case ".rtf":
		return "application/rtf"
	}
	if Classify(ext) == KindText {
		return "text/plain"
	}
	return "application/octet-stream"
}

// PdfToTextAvailable reports whether pdftotext is installed in PATH.
func PdfToTextAvailable() bool {
	_, err := exec.LookPath(PdfToTextBin)
	return err == nil
}

// ErrNotAvailable is returned when a required converter binary is
// missing or a format isn't supported by the native pipeline.
var ErrNotAvailable = errors.New("convert: not available")

// PDFToTextFile extracts text from pdfPath to outPath via pdftotext.
// Writes to disk directly rather than buffering in Go memory so large
// PDFs don't OOM the process. pdftotext's stderr (which can be
// enormous on malformed-but-partially-readable PDFs) is capped.
func PDFToTextFile(ctx context.Context, pdfPath, outPath string) error {
	if !PdfToTextAvailable() {
		return fmt.Errorf("%w: pdftotext", ErrNotAvailable)
	}
	cmd := exec.CommandContext(ctx, PdfToTextBin, "-layout", "-enc", "UTF-8", pdfPath, outPath)
	stderr := newCappedWriter(8 << 10)
	cmd.Stderr = stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pdftotext %s: %w: %s", pdfPath, err, stderr.String())
	}
	return nil
}

// cappedWriter is an io.Writer that keeps at most max bytes, discarding
// the rest. Useful for capturing process stderr without unbounded memory.
type cappedWriter struct {
	max int
	buf []byte
}

func newCappedWriter(max int) *cappedWriter { return &cappedWriter{max: max} }

func (c *cappedWriter) Write(p []byte) (int, error) {
	if len(c.buf) < c.max {
		room := c.max - len(c.buf)
		if room > len(p) {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
	}
	return len(p), nil
}

func (c *cappedWriter) String() string { return string(c.buf) }
