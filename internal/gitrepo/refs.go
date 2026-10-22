package gitrepo

// Refs as a push names them. Core holds an agent's git_push to these
// rules and the agent-shell daemon holds the job to them again:
//
//   - A name without refs/ is a branch: agent/x is refs/heads/agent/x.
//     A tag is written tags/<name> or refs/tags/<name>; a bare v1.2.0
//     is the branch refs/heads/v1.2.0. refs/heads/... and refs/tags/...
//     are used as written. A push writes branches and tags, nothing
//     else.
//   - A pattern is normalised the same way (agent/* is refs/heads/agent/*,
//     tags/v* is refs/tags/v*) except * alone, which is every branch and
//     every tag (refs/heads/* and refs/tags/*). * is its only wildcard
//     (? and [...] are refused: path.Match lets a class match a /). It
//     is matched against the whole ref with path.Match: * stands within
//     one segment and never across a /, so agent/* does not match
//     agent/a/b. Case counts, as it does in git.
//   - A refspec is [+]<src>:<dst>. dst is a ref name as above. src is a
//     ref name (refs/... as written, a bare name a branch, tags/... a
//     tag), HEAD, or a 40- or 64-hex object id. An empty src deletes
//     the remote ref, which is never done (ErrDeleteRef). + forces. A
//     bare ref x is the refspec refs/heads/x:refs/heads/x (a tag the
//     same). Wildcards are not refspecs here.

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// The two namespaces a push writes.
const (
	BranchPrefix = "refs/heads/"
	TagPrefix    = "refs/tags/"
)

// maxRefLen bounds a ref name.
const maxRefLen = 255

// ErrDeleteRef is a refspec with an empty source: a deletion of the
// remote ref.
var ErrDeleteRef = errors.New("deleting a remote ref is not supported")

// NormalizeRef is a branch or a tag as a push's destination names it,
// made a full ref: refs/heads/<name> for a name without refs/,
// refs/tags/<name> for tags/<name>, refs/heads/... and refs/tags/... as
// written. Anything else, or a name git would refuse, is an error.
func NormalizeRef(name string) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", errors.New("name a branch or a tag")
	}
	full := qualify(n)
	if !underBranchOrTag(full) {
		return "", fmt.Errorf("%q is neither a branch (refs/heads/...) nor a tag (refs/tags/...)", name)
	}
	if !validRefName(full, false) || !validShortName(full) {
		return "", fmt.Errorf("%q is not a branch or tag name git accepts", name)
	}
	return full, nil
}

// qualify is n with refs/heads/ or refs/ in front, by the rules above.
func qualify(n string) string {
	switch {
	case strings.HasPrefix(n, "refs/"):
		return n
	case strings.HasPrefix(n, "tags/"):
		return "refs/" + n
	default:
		return BranchPrefix + n
	}
}

func underBranchOrTag(full string) bool {
	for _, p := range []string{BranchPrefix, TagPrefix} {
		if rest, ok := strings.CutPrefix(full, p); ok && rest != "" {
			return true
		}
	}
	return false
}

// validShortName refuses the branch or tag names git's own commands
// refuse though the full ref would pass: HEAD, and a name starting
// with a dash.
func validShortName(full string) bool {
	short := strings.TrimPrefix(strings.TrimPrefix(full, BranchPrefix), TagPrefix)
	return short != "HEAD" && !strings.HasPrefix(short, "-")
}

// NormalizeRefPattern is an access pattern of branches and tags as
// MatchRef reads it: * alone is refs/heads/* and refs/tags/*; any other
// is normalised as NormalizeRef normalises a name. It must be under
// refs/heads/ or refs/tags/, with * as its only wildcard.
func NormalizeRefPattern(p string) ([]string, error) {
	n := strings.TrimSpace(p)
	if n == "" {
		return nil, errors.New("an empty pattern")
	}
	if n == "*" {
		return []string{BranchPrefix + "*", TagPrefix + "*"}, nil
	}
	full := qualify(n)
	if !underBranchOrTag(full) || !validRefName(full, true) {
		return nil, fmt.Errorf("%q is not a branch or tag pattern such as agent/* or tags/v*", p)
	}
	if _, err := path.Match(full, ""); err != nil {
		return nil, fmt.Errorf("%q is not a branch or tag pattern such as agent/* or tags/v*", p)
	}
	return []string{full}, nil
}

// NormalizeRefPatterns is every pattern of in, normalised, in order. One
// that does not read refuses them all.
func NormalizeRefPatterns(in []string) ([]string, error) {
	var out []string
	for _, p := range in {
		n, err := NormalizeRefPattern(p)
		if err != nil {
			return nil, err
		}
		out = append(out, n...)
	}
	return out, nil
}

// MatchRef reports whether the full ref is one of patterns, each
// normalised by NormalizeRefPattern and matched with path.Match. A
// pattern that does not read matches nothing.
func MatchRef(patterns []string, ref string) bool {
	for _, p := range patterns {
		ns, err := NormalizeRefPattern(p)
		if err != nil {
			continue
		}
		for _, n := range ns {
			if ok, err := path.Match(n, ref); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// PushRefSpec is one refspec of a push, normalised: Src a full ref
// name, HEAD or an object id; Dst a branch or a tag; Force whether it
// may move Dst other than fast-forward.
type PushRefSpec struct {
	Src   string
	Dst   string
	Force bool
}

// String is the refspec as git writes it: [+]src:dst.
func (s PushRefSpec) String() string {
	if s.Force {
		return "+" + s.Src + ":" + s.Dst
	}
	return s.Src + ":" + s.Dst
}

// IsObjectID reports whether s is a full SHA-1 or SHA-256 object id in
// lower-case hex.
func IsObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	return strings.IndexFunc(s, func(c rune) bool { return (c < '0' || c > '9') && (c < 'a' || c > 'f') }) < 0
}

// ParsePushRefSpec reads one entry of a push's refs by the rules
// above. A deletion is ErrDeleteRef.
func ParsePushRefSpec(raw string) (PushRefSpec, error) {
	s := strings.TrimSpace(raw)
	var spec PushRefSpec
	if rest, ok := strings.CutPrefix(s, "+"); ok {
		spec.Force, s = true, rest
	}
	if len(s) > 2*maxRefLen+1 {
		return spec, fmt.Errorf("%q is too long", clipRef(raw))
	}
	src, dst, hasDst := strings.Cut(s, ":")
	if !hasDst {
		if s == "HEAD" || IsObjectID(s) {
			return spec, fmt.Errorf("%q needs a destination, such as %s:refs/heads/agent/x", raw, s)
		}
		full, err := NormalizeRef(s)
		if err != nil {
			return spec, err
		}
		spec.Src, spec.Dst = full, full
		return spec, nil
	}
	if strings.TrimSpace(src) == "" {
		return spec, ErrDeleteRef
	}
	if strings.TrimSpace(dst) == "" {
		return spec, fmt.Errorf("%q names no destination", raw)
	}
	switch {
	case src == "HEAD" || IsObjectID(src):
		spec.Src = src
	default:
		full := qualify(src)
		if !validRefName(full, false) {
			return spec, fmt.Errorf("%q is not a ref, HEAD or a commit's full hash", src)
		}
		spec.Src = full
	}
	full, err := NormalizeRef(dst)
	if err != nil {
		return spec, err
	}
	spec.Dst = full
	return spec, nil
}

func clipRef(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// validRefName holds a full ref name (refs/...) to git's
// check-ref-format: segments that are not empty, do not start with a
// dot or end with .lock; no "..", "@{", "//", control characters,
// space, ~ ^ : ? [ ] \ or DEL; not ending with / or .; not "@". *
// is allowed only in a pattern.
func validRefName(ref string, pattern bool) bool {
	if len(ref) > maxRefLen || !strings.HasPrefix(ref, "refs/") || ref == "@" ||
		strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") {
		return false
	}
	for _, c := range ref {
		switch {
		case c < 0x20 || c == 0x7f:
			return false
		case c == ' ' || c == '~' || c == '^' || c == ':' || c == '\\' || c == '?' || c == '[' || c == ']':
			return false
		case c == '*':
			if !pattern {
				return false
			}
		}
	}
	for _, seg := range strings.Split(ref, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return false
		}
	}
	return true
}
