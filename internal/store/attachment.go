package store

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/convert"
)

// Attachment is a content-addressed blob used by agent chat / inbox
// messages. It mirrors the project-file pattern but is scoped by
// reference rather than listed in a global index: anyone who holds a
// (sha, name) pair can read the bytes and the canonical text.
//
// The canonical form (when available) is always text — produced by the
// same pipeline that handles project files (text as-is; PDF via
// pdftotext; office messages via libreoffice → pdftotext).
type Attachment struct {
	SHA           string    `json:"sha"`
	Name          string    `json:"name"`                     // display name at creation time
	Ext           string    `json:"ext,omitempty"`            // includes leading dot
	MIME          string    `json:"mime,omitempty"`           // original MIME
	CanonicalName string    `json:"canonical_name,omitempty"` // filename of canonical form, if any
	CanonicalMIME string    `json:"canonical_mime,omitempty"`
	Size          int64     `json:"size"`
	CreatedAt     time.Time `json:"created_at"`
}

const attachmentMetaFilename = "meta.json"

// AddAttachment writes the reader to attachments/<sha>/blob<ext> and
// drops a meta.json alongside. Re-adding identical bytes is a no-op;
// the existing attachment (with its original name) is returned.
//
// The canonical text form is produced via the standard convert pipeline
// when the classifier knows the type. Images / unknown types have no
// canonical form and surface only as a named reference.
func (s *FSStore) AddAttachment(ctx context.Context, originalName string, r io.Reader) (Attachment, error) {
	return s.addAttachment(ctx, originalName, r, true)
}

// AddAttachmentSnapshot stores bytes content-addressed without
// producing a canonical text form. The knowledge-graph pass uses it to
// keep every version of every node readable: a snapshot needs the
// bytes and nothing else, and canonicalising a PDF or office payload
// would run pdftotext or libreoffice under the pass's lock, stalling
// every agent's wake behind one slow file.
func (s *FSStore) AddAttachmentSnapshot(originalName string, r io.Reader) (Attachment, error) {
	return s.addAttachment(context.Background(), originalName, r, false)
}

func (s *FSStore) addAttachment(ctx context.Context, originalName string, r io.Reader, canonicalize bool) (Attachment, error) {
	root := s.path("attachments")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Attachment{}, err
	}
	tmp, err := os.CreateTemp(root, "upload-*")
	if err != nil {
		return Attachment{}, err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		_ = tmp.Close()
		return Attachment{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Attachment{}, err
	}
	if err := tmp.Close(); err != nil {
		return Attachment{}, err
	}

	sha := hex.EncodeToString(h.Sum(nil))
	ext := strings.ToLower(filepath.Ext(originalName))
	dir := s.path("attachments", sha)
	// Per-SHA lock for everything below: blob rename, canonical
	// extraction, meta.json write. Two concurrent AddAttachment for
	// the same content would otherwise both canonicalize (wasted CPU,
	// possibly a corrupted canonical.txt mid-write) and both write
	// meta.json with their own caller-supplied Name, last-writer-wins.
	// The lock makes the second arrival a no-op
	// via the GetAttachment check below.
	s.LockSHA(sha)
	defer s.UnlockSHA(sha)
	if existing, err := s.GetAttachment(sha); err == nil {
		return existing, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Attachment{}, err
	}
	blobName := "blob" + ext
	blobPath := filepath.Join(dir, blobName)
	if err := os.Rename(tmpPath, blobPath); err != nil {
		return Attachment{}, err
	}

	att := Attachment{
		SHA:       sha,
		Name:      originalName,
		Ext:       ext,
		MIME:      convert.MIMEFor(ext),
		Size:      n,
		CreatedAt: time.Now().UTC(),
	}
	if !canonicalize {
		if err := writeAttachmentMeta(dir, att); err != nil {
			return Attachment{}, err
		}
		return att, nil
	}

	res, cerr := convert.Canonicalize(ctx, blobPath, ext, dir)
	if cerr != nil {
		// Canonicalization failed for a non-binary-missing reason. Keep
		// the blob so the original is still downloadable and referenceable,
		// but surface the error to the caller.
		if werr := writeAttachmentMeta(dir, att); werr != nil {
			return att, cerr
		}
		return att, cerr
	}
	if res.Name != "" {
		if res.UsesOriginal {
			att.CanonicalName = blobName
		} else {
			att.CanonicalName = res.Name
		}
		att.CanonicalMIME = res.MIME
	} else if ext == ".zip" {
		// Zip: no text content per se, but expose the file listing as
		// the canonical so recipients see what's inside without having
		// to download the blob.
		if manifest, err := zipManifest(blobPath); err == nil && manifest != "" {
			canonicalPath := filepath.Join(dir, "canonical.txt")
			if err := os.WriteFile(canonicalPath, []byte(manifest), 0o644); err == nil {
				att.CanonicalName = "canonical.txt"
				att.CanonicalMIME = "text/plain"
			}
		}
	}
	if err := writeAttachmentMeta(dir, att); err != nil {
		return Attachment{}, err
	}
	return att, nil
}

// zipManifest opens a zip file and renders its file listing as a short
// text manifest. Returns "" (and nil error) if the file isn't a valid
// zip — callers treat that as "no canonical produced" rather than an
// error.
func zipManifest(zipPath string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", nil
	}
	defer func() { _ = zr.Close() }()
	var b strings.Builder
	fmt.Fprintf(&b, "Zip bundle — %d entries\n", len(zr.File))
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			fmt.Fprintf(&b, "  %s/\n", f.Name)
			continue
		}
		fmt.Fprintf(&b, "  %s (%d bytes)\n", f.Name, f.UncompressedSize64)
	}
	return b.String(), nil
}

// GetAttachment returns the attachment metadata, or ErrNotFound.
func (s *FSStore) GetAttachment(sha string) (Attachment, error) {
	b, err := os.ReadFile(s.path("attachments", sha, attachmentMetaFilename))
	if errors.Is(err, fs.ErrNotExist) {
		return Attachment{}, ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	var att Attachment
	if err := json.Unmarshal(b, &att); err != nil {
		return Attachment{}, err
	}
	return att, nil
}

// OpenAttachmentOriginal returns a reader for the uploaded bytes.
func (s *FSStore) OpenAttachmentOriginal(sha string) (io.ReadCloser, error) {
	att, err := s.GetAttachment(sha)
	if err != nil {
		return nil, err
	}
	return os.Open(s.path("attachments", sha, "blob"+att.Ext))
}

// ReadAttachmentText returns the canonical text form of the attachment
// (capped at MaxInlinedFileBytes), or "" if no canonical form exists.
func (s *FSStore) ReadAttachmentText(sha string) (string, error) {
	att, err := s.GetAttachment(sha)
	if err != nil {
		return "", err
	}
	if att.CanonicalName == "" {
		return "", nil
	}
	f, err := os.Open(s.path("attachments", sha, att.CanonicalName))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return readCapped(f, MaxInlinedFileBytes)
}

// AttachmentHasTextSidecar reports whether an attachment's canonical
// text is a separate file from its original blob — i.e. whether the
// /files/attachments/<name>.txt sidecar symlink will exist after a
// SyncAgentFilesystem. True for PDFs, office docs, and zip listings;
// false for text uploads (canonical IS the blob), images, and opaque
// binaries. Mirrors attachmentLinkTargets so the render path agrees
// with the sync path on what's actually on disk.
func (s *FSStore) AttachmentHasTextSidecar(sha string) bool {
	att, err := s.GetAttachment(sha)
	if err != nil {
		return false
	}
	_, txt := attachmentLinkTargets(att)
	return txt != ""
}

// AddAttachmentFromText is a convenience for text-only attachments:
// the content is text, so the bytes are the canonical form. Falls back
// to a ".txt" extension when the provided name has none.
func (s *FSStore) AddAttachmentFromText(name, content string) (Attachment, error) {
	return s.AddAttachment(context.Background(), ensureTextExt(name), strings.NewReader(content))
}

func ensureTextExt(name string) string {
	if filepath.Ext(name) == "" {
		return name + ".txt"
	}
	return name
}

// SetAttachmentCanonicalText writes a text canonical for an attachment
// whose original bytes aren't text (e.g. a zip bundle's file listing
// serves as the canonical so recipients see the manifest in-prompt
// without downloading). Creates canonical.txt alongside the blob and
// updates meta.json.
func (s *FSStore) SetAttachmentCanonicalText(sha, text string) error {
	att, err := s.GetAttachment(sha)
	if err != nil {
		return err
	}
	dir := s.path("attachments", sha)
	if err := os.WriteFile(filepath.Join(dir, "canonical.txt"), []byte(text), 0o644); err != nil {
		return err
	}
	att.CanonicalName = "canonical.txt"
	att.CanonicalMIME = "text/plain"
	return writeAttachmentMeta(dir, att)
}

func writeAttachmentMeta(dir string, att Attachment) error {
	b, err := json.MarshalIndent(att, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, attachmentMetaFilename), b, 0o644)
}
