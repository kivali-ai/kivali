package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/kivali-ai/kivali/internal/builtinskills"
)

// skillSettingsFile holds per-skill switches that are NOT part of the
// skill's own content. Today that is one thing: whether the skill is
// enabled.
//
// Kept out of the skill directory on purpose. A disabled built-in skill
// is re-materialised from the binary on every boot, so anything stored
// inside its directory would be overwritten by the next deploy — the
// switch has to live somewhere the materialiser does not touch.
const skillSettingsFile = "skill_settings.json"

// skillSettings is the on-disk shape. Disabled is a list rather than a
// map of every skill to a bool so that the common case — nothing
// disabled — is an empty file, and so that uploading a skill does not
// require writing a settings entry for it.
type skillSettings struct {
	Disabled []string `json:"disabled"`
}

// DisabledSkills returns the set of skill names the CEO has switched
// off. A missing or unreadable settings file reads as "nothing
// disabled": losing the file should cost a switch position, never
// hide every skill in the org.
func (s *FSStore) DisabledSkills() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(s.path(skillSettingsFile))
	if err != nil {
		return out
	}
	var cfg skillSettings
	if err := json.Unmarshal(b, &cfg); err != nil {
		return out
	}
	for _, n := range cfg.Disabled {
		out[n] = true
	}
	return out
}

// SkillEnabled reports whether one skill is switched on.
func (s *FSStore) SkillEnabled(name string) bool {
	return !s.DisabledSkills()[name]
}

// SetSkillEnabled switches a skill on or off.
//
// Disable is the operation that exists for every skill, including
// built-ins, which is the reason built-ins need no delete: switching
// one off is what "remove it" actually means for something that would
// reappear on the next boot anyway.
//
// Holds skillSettingsMu across the read-modify-write. Two toggles
// landing together without it would silently lose one.
func (s *FSStore) SetSkillEnabled(name string, enabled bool) error {
	if !ValidSkillName(name) {
		return fmt.Errorf("skill name %q is invalid", name)
	}
	s.skillSettingsMu.Lock()
	defer s.skillSettingsMu.Unlock()

	var cfg skillSettings
	b, err := os.ReadFile(s.path(skillSettingsFile))
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &cfg); err != nil {
			// A corrupt settings file should not wedge the toggle. Start
			// from "nothing disabled" and let this write repair it.
			cfg = skillSettings{}
		}
	case errors.Is(err, fs.ErrNotExist):
		// First toggle in this deployment.
	default:
		return err
	}

	set := map[string]bool{}
	for _, n := range cfg.Disabled {
		set[n] = true
	}
	if enabled {
		delete(set, name)
	} else {
		set[name] = true
	}
	cfg.Disabled = make([]string, 0, len(set))
	for n := range set {
		cfg.Disabled = append(cfg.Disabled, n)
	}
	sort.Strings(cfg.Disabled)

	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path(skillSettingsFile), append(body, '\n'), 0o644)
}

// InstallBuiltinSkills materialises every skill compiled into the
// binary onto disk under data/skills/.
//
// Writing them to the same place uploaded skills live is the point:
// from there on, one mechanism serves both. The /files/skills/ mirror,
// the agent-pod staging path, file_view, and list_skills need no
// notion of "built-in" at all.
//
// Idempotent, and quiet about it — a file whose bytes already match is
// left alone rather than rewritten, so an unchanged skill does not get
// a fresh mtime (and a fresh "updated just now") on every restart.
//
// Called at boot, before anything reads the skill library. Failures
// are returned rather than logged: a deployment that cannot write its
// own built-in skills has a broken data volume, and finding that out at
// boot is better than finding it out when an agent looks for a skill
// that should be there.
func (s *FSStore) InstallBuiltinSkills() error {
	for _, name := range builtinskills.Names() {
		if !ValidSkillName(name) {
			return fmt.Errorf("builtin skill %q is not a valid skill name", name)
		}
		dir := filepath.Join(s.skillsDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("builtin skill %s: %w", name, err)
		}
		for _, f := range builtinskills.Files(name) {
			dest := filepath.Join(dir, filepath.FromSlash(f.Path))
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("builtin skill %s: %w", name, err)
			}
			if existing, err := os.ReadFile(dest); err == nil && string(existing) == string(f.Data) {
				continue
			}
			if err := writeAtomic(dest, f.Data, 0o644); err != nil {
				return fmt.Errorf("builtin skill %s: write %s: %w", name, f.Path, err)
			}
		}
	}
	return nil
}

// IsBuiltinSkill reports whether a skill ships with the binary and so
// cannot be deleted or replaced by upload.
func (s *FSStore) IsBuiltinSkill(name string) bool { return builtinskills.Has(name) }
