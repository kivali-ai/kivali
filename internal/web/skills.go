package web

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/kivali-ai/kivali/internal/store"
)

// maxSkillUpload caps the size of an uploaded .zip / .skill / .md
// body. Matches the per-file cap on run_shell inputs so we never
// accept something we can't stage into an agent pod downstream.
const maxSkillUpload = 10 << 20

// deleteSkill removes a skill that did not ship with Kivali.
func (s *Server) deleteSkill(name string) error {
	if name == "" {
		return refuse(http.StatusBadRequest, "name required")
	}
	if s.Store.IsBuiltinSkill(name) {
		return refuse(http.StatusConflict, fmt.Sprintf("%q ships with Kivali and cannot be deleted — disable it instead", name))
	}
	if _, err := s.Store.ReadSkill(name); errors.Is(err, store.ErrNotFound) {
		return refuse(http.StatusNotFound, fmt.Sprintf("skill %q doesn't exist", name))
	}
	if err := s.Store.DeleteSkill(name); err != nil {
		return err
	}
	s.syncSkillsAfterChange()
	return nil
}

// setSkillEnabled switches a skill on or off and resyncs, so the
// change reaches agents now rather than at the next bootstrap: a skill
// switched off has to stop being readable, and one switched on should
// be usable immediately.
func (s *Server) setSkillEnabled(name string, enabled bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return refuse(http.StatusBadRequest, "name required")
	}
	if _, err := s.Store.ReadSkill(name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return refuse(http.StatusNotFound, fmt.Sprintf("skill %q doesn't exist", name))
		}
		return err
	}
	if err := s.Store.SetSkillEnabled(name, enabled); err != nil {
		return err
	}
	s.syncSkillsAfterChange()
	return nil
}

// syncSkillsAfterChange fans the current skill set out to every
// agent's /files/skills/ mirror. Best-effort: a transient sync
// failure doesn't block the HTTP response because the next filesystem
// bootstrap (on release start) will reconcile.
func (s *Server) syncSkillsAfterChange() {
	_ = s.Store.SyncAllFilesystems()
}

// skillUpload is one parsed skill upload: its manifest plus the bytes,
// either a bundle (zipData) or a lone SKILL.md (body).
type skillUpload struct {
	manifest store.SkillManifest
	zipData  []byte
	body     string
}

// skillDowngradeRefusal is an upload whose version is not newer than
// the installed one: equal counts, so "edit and re-upload the same
// version" cannot silently overwrite the bytes. The caller asks the
// person to confirm, then installs again with force.
type skillDowngradeRefusal struct {
	name, oldVersion, newVersion string
}

func (e *skillDowngradeRefusal) Error() string {
	return fmt.Sprintf("uploaded version %s is not newer than installed %s — confirm to replace", e.newVersion, e.oldVersion)
}

// installSkill adds an uploaded skill, or replaces the installed one of
// the same name, and resyncs every agent. force lands a version that
// is not newer than the installed one; without it that is a
// *skillDowngradeRefusal. A skill that ships with Kivali is never
// replaced: the next deploy re-materialises built-ins from the binary,
// so an uploaded replacement would work until it silently did not.
func (s *Server) installSkill(up skillUpload, force bool) error {
	name := up.manifest.Name
	existing, err := s.Store.ReadSkill(name)
	exists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if exists && s.Store.IsBuiltinSkill(name) {
		return refuse(http.StatusConflict, fmt.Sprintf("%q ships with Kivali and cannot be replaced by upload — it is updated with the binary. Disable it and upload your own under a different name if you want to change it.", name))
	}
	if exists && !force && store.CompareSkillVersion(up.manifest.Version, existing.Version) <= 0 {
		return &skillDowngradeRefusal{name: name, oldVersion: existing.Version, newVersion: up.manifest.Version}
	}
	if err := s.writeSkill(name, up.zipData, up.body); err != nil {
		return refuse(http.StatusBadRequest, err.Error())
	}
	s.syncSkillsAfterChange()
	return nil
}

// parseSkillUpload reads one uploaded file into memory, peeks the
// SKILL.md frontmatter, and returns the full parsed manifest plus the
// raw bytes. fh nil is "nothing uploaded". Every failure is a refusal
// saying what is wrong with the file.
func parseSkillUpload(fh *multipart.FileHeader) (skillUpload, error) {
	if fh == nil {
		return skillUpload{}, refuse(http.StatusBadRequest, "upload a .zip/.skill bundle or a SKILL.md file")
	}
	tooBig := refuse(http.StatusRequestEntityTooLarge, fmt.Sprintf("upload exceeds %d-byte cap", maxSkillUpload))
	if fh.Size > maxSkillUpload {
		return skillUpload{}, tooBig
	}
	kind, err := classifySkillUpload(fh.Filename)
	if err != nil {
		return skillUpload{}, refuse(http.StatusBadRequest, err.Error())
	}
	f, err := fh.Open()
	if err != nil {
		return skillUpload{}, refuse(http.StatusBadRequest, err.Error())
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSkillUpload+1))
	if err != nil {
		return skillUpload{}, refuse(http.StatusBadRequest, err.Error())
	}
	if int64(len(b)) > maxSkillUpload {
		return skillUpload{}, tooBig
	}
	var up skillUpload
	switch kind {
	case uploadKindZip:
		up.manifest, err = store.ExtractZipSkillManifest(b)
		if err != nil {
			return skillUpload{}, refuse(http.StatusBadRequest, err.Error())
		}
		up.zipData = b
	case uploadKindManifest:
		up.body = string(b)
		up.manifest, err = store.ExtractSkillManifest(up.body)
		if err != nil {
			return skillUpload{}, refuse(http.StatusBadRequest, err.Error())
		}
	default:
		return skillUpload{}, errors.New("internal error: unknown upload kind")
	}
	if err := store.ParseSkillVersion(up.manifest.Version); err != nil {
		return skillUpload{}, refuse(http.StatusBadRequest, err.Error())
	}
	return up, nil
}

// writeSkill commits one of the two upload forms to disk via the
// matching store entry point.
func (s *Server) writeSkill(name string, zipData []byte, body string) error {
	if len(zipData) > 0 {
		return s.Store.WriteSkillFromZip(name, zipData)
	}
	return s.Store.WriteSkillFromManifest(name, body)
}

// uploadKind distinguishes what an uploaded file should become.
type uploadKind int

const (
	uploadKindZip uploadKind = iota
	uploadKindManifest
)

// classifySkillUpload decides whether an uploaded filename is a
// skill bundle (zip/.skill) or a standalone SKILL.md. Anything else
// is rejected so the agent never sees a half-formed skill.
func classifySkillUpload(filename string) (uploadKind, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".zip", ".skill":
		return uploadKindZip, nil
	case ".md", ".markdown":
		return uploadKindManifest, nil
	}
	if ext == "" {
		// No extension — treat as markdown. Lets a tool curl a raw
		// SKILL.md up without renaming it.
		return uploadKindManifest, nil
	}
	return 0, fmt.Errorf("unsupported upload type %q (use .zip, .skill, or .md)", ext)
}
