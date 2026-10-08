package web

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/backup"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// TestShouldExcludeFromBackup locks in the security-sensitive exclusion
// list. If someone adds a new exclude or removes one, this test forces
// them to confirm the change is deliberate.
func TestShouldExcludeFromBackup(t *testing.T) {
	excluded := []string{
		"claude-home",
		"claude-home/.claude/.credentials.json",
		"claude-home/.claude.json",
		"claude-home/.cache/foo",
		"claude-home/.npm/something",
		"claude-home/.claude/cache/x",
		"claude-home/.claude/backups/y",
		"claude-home/.claude/telemetry/z",
		// Session logs and downloaded WebFetch caches under
		// `.claude/projects/` are excluded too — restore relies on
		// Kivali's own chat.jsonl archives, not the CLI's session
		// log, for cross-restore continuity.
		"claude-home/.claude/projects/-/abc.jsonl",
		"claude-home/.claude/sessions/x",
		"debug/last_req_alice.json",
		// `.tmp-*` files appear anywhere writeAtomic (store/store.go)
		// is invoked during a rename race with the backup walk.
		// Covered by basename-prefix, not directory-prefix.
		".tmp-abc123",
		"agents/alice/.tmp-chat.jsonl456",
		"messages/2026-04-18/.tmp-789",
		"system_instructions.md/.tmp-foo", // pathological but harmless
	}
	for _, p := range excluded {
		if !shouldExcludeFromBackup(p) {
			t.Errorf("expected %q to be excluded", p)
		}
	}
	included := []string{
		"agents/alice/chat.jsonl",
		"agents/alice/claude_session.json",
		"messages/2026-04-18/foo.md",
		"attachments/ab/cd/blob",
		"handbook.md",
		"message_queue.json",
	}
	for _, p := range included {
		if shouldExcludeFromBackup(p) {
			t.Errorf("expected %q to be INCLUDED (not excluded)", p)
		}
	}
}

func TestSafeRestoreTarget(t *testing.T) {
	root := t.TempDir()
	ok := []string{
		"agents/alice/chat.jsonl",
		"messages/2026-04-18/foo.md",
		"handbook.md",
		"nested/dir/",
	}
	for _, n := range ok {
		if _, err := safeRestoreTarget(root, n); err != nil {
			t.Errorf("safeRestoreTarget(%q) err = %v, want nil", n, err)
		}
	}
	rejects := []string{
		"../etc/passwd",
		"/etc/passwd",
		"agents/../../etc/passwd",
		"", // empty
	}
	for _, n := range rejects {
		if _, err := safeRestoreTarget(root, n); err == nil {
			t.Errorf("safeRestoreTarget(%q) err = nil, want non-nil", n)
		}
	}
}

// TestBackupStreamsZipWithExpectedEntries drives the download handler
// against a seeded store and verifies the resulting zip contains the
// included files and omits the excluded ones.
func TestBackupStreamsZipWithExpectedEntries(t *testing.T) {
	srv := newTestServer(t)
	root := srv.Store.Root()

	// Seed a realistic mix: business state (should be backed up),
	// credentials (should be scrubbed), and ephemeral cache (skipped).
	writeFile(t, filepath.Join(root, "agents", "alice", "chat.jsonl"), "hi\n")
	writeFile(t, filepath.Join(root, "messages", "2026-04-18", "msg.md"), "body")
	writeFile(t, filepath.Join(root, "handbook.md"), "# handbook")
	writeFile(t, filepath.Join(root, "claude-home", ".claude", "projects", "-", "sess-1.jsonl"), "{}")
	writeFile(t, filepath.Join(root, "claude-home", ".claude", ".credentials.json"), `{"secret":"nope"}`)
	writeFile(t, filepath.Join(root, "claude-home", ".claude.json"), `{"also":"nope"}`)
	writeFile(t, filepath.Join(root, "claude-home", ".cache", "junk"), "junk")
	writeFile(t, filepath.Join(root, "debug", "last_req_alice.json"), "debug")

	rr := backupZip(t, srv)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("content-type"); ct != "application/zip" {
		t.Errorf("content-type = %q", ct)
	}
	zr, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
	if err != nil {
		t.Fatalf("parse zip: %v", err)
	}
	got := map[string]bool{}
	for _, e := range zr.File {
		got[e.Name] = true
	}

	wantIn := []string{
		"agents/alice/chat.jsonl",
		"messages/2026-04-18/msg.md",
		"handbook.md",
	}
	for _, n := range wantIn {
		if !got[n] {
			t.Errorf("zip missing entry %q; have %v", n, zipNames(zr))
		}
	}
	wantOut := []string{
		"claude-home/.claude/.credentials.json",
		"claude-home/.claude.json",
		"claude-home/.cache/junk",
		"claude-home/.claude/projects/-/sess-1.jsonl",
		"debug/last_req_alice.json",
	}
	for _, n := range wantOut {
		if got[n] {
			t.Errorf("zip leaked excluded entry %q", n)
		}
	}
}

// TestBackupCarriesPublishedTreesAndSkipsTheMirror seeds the layout
// production uses and asserts the HTTP backup carries the canonicals —
// the attachment blob and the published trees under public/ — and not
// the per-agent attachment hardlink mirror, which a restore re-links.
// Mirror slug "amy" sorts before "attachments" in lexical walk order,
// the case a naive inode-dedup approach gets wrong.
func TestBackupCarriesPublishedTreesAndSkipsTheMirror(t *testing.T) {
	src := newTestServer(t)
	root := src.Store.Root()

	canonAttach := filepath.Join(root, "attachments", "abc", "blob.bin")
	writeFile(t, canonAttach, "ATTACH-BYTES")
	writeFile(t, filepath.Join(files.PublishedDir(root, "owner"), "x.bin"), "PUBLIC-BYTES")
	mirrorAttach := filepath.Join(root, "agents", "amy", "attachments", "abc", "blob.bin")
	if err := os.MkdirAll(filepath.Dir(mirrorAttach), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(canonAttach, mirrorAttach); err != nil {
		t.Fatalf("link mirrorAttach: %v", err)
	}
	writeFile(t, filepath.Join(root, "agents", "amy", "agent.yaml"), "slug: amy\n")

	rr := backupZip(t, src)
	if rr.Code != http.StatusOK {
		t.Fatalf("backup code = %d, body = %s", rr.Code, rr.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len()))
	if err != nil {
		t.Fatalf("parse zip: %v", err)
	}
	got := map[string][]byte{}
	for _, e := range zr.File {
		rc, err := e.Open()
		if err != nil {
			t.Fatalf("open entry %s: %v", e.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read entry %s: %v", e.Name, err)
		}
		got[e.Name] = data
	}
	for name, body := range map[string]string{
		"attachments/abc/blob.bin": "ATTACH-BYTES",
		"public/owner/x.bin":       "PUBLIC-BYTES",
		"agents/amy/agent.yaml":    "slug: amy\n",
	} {
		if data, ok := got[name]; !ok || string(data) != body {
			t.Errorf("zip entry %s = %q (present %v), want %q; have %v", name, data, ok, body, zipNames(zr))
		}
	}
	if _, ok := got["agents/amy/attachments/abc/blob.bin"]; ok {
		t.Error("zip leaked the hardlink-mirror entry (restore re-links from canonical)")
	}
}

// A backup carries every agent's published tree under public/<slug>/.
// Restoring it publishes exactly the archive's files, indexes them, and
// leaves the agent's own tree with empty mount points.
func TestRestoreReplacesPublishedFiles(t *testing.T) {
	src := newTestServer(t)
	if err := src.Store.CreateAgent(store.Agent{Slug: "owner", Role: "Analyst", ReportsTo: "ceo"}, "# role"); err != nil {
		t.Fatal(err)
	}
	srcStorage := files.StorageRoot(filepath.Join(src.Store.Root(), "agents", "owner"))
	writeFile(t, filepath.Join(files.PublishedDir(src.Store.Root(), "owner"), "spec.md"), "---\nid: spec\n---\nthe spec\n")
	writeFile(t, filepath.Join(files.PublishedDir(src.Store.Root(), "owner"), "kit", "a.csv"), "a,b\n")
	writeFile(t, filepath.Join(srcStorage, "artifacts", "private", "notes.md"), "mine")
	dl := backupZip(t, src)
	if dl.Code != http.StatusOK {
		t.Fatalf("backup code = %d", dl.Code)
	}

	// The fresh deployment already has published files (a seed's): the
	// restore empties the published trees first, so the archive's
	// spec.md replaces the seed's, and nothing of the seed's stays
	// published.
	dst := newTestServer(t)
	writeFile(t, filepath.Join(files.PublishedDir(dst.Store.Root(), "owner"), "spec.md"), "---\nid: spec\n---\nthe seed's spec\n")
	writeFile(t, filepath.Join(files.PublishedDir(dst.Store.Root(), "chief-of-staff"), "seed.md"), "seeded")
	rr := restoreZip(t, dst, "old.zip", dl.Body.Bytes())
	if rr.Code != http.StatusOK {
		t.Fatalf("restore code = %d, body = %s", rr.Code, rr.Body.String())
	}
	root := dst.Store.Root()
	if _, err := os.Lstat(filepath.Join(files.PublishedDir(root, "chief-of-staff"), "seed.md")); !os.IsNotExist(err) {
		t.Errorf("the fresh deployment's published file survived the restore: %v", err)
	}
	// The owner's directory stays (a running pod mounts it); its
	// contents go.
	if info, err := os.Lstat(files.PublishedDir(root, "chief-of-staff")); err != nil || !info.IsDir() {
		t.Errorf("public/chief-of-staff after restore: %v, %v; want the directory kept", info, err)
	}
	for rel, want := range map[string]string{"spec.md": "the spec", "kit/a.csv": "a,b"} {
		b, err := os.ReadFile(filepath.Join(files.PublishedDir(root, "owner"), filepath.FromSlash(rel)))
		if err != nil || !strings.Contains(string(b), want) {
			t.Errorf("public/owner/%s = %q, %v", rel, b, err)
		}
	}
	storage := files.StorageRoot(filepath.Join(root, "agents", "owner"))
	for _, d := range files.PublishedMountPoints {
		entries, err := os.ReadDir(filepath.Join(storage, filepath.FromSlash(d)))
		if err != nil || len(entries) != 0 {
			t.Errorf("%s after restore: %d entries, %v; want an empty mount point", d, len(entries), err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(storage, "artifacts", "private", "notes.md")); err != nil || string(b) != "mine" {
		t.Errorf("private workspace after restore: %q, %v", b, err)
	}
	ix, err := dst.Store.ReadGraphIndex()
	if err != nil {
		t.Fatalf("no index after restore: %v", err)
	}
	if _, ok := ix.Get("owner/spec"); !ok {
		t.Error("the restored file is not in the restored graph")
	}
}

func TestIsFreshForRestore(t *testing.T) {
	srv := newTestServer(t)

	// No agents at all → fresh.
	if !srv.IsFreshForRestore() {
		t.Error("bare store should be fresh")
	}

	// Seed CEO only — still fresh.
	if err := srv.Store.CreateAgent(store.Agent{Slug: "ceo", Role: "CEO"}, ""); err != nil {
		t.Fatalf("seed ceo: %v", err)
	}
	if !srv.IsFreshForRestore() {
		t.Error("ceo-only should be fresh")
	}

	// Seed CoS — still fresh (no chat).
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "Chief of Staff"}, "k"); err != nil {
		t.Fatalf("seed cos: %v", err)
	}
	if !srv.IsFreshForRestore() {
		t.Error("ceo+cos with no chat should be fresh")
	}

	// Put a chat entry on CoS → not fresh anymore.
	_ = srv.Store.AppendChatMessage("chief-of-staff", store.ChatMessage{Role: store.RoleReceived, Content: "hi"})
	if srv.IsFreshForRestore() {
		t.Error("cos with chat history should NOT be fresh")
	}
}

func TestIsFreshForRestoreRejectsExtraAgents(t *testing.T) {
	srv := newTestServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "ceo", Role: "CEO"}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := srv.Store.CreateAgent(store.Agent{Slug: "analyst", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if srv.IsFreshForRestore() {
		t.Error("extra non-seed agent should disqualify fresh state")
	}
}

// TestRestoreRoundTrip packs a backup, then tries to restore it into a
// fresh store and verifies the expected files land in place.
func TestRestoreRoundTrip(t *testing.T) {
	src := newTestServer(t)
	writeFile(t, filepath.Join(src.Store.Root(), "agents", "alice", "chat.jsonl"), `{"role":"received","content":"hi"}`)
	writeFile(t, filepath.Join(src.Store.Root(), "handbook.md"), "# the rules")

	downloadRR := backupZip(t, src)
	if downloadRR.Code != http.StatusOK {
		t.Fatalf("backup code = %d", downloadRR.Code)
	}
	zipBytes := downloadRR.Body.Bytes()

	// Fresh destination server whose setup already wrote a handbook;
	// upload the zip and verify state lands: the archive's handbook
	// replaces the one setup wrote.
	dst := newTestServer(t)
	if err := dst.Store.WriteHandbook("# the seeded default"); err != nil {
		t.Fatal(err)
	}
	upRR := restoreZip(t, dst, "backup.zip", zipBytes)
	if upRR.Code != http.StatusOK {
		t.Fatalf("restore code = %d, body = %s", upRR.Code, upRR.Body.String())
	}
	// Spot-check the restored files.
	got, err := os.ReadFile(filepath.Join(dst.Store.Root(), "handbook.md"))
	if err != nil {
		t.Fatalf("read restored handbook: %v", err)
	}
	if string(got) != "# the rules" {
		t.Errorf("handbook content = %q", string(got))
	}
	chat, err := os.ReadFile(filepath.Join(dst.Store.Root(), "agents", "alice", "chat.jsonl"))
	if err != nil {
		t.Fatalf("read restored chat: %v", err)
	}
	if !strings.Contains(string(chat), `"hi"`) {
		t.Errorf("chat content = %q", string(chat))
	}
}

func TestRestoreRejectsWhenNotFresh(t *testing.T) {
	srv := newTestServer(t)
	// Plant a non-seed agent so the fresh gate fails.
	if err := srv.Store.CreateAgent(store.Agent{Slug: "analyst", Role: "Analyst"}, "k"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rr := restoreZip(t, srv, "tiny.zip", minimalZip(t))
	if rr.Code != http.StatusConflict {
		t.Errorf("code = %d, want 409", rr.Code)
	}
}

func TestRestoreRejectsTraversalEntry(t *testing.T) {
	srv := newTestServer(t)
	// Build a zip with one safe entry + one traversal entry. Handler
	// must reject outright — no partial write.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	safe, _ := zw.Create("agents/alice/chat.jsonl")
	_, _ = safe.Write([]byte("safe"))
	evil, _ := zw.Create("../escaped.txt")
	_, _ = evil.Write([]byte("pwned"))
	_ = zw.Close()

	rr := restoreZip(t, srv, "bad.zip", buf.Bytes())
	if rr.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400", rr.Code)
	}
	// Verify the "safe" entry did NOT land — we pre-validate before writing.
	if _, err := os.Stat(filepath.Join(srv.Store.Root(), "agents", "alice", "chat.jsonl")); err == nil {
		t.Error("safe entry should not have been written; pre-validation must reject the whole zip")
	}
}

// TestRestoreCarriesGraphAndAssignments pins that the knowledge graph and
// the assignment tracker ride the backup without either walker naming
// them: everything they keep is a regular file under DataDir, so the
// catch-all walk ships it and the restore lands it. It also pins the
// two things a restore has to do afterwards, because the process is
// not restarted: the graph pass leaves a restored index alone when
// nothing moved (unpacking resets every mtime, so the pass has to
// reconcile by hash rather than cut a phantom version), and a wake the
// archive carried in assignments/pending/ is routed the way boot routes it.
func TestRestoreCarriesGraphAndAssignments(t *testing.T) {
	src := e2eServer(t)
	if err := src.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// An assignment with a log, filed by the CEO so its wake delivers at
	// once and nothing sits in the queue; then a pending record the
	// tracker never got to route, as a crash mid-change leaves one.
	if _, err := src.tracker().Create(ctx, agent.CEOSlug, assignments.CreateInput{Title: "Draft the release notes", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	pending := store.PendingWakes{
		Seq: 2, Assignment: 1, By: "chief-of-staff", At: time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC),
		Wakes: []assignments.Wake{{To: "alice", Op: assignments.OpAmended, Title: "chief-of-staff amended #1", Body: "clearer spec"}},
	}
	if err := src.Store.WritePendingWakes(pending); err != nil {
		t.Fatal(err)
	}

	// One public artifact, indexed, and a watermark saying alice saw it.
	pub := files.PublishedDir(src.Store.Root(), "alice")
	writeFile(t, filepath.Join(pub, "notes.md"), "---\nid: notes\nsummary: Release notes\n---\nbody")
	if err := src.Store.Graph().Scan(ctx); err != nil {
		t.Fatalf("graph scan: %v", err)
	}
	srcIx := src.Store.Graph().Index()
	if err := src.Store.WriteGraphWatermark("alice", store.GraphWatermark{Seq: srcIx.Seq}); err != nil {
		t.Fatal(err)
	}

	// --- the backup carries every file both features keep ---
	dlRR := backupZip(t, src)
	if dlRR.Code != http.StatusOK {
		t.Fatalf("backup code = %d", dlRR.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(dlRR.Body.Bytes()), int64(dlRR.Body.Len()))
	if err != nil {
		t.Fatalf("parse zip: %v", err)
	}
	have := map[string]bool{}
	for _, name := range zipNames(zr) {
		have[name] = true
	}
	for _, want := range []string{
		"assignments/000001.md",
		"assignments/pending/000000000002.json",
		"graph/index.json",
		"graph/versions.jsonl",
		"agents/alice/graph_watermark.json",
		"public/alice/notes.md",
	} {
		if !have[want] {
			t.Errorf("zip missing %q; have %v", want, zipNames(zr))
		}
	}

	// --- restore into a fresh, tracker-wired server ---
	dst := e2eServer(t)
	upRR := restoreZip(t, dst, "backup.zip", dlRR.Body.Bytes())
	if upRR.Code != http.StatusOK {
		t.Fatalf("restore code = %d, body = %s", upRR.Code, upRR.Body.String())
	}

	// The assignment reads back whole, and the pending wake was routed:
	// the record is gone and alice's queue holds the amendment (a
	// chief-of-staff change waits for the CEO to release it).
	srcIss, err := src.Store.ReadAssignment(1)
	if err != nil {
		t.Fatal(err)
	}
	dstIss, err := dst.Store.ReadAssignment(1)
	if err != nil {
		t.Fatalf("restored assignment: %v", err)
	}
	if dstIss.Title != srcIss.Title || dstIss.Assignee != srcIss.Assignee || dstIss.Creator != srcIss.Creator || len(dstIss.Log) != len(srcIss.Log) {
		t.Errorf("restored assignment = %+v, want %+v", dstIss, srcIss)
	}
	if left, err := dst.Store.ListPendingWakes(); err != nil || len(left) != 0 {
		t.Errorf("pending wakes after restore = %v (err %v); a restore routes them like boot does", left, err)
	}
	q, err := dst.Store.ReadMessageQueue()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(q.Agents["alice"].Inbox); n != 1 {
		t.Errorf("alice has %d queued after restore, want the one routed pending wake: %v", n, q.Agents["alice"].Inbox)
	}

	// The graph reads back whole, and the restore's own pass left it
	// alone: same sequence, still one version of the node.
	dstIx, err := dst.Store.ReadGraphIndex()
	if err != nil {
		t.Fatalf("restored index: %v", err)
	}
	if dstIx.Seq != srcIx.Seq {
		t.Errorf("index seq after restore = %d, want %d (nothing moved)", dstIx.Seq, srcIx.Seq)
	}
	node, ok := dstIx.Get("alice/notes")
	if !ok || len(node.Versions) != 1 || node.Summary != "Release notes" {
		t.Errorf("restored node = %+v, ok = %v", node, ok)
	}
	if wm, ok, err := dst.Store.ReadGraphWatermark("alice"); err != nil || !ok || wm.Seq != srcIx.Seq {
		t.Errorf("restored watermark = %+v ok=%v err=%v, want seq %d", wm, ok, err, srcIx.Seq)
	}
}

// ---- helpers -----------------------------------------------------------

// backupZip POSTs /api/v1/org/backup signed in, from this origin; the
// body is the zip.
func backupZip(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/org/backup", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	return rr
}

// restoreZip uploads data as the "archive" field of POST
// /api/v1/org/restore, signed in, from this origin.
func restoreZip(t *testing.T, srv *Server, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := multipartZipUpload(t, "archive", filename, data)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/org/restore", body)
	req.Header.Set("content-type", contentType)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	return rr
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func multipartZipUpload(t *testing.T, field, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := io.Copy(fw, bytes.NewReader(data)); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &buf, w.FormDataContentType()
}

func minimalZip(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "placeholder.txt"), "hi")
	var buf bytes.Buffer
	if _, err := backup.WriteZip(dir, &buf, "test"); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipNames(zr *zip.Reader) []string {
	out := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		out = append(out, f.Name)
	}
	return out
}

// A file the backup cannot read aborts the download, rather than being
// left out of a zip that still downloads as valid.
func TestBackupDownloadAbortsOnAFileItCannotRead(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	srv := newTestServer(t)
	p := filepath.Join(srv.Store.Root(), "agents", "alice", "chat.jsonl")
	writeFile(t, p, "hi\n")
	if err := os.Chmod(p, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(p, 0o644) }()
	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", r)
		}
	}()
	srv.handleBackupDownload(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/org/backup", nil))
	t.Error("the download finished")
}

// A backup is the zip the app downloads; a tarball (the format an older
// command-line backup wrote) is refused before anything is written, and
// the answer says what a backup is.
func TestRestoreRefusesATarballUpload(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("# rules")
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "handbook.md", Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	srv := newTestServer(t)
	rr := restoreZip(t, srv, "b.tar.gz", buf.Bytes())
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "a backup is the .zip the app downloads") {
		t.Errorf("code = %d, body = %s; want 400 saying a backup is the .zip the app downloads", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(srv.Store.Root(), "handbook.md")); err == nil {
		t.Error("a file was written from a refused upload")
	}
}

// flushOrder records whether the headers were flushed before the first
// byte of the body.
type flushOrder struct {
	*httptest.ResponseRecorder
	flushedBeforeWrite bool
	wrote              bool
}

func (f *flushOrder) Write(p []byte) (int, error) {
	f.wrote = true
	return f.ResponseRecorder.Write(p)
}

func (f *flushOrder) Flush() {
	if !f.wrote {
		f.flushedBeforeWrite = true
	}
	f.ResponseRecorder.Flush()
}

// The Org page's button is a plain form POST into a hidden frame: the
// browser's download manager owns the transfer. Asked the way a form
// asks, the backup is answered as an attachment whose headers go out
// before the archive's first byte (so the download shows at once), and
// it is never gzipped (so it streams as written).
func TestBackupAnswersABrowserFormPostAsAStreamedAttachment(t *testing.T) {
	srv := newTestServer(t)
	writeFile(t, filepath.Join(srv.Store.Root(), "agents", "alice", "chat.jsonl"), "hi\n")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/org/backup", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "iframe")
	rr := &flushOrder{ResponseRecorder: httptest.NewRecorder()}
	authedHandler(t, srv).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rr.Code, rr.Body.String())
	}
	if cd := rr.Header().Get("content-disposition"); !strings.HasPrefix(cd, "attachment; filename=kivali-backup-") {
		t.Errorf("content-disposition = %q", cd)
	}
	if ce := rr.Header().Get("content-encoding"); ce != "" {
		t.Errorf("content-encoding = %q, want none", ce)
	}
	if !rr.flushedBeforeWrite {
		t.Error("the headers waited for the archive's first bytes")
	}
	if _, err := zip.NewReader(bytes.NewReader(rr.Body.Bytes()), int64(rr.Body.Len())); err != nil {
		t.Fatalf("parse zip: %v", err)
	}
}
