package convert

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// CanonicalResult describes the canonical (Claude-consumable) form a
// Canonicalize call produced, if any. When no canonical is available
// (images / unknown types / missing converter binary), Name is empty.
type CanonicalResult struct {
	// Name is the filename (relative to outDir) of the canonical form,
	// or "" if no canonical form was produced.
	Name string
	// MIME is the MIME type of the canonical form (e.g. "text/plain").
	MIME string
	// UsesOriginal is true when the canonical and original file are the
	// same bytes on disk — the caller can skip writing a second copy.
	UsesOriginal bool
}

// Canonicalize produces a text canonical of the file at inputPath,
// writing it under outDir when conversion is required. The input is
// classified by ext (which should include the leading dot, e.g. ".pdf").
//
// Behavior:
//   - Text kinds: the original is its own canonical — UsesOriginal=true,
//     Name is filepath.Base(inputPath), MIME via MIMEFor(ext).
//   - PDF: pdftotext → outDir/canonical.txt (MIME text/plain).
//   - Office: native zip+XML extractors → outDir/canonical.txt.
//     Legacy binary formats (.doc/.ppt/.xls/.rtf) soft-fail.
//   - Image / Unknown: empty CanonicalResult (no canonical).
//
// Errors: returns nil error and empty CanonicalResult when a converter
// binary is missing or a format isn't supported (ErrNotAvailable) —
// same soft-fail as the prior project-file upload flow. Any other
// conversion failure is returned.
func Canonicalize(ctx context.Context, inputPath, ext, outDir string) (CanonicalResult, error) {
	kind := Classify(ext)
	switch kind {
	case KindText:
		return CanonicalResult{
			Name:         filepath.Base(inputPath),
			MIME:         MIMEFor(ext),
			UsesOriginal: true,
		}, nil

	case KindPDF:
		txtPath := filepath.Join(outDir, "canonical.txt")
		if err := PDFToTextFile(ctx, inputPath, txtPath); err != nil {
			if errors.Is(err, ErrNotAvailable) {
				return CanonicalResult{}, nil
			}
			return CanonicalResult{}, fmt.Errorf("pdftotext: %w", err)
		}
		return CanonicalResult{Name: "canonical.txt", MIME: "text/plain"}, nil

	case KindOffice:
		txtPath := filepath.Join(outDir, "canonical.txt")
		if err := OfficeToText(ctx, inputPath, txtPath, ext); err != nil {
			if errors.Is(err, ErrNotAvailable) {
				return CanonicalResult{}, nil
			}
			return CanonicalResult{}, fmt.Errorf("office: %w", err)
		}
		return CanonicalResult{Name: "canonical.txt", MIME: "text/plain"}, nil

	case KindImage, KindUnknown:
		return CanonicalResult{}, nil
	}
	return CanonicalResult{}, nil
}
