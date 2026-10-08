package store

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteSkillFromManifestAndRead(t *testing.T) {
	s := mustStore(t)
	body := "---\ndescription: unit test skill\nwhen_to_use: never\n---\n\n# body\n"
	if err := s.WriteSkillFromManifest("unit-test", body); err != nil {
		t.Fatalf("WriteSkillFromManifest: %v", err)
	}
	got, err := s.ReadSkill("unit-test")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if got.Name != "unit-test" {
		t.Errorf("Name=%q", got.Name)
	}
	if got.Description != "unit test skill" {
		t.Errorf("Description=%q", got.Description)
	}
	if got.WhenToUse != "never" {
		t.Errorf("WhenToUse=%q", got.WhenToUse)
	}
	if got.FileCount != 1 {
		t.Errorf("FileCount=%d (expected 1 for a manifest-only skill)", got.FileCount)
	}
	if len(got.Files) != 1 || got.Files[0].Path != "SKILL.md" {
		t.Errorf("Files=%+v", got.Files)
	}

	// Manifest body round-trip
	mb, err := s.ReadSkillManifest("unit-test")
	if err != nil {
		t.Fatalf("ReadSkillManifest: %v", err)
	}
	if mb != body {
		t.Errorf("manifest roundtrip mismatch")
	}
}

func TestWriteSkillFromZipFlatLayout(t *testing.T) {
	s := mustStore(t)
	z := buildZip(t, map[string]zipEntry{
		"SKILL.md":        {Body: "---\ndescription: flat\n---\n# hi\n"},
		"scripts/run.sh":  {Body: "#!/bin/bash\necho hi\n", Mode: 0o755},
		"assets/data.txt": {Body: "payload\n"},
	})
	if err := s.WriteSkillFromZip("flat", z); err != nil {
		t.Fatalf("WriteSkillFromZip: %v", err)
	}
	sk, err := s.ReadSkill("flat")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if sk.FileCount != 3 {
		t.Errorf("FileCount=%d (expected 3)", sk.FileCount)
	}
	gotExec := false
	for _, f := range sk.Files {
		if f.Path == "scripts/run.sh" && f.Executable {
			gotExec = true
		}
	}
	if !gotExec {
		t.Errorf("exec bit not preserved on scripts/run.sh: files=%+v", sk.Files)
	}
}

func TestWriteSkillFromZipSingleTopLevelFolder(t *testing.T) {
	// `zip -r my-skill my-skill/` produces entries prefixed with
	// "my-skill/" — we strip that prefix so the skill's on-disk
	// layout is the same whether the archive was flat or folder-wrapped.
	s := mustStore(t)
	z := buildZip(t, map[string]zipEntry{
		"my-skill/SKILL.md":       {Body: "---\ndescription: nested\n---\n"},
		"my-skill/scripts/run.sh": {Body: "#!/bin/bash\n", Mode: 0o755},
	})
	if err := s.WriteSkillFromZip("my-skill", z); err != nil {
		t.Fatalf("WriteSkillFromZip: %v", err)
	}
	sk, err := s.ReadSkill("my-skill")
	if err != nil {
		t.Fatalf("ReadSkill: %v", err)
	}
	if sk.Description != "nested" {
		t.Errorf("frontmatter not parsed: %+v", sk)
	}
	// Paths should be skill-relative (no "my-skill/" prefix)
	paths := make([]string, 0, len(sk.Files))
	for _, f := range sk.Files {
		paths = append(paths, f.Path)
	}
	want := []string{"SKILL.md", "scripts/run.sh"}
	if !sameStrings(paths, want) {
		t.Errorf("paths=%v want=%v", paths, want)
	}
}

func TestWriteSkillFromZipMissingManifestRejected(t *testing.T) {
	s := mustStore(t)
	z := buildZip(t, map[string]zipEntry{
		"scripts/run.sh": {Body: "#!/bin/bash\n", Mode: 0o755},
	})
	err := s.WriteSkillFromZip("bad", z)
	if err == nil {
		t.Fatal("expected error for missing SKILL.md")
	}
	if !strings.Contains(err.Error(), "SKILL.md") {
		t.Errorf("error %q should mention SKILL.md", err)
	}
	// The skill directory should not exist on disk.
	if _, err := s.ReadSkill("bad"); !errorsIsStatNotExist(err) {
		t.Errorf("rejected zip left a skill on disk: %v", err)
	}
}

func TestWriteSkillFromZipPathTraversalRejected(t *testing.T) {
	// A zip with a "../" entry must not write outside the skill dir.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	manifest, _ := zw.Create("SKILL.md")
	_, _ = manifest.Write([]byte("---\ndescription: x\n---\n"))
	evil, _ := zw.Create("../evil.txt")
	_, _ = evil.Write([]byte("pwned"))
	_ = zw.Close()
	s := mustStore(t)
	if err := s.WriteSkillFromZip("trav", buf.Bytes()); err == nil {
		t.Fatal("expected traversal rejection")
	}
	// Stage/trash sibling dirs are fine, but an "evil.txt" above the
	// skills dir must not exist.
	abs := filepath.Join(s.SkillsDir(), "..", "evil.txt")
	if _, err := statFS(abs); err == nil {
		t.Errorf("path traversal wrote to %s", abs)
	}
}

func TestWriteSkillFromZipOverwritesAtomically(t *testing.T) {
	s := mustStore(t)
	// First version: 2 files.
	z1 := buildZip(t, map[string]zipEntry{
		"SKILL.md": {Body: "---\ndescription: v1\n---\n"},
		"old.txt":  {Body: "stale"},
	})
	if err := s.WriteSkillFromZip("mut", z1); err != nil {
		t.Fatal(err)
	}
	// Second version: different files. old.txt should be gone.
	z2 := buildZip(t, map[string]zipEntry{
		"SKILL.md": {Body: "---\ndescription: v2\n---\n"},
		"new.txt":  {Body: "fresh"},
	})
	if err := s.WriteSkillFromZip("mut", z2); err != nil {
		t.Fatal(err)
	}
	sk, err := s.ReadSkill("mut")
	if err != nil {
		t.Fatal(err)
	}
	if sk.Description != "v2" {
		t.Errorf("Description=%q (expected v2)", sk.Description)
	}
	for _, f := range sk.Files {
		if f.Path == "old.txt" {
			t.Errorf("old.txt should be gone after replacement")
		}
	}
}

func TestDeleteSkillRemovesDirectory(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteSkillFromManifest("gone", "---\ndescription: x\n---\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSkill("gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadSkill("gone"); !errors.Is(err, ErrNotFound) && !errorsIsStatNotExist(err) {
		t.Errorf("ReadSkill after delete: %v", err)
	}
	// Deleting twice should be a no-op.
	if err := s.DeleteSkill("gone"); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

func TestExtractZipSkillName(t *testing.T) {
	cases := []struct {
		desc      string
		entries   map[string]zipEntry
		wantName  string
		wantErrIn string // substring expected in error; "" when no error
	}{
		{
			desc: "flat layout with valid name",
			entries: map[string]zipEntry{
				"SKILL.md": {Body: "---\nname: my-skill\ndescription: x\n---\n"},
			},
			wantName: "my-skill",
		},
		{
			desc: "single-top-level folder with valid name",
			entries: map[string]zipEntry{
				"wrapper/SKILL.md":     {Body: "---\nname: inner-name\n---\n"},
				"wrapper/scripts/x.sh": {Body: "#!/bin/bash\n", Mode: 0o755},
			},
			wantName: "inner-name",
		},
		{
			desc: "missing frontmatter",
			entries: map[string]zipEntry{
				"SKILL.md": {Body: "# heading\n\nno frontmatter here\n"},
			},
			wantErrIn: "frontmatter",
		},
		{
			desc: "missing name field",
			entries: map[string]zipEntry{
				"SKILL.md": {Body: "---\ndescription: missing name\n---\n"},
			},
			wantErrIn: "name:",
		},
		{
			desc: "invalid name slug",
			entries: map[string]zipEntry{
				"SKILL.md": {Body: "---\nname: Bad Name\n---\n"},
			},
			wantErrIn: "invalid",
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			z := buildZip(t, c.entries)
			got, err := ExtractZipSkillName(z)
			if c.wantErrIn != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (name=%q)", c.wantErrIn, got)
				}
				if !strings.Contains(err.Error(), c.wantErrIn) {
					t.Errorf("error %q missing %q", err, c.wantErrIn)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.wantName {
				t.Errorf("name=%q want %q", got, c.wantName)
			}
		})
	}
}

func TestExtractManifestSkillName(t *testing.T) {
	got, err := ExtractManifestSkillName("---\nname: paste-me\ndescription: x\n---\n\nhi\n")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if got != "paste-me" {
		t.Errorf("name=%q want paste-me", got)
	}
	if _, err := ExtractManifestSkillName(""); err == nil {
		t.Error("expected error on empty body")
	}
}

func TestListSkillsIgnoresStagingDirs(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteSkillFromManifest("real", "---\ndescription: real\n---\n"); err != nil {
		t.Fatal(err)
	}
	// Simulate a leftover stage dir from a crashed upload.
	if err := osMkdirAll(filepath.Join(s.SkillsDir(), ".skill-stage-abc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// And a plain flat .md file (not a valid skill dir; should be ignored).
	_ = osWriteFile(filepath.Join(s.SkillsDir(), "flat.md"), []byte("flat"), 0o644)
	list, err := s.ListSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "real" {
		t.Errorf("list=%+v (expected only [real])", list)
	}
}

// ---- test helpers ----

type zipEntry struct {
	Body string
	Mode uint32 // zero → 0o644
}

func buildZip(t *testing.T, entries map[string]zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, e := range entries {
		mode := e.Mode
		if mode == 0 {
			mode = 0o644
		}
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(toFileMode(mode))
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(e.Body)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

func toFileMode(m uint32) os.FileMode { return os.FileMode(m) }

// errorsIsStatNotExist accepts both the raw fs.ErrNotExist (from a
// direct os.Stat) and store.ErrNotFound (wrapped by loadSkill) so
// tests that want "skill isn't there" stay single-line.
func errorsIsStatNotExist(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNotFound)
}

func statFS(p string) (os.FileInfo, error) { return os.Stat(p) }

func osMkdirAll(p string, mode os.FileMode) error { return os.MkdirAll(p, mode) }

func osWriteFile(p string, data []byte, mode os.FileMode) error {
	return os.WriteFile(p, data, mode)
}

// TestSkillVersionDefaultsToZero covers the grace floor: an
// on-disk SKILL.md without a `version:` field reads as "0.0.0", so
// any subsequent upload that does carry a version automatically
// counts as a bump (no force flag needed to roll it forward).
func TestSkillVersionDefaultsToZero(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteSkillFromManifest("unversioned", "---\nname: unversioned\ndescription: no version\n---\n"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadSkill("unversioned")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != DefaultSkillVersion {
		t.Errorf("Version=%q want %q (default floor)", got.Version, DefaultSkillVersion)
	}
}

// TestSkillVersionFromFrontmatter is the happy-path read: a SKILL.md
// that declares `version: 2.3.4` surfaces that string verbatim
// through the Skill struct.
func TestSkillVersionFromFrontmatter(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteSkillFromManifest("v1", "---\nname: v1\nversion: 2.3.4\n---\n"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadSkill("v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "2.3.4" {
		t.Errorf("Version=%q want 2.3.4", got.Version)
	}
}

// TestParseSkillVersionAccepts walks the shapes we accept as
// semver-ish from a SKILL.md upload.
func TestParseSkillVersionAccepts(t *testing.T) {
	for _, v := range []string{"0.0.0", "1.0.0", "10.20.30", "1.0.0-beta", "1.0.0-rc.1", "1.0.0+build.7", "1.0.0-beta+meta"} {
		if err := ParseSkillVersion(v); err != nil {
			t.Errorf("ParseSkillVersion(%q) err=%v want nil", v, err)
		}
	}
}

// TestParseSkillVersionRejects catches inputs that would either bypass
// the comparator (empty) or break it (non-numeric, wrong number of
// triple components, leading "v").
func TestParseSkillVersionRejects(t *testing.T) {
	for _, v := range []string{"", "1", "1.0", "1.0.0.0", "v1.0.0", "latest", "abc"} {
		if err := ParseSkillVersion(v); err == nil {
			t.Errorf("ParseSkillVersion(%q) err=nil want error", v)
		}
	}
}

// TestCompareSkillVersion covers the ordering rules the upload-replace
// path leans on: numeric components compare numerically; a prerelease
// of an otherwise-equal triple ranks below the release; unparseable
// inputs sort below everything parseable (so the DefaultSkillVersion
// floor still works even if a string sneaks through).
func TestCompareSkillVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.10.0", "1.9.0", 1},
		{"2.0.0", "10.0.0", -1},
		{"1.0.0-beta", "1.0.0", -1},
		{"1.0.0", "1.0.0-beta", 1},
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"1.0.0+build", "1.0.0", 0},
		{"0.0.0", "0.0.1", -1},
	}
	for _, c := range cases {
		if got := CompareSkillVersion(c.a, c.b); got != c.want {
			t.Errorf("CompareSkillVersion(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
