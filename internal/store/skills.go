package store

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kivali-ai/kivali/internal/builtinskills"
)

// Skill is one standard procedure / body-of-knowledge agents can
// follow. Scope is app-wide: a single shared library under
// data/skills/, symlinked into every agent's /files/skills/ at
// memory-sync time, where run_shell can execute their scripts.
// Managed exclusively via the UI; agents can read but not write.
//
// Each skill is its own directory containing SKILL.md at the root
// plus any number of supporting files (scripts/, assets/, etc.). The
// YAML frontmatter on SKILL.md supplies `description` and
// `when_to_use` which get surfaced in the system-prompt listing so
// the agent knows a skill exists without opening every file.
type Skill struct {
	Name        string
	Description string
	WhenToUse   string
	Version     string      // semver-ish from frontmatter; "0.0.0" when missing (grace floor)
	Path        string      // relative to store root, points at SKILL.md
	Dir         string      // relative to store root, points at the skill dir
	Size        int64       // total bytes across all files in the skill dir
	FileCount   int         // number of files in the skill dir
	Files       []SkillFile // sorted tree of non-empty files
	UpdatedAt   time.Time   // max mtime across files in the skill dir

	// Builtin marks a skill that ships with the binary. It can be
	// switched off but not deleted or replaced by upload: it would
	// reappear on the next boot, so a delete button would be a lie and
	// an upload would be silently reverted by the next deploy.
	Builtin bool

	// Enabled is the CEO's switch. A disabled skill stays visible in
	// the UI — it is a switch, not a deletion — but is withheld from
	// every agent-facing surface: no /files/skills/ mirror, no
	// list_skills entry, no mention in the system prompt.
	Enabled bool
}

// SkillFile is a single file inside a skill directory, relative to
// the skill root. Executable is the Unix exec bit on the stored
// file, preserved end-to-end from the uploaded zip so scripts stay
// runnable in the sandbox.
type SkillFile struct {
	Path       string // forward-slash path relative to skill dir (e.g. "scripts/foo.sh")
	Size       int64
	Executable bool
}

type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	WhenToUse   string `yaml:"when_to_use"`
	Version     string `yaml:"version"`
}

// DefaultSkillVersion is the placeholder used when an on-disk SKILL.md
// has no `version:` in its frontmatter. Old skills predate the field;
// they read as 0.0.0 so any new upload (which must declare a version)
// automatically counts as a bump. New uploads are rejected by the web
// handler if they omit `version:` — this constant only ever lands on
// already-installed skills.
const DefaultSkillVersion = "0.0.0"

// SkillManifest is the metadata block in SKILL.md. Exported so the
// web handler can surface parse errors as validation failures.
type SkillManifest = skillFrontmatter

// skillNameRE enforces a filesystem-safe skill name (dns-1123 label-ish:
// lowercase + digits + hyphen, no leading/trailing hyphen).
var skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}[a-z0-9]$|^[a-z0-9]$`)

// skillVersionRE matches a semver-2.0-ish triple. Three numeric
// components are required; an optional `-prerelease` and `+build`
// suffix is allowed but neither participates in ordering beyond the
// rule that any prerelease is less than the equivalent release.
var skillVersionRE = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

// parsedSkillVersion is the ordered representation of a skill
// version. Captured separately from the raw string so CompareSkillVersion
// can rank prereleases below their corresponding releases.
type parsedSkillVersion struct {
	major, minor, patch int
	prerelease          string // empty for non-prerelease (sorts above prereleases)
}

// ParseSkillVersion validates a `version:` value from SKILL.md
// frontmatter. Accepts MAJOR.MINOR.PATCH with optional `-prerelease`
// and `+build` suffixes. Returns a user-facing error message when the
// version doesn't fit, so the upload handler can surface it directly.
func ParseSkillVersion(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("SKILL.md frontmatter is missing `version:` — pick a semver-style string like `1.0.0`")
	}
	if _, err := parseSkillVersionInternal(v); err != nil {
		return err
	}
	return nil
}

func parseSkillVersionInternal(v string) (parsedSkillVersion, error) {
	m := skillVersionRE.FindStringSubmatch(v)
	if m == nil {
		return parsedSkillVersion{}, fmt.Errorf("SKILL.md frontmatter `version: %q` is not semver-style — use MAJOR.MINOR.PATCH (e.g. `1.0.0`, `2.3.1-beta`)", v)
	}
	mj, _ := strconv.Atoi(m[1])
	mn, _ := strconv.Atoi(m[2])
	pt, _ := strconv.Atoi(m[3])
	return parsedSkillVersion{major: mj, minor: mn, patch: pt, prerelease: m[4]}, nil
}

// CompareSkillVersion returns -1 if a < b, 0 if equal (for ordering
// purposes), or +1 if a > b. Unparseable inputs sort below everything
// parseable so an on-disk skill without a version (read as
// DefaultSkillVersion) is treated as the floor. Build metadata (the
// `+...` suffix) is ignored. Prereleases of an otherwise-equal triple
// sort below the release.
func CompareSkillVersion(a, b string) int {
	pa, errA := parseSkillVersionInternal(strings.TrimSpace(a))
	pb, errB := parseSkillVersionInternal(strings.TrimSpace(b))
	switch {
	case errA != nil && errB != nil:
		return 0
	case errA != nil:
		return -1
	case errB != nil:
		return 1
	}
	if pa.major != pb.major {
		if pa.major < pb.major {
			return -1
		}
		return 1
	}
	if pa.minor != pb.minor {
		if pa.minor < pb.minor {
			return -1
		}
		return 1
	}
	if pa.patch != pb.patch {
		if pa.patch < pb.patch {
			return -1
		}
		return 1
	}
	// Equal triple: a prerelease ranks below a non-prerelease (semver 2.0 §11).
	switch {
	case pa.prerelease == "" && pb.prerelease == "":
		return 0
	case pa.prerelease == "":
		return 1
	case pb.prerelease == "":
		return -1
	}
	if pa.prerelease == pb.prerelease {
		return 0
	}
	if pa.prerelease < pb.prerelease {
		return -1
	}
	return 1
}

// ValidSkillName reports whether name is a legal skill directory name.
func ValidSkillName(name string) bool {
	return skillNameRE.MatchString(name)
}

// SkillManifestName is the required filename at the root of every
// skill directory. Matches the Claude Code skill convention.
const SkillManifestName = "SKILL.md"

// skillsDir is the app-wide skills directory. Created lazily.
func (s *FSStore) skillsDir() string {
	return s.path("skills")
}

// SkillsDir exposes the absolute host path to the app-wide skills
// library, so callers outside the package can walk it without
// re-deriving the path.
func (s *FSStore) SkillsDir() string {
	return s.skillsDir()
}

// SkillDir returns the absolute path to a single skill's directory
// without requiring it to exist.
func (s *FSStore) SkillDir(name string) (string, error) {
	if !ValidSkillName(name) {
		return "", fmt.Errorf("skill name %q is invalid", name)
	}
	return filepath.Join(s.skillsDir(), name), nil
}

// ListSkills returns all skills sorted by name, INCLUDING disabled
// ones. This is the CEO-facing listing: a skill that has been switched
// off still has to be visible, or there would be no way to switch it
// back on. Agent-facing callers want ListEnabledSkills.
//
// A skill is any immediate subdirectory of data/skills/ that contains
// a SKILL.md. Flat *.md files are ignored.
func (s *FSStore) ListSkills() ([]Skill, error) {
	dir := s.skillsDir()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !ValidSkillName(e.Name()) {
			continue
		}
		skill, err := s.loadSkill(e.Name())
		if err != nil {
			continue // malformed skill dir — skip silently rather than poisoning the list
		}
		out = append(out, skill)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListEnabledSkills returns only the skills agents should know about.
//
// Every agent-facing path goes through this rather than filtering at
// the call site, because the failure mode of a missed filter is
// invisible: a disabled skill would keep showing up in one surface
// while the CEO believed it was off.
func (s *FSStore) ListEnabledSkills() ([]Skill, error) {
	all, err := s.ListSkills()
	if err != nil {
		return nil, err
	}
	out := make([]Skill, 0, len(all))
	for _, sk := range all {
		if sk.Enabled {
			out = append(out, sk)
		}
	}
	return out, nil
}

// ReadSkill returns a single skill's metadata + file tree, or
// ErrNotFound.
func (s *FSStore) ReadSkill(name string) (Skill, error) {
	if !ValidSkillName(name) {
		return Skill{}, fmt.Errorf("skill name %q is invalid", name)
	}
	return s.loadSkill(name)
}

// ReadSkillManifest returns the raw SKILL.md body for a skill (used
// by the edit form so the CEO can tweak the manifest in-browser).
func (s *FSStore) ReadSkillManifest(name string) (string, error) {
	if !ValidSkillName(name) {
		return "", fmt.Errorf("skill name %q is invalid", name)
	}
	p := filepath.Join(s.skillsDir(), name, SkillManifestName)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ExtractZipSkillName peeks at a skill bundle's SKILL.md frontmatter
// to return the skill's canonical name without writing anything to
// disk. The name is what the manifest declares as `name:` — the
// on-disk directory is derived from that, so there is exactly one
// source of truth. Returns an error when the zip is malformed,
// missing SKILL.md, missing a name, or declares a name that isn't a
// valid skill slug.
func ExtractZipSkillName(data []byte) (string, error) {
	m, err := ExtractZipSkillManifest(data)
	if err != nil {
		return "", err
	}
	return m.Name, nil
}

// ExtractManifestSkillName peeks at a raw SKILL.md body and returns
// the skill name from its frontmatter. Mirrors ExtractZipSkillName
// for the .md-file-upload path.
func ExtractManifestSkillName(body string) (string, error) {
	m, err := ExtractSkillManifest(body)
	if err != nil {
		return "", err
	}
	return m.Name, nil
}

// ExtractZipSkillManifest returns the full parsed frontmatter from a
// skill bundle, after the same path / name validation as
// ExtractZipSkillName. Use this when callers need fields beyond
// `name:` (e.g. the version pin for downgrade detection on upload).
func ExtractZipSkillManifest(data []byte) (SkillManifest, error) {
	if len(data) == 0 {
		return SkillManifest{}, errors.New("skill zip is empty")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return SkillManifest{}, fmt.Errorf("skill zip: %w", err)
	}
	prefix, err := detectSkillZipPrefix(zr)
	if err != nil {
		return SkillManifest{}, err
	}
	if !zipContainsManifest(zr, prefix) {
		return SkillManifest{}, fmt.Errorf("skill zip: missing %s at the top of the archive", SkillManifestName)
	}
	want := prefix + SkillManifestName
	for _, f := range zr.File {
		if path.Clean(f.Name) != want || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return SkillManifest{}, fmt.Errorf("skill zip: open %s: %w", SkillManifestName, err)
		}
		body, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		_ = rc.Close()
		if err != nil {
			return SkillManifest{}, fmt.Errorf("skill zip: read %s: %w", SkillManifestName, err)
		}
		return parseManifest(string(body))
	}
	return SkillManifest{}, fmt.Errorf("skill zip: missing %s", SkillManifestName)
}

// ExtractSkillManifest returns the full parsed frontmatter from a raw
// SKILL.md body. Mirrors ExtractZipSkillManifest for the
// .md-file-upload path.
func ExtractSkillManifest(body string) (SkillManifest, error) {
	if strings.TrimSpace(body) == "" {
		return SkillManifest{}, errors.New("skill body is empty")
	}
	return parseManifest(body)
}

// parseManifest pulls the YAML frontmatter out of a SKILL.md body,
// validating the slug-shaped `name:` field. A missing frontmatter,
// missing name field, or invalid slug all surface as a user-facing
// error so the UI can explain what to fix. Other fields (description,
// when_to_use, version) are returned as-is — version validation lives
// at the upload boundary, not here, so the read path stays tolerant
// of a skill written by hand.
func parseManifest(body string) (SkillManifest, error) {
	m := frontmatterFenceRE.FindStringSubmatch(body)
	if m == nil {
		return SkillManifest{}, fmt.Errorf("SKILL.md must start with a YAML frontmatter block declaring `name:` (between `---` fences)")
	}
	var fm skillFrontmatter
	if err := yaml.Unmarshal([]byte(m[1]), &fm); err != nil {
		return SkillManifest{}, fmt.Errorf("SKILL.md frontmatter: %w", err)
	}
	fm.Name = strings.TrimSpace(fm.Name)
	fm.Version = strings.TrimSpace(fm.Version)
	if fm.Name == "" {
		return SkillManifest{}, fmt.Errorf("SKILL.md frontmatter is missing `name:` — the skill's directory is derived from that field, so it has to be set")
	}
	if !ValidSkillName(fm.Name) {
		return SkillManifest{}, fmt.Errorf("SKILL.md frontmatter `name: %q` is invalid — use lowercase letters, digits, and hyphens only (no leading/trailing hyphen)", fm.Name)
	}
	return fm, nil
}

// ReadSkillFile returns the raw bytes of a single file inside a
// skill dir. rel is forward-slash, relative to the skill root. Path
// safety is enforced: ".." and absolute paths are rejected.
func (s *FSStore) ReadSkillFile(name, rel string) ([]byte, error) {
	if !ValidSkillName(name) {
		return nil, fmt.Errorf("skill name %q is invalid", name)
	}
	cleaned, err := safeSkillRel(rel)
	if err != nil {
		return nil, err
	}
	full := filepath.Join(s.skillsDir(), name, filepath.FromSlash(cleaned))
	b, err := os.ReadFile(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

// loadSkill builds a Skill from its directory. Returns ErrNotFound
// when either the skill directory or its SKILL.md is absent, so
// callers can distinguish "doesn't exist" from "corrupt". Other I/O
// errors propagate wrapped. Frontmatter parse errors are non-fatal
// (empty description/when_to_use).
func (s *FSStore) loadSkill(name string) (Skill, error) {
	dir := filepath.Join(s.skillsDir(), name)
	manifestPath := filepath.Join(dir, SkillManifestName)
	mInfo, err := os.Stat(manifestPath)
	if errors.Is(err, fs.ErrNotExist) {
		return Skill{}, ErrNotFound
	}
	if err != nil {
		return Skill{}, fmt.Errorf("skill %q: %w", name, err)
	}
	skill := Skill{
		Name:      name,
		Path:      filepath.Join("skills", name, SkillManifestName),
		Dir:       filepath.Join("skills", name),
		UpdatedAt: mInfo.ModTime(),
		Version:   DefaultSkillVersion,
		// Built-in-ness comes from what is compiled into this binary,
		// never from anything on disk. A marker file would be simpler
		// and would also let an uploaded bundle make itself
		// undeletable by including one.
		Builtin: builtinskills.Has(name),
		Enabled: s.SkillEnabled(name),
	}
	if meta, err := readSkillFrontmatter(manifestPath); err == nil {
		skill.Description = meta.Description
		skill.WhenToUse = meta.WhenToUse
		if v := strings.TrimSpace(meta.Version); v != "" {
			skill.Version = v
		}
	}
	// Walk the whole skill dir for size + file count + tree.
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		skill.Files = append(skill.Files, SkillFile{
			Path:       filepath.ToSlash(rel),
			Size:       info.Size(),
			Executable: info.Mode().Perm()&0o111 != 0,
		})
		skill.Size += info.Size()
		skill.FileCount++
		if mt := info.ModTime(); mt.After(skill.UpdatedAt) {
			skill.UpdatedAt = mt
		}
		return nil
	})
	if err != nil {
		return Skill{}, err
	}
	sort.Slice(skill.Files, func(i, j int) bool { return skill.Files[i].Path < skill.Files[j].Path })
	return skill, nil
}

// WriteSkillFromManifest creates or overwrites a skill directory
// with a single SKILL.md file. Used for the paste-a-markdown-body
// path from the UI; zip uploads go through WriteSkillFromZip.
func (s *FSStore) WriteSkillFromManifest(name, body string) error {
	if !ValidSkillName(name) {
		return fmt.Errorf("skill name %q: lowercase letters, digits, and hyphens only; no leading/trailing hyphen", name)
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("skill body is empty")
	}
	dir := filepath.Join(s.skillsDir(), name)
	if err := replaceSkillDir(dir, func(stage string) error {
		return os.WriteFile(filepath.Join(stage, SkillManifestName), []byte(body), 0o644)
	}); err != nil {
		return err
	}
	return nil
}

// WriteSkillFromZip unpacks an uploaded .zip / .skill bundle into
// data/skills/<name>/. The zip must contain SKILL.md at the top
// level (either directly, or inside a single top-level directory
// which we auto-strip so bundles created via "zip -r foo foo/" work
// naturally). File mode bits are preserved so scripts with chmod +x
// stay executable downstream.
//
// The write is staged in a sibling temp dir and renamed into place,
// so a half-extracted bundle never pollutes the live skill path. On
// any validation failure the temp dir is removed.
func (s *FSStore) WriteSkillFromZip(name string, data []byte) error {
	if !ValidSkillName(name) {
		return fmt.Errorf("skill name %q: lowercase letters, digits, and hyphens only; no leading/trailing hyphen", name)
	}
	if len(data) == 0 {
		return errors.New("skill zip is empty")
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("skill zip: %w", err)
	}
	prefix, err := detectSkillZipPrefix(zr)
	if err != nil {
		return err
	}
	if !zipContainsManifest(zr, prefix) {
		return fmt.Errorf("skill zip: missing %s at the top of the archive", SkillManifestName)
	}
	dir := filepath.Join(s.skillsDir(), name)
	return replaceSkillDir(dir, func(stage string) error {
		return extractZipInto(zr, prefix, stage)
	})
}

// DeleteSkill removes a skill directory tree. Silent on missing.
func (s *FSStore) DeleteSkill(name string) error {
	if !ValidSkillName(name) {
		return fmt.Errorf("skill name %q is invalid", name)
	}
	// Refused at the store, not just in the handler. Deleting a
	// built-in would appear to work and then undo itself on the next
	// boot, which is worse than a clear refusal — and the caller that
	// wanted it gone wants SetSkillEnabled(name, false).
	if builtinskills.Has(name) {
		return fmt.Errorf("skill %q ships with Kivali and cannot be deleted — disable it instead", name)
	}
	dir := filepath.Join(s.skillsDir(), name)
	if err := os.RemoveAll(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// replaceSkillDir runs build() against a fresh staging dir, then
// atomically swaps it in for the target skill dir. The parent
// (skills/) is created if missing. Any prior version is moved to a
// trash-dir and removed after the swap, so a concurrent reader
// always sees a consistent directory.
func replaceSkillDir(dir string, build func(stage string) error) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".skill-stage-*")
	if err != nil {
		return err
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := build(stage); err != nil {
		return err
	}
	// If a prior version exists, rename it aside first so the swap
	// is still near-atomic: `rename(stage, dir)` fails on a non-empty
	// target on many filesystems, so we do it in two steps.
	trash := ""
	if _, err := os.Stat(dir); err == nil {
		trash, err = os.MkdirTemp(parent, ".skill-trash-*")
		if err != nil {
			return err
		}
		// Move the old dir *into* the trash dir so the name stays
		// unique; on the rare failure of Rename we leave it behind
		// for manual cleanup rather than corrupt the live path.
		if err := os.Rename(dir, filepath.Join(trash, "old")); err != nil {
			_ = os.RemoveAll(trash)
			return err
		}
	}
	if err := os.Rename(stage, dir); err != nil {
		// Best-effort restore from trash.
		if trash != "" {
			_ = os.Rename(filepath.Join(trash, "old"), dir)
			_ = os.RemoveAll(trash)
		}
		return err
	}
	cleanupStage = false
	if trash != "" {
		_ = os.RemoveAll(trash)
	}
	return nil
}

// detectSkillZipPrefix returns the path prefix to strip from every
// zip entry so the zip's top-level content becomes the skill root.
// Handles two common layouts:
//
//   - Flat: SKILL.md at the root of the archive → prefix "".
//   - Single folder: foo/SKILL.md, foo/scripts/... → prefix "foo/".
//
// Rejects archives that have SKILL.md at neither location.
func detectSkillZipPrefix(zr *zip.Reader) (string, error) {
	// Pass 1: is SKILL.md at the flat root?
	for _, f := range zr.File {
		if !f.FileInfo().IsDir() && path.Clean(f.Name) == SkillManifestName {
			return "", nil
		}
	}
	// Pass 2: is there exactly one top-level dir and does it hold SKILL.md?
	topDirs := map[string]bool{}
	for _, f := range zr.File {
		name := strings.TrimLeft(path.Clean(f.Name), "/")
		if name == "" || name == "." {
			continue
		}
		first := name
		if i := strings.IndexByte(name, '/'); i >= 0 {
			first = name[:i]
		}
		if first == "__MACOSX" { // macOS zip turds — ignore
			continue
		}
		topDirs[first] = true
	}
	if len(topDirs) != 1 {
		return "", fmt.Errorf("skill zip: expected %s at the archive root or inside a single top-level folder", SkillManifestName)
	}
	var only string
	for k := range topDirs {
		only = k
	}
	return only + "/", nil
}

// zipContainsManifest reports whether SKILL.md exists directly under
// prefix. Used as a sanity check — detectSkillZipPrefix may pick a
// prefix that technically fits the shape rule but where SKILL.md is
// buried deeper, which we still reject.
func zipContainsManifest(zr *zip.Reader, prefix string) bool {
	want := prefix + SkillManifestName
	for _, f := range zr.File {
		if path.Clean(f.Name) == want && !f.FileInfo().IsDir() {
			return true
		}
	}
	return false
}

// extractZipInto writes every zip entry whose name starts with
// prefix into dst (with the prefix stripped). Mode bits are
// preserved for regular files. Directories are created on demand.
// Rejects: absolute paths, `..` traversal, symlinks, hard links,
// special files (device nodes etc.). Skips __MACOSX/ metadata.
func extractZipInto(zr *zip.Reader, prefix, dst string) error {
	// 10 MB per file, 200 MB total bundle — generous but not a
	// reasonable skill. Prevents a crafted zip from filling disk.
	const perFileCap = 10 << 20
	const totalCap = 200 << 20
	var total int64
	for _, f := range zr.File {
		cleanedName := path.Clean(f.Name)
		if strings.HasPrefix(cleanedName, "__MACOSX") || strings.Contains(cleanedName, "/__MACOSX/") {
			continue
		}
		if prefix != "" && !strings.HasPrefix(cleanedName, prefix) {
			// Entry outside our single top-level dir — shouldn't
			// happen after detectSkillZipPrefix, but belt + braces.
			continue
		}
		rel := strings.TrimPrefix(cleanedName, prefix)
		if rel == "" || rel == "." {
			continue
		}
		if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") || rel == ".." {
			return fmt.Errorf("skill zip: unsafe path %q", f.Name)
		}
		mode := f.Mode()
		if mode&(os.ModeSymlink|os.ModeDevice|os.ModeNamedPipe|os.ModeSocket|os.ModeCharDevice) != 0 {
			return fmt.Errorf("skill zip: %q has unsupported file type", f.Name)
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		// Re-verify the computed path stays under dst even after
		// filepath.Join resolves any symbolic `..` that slipped through.
		if !strings.HasPrefix(target, dst+string(filepath.Separator)) && target != dst {
			return fmt.Errorf("skill zip: unsafe path %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// Honor exec bit; ignore writeability flags — everything
		// lands as user-writable for the Kivali process.
		perm := os.FileMode(0o644)
		if mode.Perm()&0o111 != 0 {
			perm = 0o755
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("skill zip: open %q: %w", f.Name, err)
		}
		limited := io.LimitReader(rc, perFileCap+1)
		buf, err := io.ReadAll(limited)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("skill zip: read %q: %w", f.Name, err)
		}
		if int64(len(buf)) > perFileCap {
			return fmt.Errorf("skill zip: %q exceeds %d-byte per-file cap", f.Name, perFileCap)
		}
		total += int64(len(buf))
		if total > totalCap {
			return fmt.Errorf("skill zip: bundle exceeds %d-byte total cap", totalCap)
		}
		if err := os.WriteFile(target, buf, perm); err != nil {
			return err
		}
	}
	return nil
}

// safeSkillRel cleans a forward-slash skill-relative path and
// rejects anything that tries to escape the skill dir.
func safeSkillRel(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", errors.New("path required")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("path %q must be relative", rel)
	}
	cleaned := path.Clean(rel)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("path %q escapes skill dir", rel)
	}
	return cleaned, nil
}

// frontmatterFenceRE matches an optional YAML header at the top of a
// markdown file.
var frontmatterFenceRE = regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---\r?\n?(.*)\z`)

func readSkillFrontmatter(p string) (skillFrontmatter, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return skillFrontmatter{}, err
	}
	m := frontmatterFenceRE.FindSubmatch(b)
	if m == nil {
		return skillFrontmatter{}, nil
	}
	var fm skillFrontmatter
	if err := yaml.Unmarshal(m[1], &fm); err != nil {
		return skillFrontmatter{}, err
	}
	return fm, nil
}
