package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// ProjectFile is a user-uploaded context file (biz plan, market research,
// etc.). The original bytes are preserved verbatim; a canonical text form
// is generated separately by internal/convert so file_view can serve it.
// Binary forms (PDFs, images) reach the model through the MCP resources
// capability (internal/mcp/resources.go).
type ProjectFile struct {
	SHA           string    `yaml:"sha"`
	OriginalName  string    `yaml:"original_name"`
	OriginalExt   string    `yaml:"original_ext"`
	CanonicalName string    `yaml:"canonical_name,omitempty"`
	MIME          string    `yaml:"mime,omitempty"`
	Size          int64     `yaml:"size"`
	UploadedAt    time.Time `yaml:"uploaded_at"`

	// Summary is a short one-sentence description of what this file
	// is about — generated asynchronously after upload by a Haiku
	// call over the canonical text (see web.summarizeProjectFile).
	// Surfaced by the list_project_files MCP tool so agents can tell
	// at a glance whether a file is relevant without loading it.
	//
	// Empty when: (a) the summarizer hasn't run yet (race between
	// upload and first list_project_files call), (b) the file has
	// no canonical text (binary image, PDF extraction failed), or
	// (c) the summarizer errored and we left it blank rather than
	// retrying inline. Consumers handle empty Summary by rendering
	// "(no summary yet)".
	Summary string `yaml:"summary,omitempty"`

	// Kind is the knowledge-graph kind the CEO chose at upload: one of
	// graph.Kinds, or ProjectFileKindArtifact for an upload that is
	// authored primary material with no kind. Empty reads as
	// "reference": an upload is material captured from outside unless
	// the CEO says otherwise. See docs/developers/knowledge-graph.md.
	Kind string `yaml:"kind,omitempty"`
}

type projectFilesIndex struct {
	Files []ProjectFile `yaml:"files"`
}

const projectFilesIndexFilename = "index.yaml"

// AddProjectFile writes the reader to project_files/<sha>/original.<ext>
// and adds an index entry. Re-uploading a file with identical bytes is a
// no-op that returns the existing entry.
func (s *FSStore) AddProjectFile(originalName string, r io.Reader) (ProjectFile, error) {
	pfDir := s.path("project_files")
	tmp, err := os.CreateTemp(pfDir, "upload-*")
	if err != nil {
		return ProjectFile{}, err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		_ = tmp.Close()
		return ProjectFile{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return ProjectFile{}, err
	}
	if err := tmp.Close(); err != nil {
		return ProjectFile{}, err
	}

	sha := hex.EncodeToString(h.Sum(nil))
	ext := filepath.Ext(originalName)
	dir := s.path("project_files", sha)
	// Per-SHA lock — same rationale as AddAttachment. Two uploads
	// of the same content would otherwise race on the index.yaml
	// read-modify-write done by upsertProjectFile (lost upserts).
	s.LockSHA(sha)
	defer s.UnlockSHA(sha)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ProjectFile{}, err
	}
	origPath := filepath.Join(dir, "original"+ext)
	if _, err := os.Stat(origPath); err == nil {
		if existing, err := s.GetProjectFile(sha); err == nil {
			return existing, nil
		}
	}
	if err := os.Rename(tmpPath, origPath); err != nil {
		return ProjectFile{}, err
	}
	pf := ProjectFile{
		SHA:          sha,
		OriginalName: originalName,
		OriginalExt:  ext,
		Size:         n,
		UploadedAt:   time.Now().UTC(),
	}
	if err := s.upsertProjectFile(pf); err != nil {
		return ProjectFile{}, err
	}
	return pf, nil
}

// GetProjectFile returns a file by SHA, or ErrNotFound.
func (s *FSStore) GetProjectFile(sha string) (ProjectFile, error) {
	idx, err := s.readProjectFilesIndex()
	if err != nil {
		return ProjectFile{}, err
	}
	for _, f := range idx.Files {
		if f.SHA == sha {
			return f, nil
		}
	}
	return ProjectFile{}, ErrNotFound
}

// ListProjectFiles returns all files sorted by upload time (oldest first).
func (s *FSStore) ListProjectFiles() ([]ProjectFile, error) {
	idx, err := s.readProjectFilesIndex()
	if err != nil {
		return nil, err
	}
	files := append([]ProjectFile(nil), idx.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].UploadedAt.Before(files[j].UploadedAt) })
	return files, nil
}

// RemoveProjectFile deletes the file and its index entry.
func (s *FSStore) RemoveProjectFile(sha string) error {
	dir := s.path("project_files", sha)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	idx, err := s.readProjectFilesIndex()
	if err != nil {
		return err
	}
	kept := idx.Files[:0]
	for _, f := range idx.Files {
		if f.SHA != sha {
			kept = append(kept, f)
		}
	}
	idx.Files = kept
	return s.writeProjectFilesIndex(idx)
}

// SetCanonical records the canonical (Claude-consumable) form of the
// file as produced by internal/convert.
func (s *FSStore) SetCanonical(sha, canonicalName, mime string) error {
	return s.mutateProjectFile(sha, func(pf *ProjectFile) {
		pf.CanonicalName = canonicalName
		pf.MIME = mime
	})
}

// SetProjectFileSummary stores a one-sentence description of what
// the file is about, intended to help agents triage the project-
// files catalog without having to load every file. Written
// asynchronously by the web layer after a Haiku summarization pass
// over the canonical text; callers treat write errors as non-fatal
// (the catalog still works, entries just render "no summary yet").
func (s *FSStore) SetProjectFileSummary(sha, summary string) error {
	return s.mutateProjectFile(sha, func(pf *ProjectFile) {
		pf.Summary = summary
	})
}

// OpenOriginal returns a reader for the uploaded bytes.
func (s *FSStore) OpenOriginal(sha string) (io.ReadCloser, error) {
	pf, err := s.GetProjectFile(sha)
	if err != nil {
		return nil, err
	}
	return os.Open(s.path("project_files", sha, "original"+pf.OriginalExt))
}

// OpenCanonical returns a reader for the canonical form, or ErrNotFound
// if none exists yet.
func (s *FSStore) OpenCanonical(sha string) (io.ReadCloser, error) {
	pf, err := s.GetProjectFile(sha)
	if err != nil {
		return nil, err
	}
	if pf.CanonicalName == "" {
		return nil, ErrNotFound
	}
	return os.Open(s.path("project_files", sha, pf.CanonicalName))
}

// CanonicalPath returns the path where a canonical form should be
// written. The canonical producer (internal/convert) writes to this path
// and then calls SetCanonical to register it.
func (s *FSStore) CanonicalPath(sha, canonicalName string) string {
	return s.path("project_files", sha, canonicalName)
}

func (s *FSStore) mutateProjectFile(sha string, fn func(*ProjectFile)) error {
	idx, err := s.readProjectFilesIndex()
	if err != nil {
		return err
	}
	for i := range idx.Files {
		if idx.Files[i].SHA == sha {
			fn(&idx.Files[i])
			return s.writeProjectFilesIndex(idx)
		}
	}
	return ErrNotFound
}

func (s *FSStore) readProjectFilesIndex() (projectFilesIndex, error) {
	b, err := os.ReadFile(s.path("project_files", projectFilesIndexFilename))
	if errors.Is(err, os.ErrNotExist) {
		return projectFilesIndex{}, nil
	}
	if err != nil {
		return projectFilesIndex{}, err
	}
	var idx projectFilesIndex
	if err := yaml.Unmarshal(b, &idx); err != nil {
		return projectFilesIndex{}, fmt.Errorf("project_files/index.yaml: %w", err)
	}
	return idx, nil
}

func (s *FSStore) writeProjectFilesIndex(idx projectFilesIndex) error {
	b, err := yaml.Marshal(idx)
	if err != nil {
		return err
	}
	return writeAtomic(s.path("project_files", projectFilesIndexFilename), b, 0o644)
}

func (s *FSStore) upsertProjectFile(pf ProjectFile) error {
	idx, err := s.readProjectFilesIndex()
	if err != nil {
		return err
	}
	for i := range idx.Files {
		if idx.Files[i].SHA == pf.SHA {
			idx.Files[i] = pf
			return s.writeProjectFilesIndex(idx)
		}
	}
	idx.Files = append(idx.Files, pf)
	return s.writeProjectFilesIndex(idx)
}
