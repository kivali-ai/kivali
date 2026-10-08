// Package builtinskills holds the skills that ship with Kivali.
//
// A skill is normally something the CEO uploads: an org procedure that
// belongs to the org, versioned independently of the binary. A built-in
// skill is the exception — a procedure that describes how Kivali
// itself works, which would be strange to make every deployment
// discover and write for itself.
//
// They behave like ordinary skills everywhere it matters. They are
// materialised into data/skills/ at boot, so `list_skills`, file_view,
// the /files/skills/ mirror, and the agent-pod staging path all treat
// them as exactly what they are: a directory with a SKILL.md in it.
// One mechanism, not two.
//
// The differences are deliberate and narrow:
//
//   - They cannot be deleted. Deleting one would only make it come
//     back on the next boot, so the button would be a lie. Disable is
//     the real operation, and it is available for every skill.
//   - They cannot be replaced by upload. An edit would be silently
//     reverted by the next deploy, which is worse than being refused.
//     Editing one means editing this package.
//
// To add a built-in skill, drop a directory under skills/ with a
// SKILL.md inside. The embed picks it up; nothing here needs a list.
package builtinskills

import (
	"embed"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed all:skills
var embedded embed.FS

// ManifestName matches store.SkillManifestName. Duplicated rather
// than imported because store depends on this package, and the reverse
// import would be a cycle. It is a filename fixed by the Claude Code
// skill convention, not a setting either package owns.
const ManifestName = "SKILL.md"

// Names returns every built-in skill's name, sorted. The name is the
// directory name, which the materialiser and the store both key on.
func Names() []string {
	entries, err := fs.ReadDir(embedded, "skills")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Has reports whether name is a built-in skill. This is the single
// source of truth for "is this skill undeletable" — derived from what
// is compiled in, never from a marker on disk, so an uploaded bundle
// cannot make itself permanent by including one.
func Has(name string) bool {
	if name == "" || strings.ContainsAny(name, "/\\.") {
		return false
	}
	info, err := fs.Stat(embedded, path.Join("skills", name))
	return err == nil && info.IsDir()
}

// File is one file inside a built-in skill, with its path relative to
// the skill directory.
type File struct {
	Path string
	Data []byte
}

// Files returns every file in a built-in skill, sorted by path.
// Returns nil for an unknown name.
func Files(name string) []File {
	if !Has(name) {
		return nil
	}
	root := path.Join("skills", name)
	var out []File
	_ = fs.WalkDir(embedded, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err //nolint:nilerr // walk errors propagate; dirs are skipped
		}
		b, rerr := embedded.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepathRel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, File{Path: rel, Data: b})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// filepathRel is path.Rel for embed's always-slash paths. The stdlib
// has no slash-only Rel, and filepath.Rel would do the wrong thing on
// Windows, where embedded paths are still slash-separated.
func filepathRel(base, target string) (string, error) {
	base = strings.TrimSuffix(base, "/")
	if target == base {
		return ".", nil
	}
	if !strings.HasPrefix(target, base+"/") {
		return "", fs.ErrInvalid
	}
	return strings.TrimPrefix(target, base+"/"), nil
}
