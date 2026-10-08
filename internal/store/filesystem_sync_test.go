package store

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustDecodeBase64(t *testing.T, s string) []byte {
	t.Helper()
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	return out
}

// minimalPNG is a 67-byte 1×1 transparent PNG. Same fixture pattern
// as internal/mcp/server_test.go's image tests.
const minimalPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNgYAAAAAMAASsJTYQAAAAASUVORK5CYII="

// TestAttachmentLinkTargetsTextEmitsBinaryOnly — a text upload has
// canonical == blob, so we expose only the original-named link
// (no redundant .txt sidecar).
func TestAttachmentLinkTargetsTextEmitsBinaryOnly(t *testing.T) {
	att := Attachment{
		MIME:          "text/markdown",
		Ext:           ".md",
		CanonicalName: "blob.md",
	}
	bin, txt := attachmentLinkTargets(att)
	if bin != "blob.md" || txt != "" {
		t.Errorf("text targets = (%q, %q), want (blob.md, \"\")", bin, txt)
	}
}

// TestAttachmentLinkTargetsImageEmitsBinaryOnly — images have no
// text canonical; file_view returns them as a vision content block
// from the original blob.
func TestAttachmentLinkTargetsImageEmitsBinaryOnly(t *testing.T) {
	cases := []struct {
		mime string
		ext  string
		want string
	}{
		{"image/png", ".png", "blob.png"},
		{"image/jpeg", ".jpg", "blob.jpg"},
		{"image/gif", ".gif", "blob.gif"},
		{"image/webp", ".webp", "blob.webp"},
	}
	for _, c := range cases {
		t.Run(c.mime, func(t *testing.T) {
			att := Attachment{MIME: c.mime, Ext: c.ext}
			bin, txt := attachmentLinkTargets(att)
			if bin != c.want || txt != "" {
				t.Errorf("%s: targets = (%q, %q), want (%q, \"\")", c.mime, bin, txt, c.want)
			}
		})
	}
}

// TestAttachmentLinkTargetsPDFEmitsBoth — PDFs (and office, zip)
// have a separate text canonical, so we expose both the original
// blob (so the agent sees the file by its real name) AND the
// canonical.txt sidecar (so file_view returns extracted text).
func TestAttachmentLinkTargetsPDFEmitsBoth(t *testing.T) {
	att := Attachment{
		MIME:          "application/pdf",
		Ext:           ".pdf",
		CanonicalName: "canonical.txt",
	}
	bin, txt := attachmentLinkTargets(att)
	if bin != "blob.pdf" || txt != "canonical.txt" {
		t.Errorf("PDF targets = (%q, %q), want (blob.pdf, canonical.txt)", bin, txt)
	}
}

// TestAttachmentLinkTargetsOpaqueBinaryEmitsBinaryOnly — an opaque
// binary upload (no canonical produced) still gets its original-named
// link so the agent sees the file in the listing. file_view of it
// returns raw bytes; the agent uses the message-body annotation to
// decide whether to look at it directly or skip.
func TestAttachmentLinkTargetsOpaqueBinaryEmitsBinaryOnly(t *testing.T) {
	att := Attachment{MIME: "application/octet-stream", Ext: ".bin"}
	bin, txt := attachmentLinkTargets(att)
	if bin != "blob.bin" || txt != "" {
		t.Errorf("opaque targets = (%q, %q), want (blob.bin, \"\")", bin, txt)
	}
}

// TestSyncAgentFilesystemImageAttachmentSymlinks is the end-to-end
// proof for Gap 3: an image attachment referenced from chat.jsonl
// resolves to a symlink at /files/attachments/<name>, and the symlink
// points at the original blob (so file_view's image-vision path
// reaches real bytes).
func TestSyncAgentFilesystemImageAttachmentSymlinks(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	pngBytes := mustDecodeBase64(t, minimalPNGBase64)
	att, err := s.AddAttachment(context.Background(), "shot.png", strings.NewReader(string(pngBytes)))
	if err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}
	if att.CanonicalName != "" {
		t.Fatalf("image canonical should be empty, got %q", att.CanonicalName)
	}
	// Anchor the SHA in alice's chat history so collectAttachmentRefs
	// picks it up — same shape the new MCP shell handler writes.
	if err := s.AppendChatMessage("alice", ChatMessage{
		Role:        RoleSent,
		Kind:        "shell_captured",
		Attachments: []MessageAttachment{{SHA: att.SHA, Name: "shot.png"}},
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem: %v", err)
	}

	link := filepath.Join(s.path("agents", "alice", "memory", "attachments"), "shot.png")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("Readlink %s: %v", link, err)
	}
	wantSuffix := filepath.Join("attachments", att.SHA, "blob.png")
	if !strings.HasSuffix(target, wantSuffix) {
		t.Errorf("symlink target = %q, want suffix %q", target, wantSuffix)
	}
	// And the symlink resolves to bytes-equal-to-original.
	resolved, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("ReadFile resolved: %v", err)
	}
	if string(resolved) != string(pngBytes) {
		t.Errorf("resolved bytes mismatch: got %d, want %d", len(resolved), len(pngBytes))
	}
}

// TestSyncAgentFilesystemPDFEmitsBothLinks proves a binary attachment
// with a separate text canonical (PDF, office, zip listing) appears
// under /files/attachments/ as TWO symlinks: the original-named link
// pointing at the binary blob, and a .txt sidecar pointing at
// canonical.txt. Lets the agent see the file by its real name AND
// read the extracted text via file_view of the sidecar.
func TestSyncAgentFilesystemPDFEmitsBothLinks(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// Synthetic PDF: minimal header bytes are enough — pdftotext may
	// not be available on the test host. We seed canonical.txt
	// manually via SetAttachmentCanonicalText to simulate a successful
	// extraction.
	pdfBytes := []byte("%PDF-1.4\n% fake content\n")
	att, err := s.AddAttachment(context.Background(), "report.pdf", strings.NewReader(string(pdfBytes)))
	if err != nil && !strings.Contains(err.Error(), "pdftotext") {
		t.Fatalf("AddAttachment: %v", err)
	}
	if err := s.SetAttachmentCanonicalText(att.SHA, "extracted page 1\n"); err != nil {
		t.Fatalf("SetAttachmentCanonicalText: %v", err)
	}
	if err := s.AppendChatMessage("alice", ChatMessage{
		Role:        RoleReceived,
		Kind:        "direct_chat",
		Attachments: []MessageAttachment{{SHA: att.SHA, Name: "report.pdf"}},
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem: %v", err)
	}

	attachmentsDir := s.path("agents", "alice", "memory", "attachments")
	binLink := filepath.Join(attachmentsDir, "report.pdf")
	txtLink := filepath.Join(attachmentsDir, "report.pdf.txt")

	binTarget, err := os.Readlink(binLink)
	if err != nil {
		t.Fatalf("Readlink %s: %v", binLink, err)
	}
	if !strings.HasSuffix(binTarget, filepath.Join(att.SHA, "blob.pdf")) {
		t.Errorf("binary link target = %q, want suffix .../%s/blob.pdf", binTarget, att.SHA)
	}

	txtTarget, err := os.Readlink(txtLink)
	if err != nil {
		t.Fatalf("Readlink %s: %v", txtLink, err)
	}
	if !strings.HasSuffix(txtTarget, filepath.Join(att.SHA, "canonical.txt")) {
		t.Errorf("sidecar link target = %q, want suffix .../%s/canonical.txt", txtTarget, att.SHA)
	}

	// And ResolveAgentAttachmentByLinkName accepts both names.
	for _, name := range []string{"report.pdf", "report.pdf.txt"} {
		if _, err := s.ResolveAgentAttachmentByLinkName("alice", name); err != nil {
			t.Errorf("ResolveAgentAttachmentByLinkName(%q): %v", name, err)
		}
	}
}

// TestSyncAgentAttachmentLinksMaterializesPerAgentHardlinks proves the
// per-agent hardlink dir gets one inode entry per (agent, sha, file)
// after a SyncAgentFilesystem, and that the hardlink shares an inode
// with the canonical file (cost == 0 extra bytes).
func TestSyncAgentAttachmentLinksMaterializesPerAgentHardlinks(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	att, err := s.AddAttachmentFromText("notes.txt", "hello")
	if err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}
	if err := s.AppendChatMessage("alice", ChatMessage{
		Role:        RoleReceived,
		Kind:        "direct_chat",
		Attachments: []MessageAttachment{{SHA: att.SHA, Name: "notes.txt"}},
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem: %v", err)
	}

	canonicalBlob := s.path("attachments", att.SHA, "blob.txt")
	perAgentBlob := s.path("agents", "alice", "attachments", att.SHA, "blob.txt")
	canonicalStat, err := os.Stat(canonicalBlob)
	if err != nil {
		t.Fatalf("stat canonical blob: %v", err)
	}
	perAgentStat, err := os.Stat(perAgentBlob)
	if err != nil {
		t.Fatalf("stat per-agent blob: %v", err)
	}
	if !os.SameFile(canonicalStat, perAgentStat) {
		t.Errorf("per-agent blob is not a hardlink to canonical: %s vs %s", canonicalBlob, perAgentBlob)
	}

	// Bob (no chat history with this SHA) must NOT get a hardlink.
	if err := s.CreateAgent(Agent{Slug: "bob", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent bob: %v", err)
	}
	if err := s.SyncAgentFilesystem("bob"); err != nil {
		t.Fatalf("SyncAgentFilesystem bob: %v", err)
	}
	if _, err := os.Stat(s.path("agents", "bob", "attachments", att.SHA)); !os.IsNotExist(err) {
		t.Errorf("bob should not see alice's SHA, got err=%v", err)
	}
}

// TestSyncAgentAttachmentLinksDropsRevokedSHAs proves the reconciler
// removes per-agent dirs for SHAs no longer in the chat history.
func TestSyncAgentAttachmentLinksDropsRevokedSHAs(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	revoked := s.path("agents", "alice", "attachments", "deadbeef")
	if err := os.MkdirAll(revoked, 0o755); err != nil {
		t.Fatalf("mkdir stale: %v", err)
	}
	// SyncAgentFilesystem on a clean chat history wipes the stale entry.
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem: %v", err)
	}
	if _, err := os.Stat(revoked); !os.IsNotExist(err) {
		t.Errorf("stale per-agent SHA dir should be removed, got err=%v", err)
	}
}

// TestArchiveAttachmentIndexAvoidsRescan proves the index makes
// post-rotation syncs cheap: once an archive is in the index, a
// subsequent sync reads the index instead of re-reading the archived
// chat.jsonl. We verify by deleting the archive's chat.jsonl after
// the first post-rotation sync — if Sync still produces the right
// /files/attachments/ symlink, it can't have re-scanned the archive.
func TestArchiveAttachmentIndexAvoidsRescan(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	att, err := s.AddAttachmentFromText("brief.md", "# brief\n")
	if err != nil {
		t.Fatalf("AddAttachmentFromText: %v", err)
	}
	if err := s.AppendChatMessage("alice", ChatMessage{
		Role:        RoleReceived,
		Kind:        "direct_chat",
		Attachments: []MessageAttachment{{SHA: att.SHA, Name: "brief.md"}},
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	ts, err := s.ArchiveChat("alice")
	if err != nil {
		t.Fatalf("ArchiveChat: %v", err)
	}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("first post-rotation sync: %v", err)
	}

	// Sabotage the archive's chat.jsonl. Any subsequent sync that
	// tried to re-read it would either fail or drop the attachment;
	// the index path returns the attachment without touching the file.
	archivedJSONL := s.path("agents", "alice", "chats", ts, "chat.jsonl")
	if err := os.Remove(archivedJSONL); err != nil {
		t.Fatalf("remove archived chat.jsonl: %v", err)
	}

	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("second sync after archive sabotage: %v", err)
	}
	link := filepath.Join(s.path("agents", "alice", "memory", "attachments"), "brief.md")
	if _, err := os.Readlink(link); err != nil {
		t.Errorf("index didn't carry the SHA across a re-sync: %v", err)
	}
}

// TestArchiveAttachmentIndexSentinelForEmptyArchive proves an
// archive with zero attachments doesn't get re-scanned on every
// sync — the index records a sentinel so the second call skips it.
func TestArchiveAttachmentIndexSentinelForEmptyArchive(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// Archive an empty chat.jsonl. ArchiveChat creates the dir + an
	// empty file when there's no chat yet.
	ts, err := s.ArchiveChat("alice")
	if err != nil {
		t.Fatalf("ArchiveChat: %v", err)
	}
	if _, err := s.ensureArchiveAttachmentIndex("alice"); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	raw, err := s.readArchiveAttachmentIndex("alice")
	if err != nil {
		t.Fatalf("readArchiveAttachmentIndex: %v", err)
	}
	if len(raw) != 1 || raw[0].ArchiveTS != ts || raw[0].SHA != "" {
		t.Fatalf("expected single sentinel for ts=%s, got %+v", ts, raw)
	}

	// Sabotage the archive; the index sentinel should keep the next
	// ensure from touching it.
	if err := os.Remove(s.path("agents", "alice", "chats", ts, "chat.jsonl")); err != nil {
		t.Fatalf("remove archived chat.jsonl: %v", err)
	}
	got, err := s.ensureArchiveAttachmentIndex("alice")
	if err != nil {
		t.Fatalf("second ensure after sabotage: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("empty archive returned entries: %+v", got)
	}
}

// TestSyncAgentFilesystemAttachmentsSurviveRotation: an attachment
// seen in chat 1 must remain reachable
// from /files/attachments/ after the chat rotates and chat.jsonl is
// archived. The per-agent hardlink dir must persist too, since
// /files/attachments/ symlinks resolve through it inside the
// agent pod.
func TestSyncAgentFilesystemAttachmentsSurviveRotation(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	att, err := s.AddAttachmentFromText("brief.md", "# brief\n")
	if err != nil {
		t.Fatalf("AddAttachmentFromText: %v", err)
	}
	if err := s.AppendChatMessage("alice", ChatMessage{
		Role:        RoleReceived,
		Kind:        "direct_chat",
		Attachments: []MessageAttachment{{SHA: att.SHA, Name: "brief.md"}},
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem (pre-rotation): %v", err)
	}
	link := filepath.Join(s.path("agents", "alice", "memory", "attachments"), "brief.md")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("pre-rotation symlink missing: %v", err)
	}

	// Rotate: archive chat.jsonl and re-sync exactly like
	// finalizeRotation does.
	if _, err := s.ArchiveChat("alice"); err != nil {
		t.Fatalf("ArchiveChat: %v", err)
	}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem (post-rotation): %v", err)
	}

	// The /files/attachments/ symlink must still exist and resolve
	// to the canonical blob.
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("post-rotation Readlink %s: %v", link, err)
	}
	if !strings.HasSuffix(target, filepath.Join(att.SHA, "blob.md")) {
		t.Errorf("post-rotation symlink target = %q, want suffix .../%s/blob.md", target, att.SHA)
	}
	if _, err := os.ReadFile(link); err != nil {
		t.Errorf("post-rotation symlink unreadable: %v", err)
	}

	// And the per-agent hardlink dir for the SHA must still exist —
	// the agentpod's /data/attachments mount targets this subtree, so
	// dropping it here breaks in-pod resolution of the symlink even
	// when the canonical blob is intact.
	perAgent := s.path("agents", "alice", "attachments", att.SHA, "blob.md")
	if _, err := os.Stat(perAgent); err != nil {
		t.Errorf("post-rotation per-agent hardlink missing: %v", err)
	}

	// And the link-name resolver still finds the SHA — covers
	// run_shell.inputs and the attachment-download path.
	if got, err := s.ResolveAgentAttachmentByLinkName("alice", "brief.md"); err != nil {
		t.Errorf("ResolveAgentAttachmentByLinkName post-rotation: %v", err)
	} else if got.SHA != att.SHA {
		t.Errorf("ResolveAgentAttachmentByLinkName SHA = %q, want %q", got.SHA, att.SHA)
	}

	// And the access-control set surfaces the archived SHA so
	// run_shell.inputs is allowed to stage it.
	shas, err := s.AttachmentSHAsForAgent("alice")
	if err != nil {
		t.Fatalf("AttachmentSHAsForAgent: %v", err)
	}
	if !shas[att.SHA] {
		t.Errorf("AttachmentSHAsForAgent post-rotation missing %s; got %v", att.SHA, shas)
	}
}

// TestAttachmentLinkNamesDisambiguatesResentFilename is the store half
// of the "delivery header quoted the wrong file" bug.
//
// Three distinct blobs arrive under one filename, as a re-seeded
// handbook does. Only the first can hold the clean name; the
// other two are written beside it with a short-SHA prefix. The map
// AttachmentLinkNames returns is what the prompt header quotes, so
// every SHA must map to the name that actually resolves to ITS bytes,
// never the sender's filename for all three, which would silently
// point the last two recipients at the oldest blob.
func TestAttachmentLinkNamesDisambiguatesResentFilename(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	bodies := []string{
		"# handbook v1\nfirst seed\n",
		"# handbook v2\nsecond seed, adds cancel_task_request\n",
		"# handbook v3\nthird seed, adds propose_hire\n",
	}
	var shas []string
	for _, body := range bodies {
		att, err := s.AddAttachment(context.Background(), "handbook.md", strings.NewReader(body))
		if err != nil {
			t.Fatalf("AddAttachment: %v", err)
		}
		shas = append(shas, att.SHA)
		if err := s.AppendChatMessage("alice", ChatMessage{
			Role:        RoleReceived,
			Kind:        "inbox_delivery",
			Attachments: []MessageAttachment{{SHA: att.SHA, Name: "handbook.md"}},
		}); err != nil {
			t.Fatalf("AppendChatMessage: %v", err)
		}
	}
	if shas[0] == shas[1] || shas[1] == shas[2] || shas[0] == shas[2] {
		t.Fatalf("test needs three distinct SHAs, got %v", shas)
	}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem: %v", err)
	}

	links, err := s.AttachmentLinkNames("alice")
	if err != nil {
		t.Fatalf("AttachmentLinkNames: %v", err)
	}

	// Every SHA is named, the names are distinct, and only the first
	// delivery keeps the clean name.
	seenNames := map[string]bool{}
	for i, sha := range shas {
		name, ok := links[sha]
		if !ok {
			t.Fatalf("delivery %d (sha %s): no link name", i, sha)
		}
		if seenNames[name] {
			t.Fatalf("delivery %d: link name %q reused across distinct SHAs", i, name)
		}
		seenNames[name] = true
	}
	if links[shas[0]] != "handbook.md" {
		t.Errorf("first delivery link = %q, want the clean name handbook.md", links[shas[0]])
	}
	for _, i := range []int{1, 2} {
		if links[shas[i]] == "handbook.md" {
			t.Errorf("delivery %d also claims the clean name", i)
		}
		if !strings.HasSuffix(links[shas[i]], "-handbook.md") {
			t.Errorf("delivery %d link = %q, want a short-SHA-prefixed handbook.md", i, links[shas[i]])
		}
	}

	// The load-bearing assertion: reading each quoted path off disk
	// returns the bytes that delivery carried. This is the check the
	// recipient can't make for themselves without a stated hash.
	attachmentsDir := s.path("agents", "alice", "memory", "attachments")
	for i, sha := range shas {
		got, err := os.ReadFile(filepath.Join(attachmentsDir, links[sha]))
		if err != nil {
			t.Fatalf("delivery %d: read quoted path %q: %v", i, links[sha], err)
		}
		if string(got) != bodies[i] {
			t.Errorf("delivery %d: quoted path %q resolved to the wrong bytes\n got: %q\nwant: %q",
				i, links[sha], got, bodies[i])
		}
	}

	// And the reverse lookup agrees with the map in both directions.
	for i, sha := range shas {
		att, err := s.ResolveAgentAttachmentByLinkName("alice", links[sha])
		if err != nil {
			t.Fatalf("delivery %d: ResolveAgentAttachmentByLinkName(%q): %v", i, links[sha], err)
		}
		if att.SHA != sha {
			t.Errorf("delivery %d: %q resolved to sha %s, want %s", i, links[sha], att.SHA, sha)
		}
	}
}

// TestAttachmentLinkNamesStableAcrossResync guards the property that
// makes first-seen-wins safe: a name, once bound to a SHA, keeps
// pointing at the same bytes. An agent that wrote a path into its
// memory must not find different content there after later
// same-named deliveries land.
func TestAttachmentLinkNamesStableAcrossResync(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	first, err := s.AddAttachment(context.Background(), "spec.md", strings.NewReader("v1\n"))
	if err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}
	if err := s.AppendChatMessage("alice", ChatMessage{
		Role: RoleReceived, Kind: "inbox_delivery",
		Attachments: []MessageAttachment{{SHA: first.SHA, Name: "spec.md"}},
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem: %v", err)
	}
	before, err := s.AttachmentLinkNames("alice")
	if err != nil {
		t.Fatalf("AttachmentLinkNames: %v", err)
	}
	if before[first.SHA] != "spec.md" {
		t.Fatalf("first delivery link = %q, want spec.md", before[first.SHA])
	}

	second, err := s.AddAttachment(context.Background(), "spec.md", strings.NewReader("v2\n"))
	if err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}
	if err := s.AppendChatMessage("alice", ChatMessage{
		Role: RoleReceived, Kind: "inbox_delivery",
		Attachments: []MessageAttachment{{SHA: second.SHA, Name: "spec.md"}},
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem: %v", err)
	}
	after, err := s.AttachmentLinkNames("alice")
	if err != nil {
		t.Fatalf("AttachmentLinkNames: %v", err)
	}
	if after[first.SHA] != "spec.md" {
		t.Errorf("first delivery's link moved to %q after a re-send; paths must not change bytes underneath an agent", after[first.SHA])
	}

	attachmentsDir := s.path("agents", "alice", "memory", "attachments")
	got, err := os.ReadFile(filepath.Join(attachmentsDir, "spec.md"))
	if err != nil {
		t.Fatalf("read spec.md: %v", err)
	}
	if string(got) != "v1\n" {
		t.Errorf("spec.md = %q after the re-send, want v1", got)
	}
	newer, err := os.ReadFile(filepath.Join(attachmentsDir, after[second.SHA]))
	if err != nil {
		t.Fatalf("read %q: %v", after[second.SHA], err)
	}
	if string(newer) != "v2\n" {
		t.Errorf("%s = %q, want v2", after[second.SHA], newer)
	}
}

// TestSyncAgentAttachmentsReachableBeforeAppend: a message buffered
// behind a running turn is not in chat.jsonl, yet the turn it is
// folded into is told where its files are. They have to be there
// before it is told — the symlink under /files/attachments/ and the
// blob in the per-agent mirror the pod resolves through — and under
// the names the next full sync, which will find the message on disk,
// keeps. The pending message collides with a name an earlier delivery
// already holds, so the collision rule is exercised on the way.
func TestSyncAgentAttachmentsReachableBeforeAppend(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	first, err := s.AddAttachment(context.Background(), "spec.md", strings.NewReader("v1\n"))
	if err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}
	if err := s.AppendChatMessage("alice", ChatMessage{
		Role: RoleReceived, Kind: "direct_chat",
		Attachments: []MessageAttachment{{SHA: first.SHA, Name: "spec.md"}},
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem: %v", err)
	}

	second, err := s.AddAttachment(context.Background(), "spec.md", strings.NewReader("v2\n"))
	if err != nil {
		t.Fatalf("AddAttachment: %v", err)
	}
	notes, err := s.AddAttachmentFromText("notes.txt", "hello")
	if err != nil {
		t.Fatalf("AddAttachmentFromText: %v", err)
	}
	pending := []MessageAttachment{
		{SHA: second.SHA, Name: "spec.md"},
		{SHA: notes.SHA, Name: "notes.txt"},
	}

	names, err := s.SyncAgentAttachments("alice", pending)
	if err != nil {
		t.Fatalf("SyncAgentAttachments: %v", err)
	}
	if names[first.SHA] != "spec.md" {
		t.Errorf("earlier delivery's name = %q, want spec.md (a pending message must not move a name already bound)", names[first.SHA])
	}
	if names[notes.SHA] != "notes.txt" {
		t.Errorf("notes link = %q, want notes.txt", names[notes.SHA])
	}
	wantSecond := shortSHA(second.SHA) + "-spec.md"
	if names[second.SHA] != wantSecond {
		t.Errorf("colliding pending link = %q, want %q", names[second.SHA], wantSecond)
	}

	attachmentsDir := s.path("agents", "alice", "memory", "attachments")
	for link, want := range map[string]string{
		"spec.md":   "v1\n",
		wantSecond:  "v2\n",
		"notes.txt": "hello",
	} {
		got, err := os.ReadFile(filepath.Join(attachmentsDir, link))
		if err != nil {
			t.Fatalf("read %s before the message is on disk: %v", link, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", link, got, want)
		}
	}
	for _, sha := range []string{second.SHA, notes.SHA} {
		canonical := s.path("attachments", sha)
		mirror := s.path("agents", "alice", "attachments", sha)
		entries, err := os.ReadDir(canonical)
		if err != nil {
			t.Fatalf("read canonical %s: %v", sha, err)
		}
		for _, e := range entries {
			c, err := os.Stat(filepath.Join(canonical, e.Name()))
			if err != nil {
				t.Fatalf("stat canonical %s/%s: %v", sha, e.Name(), err)
			}
			m, err := os.Stat(filepath.Join(mirror, e.Name()))
			if err != nil {
				t.Fatalf("pending SHA %s missing %s from the per-agent mirror the pod resolves through: %v", sha, e.Name(), err)
			}
			if !os.SameFile(c, m) {
				t.Errorf("%s/%s in the mirror is not a hardlink to the canonical", sha, e.Name())
			}
		}
	}
	hist, err := s.ReadChatHistory("alice")
	if err != nil {
		t.Fatalf("ReadChatHistory: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("chat.jsonl has %d rows; the pending sync must not write the message", len(hist))
	}

	// A full sync before the message lands — a graph tool call in the
	// same turn runs one — must keep the links: the message is still
	// owed, and reconciling from chat.jsonl alone would drop them.
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem (mid-turn): %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(attachmentsDir, wantSecond)); err != nil || string(got) != "v2\n" {
		t.Fatalf("%s after a mid-turn full sync = %q, %v; the pending link was reconciled away", wantSecond, got, err)
	}
	if _, err := os.Stat(s.path("agents", "alice", "attachments", second.SHA, "blob.md")); err != nil {
		t.Fatalf("pending SHA dropped from the per-agent mirror by a mid-turn full sync: %v", err)
	}

	// The message lands and the next full sync runs from disk alone:
	// every name holds, and nothing is pending any more.
	if err := s.AppendChatMessage("alice", ChatMessage{
		Role: RoleReceived, Kind: "direct_chat", Attachments: pending,
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	if held := s.pendingAttachmentsFor("alice"); len(held) != 0 {
		t.Errorf("pending attachments after the message landed = %v, want none", held)
	}
	if err := s.SyncAgentFilesystem("alice"); err != nil {
		t.Fatalf("SyncAgentFilesystem: %v", err)
	}
	after, err := s.AttachmentLinkNames("alice")
	if err != nil {
		t.Fatalf("AttachmentLinkNames: %v", err)
	}
	for _, sha := range []string{first.SHA, second.SHA, notes.SHA} {
		if after[sha] != names[sha] {
			t.Errorf("sha %s: name %q before the flush, %q after; the fold quoted a path the next turn moved", shortSHA(sha), names[sha], after[sha])
		}
	}
	got, err := os.ReadFile(filepath.Join(attachmentsDir, wantSecond))
	if err != nil {
		t.Fatalf("read %s after the full sync: %v", wantSecond, err)
	}
	if string(got) != "v2\n" {
		t.Errorf("%s = %q after the full sync, want v2", wantSecond, got)
	}
}

// A row with attachments landing is the attachments farm's edge: the
// files resolve under /files/attachments/ (and the per-agent
// hardlinks exist) as soon as AppendChatMessage returns, with no sync
// run for the turn that reads the row.
func TestAppendChatMessageLinksItsAttachments(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	att, err := s.AddAttachmentFromText("brief.txt", "the brief")
	if err != nil {
		t.Fatalf("AddAttachmentFromText: %v", err)
	}
	if err := s.AppendChatMessage("alice", ChatMessage{
		Role: RoleReceived, Kind: "direct_chat", Content: "see attached",
		Attachments: []MessageAttachment{{SHA: att.SHA, Name: "brief.txt"}},
	}); err != nil {
		t.Fatalf("AppendChatMessage: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(s.path("agents", "alice", "memory", "attachments"), "brief.txt"))
	if err != nil || string(got) != "the brief" {
		t.Fatalf("attachment link after the row landed: %q, %v", got, err)
	}
	if _, err := os.Stat(s.path("agents", "alice", "attachments", att.SHA)); err != nil {
		t.Errorf("per-agent hardlink dir after the row landed: %v", err)
	}
}

// A caller holding a lock appends with AppendChatMessageLinkLater and
// links after releasing it: nothing is linked until LinkChatAttachments,
// which links once and then has nothing to do. The CEO has no farm, so
// its rows mark nothing.
func TestAppendChatMessageLinkLater(t *testing.T) {
	s := mustStore(t)
	for _, a := range []Agent{{Slug: "alice", Role: "Eng", ReportsTo: "ceo"}, {Slug: "ceo", Role: "CEO"}} {
		if err := s.CreateAgent(a, ""); err != nil {
			t.Fatalf("CreateAgent %s: %v", a.Slug, err)
		}
	}
	att, err := s.AddAttachmentFromText("brief.txt", "the brief")
	if err != nil {
		t.Fatalf("AddAttachmentFromText: %v", err)
	}
	row := ChatMessage{
		Role: RoleReceived, Kind: "direct_chat", Content: "see attached",
		Attachments: []MessageAttachment{{SHA: att.SHA, Name: "brief.txt"}},
	}
	if err := s.AppendChatMessageLinkLater("alice", row); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.path("agents", "alice", "memory", "attachments"), "brief.txt")
	if _, err := os.Lstat(link); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("linked before LinkChatAttachments: %v", err)
	}
	s.LinkChatAttachments("alice")
	if got, err := os.ReadFile(link); err != nil || string(got) != "the brief" {
		t.Fatalf("after LinkChatAttachments: %q, %v", got, err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	s.LinkChatAttachments("alice")
	if _, err := os.Lstat(link); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a second LinkChatAttachments synced with nothing marked: %v", err)
	}

	if err := s.AppendChatMessage("ceo", row); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(s.path("agents", "ceo", "memory", "attachments", "brief.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a CEO row linked its attachments: %v", err)
	}
}
