package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kivali-ai/kivali/internal/files"
)

// SyncAgentFilesystem reconciles every core-managed farm under one
// agent's /files/: project/, past-chats/, episodes/, skills/ and
// attachments/ (with the per-agent attachment hardlinks), and makes
// core's own directories real. Idempotent.
//
// Nothing runs it per turn. Each farm is kept current by the edge that
// changes what it mirrors:
//   - project/: upload, delete (web afterProjectFilesChanged →
//     SyncAllFilesystems)
//   - skills/: install, delete, enable, disable (web
//     syncSkillsAfterChange → SyncAllFilesystems)
//   - past-chats/: rotation (web finalizeRotation, handoff.go)
//   - episodes/: a digest written (web linkEpisodes)
//   - attachments/: a row with attachments landing
//     (AppendChatMessage), or one folded into a running turn ahead of
//     its row (SyncAgentAttachments)
//   - everything, for one agent: hire (agent provisioning), its pod
//     being created (BulkProvisionAgentPods)
//   - everything, for every agent: boot (main.go) and restore, the
//     backstop for anything an edge missed
//
// An upload is canonicalised before its edge runs, so the project/
// farm links each file's canonical form as the index records it.
func (s *FSStore) SyncAgentFilesystem(slug string) error {
	pfs, err := s.ListProjectFiles()
	if err != nil {
		return err
	}
	refs := make([]files.ProjectFileRef, 0, len(pfs))
	for _, f := range pfs {
		refs = append(refs, files.ProjectFileRef{
			SHA:           f.SHA,
			OriginalName:  f.OriginalName,
			CanonicalName: f.CanonicalName,
		})
	}
	atts, err := s.collectAttachmentRefs(slug)
	if err != nil {
		return err
	}
	// The agent pod mounts chats/ by subPath (its past-chats/ farm
	// resolves through it), and a new hire has none until its first
	// rotation. Kubelet would make the missing directory as root, and
	// that rotation could then not write its archive.
	if err := os.MkdirAll(s.path("agents", slug, "chats"), 0o755); err != nil {
		return err
	}
	if err := files.Sync(files.BootstrapOptions{
		AgentRoot:        s.path("agents", slug),
		ProjectFilesRoot: s.path("project_files"),
		ChatsRoot:        s.path("agents", slug, "chats"),
		SkillsRoot:       s.path("skills"), // app-wide skills library
		DisabledSkills:   s.DisabledSkills(),
		AttachmentsRoot:  s.path("attachments"),
		ProjectFiles:     refs,
		Attachments:      atts,
		// The agent pod mounts data/public/<slug> and data/public.
		DataDir: s.root,
		Slug:    slug,
	}); err != nil {
		return err
	}
	return s.SyncAgentAttachmentLinks(slug)
}

// SyncAgentAttachmentLinks reconciles the per-agent attachment hardlink
// dir at data/agents/<slug>/attachments/<sha>/ with the access set
// derived from the agent's chat history. For every SHA the agent has
// seen, we ensure agents/<slug>/attachments/<sha>/<file> hardlinks
// exist for every file in the canonical attachments/<sha>/ dir. SHAs
// no longer in the access set are removed.
//
// This is the access-control primitive for the unified-files agent
// pod's RO mount at /data/attachments/ (see docs/developers/files-and-publishing.md
// §"Per-agent attachments isolation"). Each agent pod will mount only
// its own subdir there, so a `ls /data/attachments/` from inside agent
// A can never enumerate B's SHAs.
//
// Hardlinks share inodes with the canonical files — cost is one
// inode entry per (agent, sha, file), no extra bytes. Idempotent:
// safe to call repeatedly. Walks the canonical SHA dir at apply time
// so a canonical.txt added later shows up
// on the next sync.
func (s *FSStore) SyncAgentAttachmentLinks(slug string) error {
	if slug == "" {
		return errors.New("SyncAgentAttachmentLinks: slug is required")
	}
	want, err := s.AttachmentSHAsForAgent(slug)
	if err != nil {
		return err
	}
	dst := s.path("agents", slug, "attachments")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !want[e.Name()] {
			if rerr := os.RemoveAll(filepath.Join(dst, e.Name())); rerr != nil {
				return rerr
			}
		}
	}
	canonicalRoot := s.path("attachments")
	for sha := range want {
		srcDir := filepath.Join(canonicalRoot, sha)
		srcInfo, err := os.Stat(srcDir)
		if err != nil {
			// Canonical missing (deleted, or never landed). Skip — the
			// reference is stale and the per-agent dir stays empty for
			// this SHA so the mount sees no files.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if !srcInfo.IsDir() {
			continue
		}
		// Per-SHA lock so we don't race with AddAttachment writing the
		// canonical mid-walk and producing partial hardlinks.
		s.LockSHA(sha)
		err = mirrorAttachmentSHADir(srcDir, filepath.Join(dst, sha))
		s.UnlockSHA(sha)
		if err != nil {
			return err
		}
	}
	return nil
}

// SyncAgentAttachments makes the files on one message reachable under
// /files/attachments/ before that message is in chat.jsonl, and
// returns sha → the name each one was written under.
//
// The farm is derived from chat.jsonl, and AppendChatMessage re-derives
// it whenever a row with attachments lands. A message that arrives
// while a turn is running is buffered off disk and folded into that
// turn as text — so nothing on disk knows about its files until the
// row lands, after the turn. Any /files/attachments/ path the model is
// given, or infers, for such a file does not resolve until the next
// turn: not for its own file_view, and not for a subagent handed the
// path, which reports the file absent as if that were a finding.
//
// This is the attachments slice of SyncAgentFilesystem — the symlink
// farm and the per-agent hardlink mirror — run with the message's
// attachments held as pending. The walk visits them where the flush
// will put the message, at the end of the live chat, so the names
// written here are the names every later sync keeps; and because
// they stay pending until the message lands, a sync that runs in
// between (another row's attachments landing, a rotation, a restore)
// keeps the links rather than reconciling them away.
func (s *FSStore) SyncAgentAttachments(slug string, atts []MessageAttachment) (map[string]string, error) {
	if slug == "" {
		return nil, errors.New("SyncAgentAttachments: slug is required")
	}
	s.holdPendingAttachments(slug, atts)
	links, err := s.resolveAttachmentLinks(slug)
	if err != nil {
		return nil, err
	}
	if err := files.SyncAttachments(files.BootstrapOptions{
		AgentRoot:       s.path("agents", slug),
		AttachmentsRoot: s.path("attachments"),
		Attachments:     attachmentRefsFor(links),
	}); err != nil {
		return nil, err
	}
	if err := s.SyncAgentAttachmentLinks(slug); err != nil {
		return nil, err
	}
	return linkNamesBySHA(links), nil
}

// holdPendingAttachments records attachments linked ahead of the
// message carrying them, until releasePendingAttachments sees that
// message appended. One entry per SHA.
func (s *FSStore) holdPendingAttachments(slug string, atts []MessageAttachment) {
	if len(atts) == 0 {
		return
	}
	s.pendingAttachmentsMu.Lock()
	defer s.pendingAttachmentsMu.Unlock()
	if s.pendingAttachments == nil {
		s.pendingAttachments = map[string][]MessageAttachment{}
	}
	held := s.pendingAttachments[slug]
	for _, a := range atts {
		if a.SHA == "" {
			continue
		}
		dup := false
		for _, h := range held {
			if h.SHA == a.SHA {
				dup = true
				break
			}
		}
		if !dup {
			held = append(held, a)
		}
	}
	s.pendingAttachments[slug] = held
}

// pendingAttachmentsFor is a snapshot of what holdPendingAttachments
// has recorded for slug and releasePendingAttachments has not yet
// dropped. Nil for the common case of nothing pending.
func (s *FSStore) pendingAttachmentsFor(slug string) []MessageAttachment {
	s.pendingAttachmentsMu.Lock()
	defer s.pendingAttachmentsMu.Unlock()
	held := s.pendingAttachments[slug]
	if len(held) == 0 {
		return nil
	}
	return append([]MessageAttachment(nil), held...)
}

// releasePendingAttachments drops the given SHAs from slug's pending
// set: their message is on disk now, and the walk finds them there.
func (s *FSStore) releasePendingAttachments(slug string, atts []MessageAttachment) {
	if len(atts) == 0 {
		return
	}
	s.pendingAttachmentsMu.Lock()
	defer s.pendingAttachmentsMu.Unlock()
	held := s.pendingAttachments[slug]
	if len(held) == 0 {
		return
	}
	landed := map[string]bool{}
	for _, a := range atts {
		landed[a.SHA] = true
	}
	kept := held[:0]
	for _, h := range held {
		if !landed[h.SHA] {
			kept = append(kept, h)
		}
	}
	if len(kept) == 0 {
		delete(s.pendingAttachments, slug)
		return
	}
	s.pendingAttachments[slug] = kept
}

// mirrorAttachmentSHADir reconciles dst's contents against src using
// hardlinks. Files in src missing from dst are linked; dst entries
// missing from src or pointing at a different inode are dropped and
// re-linked. Same shape as the symlink reconcile in files.Sync.
func mirrorAttachmentSHADir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	srcEntries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, e := range srcEntries {
		if e.IsDir() {
			// Today's attachment SHA dirs are flat (blob.X, canonical.X,
			// meta.json). If a future canonicalizer adds a subdir, log-
			// and-skip rather than recurse — a directory hardlink would
			// be a privilege escalation surface (POSIX disallows them
			// for non-root anyway).
			continue
		}
		want[e.Name()] = true
	}
	dstEntries, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, e := range dstEntries {
		dstPath := filepath.Join(dst, e.Name())
		if !want[e.Name()] {
			if rerr := os.Remove(dstPath); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				return rerr
			}
			continue
		}
		// Drop and relink if the dst entry doesn't share an inode with
		// the canonical file (paranoia — covers hand-edits + accidental
		// non-hardlink writes).
		srcStat, err := os.Stat(filepath.Join(src, e.Name()))
		if err != nil {
			return err
		}
		dstStat, err := os.Stat(dstPath)
		if err != nil {
			return err
		}
		if !os.SameFile(srcStat, dstStat) {
			if rerr := os.Remove(dstPath); rerr != nil {
				return rerr
			}
		}
	}
	for name := range want {
		dstPath := filepath.Join(dst, name)
		if _, err := os.Lstat(dstPath); err == nil {
			continue
		}
		if err := os.Link(filepath.Join(src, name), dstPath); err != nil {
			return err
		}
	}
	return nil
}

// walkAllChatAttachments visits every MessageAttachment referenced
// from the agent's chat history. Iteration order is load-bearing:
// resolveAttachmentLinks lets the first reference to see a given
// filename keep it, so this order decides which of two same-named
// blobs holds the clean name. It is chronological within the
// live chat, so the EARLIEST delivery of a given filename keeps it
// and later distinct SHAs take the short-SHA prefix — a name→SHA
// binding, once made, never changes bytes underneath an agent that
// wrote the path down. Order is:
//
//  1. live chat.jsonl, re-read each call, in append (oldest-first) order
//  2. archived generations, newest archive first, append order within each
//
// The live chat is intentionally NOT cached — it mutates on every
// message append and must be re-walked anyway for sync to see
// in-progress attachments. The archive side IS cached, in the
// per-agent attachment_index (ensureArchiveAttachmentIndex), so
// steady-state cost is one small index read instead of N archive
// scans. Empty archives are sentinel-marked there so they don't
// re-scan either.
//
// Attachments linked ahead of their message (SyncAgentAttachments) are
// visited after the live chat and before the archives: that message
// will be appended at the end of the live chat, so this is where the
// walk will find them once it lands, and every name resolved here
// holds across the landing.
//
// visit returns false to stop early. A missing live chat.jsonl is
// treated as empty.
func (s *FSStore) walkAllChatAttachments(slug string, visit func(MessageAttachment) bool) error {
	hist, err := s.ReadChatHistory(slug)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	for _, m := range hist {
		for _, a := range m.Attachments {
			if !visit(a) {
				return nil
			}
		}
	}
	for _, a := range s.pendingAttachmentsFor(slug) {
		if !visit(a) {
			return nil
		}
	}
	entries, err := s.ensureArchiveAttachmentIndex(slug)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !visit(MessageAttachment{SHA: e.SHA, Name: e.Name}) {
			return nil
		}
	}
	return nil
}

// attachmentLink is one resolved attachment in an agent's chat
// history: the blob, the targets worth symlinking, and the final
// link name under /files/attachments/ after collision handling.
type attachmentLink struct {
	SHA       string
	Name      string // final base link name (collision-suffixed when needed)
	Att       Attachment
	BinTarget string
	TxtTarget string
}

// resolveAttachmentLinks is the single source of truth for what an
// agent's /files/attachments/ directory is named. Every caller that
// has to agree byte-for-byte goes through it: the symlink reconciler
// (collectAttachmentRefs), the reverse lookup
// (ResolveAgentAttachmentByLinkName) and the prompt-side header
// (AttachmentLinkNames). A header that printed the raw filename would
// quote, for a re-sent name, a path that resolves to the FIRST blob
// while the new bytes sit beside it under a SHA prefix. Both files
// parse cleanly, so the wrong one would open silently.
//
// Why walk archives too: a chat rotation moves the live chat.jsonl
// into chats/<ts>/, leaving a fresh empty file. If we only looked at
// the live chat, /files/attachments/ would clear on every rotation
// (P05) — violating the handbook's /files/ durability model.
//
// Names collide on purpose: we want stable "nice" filenames in
// /files/attachments/. Iteration order is live chat (oldest entry
// first) → newest archive → … → oldest archive, and the first-seen
// reference for a name keeps it. That makes a name→SHA binding
// PERMANENT: an attachment path an agent recorded in memory never
// silently changes bytes underneath it. A later distinct SHA with
// the same filename gets the base name suffixed with its short SHA
// (and its .txt sidecar inherits the same prefix) so both stay
// addressable — and the delivery header quotes whichever of the two
// that message actually carried.
func (s *FSStore) resolveAttachmentLinks(slug string) ([]attachmentLink, error) {
	seen := map[string]bool{}        // sha → already resolved
	nameTaken := map[string]string{} // link name → sha
	var out []attachmentLink
	err := s.walkAllChatAttachments(slug, func(a MessageAttachment) bool {
		if a.SHA == "" || seen[a.SHA] {
			return true
		}
		att, err := s.GetAttachment(a.SHA)
		if err != nil {
			return true
		}
		binTarget, txtTarget := attachmentLinkTargets(att)
		if binTarget == "" && txtTarget == "" {
			return true
		}
		name := attachmentLinkName(a.Name, att.Name)
		if existing, taken := nameTaken[name]; taken && existing != a.SHA {
			name = shortSHA(a.SHA) + "-" + name
		}
		seen[a.SHA] = true
		nameTaken[name] = a.SHA
		out = append(out, attachmentLink{
			SHA:       a.SHA,
			Name:      name,
			Att:       att,
			BinTarget: binTarget,
			TxtTarget: txtTarget,
		})
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AttachmentLinkNames returns sha → the base filename that SHA is
// actually reachable under in /files/attachments/ for this agent.
// The prompt-rendering side uses it so the "--- attached files ---"
// header on a delivery quotes the disambiguated name that was
// written to disk rather than the original filename. A SHA absent
// from the map has no viewable form (opaque binary with no canonical)
// or isn't in this agent's chat history.
func (s *FSStore) AttachmentLinkNames(slug string) (map[string]string, error) {
	links, err := s.resolveAttachmentLinks(slug)
	if err != nil {
		return nil, err
	}
	return linkNamesBySHA(links), nil
}

func linkNamesBySHA(links []attachmentLink) map[string]string {
	out := make(map[string]string, len(links))
	for _, l := range links {
		out[l.SHA] = l.Name
	}
	return out
}

// collectAttachmentRefs turns the resolved link set into the symlink
// farm under /files/attachments/. A binary attachment with a separate
// text canonical (PDF, office, zip) produces TWO refs: the
// original-named link to the blob, and a .txt sidecar pointing at
// canonical.txt. Text and image attachments produce a single ref.
// Opaque binaries (no canonical of any kind) still get a named ref so
// the agent sees the file exists; file_view of those returns raw
// bytes — that's the agent's job to handle.
func (s *FSStore) collectAttachmentRefs(slug string) ([]files.AttachmentRef, error) {
	links, err := s.resolveAttachmentLinks(slug)
	if err != nil {
		return nil, err
	}
	return attachmentRefsFor(links), nil
}

// attachmentRefsFor is collectAttachmentRefs after the walk: the
// symlink set for an already-resolved link list.
func attachmentRefsFor(links []attachmentLink) []files.AttachmentRef {
	var out []files.AttachmentRef
	for _, l := range links {
		if l.BinTarget != "" {
			out = append(out, files.AttachmentRef{
				SHA:        l.SHA,
				Link:       l.Name,
				LinkTarget: l.BinTarget,
			})
		}
		if l.TxtTarget != "" {
			out = append(out, files.AttachmentRef{
				SHA:        l.SHA,
				Link:       l.Name + ".txt",
				LinkTarget: l.TxtTarget,
			})
		}
	}
	return out
}

// attachmentLinkTargets picks which files inside attachments/<sha>/
// should be exposed as symlinks under /files/attachments/.
//
//   - binTarget: the original blob (e.g. "blob.pdf"). Always set when
//     the upload succeeded — the agent should see the file by its
//     original name. file_view of an opaque binary returns raw bytes,
//     which is the agent's problem; the message-body annotation is
//     what tells it whether to read the binary or the .txt sidecar.
//   - txtTarget: the canonical text file when it's a SEPARATE file
//     from the blob (PDFs, office docs, zip listings). Empty when
//     the canonical IS the blob (text uploads) or when no canonical
//     exists (images, opaque binaries).
//
// Returns "", "" only when the attachment has no recoverable form
// at all — in practice that doesn't happen for successful uploads.
func attachmentLinkTargets(att Attachment) (binTarget, txtTarget string) {
	blob := "blob" + att.Ext
	binTarget = blob
	if att.CanonicalName != "" && att.CanonicalName != blob {
		txtTarget = att.CanonicalName
	}
	return
}

// attachmentLinkName picks the display name for a symlink under
// /files/attachments/. Prefers the per-reference alias (what the
// message chose to call it), falls back to the blob's first-upload
// name. Delegates sanitization to files.AttachmentLinkName so the
// prompt-rendering side can call the same function. Locally named
// so it doesn't shadow the package-level files.AttachmentLinkName.
func attachmentLinkName(alias, fallback string) string {
	pick := alias
	if strings.TrimSpace(pick) == "" {
		pick = fallback
	}
	return files.AttachmentLinkName(pick)
}

// shortSHA returns the first 8 chars of sha for collision suffixes.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// AttachmentSHAsForAgent returns the set of attachment SHAs the agent
// has seen in its chat history — covering both inbox deliveries and
// any outbound message bubble, across the live chat AND every
// archived chat generation. Used as the access-control gate for
// run_shell.inputs (an agent may stage an attachment only if it
// already appears in this set) and as the per-agent hardlink
// reconcile set (SyncAgentAttachmentLinks). Returns an empty (not
// nil) map when the agent has no chat history yet.
//
// Why archives are included: the handbook's model treats /files/
// as durable storage. A chat rotation must not revoke an agent's
// access to attachments it has already received — see P05.
func (s *FSStore) AttachmentSHAsForAgent(slug string) (map[string]bool, error) {
	out := map[string]bool{}
	err := s.walkAllChatAttachments(slug, func(a MessageAttachment) bool {
		if a.SHA != "" {
			out[a.SHA] = true
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveAgentAttachmentByLinkName looks up an attachment the agent
// has access to by the link name used under /files/attachments/.
// Shares resolveAttachmentLinks with the symlink reconciler, so a
// name the agent saw in a file_view listing (or read off a delivery
// header, or remembered from a prior chat) resolves back to the same
// SHA by construction rather than by two copies of the rules
// happening to agree. Both the original-named link (e.g.
// "report.pdf") and the .txt sidecar ("report.pdf.txt") resolve to
// the same Attachment record. Returns ErrNotFound when no attachment
// in the agent's chat history matches.
func (s *FSStore) ResolveAgentAttachmentByLinkName(slug, name string) (Attachment, error) {
	links, err := s.resolveAttachmentLinks(slug)
	if err != nil {
		return Attachment{}, err
	}
	for _, l := range links {
		if name == l.Name || (l.TxtTarget != "" && name == l.Name+".txt") {
			return l.Att, nil
		}
	}
	return Attachment{}, ErrNotFound
}

// ResolveProjectFileByLinkName looks up a project file by the link
// name used under /files/project/. Mirrors syncProject's
// safeLinkName logic so the path the agent saw in list_project_files
// or file_view resolves back to the same SHA.
func (s *FSStore) ResolveProjectFileByLinkName(name string) (ProjectFile, error) {
	pfs, err := s.ListProjectFiles()
	if err != nil {
		return ProjectFile{}, err
	}
	links := projectFileLinkNames(pfs)
	for _, f := range pfs {
		if links[f.SHA] == name {
			return f, nil
		}
	}
	return ProjectFile{}, ErrNotFound
}

// projectFileLinkNames maps every upload's SHA to the name it is
// linked under in /files/project/, applying the same first-write-wins
// collision rule as files.syncProject: a second distinct file with the
// same sanitised name gets a short-SHA prefix. pfs must be in upload
// order (ListProjectFiles). The graph's project nodes and the link
// resolver both read this so an agent's path and the graph's Path are
// the same string by construction.
func projectFileLinkNames(pfs []ProjectFile) map[string]string {
	out := make(map[string]string, len(pfs))
	taken := map[string]string{} // link name → sha
	for _, f := range pfs {
		link := projectFileLinkName(f.OriginalName)
		if existing, dup := taken[link]; dup && existing != f.SHA {
			link = f.SHA + "-" + link
		}
		taken[link] = f.SHA
		out[f.SHA] = link
	}
	return out
}

// projectFileLinkName mirrors files.safeLinkName so the store can
// resolve an agent-supplied /files/project/<name> path without the
// files package being importable here. Kept in lock-step with the
// safeLinkName logic in internal/files — change one and change the
// other.
func projectFileLinkName(s string) string {
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

// SyncAllFilesystems runs SyncAgentFilesystem for every active (non-CEO) agent.
// Used when a shared resource changes (project-file upload / delete)
// so every agent's memory view picks up the new state.
func (s *FSStore) SyncAllFilesystems() error {
	agents, err := s.ListActiveAgents()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.Slug == "ceo" {
			continue
		}
		if err := s.SyncAgentFilesystem(a.Slug); err != nil {
			return err
		}
	}
	return nil
}
