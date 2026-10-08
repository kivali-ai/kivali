package backup

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A small org in the current format, enough for the inventory's
// classifiers and the store's readers.
var tree = map[string]string{
	"handbook.md":                                              "# rules\n",
	"agents/alice/agent.yaml":                                  "slug: alice\nrole: Analyst\nstatus: active\n",
	"agents/alice/role.md":                                     "# role\n",
	"agents/alice/chat.jsonl":                                  `{"role":"received","content":"hi","ts":"2026-09-01T00:00:00Z"}` + "\n",
	"agents/alice/agent_memory_habits.md":                      "- be brief\n",
	"agents/alice/chats/20260901T000000.000000000Z/chat.jsonl": `{"role":"received","content":"old","ts":"2026-08-01T00:00:00Z"}` + "\n",
	"agents/alice/files/background/plan.md":                    "# plan\n",
	"agents/_archived/bob/agent.yaml":                          "slug: bob\nrole: Former\nstatus: archived\n",
	"messages/2026-09-01/20260901T000000.000000000Z-assignment_event-ceo--to--alice.md": "---\ntype: assignment_event\ntitle: '#1 assigned to you'\nfrom: ceo\nto: alice\ndate: 2026-09-01T00:00:00Z\nassignment:\n    id: 1\n    seq: 1\n    op: created\n---\n\nbody\n",
	"messages/2026-09-01/20260901T000001.000000000Z-notice-ceo--to--alice.md":           "---\ntype: notice\ntitle: hello\nfrom: ceo\nto: alice\ndate: 2026-09-01T00:00:01Z\n---\n\nhi\n",
	"assignments/000001.md":    "---\nid: 1\ntitle: Draft the release notes\nstatus: open\nassignee: alice\ncreator: ceo\ncreated: 2026-09-01T00:00:00Z\nupdated: 2026-09-01T00:00:00Z\nlog:\n  - {seq: 1, ts: '2026-09-01T00:00:00Z', by: ceo, op: created, to: alice}\n---\nbody\n",
	"usage.jsonl":              `{"ts":"2026-09-01T00:00:00Z","model":"m","input_tokens":3,"output_tokens":4}` + "\n",
	"attachments/abc/blob.bin": "ATTACH",
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// snapshot is every path under dir with its content, mode and mtime.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		v := info.Mode().String()
		if info.Mode().IsRegular() {
			b, _ := os.ReadFile(p)
			v += " " + info.ModTime().UTC().Format(time.RFC3339) + " " + string(b)
		}
		out[filepath.ToSlash(rel)] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalFiles(t *testing.T, a, b map[string]string) {
	t.Helper()
	for k, v := range a {
		if !strings.HasPrefix(v, "-") {
			continue // directories: mode only, and mtimes move
		}
		if b[k] != v {
			t.Errorf("%s: restored %q, want %q", k, b[k], v)
		}
	}
	for k, v := range b {
		if strings.HasPrefix(v, "-") && a[k] == "" {
			t.Errorf("%s: restored but not in the source", k)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	src := writeTree(t, tree)
	old := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "usage.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "attachments/abc/blob.bin"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	m, err := WriteZip(src, &buf, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != len(tree) {
		t.Errorf("manifest lists %d files, want %d", len(m.Files), len(tree))
	}
	path := filepath.Join(t.TempDir(), "backup.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	src2, closeSrc, err := OpenArchive(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeSrc()
	dst := t.TempDir()
	res, err := Restore(dst, src2, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != len(tree) {
		t.Errorf("restored %d files, want %d", res.Files, len(tree))
	}
	equalFiles(t, snapshot(t, src), snapshot(t, dst))
	if _, err := os.Stat(filepath.Join(dst, ManifestName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the manifest was written into the data directory: %v", err)
	}
}

func TestWalkLeavesOutWhatABackupDoesNotCarry(t *testing.T) {
	src := writeTree(t, map[string]string{
		"keep.md":                                "k",
		"claude-home/.claude/.credentials.json":  "secret",
		"debug/last_req_alice.json":              "{}",
		"agents/alice/.tmp-123":                  "partial",
		ManifestName:                             "stale",
		"agents/amy/attachments/abc/blob.bin":    "mirror",
		"public/amy/spec.md":                     "published",
		"agents/amy/files/artifacts/public/y.md": "keep",
	})
	if err := os.Symlink("keep.md", filepath.Join(src, "link.md")); err != nil {
		t.Fatal(err)
	}
	// A socket, as the control socket is. macOS caps socket paths near
	// 104 bytes, so bind one only if the temp dir is short enough.
	if ln, err := net.Listen("unix", filepath.Join(src, "s.sock")); err == nil {
		defer func() { _ = ln.Close() }()
	}
	var got []string
	if err := Walk(src, func(rel string, info fs.FileInfo) error {
		if !info.IsDir() {
			got = append(got, rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := "agents/amy/files/artifacts/public/y.md keep.md public/amy/spec.md"
	if strings.Join(got, " ") != want {
		t.Errorf("walked %v, want %s", got, want)
	}
}

// tamper rewrites a backup zip member by member through edit, which
// may change a member's body, drop it (return nil), or add members.
func tamper(t *testing.T, archive []byte, edit func(name string, body []byte) map[string][]byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	src := NewZipSource(zr)
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for {
		e, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if e.Dir {
			hdr := &zip.FileHeader{Name: strings.TrimSuffix(e.Name, "/") + "/"}
			hdr.SetMode(fs.ModeDir | 0o755)
			if _, err := zw.CreateHeader(hdr); err != nil {
				t.Fatal(err)
			}
			continue
		}
		rc, err := e.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		_ = rc.Close()
		for name, b := range edit(e.Name, body) {
			hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
			hdr.SetMode(0o644)
			w, err := zw.CreateHeader(hdr)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(b)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// restoreBytes runs Restore straight on a backup zip's entries, without
// the CheckZip that OpenArchive runs first, so what Restore itself
// refuses is tested.
func restoreBytes(t *testing.T, archive []byte, opt Options) (string, error) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	_, err = Restore(dst, NewZipSource(zr), opt)
	return dst, err
}

func TestRestoreRefusesAnArchiveThatDoesNotMatchItsManifest(t *testing.T) {
	var buf bytes.Buffer
	if _, err := WriteZip(writeTree(t, tree), &buf, "test"); err != nil {
		t.Fatal(err)
	}
	good := buf.Bytes()
	if _, err := restoreBytes(t, good, Options{}); err != nil {
		t.Fatalf("untouched archive: %v", err)
	}
	cases := map[string]struct {
		edit func(string, []byte) map[string][]byte
		want string
	}{
		"changed content": {func(n string, b []byte) map[string][]byte {
			if n == "usage.jsonl" {
				b = bytes.Replace(b, []byte("3"), []byte("9"), 1)
			}
			return map[string][]byte{n: b}
		}, "content differs: usage.jsonl"},
		"missing file": {func(n string, b []byte) map[string][]byte {
			if n == "agents/alice/role.md" {
				return nil
			}
			return map[string][]byte{n: b}
		}, "missing: agents/alice/role.md"},
		"extra file": {func(n string, b []byte) map[string][]byte {
			out := map[string][]byte{n: b}
			if n == "usage.jsonl" {
				out["stowaway.txt"] = []byte("x")
			}
			return out
		}, "not in the manifest: stowaway.txt"},
		"no manifest": {func(n string, b []byte) map[string][]byte {
			if n == ManifestName {
				return nil
			}
			return map[string][]byte{n: b}
		}, "has no manifest"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := restoreBytes(t, tamper(t, good, c.edit), Options{})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to say %q", err, c.want)
			}
		})
	}
}

// A backup bigger than the room left on the data volume is refused, not
// cut short: by CheckZip from the manifest's sizes before anything is
// written, and by Restore itself before a file that would not fit. Each
// file fits on its own; their sum does not.
func TestABackupBiggerThanTheRoomLeftIsRefused(t *testing.T) {
	src := writeTree(t, map[string]string{"a.bin": strings.Repeat("x", 40), "b.bin": strings.Repeat("y", 40)})
	var buf bytes.Buffer
	if _, err := WriteZip(src, &buf, "test"); err != nil {
		t.Fatal(err)
	}
	opt := Options{MaxTotalBytes: 50}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckZip(zr, opt); !errors.Is(err, ErrNoRoom) {
		t.Errorf("CheckZip err = %v, want ErrNoRoom", err)
	}
	path := filepath.Join(t.TempDir(), "backup.zip")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenArchive(path, opt); !errors.Is(err, ErrNoRoom) {
		t.Errorf("OpenArchive err = %v, want ErrNoRoom", err)
	}
	dst, err := restoreBytes(t, buf.Bytes(), opt)
	if !errors.Is(err, ErrNoRoom) {
		t.Errorf("Restore err = %v, want ErrNoRoom", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "b.bin")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the file that did not fit was written: %v", err)
	}
	if _, err := restoreBytes(t, buf.Bytes(), Options{MaxTotalBytes: 80}); err != nil {
		t.Errorf("a backup that fits exactly: %v", err)
	}
}

// A symlink already in the destination must not carry a write outside it.
func TestRestoreStaysInsideTheDestination(t *testing.T) {
	outside := t.TempDir()
	src := writeTree(t, map[string]string{"link/evil.txt": "x"})
	var buf bytes.Buffer
	if _, err := WriteZip(src, &buf, "test"); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dst, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(dst, NewZipSource(zr), Options{}); err == nil {
		t.Error("the restore passed")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.txt")); err == nil {
		t.Error("a file was written outside the destination")
	}
}

func TestRestoreRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../escape.txt", "/etc/passwd", "a/../../escape.txt", ""} {
		if _, err := SafeTarget(t.TempDir(), name); err == nil {
			t.Errorf("SafeTarget(%q) accepted", name)
		}
	}
	for _, name := range []string{"a.md", "agents/alice/chat.jsonl", "dir/"} {
		if _, err := SafeTarget(t.TempDir(), name); err != nil {
			t.Errorf("SafeTarget(%q): %v", name, err)
		}
	}
}

func TestAuditWritesNothingAndCountsWhatIsThere(t *testing.T) {
	dir := writeTree(t, tree)
	before := snapshot(t, dir)
	inv, err := Audit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir); len(after) != len(before) {
		t.Errorf("audit changed the directory: %d entries before, %d after", len(before), len(after))
	}
	if len(inv.Problems) > 0 {
		t.Errorf("problems in a current-format tree: %v", inv.Problems)
	}
	want := map[string]int{
		"active": 1, "archived": 1, "chats": 1, "assignment_event": 1, "notice": 1, "open": 1, "usage": 1,
	}
	got := map[string]int{
		"active": inv.ActiveAgents, "archived": inv.ArchivedAgents, "chats": inv.ArchivedChats,
		"assignment_event": inv.Messages["assignment_event"], "notice": inv.Messages["notice"],
		"open": inv.Assignments["open"], "usage": inv.UsageRecords,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d\n%s", k, got[k], v, inv)
		}
	}
	if inv.UsageTokens != 7 {
		t.Errorf("usage tokens = %d, want 7", inv.UsageTokens)
	}
}
