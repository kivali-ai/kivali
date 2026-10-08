// Package files is the per-agent filesystem backend for the file_*
// virtual-filesystem tool family. Each agent gets its own rooted
// directory under data/agents/<slug>/<StoragePrefix>/; the model
// operates through /files/<path> and we map that to files under the
// root, with traversal protection.
//
// The layout we provision on bootstrap:
//
//	/files/project/                       canonical project files (RO)
//	/files/past-chats/                    archived chat generations (RO)
//	/files/episodes/                      one digest per archived generation (RO)
//	/files/skills/                        shared org skills (RO)
//	/files/attachments/                   chat/inbox attachments (RO)
//	/files/artifacts/private/             agent's private drafts (RW)
//	/files/artifacts/public/              agent's published files (RO; public/<slug>)
//	/files/artifacts/shared/<slug>/       every agent's published files (RO; public/)
//
// Read-only enforcement is protocol-level: the backend rejects writes
// under any RO subtree. Symlinks are followed only as far as the
// backend's own roots (backend_confine.go): a link run_shell planted
// to anywhere else is refused, for reads and writes alike.
package files

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

// StoragePrefix is the on-disk directory under each agent's data dir
// that holds this filesystem: data/agents/<slug>/memory/ is what the
// agent sees as /files/. Build host paths with StorageRoot.
const StoragePrefix = "memory"

// ModelRootPath is the agent-visible root of this filesystem. The
// model addresses files as "/files/<path>"; the backend strips this
// prefix and resolves to <Backend.Root>/<path>.
const ModelRootPath = "/files"

// ErrNotFound is returned for missing paths. ErrReadOnly for writes
// under a read-only subtree.
var (
	ErrNotFound = errors.New("files: not found")
	ErrReadOnly = errors.New("files: path is read-only")
	// ErrPublishedReadOnly is ErrReadOnly for the published trees,
	// worded so an agent that tries to write one learns the way in.
	ErrPublishedReadOnly = fmt.Errorf("%w: /files/artifacts/public/ and /files/artifacts/shared/ change only through artifact_publish and artifact_unpublish; write the file under /files/artifacts/private/ and publish it with artifact_publish", ErrReadOnly)
)

// Backend serves file_* tool calls for one agent. The Root is the
// absolute host path of that agent's /files/ view.
//
// ReadOnlyRoots, when non-nil, replaces the default RO subtree list
// (DefaultReadOnlyRoots). Subagent overlays use the defaults too —
// project/, skills/, attachments/, artifacts/shared/, past-chats/
// stay read-only.
//
// ReadRoots and WriteRoots bound where a symlink under Root may lead
// (see backend_confine.go). Reads land in Root, a WriteRoot or a
// ReadRoot; writes land in Root or a WriteRoot, outside every
// read-only subtree. Anything else — a link run_shell planted to
// /data/claude-home, to a sibling agent's tree — is refused.
type Backend struct {
	Root          string
	ReadOnlyRoots []string
	// ReadRoots are the host directories outside Root that core's
	// symlink farms point into: in the agent pod's dev-shell container
	// /data/project_files, /data/skills, /data/attachments and
	// /data/agents/<slug>/chats (see PodReadRoots). Read-only to the
	// backend.
	ReadRoots []string
	// WriteRoots are directories outside Root that are as much the
	// agent's own as Root is. A subagent's Root is a subdirectory of
	// its parent's /files/, and its overlay links (background/,
	// artifacts/public/) resolve into the parent tree, so a subagent
	// backend names the parent's /files/ here.
	WriteRoots []string
	// ReadAliases maps a directory a farm link points into to the
	// read-only directory a read that resolves there is served from,
	// at the same relative path. Core's attachments/ farm points into
	// the org-wide blob store, which core must not read from on an
	// agent's behalf; the alias serves the same <sha>/<file> from the
	// agent's own hardlink dir, which holds only what it has seen (in
	// the agent pod that dir is what /data/attachments is).
	ReadAliases map[string]string
	// Mounts maps a directory under /files/ (relative, slash-separated,
	// e.g. "artifacts/public") to the host directory served there,
	// read-only. It is core's stand-in for the agent pod's nested
	// read-only mounts: in the pod the kernel mounts public/<slug> at
	// /files/artifacts/public, and core, which reads body_path through a
	// Backend on the agent's own tree, has to find the same bytes at the
	// same model path. Nil in the pod, where the kernel does it.
	Mounts map[string]string
}

// mountFor returns the host path a Mounts entry serves rel (a cleaned,
// slash-separated path relative to /files/) at, or "" when rel lies
// under no mount.
func (b *Backend) mountFor(rel string) string {
	for at, host := range b.Mounts {
		if rel == at {
			return host
		}
		if rest, ok := strings.CutPrefix(rel, at+"/"); ok {
			return filepath.Join(host, filepath.FromSlash(rest))
		}
	}
	return ""
}

// PodReadRoots returns the ReadRoots for an agent's backend in its
// agent pod, where the file_* tools run in the dev-shell container:
// the read-only mounts the /files/ symlink farms resolve through (see
// agentpod's manifest). Nothing else outside /files/ is readable
// through them, whatever a link says.
//
// With no slug the per-agent chats root is left out rather than
// guessed: a dev-shell started without --agent serves the shared roots
// only (see cmd/dev-shell).
func PodReadRoots(slug string) []string {
	roots := []string{
		"/data/project_files",
		"/data/skills",
		"/data/attachments",
	}
	if slug == "" {
		return roots
	}
	return append(roots, filepath.Join("/data/agents", slug, "chats"))
}

// DefaultReadOnlyRoots lists subtrees where writes are rejected for
// the standard agent layout. Enforcement is on the path prefix, not
// the OS permission bits (so renames whose targets escape the
// writable area are also rejected).
//
// skills/ is read-only from the file_* tools' perspective — skills
// are managed through the Kivali UI, not by the agent itself (we
// don't want the agent silently rewriting its own procedures).
//
// artifacts/public/ and artifacts/shared/ are the published trees
// (see PublishedOwnDir): core is their only writer, through
// artifact_publish and artifact_unpublish.
var DefaultReadOnlyRoots = []string{"project", "past-chats", "episodes", "skills", "attachments", PublishedOwnDir, PublishedPeersDir}

// SharedWorkspaceDir is the agent's blackboard, its background
// workspace: one directory that the agent and every subagent working
// for it can all read AND write, reached at /files/background/ by all
// of them. Agents' plans and memories name the path, so it keeps its
// name.
//
// Deliberately absent from DefaultReadOnlyRoots. Every other shared
// subtree in the layout is a read-only mirror, which is correct when
// the reader is a leaf doing one lookup and wrong when several
// subagents are building parts of one deliverable and something has to
// assemble them.
//
// It is flat — no run ids, no lifecycle, nothing to start or finish.
// A run is delimited by what PlanFileName says, not by a directory,
// which means there is no allocation to get wrong, no collision to
// resolve, and exactly one place for the CEO to look. An agent that
// wants history moves the old plan aside itself.
const SharedWorkspaceDir = "background"

// PlanFileName is the conventional name of the plan inside
// SharedWorkspaceDir. A convention, not a schema: nothing parses it to
// decide what runs, and a malformed plan costs legibility, never
// execution. The UI reads it to show progress; agents read and rewrite
// it as work lands.
const PlanFileName = "plan.md"

// Resolve converts a model-supplied path ("/files/notes/x.md" or
// "notes/x.md") to an absolute host path rooted at b.Root, rejecting
// traversal (../), absolute roots other than /files, and paths
// that resolve outside of Root.
func (b *Backend) Resolve(modelPath string) (string, error) {
	p := strings.TrimSpace(modelPath)
	if p == "" {
		return "", errors.New("empty path")
	}
	// Accept both "/files/..." and bare "notes/..." forms.
	p = trimModelRoot(p)
	clean := filepath.Clean(p)
	if clean == "." {
		clean = ""
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes /files/")
	}
	if m := b.mountFor(filepath.ToSlash(clean)); m != "" {
		return m, nil
	}
	abs := filepath.Join(b.Root, clean)
	// Guard against a Clean that still managed to escape via symlink
	// trickery — abs must equal Root or be a proper subpath. Bare
	// HasPrefix is not enough: with Root="/a/memory", the path
	// "/a/memory-evil/x" would pass.
	if abs != b.Root && !strings.HasPrefix(abs, b.Root+string(filepath.Separator)) {
		return "", errors.New("path escapes /files/")
	}
	return abs, nil
}

// trimModelRoot strips the model-facing "/files" or "files" prefix
// (with or without leading slash) so the remainder is a plain
// host-relative subpath.
func trimModelRoot(p string) string {
	p = strings.TrimPrefix(p, ModelRootPath)
	p = strings.TrimPrefix(p, strings.TrimPrefix(ModelRootPath, "/"))
	p = strings.TrimPrefix(p, "/")
	return p
}

// unescapeShellPath relaxes a model-supplied path one step toward
// what the agent probably meant: strips a wrapping pair of matched
// quotes, replaces backslash-escaped spaces with literal spaces, and
// percent-decodes URL-style escapes. Returns the input unchanged when
// no escapes are present. Used as a fallback by resolveExisting when
// the literal path doesn't exist — agents trained on shell paths
// sometimes pre-escape spaces (`Foo\ Bar.png`) or wrap the path in
// quotes, and agents that see `/files/attachments/Foo Bar.png` in the
// prompt sometimes round-trip it as `/files/attachments/Foo%20Bar.png`
// because the path visually resembles a URL.
func unescapeShellPath(p string) string {
	if len(p) >= 2 {
		f, l := p[0], p[len(p)-1]
		if f == l && (f == '"' || f == '\'' || f == '`') {
			p = p[1 : len(p)-1]
		}
	}
	if strings.Contains(p, `\ `) {
		p = strings.ReplaceAll(p, `\ `, " ")
	}
	if strings.Contains(p, "%") {
		if dec, err := url.PathUnescape(p); err == nil {
			p = dec
		}
	}
	return p
}

// resolveExisting resolves modelPath, Lstats it, and returns the
// abs path + Lstat info. If the literal path does not exist, falls
// back once to unescapeShellPath(modelPath) — covering the common
// shell-escape mishabit. ENOENT on both attempts surfaces as
// ErrNotFound. Other Lstat errors (permission, etc.) surface
// unchanged.
//
// Used by every read-existing operation (View / StrReplace / Insert /
// Delete / Rename source / TryImageView). Write-new operations
// (Create, Rename destination) apply unescapeShellPath up-front
// instead, since "literal first" requires the file to already exist.
func (b *Backend) resolveExisting(modelPath string) (string, os.FileInfo, error) {
	abs, err := b.Resolve(modelPath)
	if err != nil {
		return "", nil, err
	}
	info, lerr := os.Lstat(abs)
	if lerr == nil {
		return abs, info, nil
	}
	if !errors.Is(lerr, fs.ErrNotExist) {
		return "", nil, lerr
	}
	alt := unescapeShellPath(modelPath)
	if alt == modelPath {
		return "", nil, ErrNotFound
	}
	altAbs, aerr := b.Resolve(alt)
	if aerr != nil {
		return "", nil, ErrNotFound
	}
	altInfo, alerr := os.Lstat(altAbs)
	if alerr != nil {
		return "", nil, ErrNotFound
	}
	return altAbs, altInfo, nil
}

// isReadOnly reports whether the given model path is under a
// protected subtree, per b.ReadOnlyRoots (or DefaultReadOnlyRoots
// when that's nil). The path may be a file or directory.
func (b *Backend) isReadOnly(modelPath string) bool {
	p := trimModelRoot(modelPath)
	roots := b.ReadOnlyRoots
	if roots == nil {
		roots = DefaultReadOnlyRoots
	}
	for _, ro := range roots {
		if p == ro || strings.HasPrefix(p, ro+"/") {
			return true
		}
	}
	return false
}

// readOnlyErr is nil when modelPath may be written, ErrPublishedReadOnly
// when it lies in a published tree, and ErrReadOnly otherwise.
func (b *Backend) readOnlyErr(modelPath string) error {
	if !b.isReadOnly(modelPath) {
		return nil
	}
	if isPublishedPath(trimModelRoot(modelPath)) {
		return ErrPublishedReadOnly
	}
	return ErrReadOnly
}

// isPublishedPath reports whether p, relative to /files/, lies in one
// of the published trees.
func isPublishedPath(p string) bool {
	for _, d := range PublishedMountPoints {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// ViewOptions controls how View renders a file. All fields are
// optional; the zero value means "return the whole file with a
// header preamble". Precedence when multiple fields are set:
// Grep > (Offset|Limit) > ViewRange > whole file.
type ViewOptions struct {
	// ViewRange slices by 1-indexed inclusive [start, end] (the
	// Anthropic memory tool's view_range field).
	ViewRange []int
	// Offset is the 1-indexed line to start reading from. 0 means
	// "from line 1". Mirrors Claude Code's Read tool.
	Offset int
	// Limit is the maximum number of lines to return starting at Offset.
	// 0 means "to end of file".
	Limit int
	// Grep, if non-empty, is a Go regexp; only matching lines are
	// returned, prefixed with their 1-indexed line number (like
	// `grep -n`). GrepContext lines of context are emitted before/after
	// each match (like `grep -C N`); separator "--" between non-adjacent
	// hunks.
	Grep        string
	GrepContext int
}

// View returns a directory listing (as a text block) or the contents
// of a file. For files, a one-line header is prepended summarising the
// file's line count and byte size, and (when sliced) the visible range
// or grep summary — so the agent can decide whether to widen or narrow
// the read without a separate `wc -l`-equivalent call.
func (b *Backend) View(modelPath string, opts ViewOptions) (string, error) {
	abs, _, err := b.resolveExisting(modelPath)
	if err != nil {
		return "", err
	}
	// Symlinks are followed — the project/ and past-chats/ subtrees are
	// entirely symlinks — but only as far as openRead allows.
	f, info, err := b.openRead(abs, modelPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if info.IsDir() {
		return listDir(f, modelPath)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return renderFile(string(data), opts)
}

// renderFile applies the ViewOptions to the raw file body and prepends
// a header preamble. Pulled out of View for unit-test clarity.
func renderFile(text string, opts ViewOptions) (string, error) {
	totalLines := lineCount(text)
	totalBytes := len(text)
	switch {
	case opts.Grep != "":
		body, matches, err := grepLines(text, opts.Grep, opts.GrepContext)
		if err != nil {
			return "", err
		}
		header := fmt.Sprintf("[file: %s lines, %s bytes — grep %q: %d match%s]\n",
			formatInt(totalLines), formatInt(totalBytes), opts.Grep, matches, plural(matches))
		return header + body, nil
	case opts.Offset > 0 || opts.Limit > 0:
		body, start, end := sliceOffsetLimit(text, opts.Offset, opts.Limit)
		header := rangeHeader(totalLines, totalBytes, start, end)
		return header + body, nil
	case opts.ViewRange != nil:
		body, err := sliceLines(text, opts.ViewRange)
		if err != nil {
			return "", err
		}
		start, end := clampRange(opts.ViewRange, totalLines)
		header := rangeHeader(totalLines, totalBytes, start, end)
		return header + body, nil
	default:
		header := fmt.Sprintf("[file: %s lines, %s bytes]\n", formatInt(totalLines), formatInt(totalBytes))
		return header + text, nil
	}
}

// listDir renders a sorted directory listing, annotating each entry
// with a type marker so the model can differentiate files vs dirs.
func listDir(dir *os.File, modelPath string) (string, error) {
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var b2 strings.Builder
	fmt.Fprintf(&b2, "Directory: %s\n", modelPath)
	for _, n := range names {
		fmt.Fprintf(&b2, "  %s\n", n)
	}
	return b2.String(), nil
}

// sliceLines returns a 1-indexed inclusive [start,end] range of lines
// from text. Out-of-range is clipped, not an error.
func sliceLines(text string, r []int) (string, error) {
	if len(r) != 2 {
		return "", errors.New("view_range must be [start, end]")
	}
	lines := strings.Split(text, "\n")
	start, end := r[0], r[1]
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return "", nil
	}
	return strings.Join(lines[start-1:end], "\n"), nil
}

// sliceOffsetLimit returns the slice starting at 1-indexed `offset`
// (treating 0 as 1) and running for `limit` lines (0 = to end). The
// returned start/end are 1-indexed inclusive and clipped to the
// available range — they're what the header should show.
func sliceOffsetLimit(text string, offset, limit int) (body string, start, end int) {
	lines := strings.Split(text, "\n")
	n := len(lines)
	if offset < 1 {
		offset = 1
	}
	if offset > n {
		return "", offset, offset - 1
	}
	end = n
	if limit > 0 {
		end = offset + limit - 1
		if end > n {
			end = n
		}
	}
	return strings.Join(lines[offset-1:end], "\n"), offset, end
}

// clampRange normalises a [start,end] view_range against the file's
// line count for the header line.
func clampRange(r []int, n int) (start, end int) {
	if len(r) != 2 {
		return 1, n
	}
	start, end = r[0], r[1]
	if start < 1 {
		start = 1
	}
	if end > n {
		end = n
	}
	if start > end {
		end = start - 1
	}
	return start, end
}

// grepLines filters `text` to lines matching the regex. ctx > 0 emits
// that many surrounding lines around each match; non-adjacent hunks
// are separated by a "--" line (matches `grep -C` shell behavior).
// Returns the rendered body, the number of matched lines, and any
// regex-compile error (a user-visible problem the model should see).
func grepLines(text, pattern string, ctx int) (body string, matches int, err error) {
	re, cerr := regexp.Compile(pattern)
	if cerr != nil {
		return "", 0, fmt.Errorf("invalid grep regex: %v", cerr)
	}
	if ctx < 0 {
		ctx = 0
	}
	lines := strings.Split(text, "\n")
	n := len(lines)
	include := make([]bool, n)
	for i, ln := range lines {
		if re.MatchString(ln) {
			matches++
			lo := i - ctx
			if lo < 0 {
				lo = 0
			}
			hi := i + ctx
			if hi >= n {
				hi = n - 1
			}
			for j := lo; j <= hi; j++ {
				include[j] = true
			}
		}
	}
	var bld strings.Builder
	prev := -2 // forces no leading "--"
	for i := 0; i < n; i++ {
		if !include[i] {
			continue
		}
		// Hunk separator is meaningful only when context > 0; with
		// ctx=0 every match is its own hunk and "--" between every
		// line is just noise (matches `grep -n` shell behavior).
		if ctx > 0 && prev >= 0 && i > prev+1 {
			bld.WriteString("--\n")
		}
		fmt.Fprintf(&bld, "%d:%s\n", i+1, lines[i])
		prev = i
	}
	return strings.TrimSuffix(bld.String(), "\n"), matches, nil
}

// lineCount returns the human-intuitive line count of text: number of
// newlines, or one more if the text doesn't end in a newline. Empty
// string is 0 lines. This matches `wc -l` for files ending in \n and
// the natural reading of "lines in this file" for files that don't.
func lineCount(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}

// formatInt prints an int with thousands separators ("18,224"). Keeps
// the header readable for multi-KB artifacts where raw digit runs
// are hard to scan.
func formatInt(n int) string {
	if n < 0 {
		return "-" + formatInt(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if len(s) > pre {
			b.WriteByte(',')
		}
	}
	for i := pre; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

// rangeHeader formats the per-read header for a sliced view.
func rangeHeader(totalLines, totalBytes, start, end int) string {
	if end < start {
		return fmt.Sprintf("[file: %s lines, %s bytes — showing %d-%d (empty range)]\n",
			formatInt(totalLines), formatInt(totalBytes), start, end)
	}
	return fmt.Sprintf("[file: %s lines, %s bytes — showing %d-%d]\n",
		formatInt(totalLines), formatInt(totalBytes), start, end)
}

// Create writes a file, rejecting writes under any RO subtree. Parent
// directories are created as needed.
func (b *Backend) Create(modelPath, body string) error {
	modelPath = unescapeShellPath(modelPath)
	if err := b.readOnlyErr(modelPath); err != nil {
		return err
	}
	abs, err := b.Resolve(modelPath)
	if err != nil {
		return err
	}
	return b.writeConfined(abs, modelPath, []byte(body))
}

// StrReplace does a single-occurrence substring replacement. The old
// string must occur exactly once; zero or multiple matches are
// errors (keeps edits precise and avoids silent over-replacement).
func (b *Backend) StrReplace(modelPath, oldStr, newStr string) error {
	if err := b.readOnlyErr(modelPath); err != nil {
		return err
	}
	abs, _, err := b.resolveExisting(modelPath)
	if err != nil {
		return err
	}
	data, err := b.readFile(abs, modelPath)
	if err != nil {
		return err
	}
	orig := string(data)
	count := strings.Count(orig, oldStr)
	if count == 0 {
		return errors.New("old_str not found")
	}
	if count > 1 {
		return fmt.Errorf("old_str matches %d times; must be unique", count)
	}
	updated := strings.Replace(orig, oldStr, newStr, 1)
	return b.writeConfined(abs, modelPath, []byte(updated))
}

// Insert inserts text after the given 1-indexed line (0 to prepend).
func (b *Backend) Insert(modelPath string, line int, text string) error {
	if err := b.readOnlyErr(modelPath); err != nil {
		return err
	}
	abs, _, err := b.resolveExisting(modelPath)
	if err != nil {
		return err
	}
	data, err := b.readFile(abs, modelPath)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	if line < 0 || line > len(lines) {
		return fmt.Errorf("insert_line out of range (have %d lines)", len(lines))
	}
	// Split the insertion on newlines so the model's \n-containing text
	// becomes multiple lines in the file rather than a literal "\n".
	newLines := strings.Split(text, "\n")
	before := append([]string{}, lines[:line]...)
	after := append([]string{}, lines[line:]...)
	out := append(before, newLines...)
	out = append(out, after...)
	updated := strings.Join(out, "\n")
	return b.writeConfined(abs, modelPath, []byte(updated))
}

// Delete removes a file in a writable subtree. Directories must be
// removed with a separate admin action; refusing here keeps the
// blast radius of a bad tool call bounded. A symlink is removed
// itself, never its target.
func (b *Backend) Delete(modelPath string) error {
	if err := b.readOnlyErr(modelPath); err != nil {
		return err
	}
	abs, _, err := b.resolveExisting(modelPath)
	if err != nil {
		return err
	}
	root, rel, err := b.openWrite(abs, modelPath)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return confinedErr(err, modelPath)
	}
	if info.IsDir() {
		return errors.New("refusing to delete a directory")
	}
	return confinedErr(root.Remove(rel), modelPath)
}

// Copy duplicates a file. Source can be any readable path (including
// the RO subtrees — that's the whole point: pulling a project file or
// attachment into a writable workspace); destination must sit under a
// writable subtree. Symlinks at the source are followed as far as a
// read may go (the copy is a real file, not a dangling link). Existing
// destinations are overwritten, matching Create's semantics.
func (b *Backend) Copy(srcPath, dstPath string) error {
	dstPath = unescapeShellPath(dstPath)
	if err := b.readOnlyErr(dstPath); err != nil {
		return err
	}
	srcAbs, _, err := b.resolveExisting(srcPath)
	if err != nil {
		return err
	}
	f, info, err := b.openRead(srcAbs, srcPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if info.IsDir() {
		return errors.New("refusing to copy a directory")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	dstAbs, err := b.Resolve(dstPath)
	if err != nil {
		return err
	}
	return b.writeConfined(dstAbs, dstPath, data)
}

// Rename moves a file. Both source and destination must be inside a
// writable subtree; cross-subtree renames are rejected. The entry is
// moved as it is: a symlink moves as a symlink.
func (b *Backend) Rename(oldPath, newPath string) error {
	newPath = unescapeShellPath(newPath)
	if err := b.readOnlyErr(oldPath); err != nil {
		return err
	}
	if err := b.readOnlyErr(newPath); err != nil {
		return err
	}
	srcAbs, _, err := b.resolveExisting(oldPath)
	if err != nil {
		return err
	}
	dstAbs, err := b.Resolve(newPath)
	if err != nil {
		return err
	}
	srcRoot, srcRel, err := b.openWrite(srcAbs, oldPath)
	if err != nil {
		return err
	}
	srcBase := srcRoot.Name()
	_ = srcRoot.Close()
	root, dstRel, err := b.openWrite(dstAbs, newPath)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	// The two relative paths name entries in one root only when both
	// really live under the same write root.
	if root.Name() != srcBase {
		return errors.New("cross-subtree renames are not allowed")
	}
	err = renameIn(root, srcRel, dstRel)
	if errors.Is(err, syscall.EXDEV) {
		// In the agent pod /files/artifacts/ is a mount of its own
		// (see agentpod's filesMounts), so a move between it and the
		// rest of /files/ crosses a mount, which rename(2) refuses.
		err = moveFileIn(root, srcRel, dstRel)
	}
	return confinedErr(err, newPath)
}

// moveFileIn moves the regular file oldRel to newRel, both inside
// root, by writing a copy and removing the original: the move rename(2)
// refuses across a mount. A directory or link is not moved this way.
func moveFileIn(root *os.Root, oldRel, newRel string) error {
	info, err := root.Lstat(oldRel)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("this moves a directory or link across a mount (/files/artifacts/ is one), which file_rename cannot do; use run_shell's mv")
	}
	f, err := root.OpenFile(oldRel, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return err
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		return err
	}
	if err := writeFileAtomicIn(root, newRel, data); err != nil {
		return err
	}
	return root.Remove(oldRel)
}
