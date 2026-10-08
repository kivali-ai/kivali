package graph

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Manifest is the front matter an agent writes at the top of a node
// file. Every field is optional at parse time; the rules in build.go
// decide what a given kind requires. Field names are the wire format
// and are enumerated in knownManifestFields so a typo surfaces as a
// problem instead of silently dropping an edge.
type Manifest struct {
	ID         string     `yaml:"id"`
	Kind       Kind       `yaml:"kind"`
	About      string     `yaml:"about"`
	Status     Status     `yaml:"status"`
	Condition  string     `yaml:"condition"`
	DependsOn  StringList `yaml:"depends_on"`
	Supersedes StringList `yaml:"supersedes"`
	Check      string     `yaml:"check"`
	Source     string     `yaml:"source"`
	File       string     `yaml:"file"`
	Evidence   StringList `yaml:"evidence"`
	Summary    string     `yaml:"summary"`
}

var knownManifestFields = map[string]bool{
	"id": true, "kind": true, "about": true, "status": true, "condition": true,
	"depends_on": true, "supersedes": true, "check": true, "source": true,
	"file": true, "evidence": true, "summary": true,
}

// listFields accept a single value or a list; every other known field
// is a single value. checkFieldShapes uses this to name the offending
// field when an agent writes `about: [a, b]`, which the YAML decoder
// would otherwise report only as a line number and a type tag.
var listFields = map[string]bool{"depends_on": true, "supersedes": true, "evidence": true}

// StringList accepts a YAML scalar or sequence, so `depends_on: x`
// and `depends_on: [x, y]` both load. Same asymmetry as
// store.Recipients, for the same reason: agents write both.
type StringList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (l *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var one string
		if err := n.Decode(&one); err != nil {
			return err
		}
		one = strings.TrimSpace(one)
		if one == "" {
			*l = nil
			return nil
		}
		*l = StringList{one}
		return nil
	case yaml.SequenceNode:
		var many []string
		if err := n.Decode(&many); err != nil {
			return err
		}
		out := make(StringList, 0, len(many))
		for _, s := range many {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		*l = out
		return nil
	default:
		return fmt.Errorf("expected a value or a list, got %s", yamlKindName(n.Kind))
	}
}

func yamlKindName(k yaml.Kind) string {
	switch k {
	case yaml.ScalarNode:
		return "a value"
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.AliasNode:
		return "an alias"
	case yaml.DocumentNode:
		return "a document"
	}
	return "something else"
}

// frontMatterRE matches a YAML block fenced by `---` lines at the top
// of a file. Tolerant of what editors and heredocs put in front of the
// fence — a UTF-8 byte-order mark, blank lines — and of trailing
// spaces on either fence, because a manifest that is silently a bare
// file is worse than one that parses; CRLF tolerant throughout.
var frontMatterRE = regexp.MustCompile(`(?s)\A\x{FEFF}?(?:[ \t]*\r?\n)*---[ \t]*\r?\n(.*?)\r?\n---[ \t]*\r?\n?(.*)\z`)

// HasFrontMatter reports whether the file opens with a fenced block.
// A file that does not is a bare artifact and is never parsed.
func HasFrontMatter(body []byte) bool {
	return frontMatterRE.Match(body)
}

// keyLineRE is what the first line of a YAML mapping looks like. When
// a fenced block does not parse, this decides whether it was a broken
// manifest (rejected, owner told) or prose between two horizontal
// rules (a bare artifact, nobody told).
var keyLineRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*[ \t]*:([ \t]|$)`)

// Parsed is the result of ParseManifest: the manifest, the markdown
// body after the fence, and any front-matter keys the schema does not
// know. Unknown keys are reported as problems by Build, not treated as
// rejections — `title:` at the top of a file is a habit, not a fault.
type Parsed struct {
	Manifest      Manifest
	Body          string
	UnknownFields []string
}

// Facts is everything Plan and Build take from a markdown file: its
// parsed front matter, or the reason it was rejected, and the two
// things read off the body after it (the first line, the summary when
// the manifest declares none; and the line count). A caller that keeps
// files between builds keeps these instead of the bytes. ReadFacts is
// a pure function of the bytes, so a Build from Facts is the Build
// from Body.
type Facts struct {
	// Parsed is nil for a file with no front matter (a bare artifact)
	// and for a rejected one. Its Body is left empty: FirstLine and
	// Lines are all a build uses of it.
	Parsed    *Parsed
	ParseErr  string
	FirstLine string
	Lines     int
}

// ReadFacts parses body the way Build does.
func ReadFacts(body []byte) Facts {
	if !HasFrontMatter(body) {
		return Facts{}
	}
	p, err := ParseManifest(body)
	if errors.Is(err, ErrNoFrontMatter) {
		return Facts{} // a fence around prose: a bare artifact, not a manifest
	}
	if err != nil {
		return Facts{ParseErr: err.Error()}
	}
	f := Facts{FirstLine: firstLine(p.Body), Lines: lineCount(p.Body)}
	p.Body = ""
	f.Parsed = &p
	return f
}

// ErrNoFrontMatter is returned when the body has no fenced block.
var ErrNoFrontMatter = errors.New("graph: no front matter")

// ParseManifest splits a node file into its manifest and body.
//
// A fenced block that decodes to something other than a mapping (prose
// between two horizontal rules, a list, a bare number), or that fails
// to parse and does not open with a key, is not front matter at all
// and returns ErrNoFrontMatter: the file is a bare artifact, and the
// owner is not told about a manifest they never wrote. A block that
// opens with a key and does not parse, or parses with a field of the
// wrong shape, is a rejection whose text names what is wrong.
func ParseManifest(body []byte) (Parsed, error) {
	m := frontMatterRE.FindSubmatch(body)
	if m == nil {
		return Parsed{}, ErrNoFrontMatter
	}
	block := m[1]
	var doc yaml.Node
	if err := yaml.Unmarshal(block, &doc); err != nil {
		if !looksLikeMapping(block) {
			return Parsed{}, ErrNoFrontMatter
		}
		return Parsed{}, fmt.Errorf("front matter is not valid YAML: %v", yamlErrorLine(err))
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return Parsed{}, ErrNoFrontMatter
	}
	if err := checkFieldShapes(doc.Content[0]); err != nil {
		return Parsed{}, err
	}
	var p Parsed
	if err := doc.Decode(&p.Manifest); err != nil {
		return Parsed{}, fmt.Errorf("front matter could not be read: %v", yamlErrorLine(err))
	}
	var raw map[string]any
	if err := doc.Decode(&raw); err == nil {
		for k := range raw {
			if !knownManifestFields[k] {
				p.UnknownFields = append(p.UnknownFields, k)
			}
		}
		sort.Strings(p.UnknownFields)
	}
	p.Body = string(m[2])
	p.Manifest.ID = strings.TrimSpace(p.Manifest.ID)
	p.Manifest.Kind = Kind(strings.ToLower(strings.TrimSpace(string(p.Manifest.Kind))))
	p.Manifest.About = strings.TrimSpace(p.Manifest.About)
	p.Manifest.Status = Status(strings.ToLower(strings.TrimSpace(string(p.Manifest.Status))))
	p.Manifest.Condition = strings.TrimSpace(p.Manifest.Condition)
	p.Manifest.Check = strings.TrimSpace(p.Manifest.Check)
	p.Manifest.Source = strings.TrimSpace(p.Manifest.Source)
	p.Manifest.File = strings.TrimSpace(p.Manifest.File)
	p.Manifest.Summary = strings.TrimSpace(p.Manifest.Summary)
	return p, nil
}

// looksLikeMapping reports whether the block's first non-blank line is
// shaped like a YAML key, which is how a broken manifest is told apart
// from prose that merely sits between two horizontal rules.
func looksLikeMapping(block []byte) bool {
	for _, ln := range strings.Split(string(block), "\n") {
		ln = strings.TrimRight(ln, "\r")
		if strings.TrimSpace(ln) == "" {
			continue
		}
		return keyLineRE.MatchString(ln)
	}
	return false
}

// checkFieldShapes walks the mapping's known keys and names the first
// one whose value has the wrong shape: a list where a single value
// belongs, a mapping anywhere, a list whose items are not values.
// Unknown keys are left for the caller to report as problems.
func checkFieldShapes(mapping *yaml.Node) error {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key, val := mapping.Content[i], mapping.Content[i+1]
		if key.Kind != yaml.ScalarNode || !knownManifestFields[key.Value] {
			continue
		}
		switch {
		case listFields[key.Value]:
			switch val.Kind {
			case yaml.ScalarNode:
			case yaml.SequenceNode:
				for _, item := range val.Content {
					if item.Kind != yaml.ScalarNode {
						return fmt.Errorf("field %q must list values, not %s", key.Value, yamlKindName(item.Kind))
					}
				}
			default:
				return fmt.Errorf("field %q must be a value or a list, not %s", key.Value, yamlKindName(val.Kind))
			}
		case val.Kind != yaml.ScalarNode:
			return fmt.Errorf("field %q must be a single value, not %s", key.Value, yamlKindName(val.Kind))
		}
	}
	return nil
}

// yamlErrorLine renders a yaml error as one line for the trailer: a
// type error lists every offending line, anything else keeps its first
// line.
func yamlErrorLine(err error) string {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		return strings.Join(te.Errors, "; ")
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimPrefix(s, "yaml: ")
}

// nameRE is the shape of a declared id: a slug path. Lowercase so the
// same node cannot be spelt two ways, no leading dot so nothing hides.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)

// ValidName reports whether a declared id is a slug path.
func ValidName(name string) bool {
	return nameRE.MatchString(name)
}

// DeriveName turns a public-relative file path into the node name a
// file gets when it declares none: the path without its extension,
// lowercased, with anything outside the slug alphabet folded to '-'.
// A rename changes it, which is why anything others will point at
// should declare an id instead.
func DeriveName(relPath string) string {
	return deriveName(relPath, true)
}

// deriveName is DeriveName with the extension kept when stripExt is
// false (non-markdown files, so `report.zip` and `report.md` beside each
// other stay distinct). Only the final extension is ever stripped, so
// `archive.tar.gz` keeps `.tar` either way.
func deriveName(relPath string, stripExt bool) string {
	p := strings.TrimPrefix(path.Clean(strings.ReplaceAll(relPath, "\\", "/")), "/")
	if stripExt {
		if ext := path.Ext(p); ext != "" && ext != path.Base(p) {
			p = strings.TrimSuffix(p, ext)
		}
	}
	p = strings.ToLower(p)
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-', r == '/':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-/")
	if out == "" {
		return "unnamed"
	}
	return out
}

// NodeID joins an owner slug and a node name.
func NodeID(owner, name string) string {
	return owner + "/" + name
}

// OwnerOf returns the owner segment of an artifact id, or the id
// itself for an agent (slugs carry no slash).
func OwnerOf(id string) string {
	if i := strings.IndexByte(id, '/'); i >= 0 {
		return id[:i]
	}
	return id
}

// ResolvePayload turns a manifest's `file:` value into a path relative
// to the same public root, resolved against the manifest's directory.
// ok is false when the value escapes the public tree or is empty.
func ResolvePayload(manifestRelPath, file string) (string, bool) {
	file = strings.TrimSpace(strings.ReplaceAll(file, "\\", "/"))
	if file == "" {
		return "", false
	}
	var joined string
	if strings.HasPrefix(file, "/") {
		joined = path.Clean(file)
	} else {
		joined = path.Clean(path.Join(path.Dir(manifestRelPath), file))
	}
	joined = strings.TrimPrefix(joined, "/")
	if joined == "" || joined == "." || joined == ".." || strings.HasPrefix(joined, "../") {
		return "", false
	}
	return joined, true
}

// firstLine returns the first non-empty line of body with leading
// markdown heading marks removed, truncated on a rune boundary for a
// listing. Used as the summary when the manifest declares none.
func firstLine(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(ln), "#"))
		if ln == "" {
			continue
		}
		if utf8.RuneCountInString(ln) > summaryMaxLen {
			r := []rune(ln)
			ln = strings.TrimSpace(string(r[:summaryMaxLen-1])) + "…"
		}
		return ln
	}
	return ""
}

const summaryMaxLen = 140

// lineCount counts the body's non-blank lines, the measure the
// binding-kind length problem uses.
func lineCount(body string) int {
	n := 0
	for _, ln := range strings.Split(body, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n
}
