package files

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BootstrapOptions wires the filesystem backend to a Kivali store.
// Paths are all absolute host paths.
type BootstrapOptions struct {
	// AgentRoot is data/agents/<slug>. The filesystem view lives at
	// AgentRoot/<StoragePrefix>/.
	AgentRoot string
	// ProjectFilesRoot is the absolute path to project_files/ so we
	// can symlink canonical text files into /files/project/.
	ProjectFilesRoot string
	// ChatsRoot is AgentRoot/chats/ (archived chat generations).
	ChatsRoot string
	// ProjectFiles is the current manifest of project files the CEO
	// has uploaded. Re-call Sync when this list changes.
	ProjectFiles []ProjectFileRef
	// DisabledSkills names skills the CEO has switched off. They are
	// omitted from /files/skills/ entirely rather than linked and
	// marked: an agent that can see a skill will read it, and a
	// disabled skill that still shows up is the CEO's switch quietly
	// not working.
	DisabledSkills map[string]bool

	// SkillsRoot is AgentRoot/skills/ (per-agent skill markdown files).
	// Files under this dir are mirrored into /files/skills/ so
	// agents read them via file_view.
	SkillsRoot string
	// Attachments is the current set of chat/inbox message
	// attachments the agent can reach via file_view. Each ref
	// points at a file under AttachmentsRoot/<sha>/... that holds
	// the viewable form — canonical text for text/PDF/office,
	// the original blob for images (file_view returns them as a
	// vision content block). Refs whose LinkTarget is empty are
	// skipped — opaque binaries aren't surfaced through file_view.
	Attachments []AttachmentRef
	// AttachmentsRoot is the absolute path to the shared attachment
	// blob store (data/attachments/).
	AttachmentsRoot string
	// DataDir and Slug, when both are set, name the agent's published
	// tree, data/public/<slug>, which Sync makes a real directory: the
	// agent pod mounts it (and data/public) by subPath.
	DataDir string
	Slug    string
}

// AttachmentRef names one attachment to mirror into
// /files/attachments/. LinkTarget is the filename inside the blob's
// SHA directory we'll symlink to (e.g. "canonical.txt" for text/PDF/
// office, "blob.png" for images so file_view can return them as a
// vision content block). Empty LinkTarget means "no viewable form" —
// the ref is skipped at sync time but the SHA stays reachable via the
// /attachments/<sha> download route.
//
// Link is the symlink name we expose under /files/attachments/
// (usually the original filename, collision-prefixed with short SHA).
type AttachmentRef struct {
	SHA        string
	Link       string
	LinkTarget string
}

// ProjectFileRef is a flattened view of store.ProjectFile sufficient
// for symlink bootstrapping — kept here to avoid the files package
// importing store (it stays a pure FS package).
type ProjectFileRef struct {
	SHA           string
	OriginalName  string
	CanonicalName string // relative to project_files/<sha>/
}

// StorageRoot returns the host path for an agent's filesystem root,
// data/agents/<slug>/memory. The agent-visible path is always "/files/"
// (see ModelRootPath), independent of the on-disk dir name.
func StorageRoot(agentRoot string) string {
	return filepath.Join(agentRoot, StoragePrefix)
}

// Sync ensures the per-agent /files/ directory structure exists
// and the read-only subtrees are populated with up-to-date symlinks.
// Safe to call repeatedly; stale symlinks are refreshed.
//
// The symlink approach keeps zero duplicate bytes: the real files
// live under project_files/, chats/, and other agents' artifacts/
// dirs, and the filesystem view is a thin overlay.
func Sync(opts BootstrapOptions) error {
	root := StorageRoot(opts.AgentRoot)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// Core's own subtrees first: each must be a real directory before
	// anything writes into it, or a link the agent left in its place
	// would carry core's writes wherever it points.
	for _, d := range CoreManagedDirs {
		if err := ensureRealDir(root, d); err != nil {
			return err
		}
	}
	if opts.DataDir != "" && opts.Slug != "" {
		if err := EnsurePublishedDir(opts.DataDir, opts.Slug); err != nil {
			return fmt.Errorf("published dir: %w", err)
		}
	}
	for _, d := range []string{
		"artifacts/private",
		// The agent's shared workspace with its own subagents. Created
		// here so it exists before anything is dispatched — an agent
		// should be able to write a plan into it without first having
		// spawned something to share it with.
		SharedWorkspaceDir,
	} {
		if err := ensureDir(root, d); err != nil {
			return err
		}
	}
	if err := syncProject(root, opts); err != nil {
		return fmt.Errorf("sync project/: %w", err)
	}
	if err := syncPastChats(root, opts); err != nil {
		return fmt.Errorf("sync past-chats/: %w", err)
	}
	if err := syncEpisodes(root, opts); err != nil {
		return fmt.Errorf("sync episodes/: %w", err)
	}
	if err := syncSkills(root, opts); err != nil {
		return fmt.Errorf("sync skills/: %w", err)
	}
	if err := syncAttachments(root, opts); err != nil {
		return fmt.Errorf("sync attachments/: %w", err)
	}
	return nil
}

// SyncAttachments reconciles only the /files/attachments/ symlink
// farm. For a caller that has to make one message's files reachable
// before the next full Sync — the same reconcile Sync runs for that
// subtree, so the two can never disagree about a name.
func SyncAttachments(opts BootstrapOptions) error {
	root := StorageRoot(opts.AgentRoot)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return syncAttachments(root, opts)
}

// CoreManagedDirs are the subtrees of an agent's /files/ that core
// populates and the agent only reads: the symlink farms, and the mount
// points of the published trees (PublishedMountPoints). The agent pod
// mounts each read-only (see agentpod's manifest) — a farm from the
// agent's own tree, a published tree from public/ — and Sync makes sure
// each is a real directory, never a link, before the pod is created or
// anything writes into it: a link the agent planted at a mount point
// would otherwise be resolved by the container runtime, inside the
// pod's filesystem, when it mounts there.
var CoreManagedDirs = append([]string{"project", "past-chats", "episodes", "skills", "attachments"}, PublishedMountPoints...)

// ensureRealDir makes every component of rel (slash-separated, under
// root) a real directory. A component that is a symlink or a file is
// removed — os.Remove never follows the last component, so it is the
// entry that goes, not what it points at — and a directory made in
// its place. For directories core owns (CoreManagedDirs and their
// parents): only the agent could have put something else there, and
// following it would send core's writes out of the agent's tree.
//
// The check and the writes that follow are separate path lookups, so
// a swap between them is possible where the agent can still write the
// parent. In the agent pod CoreManagedDirs are read-only mount points, which cannot be renamed or replaced, and
// so is every directory between them and /files/ (artifacts/, a RW
// mount), so none of the parents can be moved away with a nested mount
// inside it.
func ensureRealDir(root, rel string) error {
	cur := root
	for _, part := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err == nil && info.IsDir() {
			continue
		}
		if err == nil {
			if rerr := os.Remove(cur); rerr != nil {
				return rerr
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	return nil
}

// ensureDir makes rel under root when nothing is there, and otherwise
// leaves it alone: for directories the agent owns (artifacts/private,
// background/), whatever it made of them is its business. Its parent
// must already be real (Sync runs ensureRealDir on CoreManagedDirs
// first, which makes artifacts/ real), so the Mkdir cannot land
// anywhere but the agent's tree.
func ensureDir(root, rel string) error {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Lstat(p); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(p, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// readDirIn lists the directory rel inside root, sorted by name. The
// open goes through root, so a link swapped in for rel is followed
// only within root.
func readDirIn(root *os.Root, rel string) ([]fs.DirEntry, error) {
	d, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, err
}

// removeAllIn is os.RemoveAll inside root: a link is removed as an
// entry, never descended into, and nothing it does can leave root.
// (os.Root gained RemoveAll in Go 1.25; the module targets 1.24.)
func removeAllIn(root *os.Root, rel string) error {
	info, err := root.Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		entries, err := readDirIn(root, rel)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := removeAllIn(root, rel+"/"+e.Name()); err != nil {
				return err
			}
		}
	}
	if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// IsHardlinkMirrorPath reports whether rel — a DataDir-relative,
// forward-slash path — lives inside the per-agent attachment mirror,
// agents/<slug>/attachments/, which SyncAgentAttachmentLinks fills with
// hardlinks to the canonical files under the top-level attachments/.
// Backup walkers skip it so the bytes appear once in the archive (under
// the canonical), and the post-restore sync re-links it — the same
// shape symlinks already follow (skip at backup, regenerate on
// restore). Returns true for the mirror dir itself AND any path
// beneath it; lexically-aware (uses filepath.ToSlash for safety on
// platforms where filepath.Walk emits OS-native separators).
//
// We intentionally do NOT do inode-based dedup at backup time:
// filepath.Walk visits in lexical order, and "agents" < "attachments"
// so the per-agent mirror gets seen before the top-level canonical;
// dedupe-by-first-seen would emit the mirror as if it were the
// canonical and skip the real one, breaking restore.
func IsHardlinkMirrorPath(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) >= 3 && parts[0] == "agents" && parts[2] == "attachments"
}

// BackupExcludePrefixes lists DataDir-relative subtrees that backup
// walkers drop wholesale. Paths compared are forward-slash and
// filepath.Clean'd (see ShouldExcludeFromBackup).
//
//   - `claude-home` (entire subtree). Holds Claude CLI auth
//     (`.credentials.json`, `.claude.json` — OAuth tokens; shipping
//     them in a backup that gets rsynced offsite is a credential
//     leak), CLI-internal caches and telemetry, AND the CLI's
//     per-project session logs under `.claude/projects/`. With the
//     persistent-CLI architecture subprocesses come up fresh on
//     restore and load their first turn from Kivali's own chat.jsonl
//     archives, so the session logs carry no cross-restore value;
//     they grow unbounded (~85 MB on a moderately-used deployment),
//     so excluding the tree shrinks the backup ~5x with no functional
//     regression. The operator signs Claude in again on the restored side.
//   - `debug` — post-mortem dumps (last_req_<slug>.json) that rotate
//     every stream. Not useful after a restore.
var BackupExcludePrefixes = []string{
	"claude-home",
	"debug",
}

// ShouldExcludeFromBackup reports whether rel (a DataDir-relative
// path) should be dropped from a backup archive: it matches a
// BackupExcludePrefixes entry exactly OR lives beneath one, OR its
// basename carries the `.tmp-` prefix.
//
// The `.tmp-` check covers writeAtomic's rename-based writes
// (internal/store/store.go): a backup racing a write can snapshot the
// tempfile before it's renamed into place, landing orphan `.tmp-*`
// files in the archive that become garbage in DataDir on restore.
// Basename-based rather than directory-prefixed because writeAtomic
// runs from every subdir that stores state.
//
// Shared by both backup paths — the CLI tarball (backup.go, the
// k8s CronJob path) and the HTTP zip download (internal/web/backup.go)
// — so the two can't drift on what they ship.
func ShouldExcludeFromBackup(rel string) bool {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if strings.HasPrefix(filepath.Base(rel), ".tmp-") {
		return true
	}
	for _, p := range BackupExcludePrefixes {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// syncAttachments materializes /files/attachments/<link-name>
// symlinks for every AttachmentRef whose LinkTarget is set. Refs
// without a viewable form (LinkTarget == "") are skipped — those stay
// reachable via the /attachments/<sha> download route, just not
// through file_view. Images point at blob.<ext>; text/PDF/office
// point at canonical.txt (or the original for already-text files).
func syncAttachments(root string, opts BootstrapOptions) error {
	const dst = "attachments"
	if opts.AttachmentsRoot == "" || len(opts.Attachments) == 0 {
		return reconcileLinks(root, dst, nil)
	}
	want := map[string]string{}
	for _, a := range opts.Attachments {
		if a.LinkTarget == "" {
			continue
		}
		want[a.Link] = filepath.Join(opts.AttachmentsRoot, a.SHA, a.LinkTarget)
	}
	return reconcileLinks(root, dst, want)
}

// syncSkills mirrors every skill directory under SkillsRoot into
// /files/skills/<name>/ as a single symlink that points at the
// whole directory. Real ownership stays with the Kivali UI; agents
// read the symlinked tree read-only (the memory FS backend enforces
// that the skills subtree is not writable). Linking the dir rather
// than its contents keeps exec bits and auxiliary files (scripts,
// assets) reachable via file_view without per-file tracking.
func syncSkills(root string, opts BootstrapOptions) error {
	const dst = "skills"
	if opts.SkillsRoot == "" {
		return reconcileLinks(root, dst, nil)
	}
	entries, err := os.ReadDir(opts.SkillsRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return reconcileLinks(root, dst, nil)
		}
		return err
	}
	want := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip the internal staging/trash dirs the store writes during
		// atomic skill replacements. They start with "." and would
		// never be a valid skill name anyway.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		// A valid skill has SKILL.md at its root; skip partial
		// uploads / backup turds so agents never see a broken entry.
		manifest := filepath.Join(opts.SkillsRoot, e.Name(), "SKILL.md")
		if _, err := os.Stat(manifest); err != nil {
			continue
		}
		if opts.DisabledSkills[e.Name()] {
			continue
		}
		want[e.Name()] = filepath.Join(opts.SkillsRoot, e.Name())
	}
	return reconcileLinks(root, dst, want)
}

// syncProject populates /files/project/ with a symlink per file,
// keyed by the original filename. Links for files not in
// opts.ProjectFiles are removed.
func syncProject(root string, opts BootstrapOptions) error {
	const proj = "project"
	want := map[string]string{} // link-name → target-abs-path
	for _, f := range opts.ProjectFiles {
		if f.CanonicalName == "" {
			// No canonical text available (image / unknown). Skip —
			// we don't want agents opening a binary blob through
			// file_view expecting text.
			continue
		}
		// If the canonical is still a PDF (text extraction failed —
		// e.g. a malformed PDF), skip: file_view expects text and a
		// binary would confuse the model.
		low := strings.ToLower(f.CanonicalName)
		if strings.HasSuffix(low, ".pdf") {
			continue
		}
		target := filepath.Join(opts.ProjectFilesRoot, f.SHA, f.CanonicalName)
		// Prefer original_name for readability; fall back to SHA if a
		// collision ever shows up (first write wins).
		linkName := safeLinkName(f.OriginalName)
		if _, dup := want[linkName]; dup {
			linkName = f.SHA + "-" + linkName
		}
		want[linkName] = target
	}
	return reconcileLinks(root, proj, want)
}

// syncEpisodes mirrors each archived generation's digest into
// /files/episodes/<ts>.md. The digest lives beside its transcript at
// ChatsRoot/<ts>/episode.md, so the same RO mount that serves
// past-chats serves episodes; a generation the writer has not
// digested yet is simply absent until the next Sync.
func syncEpisodes(root string, opts BootstrapOptions) error {
	const dst = "episodes"
	if opts.ChatsRoot == "" {
		return reconcileLinks(root, dst, nil)
	}
	entries, err := os.ReadDir(opts.ChatsRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return reconcileLinks(root, dst, nil)
		}
		return err
	}
	want := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src := filepath.Join(opts.ChatsRoot, e.Name(), "episode.md")
		if _, err := os.Stat(src); err != nil {
			continue
		}
		want[e.Name()+".md"] = src
	}
	return reconcileLinks(root, dst, want)
}

// syncPastChats mirrors archived chat generations into
// /files/past-chats/<ts>.jsonl — one link per generation, named by
// the generation, pointing at ChatsRoot/<ts>/chat.jsonl. New chats
// appear automatically on the next Sync.
func syncPastChats(root string, opts BootstrapOptions) error {
	const dst = "past-chats"
	if opts.ChatsRoot == "" {
		return reconcileLinks(root, dst, nil)
	}
	entries, err := os.ReadDir(opts.ChatsRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return reconcileLinks(root, dst, nil)
		}
		return err
	}
	want := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src := filepath.Join(opts.ChatsRoot, e.Name(), "chat.jsonl")
		if _, err := os.Stat(src); err != nil {
			continue
		}
		want[e.Name()+".jsonl"] = src
	}
	return reconcileLinks(root, dst, want)
}

// reconcileLinks makes the contents of dir match the want map:
// create any missing symlinks, update any whose target changed, and
// remove any extra entries. Plain files or directories already in
// dir are left alone (but logged out-of-band would be nicer — for
// v1 we just skip them).
//
// rel names the directory under the agent's storage root. It is made
// a real directory first (ensureRealDir), and listing and removal go
// through an os.Root on it, so neither can reach outside it. Readlink
// and Symlink are by path — os.Root has them only from Go 1.25 — which
// is safe because the directory is core's: read-only mounted in the
// agent pod, so the agent cannot swap it for a link between the check
// and the write.
func reconcileLinks(storage, rel string, want map[string]string) error {
	if err := ensureRealDir(storage, rel); err != nil {
		return err
	}
	root, err := OpenDirNoFollow(storage, rel)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	dir := filepath.Join(storage, filepath.FromSlash(rel))
	entries, err := readDirIn(root, ".")
	if err != nil {
		return err
	}
	// Drop stale / divergent symlinks.
	for _, e := range entries {
		info, lerr := root.Lstat(e.Name())
		if lerr != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			// Leave non-symlinks alone.
			continue
		}
		target, rerr := os.Readlink(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		wanted, ok := want[e.Name()]
		if !ok || target != wanted {
			_ = root.Remove(e.Name())
		}
	}
	// Create / refresh.
	for name, target := range want {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			continue
		}
		link := filepath.Join(dir, name)
		if existing, err := os.Readlink(link); err == nil && existing == target {
			continue
		}
		_ = root.Remove(name)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}
	return nil
}

// AttachmentLinkName computes the /files/attachments/ filename
// for a given display name. Public so prompt-rendering and sync can
// agree byte-for-byte — otherwise the pointer Claude sees wouldn't
// match the symlink on disk.
//
// For simple filenames ("report.pdf") this is a no-op. Slashes become
// underscores so a malicious name can't create subdirectories.
//
// This is sanitization ONLY — it knows nothing about collisions.
// When two distinct SHAs arrive under one filename, the store writes
// the second with a short-SHA prefix, and that prefixed name is the
// only one that resolves to its bytes. So anything that shows an
// agent a path must ask the store what the file was actually written
// as (store.AttachmentLinkNames) and use this function only as the
// fallback for a SHA the store can't place.
func AttachmentLinkName(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	if name == "" {
		return "attachment"
	}
	return name
}

// safeLinkName strips directory separators and any weird bytes so a
// user-supplied filename can't break out of the link directory. We
// keep dots and dashes; everything else becomes underscore.
func safeLinkName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r == '/' || r == '\\':
			continue
		case r == '.' || r == '-' || r == '_':
			out = append(out, r)
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "unnamed"
	}
	return string(out)
}
