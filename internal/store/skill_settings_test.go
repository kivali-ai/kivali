package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinskills"
)

func newSkillTestStore(t *testing.T) *FSStore {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// TestBuiltinSkillsInstallAndListLikeAnyOther is the claim the whole
// design rests on: once materialised, a built-in skill is an ordinary
// skill on disk. Everything downstream — the /files/skills/ mirror,
// list_skills, file_view, agent-pod staging — then needs no notion of
// "built-in" at all.
func TestBuiltinSkillsInstallAndListLikeAnyOther(t *testing.T) {
	s := newSkillTestStore(t)
	if err := s.InstallBuiltinSkills(); err != nil {
		t.Fatalf("InstallBuiltinSkills: %v", err)
	}

	names := builtinskills.Names()
	if len(names) == 0 {
		t.Fatal("no builtin skills are compiled in; this package should ship at least one")
	}

	listed, err := s.ListSkills()
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	byName := map[string]Skill{}
	for _, sk := range listed {
		byName[sk.Name] = sk
	}
	for _, n := range names {
		sk, ok := byName[n]
		if !ok {
			t.Fatalf("builtin %q missing from ListSkills", n)
		}
		if !sk.Builtin {
			t.Errorf("%q: Builtin = false, want true", n)
		}
		if !sk.Enabled {
			t.Errorf("%q: Enabled = false, want built-ins on by default", n)
		}
		if sk.Description == "" || sk.WhenToUse == "" {
			t.Errorf("%q: frontmatter did not parse (description=%q when_to_use=%q)", n, sk.Description, sk.WhenToUse)
		}
		if sk.Version == DefaultSkillVersion {
			t.Errorf("%q: no version in frontmatter; built-ins should declare one like any other skill", n)
		}
	}
}

// TestInstallBuiltinSkillsIsIdempotent covers the boot path running
// on every restart. Rewriting unchanged bytes would give every skill a
// fresh mtime, so the UI would report "updated just now" after any
// restart — a change that did not happen.
func TestInstallBuiltinSkillsIsIdempotent(t *testing.T) {
	s := newSkillTestStore(t)
	if err := s.InstallBuiltinSkills(); err != nil {
		t.Fatalf("first install: %v", err)
	}
	name := builtinskills.Names()[0]
	manifest := filepath.Join(s.SkillsDir(), name, SkillManifestName)
	before, err := os.Stat(manifest)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.InstallBuiltinSkills(); err != nil {
		t.Fatalf("second install: %v", err)
	}
	after, err := os.Stat(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("mtime changed on a no-op reinstall (%v → %v); unchanged files should be left alone",
			before.ModTime(), after.ModTime())
	}
}

// TestInstallBuiltinSkillsRepairsEdits proves the other half: a
// built-in that has been altered on disk is put back. This is what
// makes "cannot be replaced by upload" honest rather than merely
// enforced at the handler.
func TestInstallBuiltinSkillsRepairsEdits(t *testing.T) {
	s := newSkillTestStore(t)
	if err := s.InstallBuiltinSkills(); err != nil {
		t.Fatalf("install: %v", err)
	}
	name := builtinskills.Names()[0]
	manifest := filepath.Join(s.SkillsDir(), name, SkillManifestName)
	if err := os.WriteFile(manifest, []byte("clobbered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.InstallBuiltinSkills(); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	b, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) == "clobbered\n" {
		t.Error("an edited builtin was not restored on the next boot")
	}
}

// TestBuiltinSkillCannotBeDeleted keeps the refusal in the store, not
// only in the handler. A delete that succeeded here would come back on
// the next boot, so the operation is not "dangerous" — it is
// incoherent, and the store is the right place to say so.
func TestBuiltinSkillCannotBeDeleted(t *testing.T) {
	s := newSkillTestStore(t)
	if err := s.InstallBuiltinSkills(); err != nil {
		t.Fatalf("install: %v", err)
	}
	name := builtinskills.Names()[0]
	if err := s.DeleteSkill(name); err == nil {
		t.Fatal("DeleteSkill on a builtin returned nil")
	}
	if _, err := s.ReadSkill(name); err != nil {
		t.Errorf("builtin disappeared despite the refusal: %v", err)
	}
}

// TestDisableHidesSkillFromAgentsButNotFromTheUI is the contract the
// CEO is promised: the switch takes the skill away from every agent
// while keeping the row they need in order to switch it back on.
func TestDisableHidesSkillFromAgentsButNotFromTheUI(t *testing.T) {
	s := newSkillTestStore(t)
	if err := s.InstallBuiltinSkills(); err != nil {
		t.Fatalf("install: %v", err)
	}
	name := builtinskills.Names()[0]

	if err := s.SetSkillEnabled(name, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	enabled, err := s.ListEnabledSkills()
	if err != nil {
		t.Fatalf("ListEnabledSkills: %v", err)
	}
	for _, sk := range enabled {
		if sk.Name == name {
			t.Fatalf("%q still appears to agents after being disabled", name)
		}
	}

	all, err := s.ListSkills()
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	var found bool
	for _, sk := range all {
		if sk.Name == name {
			found = true
			if sk.Enabled {
				t.Error("Enabled = true on a disabled skill")
			}
		}
	}
	if !found {
		t.Fatalf("%q vanished from the CEO-facing listing; it could never be re-enabled", name)
	}

	// And back on again.
	if err := s.SetSkillEnabled(name, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !s.SkillEnabled(name) {
		t.Error("re-enabling did not take effect")
	}
}

// TestDisabledStateSurvivesReinstall pins the reason the switch lives
// outside the skill directory. Built-ins are re-materialised on every
// boot, so a switch stored inside one would be reset by the next
// deploy — the CEO would disable a skill and find it back on after an
// upgrade.
func TestDisabledStateSurvivesReinstall(t *testing.T) {
	s := newSkillTestStore(t)
	if err := s.InstallBuiltinSkills(); err != nil {
		t.Fatalf("install: %v", err)
	}
	name := builtinskills.Names()[0]
	if err := s.SetSkillEnabled(name, false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if err := s.InstallBuiltinSkills(); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if s.SkillEnabled(name) {
		t.Error("a disabled builtin came back enabled after reinstall")
	}
}

// TestUploadedSkillIsNotBuiltin guards against the marker-file design
// this deliberately avoids: built-in-ness is decided by what is
// compiled in, so an uploaded bundle cannot make itself permanent.
func TestUploadedSkillIsNotBuiltin(t *testing.T) {
	s := newSkillTestStore(t)
	body := "---\nname: ceo-upload\ndescription: uploaded\nwhen_to_use: never\nversion: 1.0.0\n---\n\nbody\n"
	if err := s.WriteSkillFromManifest("ceo-upload", body); err != nil {
		t.Fatalf("WriteSkillFromManifest: %v", err)
	}
	sk, err := s.ReadSkill("ceo-upload")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if sk.Builtin {
		t.Error("an uploaded skill reported itself as built-in")
	}
	if err := s.DeleteSkill("ceo-upload"); err != nil {
		t.Errorf("uploaded skills must stay deletable: %v", err)
	}
}
