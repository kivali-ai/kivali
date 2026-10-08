package files

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"
)

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertRealDir(t *testing.T, p string) {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil || !info.IsDir() {
		t.Errorf("%s should be a real directory: %v, %v", p, info, err)
	}
}

// The published trees are mounted over artifacts/public and
// artifacts/shared, and the container runtime resolves a mount point
// inside the pod's filesystem: a link the agent planted there would
// carry the mount somewhere else. Sync, which runs before every pod is
// created, replaces such a link with a real directory, and makes the
// agent's published tree on the data volume.
func TestSyncReplacesSymlinkedPublishedMountPoints(t *testing.T) {
	tmp := t.TempDir()
	agentRoot := filepath.Join(tmp, "agents", "alice")
	root := filepath.Join(agentRoot, StoragePrefix)
	victim := victimDir(t, tmp)
	if err := os.MkdirAll(filepath.Join(root, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range PublishedMountPoints {
		if err := os.Symlink(victim, filepath.Join(root, filepath.FromSlash(d))); err != nil {
			t.Fatal(err)
		}
	}
	if err := Sync(BootstrapOptions{AgentRoot: agentRoot, DataDir: tmp, Slug: "alice"}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	assertVictimUntouched(t, victim)
	for _, d := range PublishedMountPoints {
		assertRealDir(t, filepath.Join(root, filepath.FromSlash(d)))
	}
	assertRealDir(t, PublishedDir(tmp, "alice"))
}

// Both mount points are core-managed, so the pod mounts them and Sync
// repairs them like the farms.
func TestPublishedMountPointsAreCoreManaged(t *testing.T) {
	managed := map[string]bool{}
	for _, d := range CoreManagedDirs {
		managed[d] = true
	}
	for _, d := range PublishedMountPoints {
		if !managed[d] {
			t.Errorf("%s is not in CoreManagedDirs", d)
		}
	}
}

func TestEnsurePublishedDirReplacesLink(t *testing.T) {
	tmp := t.TempDir()
	victim := victimDir(t, tmp)
	if err := os.MkdirAll(PublishedRoot(tmp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, PublishedDir(tmp, "alice")); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublishedDir(tmp, "alice"); err != nil {
		t.Fatal(err)
	}
	assertRealDir(t, PublishedDir(tmp, "alice"))
	assertVictimUntouched(t, victim)
	if err := EnsurePublishedDir(tmp, "../x"); err == nil {
		t.Error("a slug with a slash was accepted")
	}
}

func TestCleanPublishedPath(t *testing.T) {
	ok := map[string]string{
		"x.md":                               "x.md",
		"specs/api.md":                       "specs/api.md",
		"/files/artifacts/public/specs/a.md": "specs/a.md",
		"artifacts/public/a.md":              "a.md",
		"a//b/":                              "a/b",
	}
	for in, want := range ok {
		got, err := CleanPublishedPath(in)
		if err != nil || got != want {
			t.Errorf("CleanPublishedPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "/etc/passwd", "../x", "a/../../x", ".hidden", "a/.git/x", "a\\b", "a\x00b", "/files/artifacts/public", strings.Repeat("a", PublishedNameMaxBytes+1), strings.Repeat("a/", PublishedPathMaxBytes)} {
		if got, err := CleanPublishedPath(in); err == nil {
			t.Errorf("CleanPublishedPath(%q) = %q, want an error", in, got)
		}
	}
}

var testLimits = PublishLimits{MaxFileBytes: 100, MaxTotalBytes: 150, MaxFiles: 5}

// copyLimits leave CopyPublished room for every copy a test makes.
var copyLimits = PublishLimits{MaxFileBytes: 100, MaxTotalBytes: 1000}

func openTestRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestPlanPublishFileAndDirectory(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "artifacts/private/spec.md"), "spec")
	mustWrite(t, filepath.Join(ws, "artifacts/private/pack/a.md"), "a")
	mustWrite(t, filepath.Join(ws, "artifacts/private/pack/sub/b.csv"), "b")
	mustWrite(t, filepath.Join(ws, "artifacts/private/pack/.draft.swp"), "x")
	mustWrite(t, filepath.Join(ws, "artifacts/private/pack/.git/config"), "x")
	root := openTestRoot(t, ws)

	items, skipped, err := PlanPublish(root, "artifacts/private/spec.md", "spec.md", testLimits)
	if err != nil {
		t.Fatal(err)
	}
	if want := []PublishItem{{Src: "artifacts/private/spec.md", Dest: "spec.md", Size: 4}}; !reflect.DeepEqual(items, want) || skipped != nil {
		t.Errorf("file plan = %v, %v", items, skipped)
	}

	items, skipped, err = PlanPublish(root, "artifacts/private/pack", "kit", testLimits)
	if err != nil {
		t.Fatal(err)
	}
	var dests []string
	for _, it := range items {
		dests = append(dests, it.Dest)
	}
	if !reflect.DeepEqual(dests, []string{"kit/a.md", "kit/sub/b.csv"}) {
		t.Errorf("dir plan dests = %v", dests)
	}
	if !reflect.DeepEqual(skipped, []string{"artifacts/private/pack/.draft.swp", "artifacts/private/pack/.git"}) {
		t.Errorf("skipped = %v", skipped)
	}

	// A directory's contents at the top of the published tree.
	items, _, err = PlanPublish(root, "artifacts/private/pack", "", testLimits)
	if err != nil || len(items) != 2 || items[0].Dest != "a.md" {
		t.Errorf("root dest plan = %v, %v", items, err)
	}
	// A file needs a dest.
	if _, _, err := PlanPublish(root, "artifacts/private/spec.md", "", testLimits); err == nil {
		t.Error("a file with no dest was planned")
	}
	// Dot-names are refused when named.
	if _, _, err := PlanPublish(root, "artifacts/private/pack/.draft.swp", "x", testLimits); err == nil {
		t.Error("a dotfile named directly was planned")
	}
	if _, _, err := PlanPublish(root, "artifacts/private/missing.md", "x", testLimits); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing source: %v", err)
	}
}

// A symbolic link anywhere refuses the whole publish, naming it: a link
// the agent planted would otherwise read whatever it points at with
// core's rights.
func TestPlanPublishRefusesSymlinks(t *testing.T) {
	tmp := t.TempDir()
	victim := victimDir(t, tmp)
	ws := filepath.Join(tmp, "ws")
	mustWrite(t, filepath.Join(ws, "pack/a.md"), "a")
	if err := os.Symlink(filepath.Join(victim, "keep.txt"), filepath.Join(ws, "pack/leak.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(victim, "keep.txt"), filepath.Join(ws, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(ws, "linkdir")); err != nil {
		t.Fatal(err)
	}
	root := openTestRoot(t, ws)
	for src, name := range map[string]string{"pack": "pack/leak.txt", "leak.txt": "leak.txt", "linkdir": "linkdir", "linkdir/keep.txt": "linkdir"} {
		_, _, err := PlanPublish(root, src, "x", testLimits)
		if err == nil || !strings.Contains(err.Error(), "symbolic link") || !strings.Contains(err.Error(), name) {
			t.Errorf("PlanPublish(%s) = %v; want a refusal naming %s", src, err, name)
		}
	}
}

func TestPlanPublishRefusesSpecialFiles(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "pack/a.md"), "a")
	if err := syscall.Mkfifo(filepath.Join(ws, "pack/pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	root := openTestRoot(t, ws)
	if _, _, err := PlanPublish(root, "pack", "x", testLimits); err == nil || !strings.Contains(err.Error(), "pack/pipe") {
		t.Errorf("a FIFO in the tree: %v", err)
	}
}

func TestPlanPublishLimits(t *testing.T) {
	ws := t.TempDir()
	mustWrite(t, filepath.Join(ws, "big.bin"), strings.Repeat("x", 101))
	mustWrite(t, filepath.Join(ws, "many/1"), strings.Repeat("x", 60))
	mustWrite(t, filepath.Join(ws, "many/2"), strings.Repeat("x", 60))
	mustWrite(t, filepath.Join(ws, "many/3"), strings.Repeat("x", 60))
	for i := 0; i < 6; i++ {
		mustWrite(t, filepath.Join(ws, "count", string(rune('a'+i))), "x")
	}
	root := openTestRoot(t, ws)
	if _, _, err := PlanPublish(root, "big.bin", "b", testLimits); err == nil || !strings.Contains(err.Error(), "per file") {
		t.Errorf("per-file limit: %v", err)
	}
	if _, _, err := PlanPublish(root, "many", "m", testLimits); err == nil || !strings.Contains(err.Error(), "per call") {
		t.Errorf("per-call bytes: %v", err)
	}
	if _, _, err := PlanPublish(root, "count", "c", testLimits); err == nil || !strings.Contains(err.Error(), "files") {
		t.Errorf("per-call count: %v", err)
	}
}

func TestCopyPublishedReplacesAndRefusesDirectoryInTheWay(t *testing.T) {
	ws := t.TempDir()
	pub := t.TempDir()
	mustWrite(t, filepath.Join(ws, "a.md"), "new")
	mustWrite(t, filepath.Join(pub, "a.md"), "old")
	mustWrite(t, filepath.Join(pub, "dir/x"), "x")
	src := openTestRoot(t, ws)
	dst := openTestRoot(t, pub)
	written, err := CopyPublished(src, dst, []PublishItem{{Src: "a.md", Dest: "a.md"}, {Src: "a.md", Dest: "deep/er/a.md"}}, copyLimits)
	if err != nil || !reflect.DeepEqual(written, []string{"a.md", "deep/er/a.md"}) {
		t.Fatalf("CopyPublished = %v, %v", written, err)
	}
	if got := mustRead(t, filepath.Join(pub, "a.md")); got != "new" {
		t.Errorf("a.md = %q, want the new bytes", got)
	}
	if got := mustRead(t, filepath.Join(pub, "deep/er/a.md")); got != "new" {
		t.Errorf("deep/er/a.md = %q", got)
	}
	if _, err := CopyPublished(src, dst, []PublishItem{{Src: "a.md", Dest: "dir"}}, copyLimits); err == nil || !strings.Contains(err.Error(), "unpublish it first") {
		t.Errorf("a directory in the way: %v", err)
	}
	if _, err := CopyPublished(src, dst, []PublishItem{{Src: "a.md", Dest: "a.md/x"}}, copyLimits); err == nil || !strings.Contains(err.Error(), "in the way") {
		t.Errorf("a file in the way: %v", err)
	}
	// Bytes beyond the limit are refused whatever the plan measured.
	if _, err := CopyPublished(src, dst, []PublishItem{{Src: "a.md", Dest: "small.md", Size: 1}}, PublishLimits{MaxFileBytes: 2, MaxTotalBytes: 100}); err == nil || !strings.Contains(err.Error(), "limit per file") {
		t.Errorf("a file grown past the limit: %v", err)
	}
	// And so are bytes beyond the call's total: each file is under the
	// per-file limit, but together they grew past the total.
	written, err = CopyPublished(src, dst, []PublishItem{{Src: "a.md", Dest: "t1.md", Size: 1}, {Src: "a.md", Dest: "t2.md", Size: 1}}, PublishLimits{MaxFileBytes: 100, MaxTotalBytes: 5})
	if err == nil || !strings.Contains(err.Error(), "limit per call") || !reflect.DeepEqual(written, []string{"t1.md"}) {
		t.Errorf("files grown past the total: %v, %v", written, err)
	}
	if _, err := os.Lstat(filepath.Join(pub, "t2.md")); err == nil {
		t.Error("the copy past the total was left in place")
	}
	if _, err := os.Lstat(filepath.Join(pub, "small.md")); err == nil {
		t.Error("an oversize copy was left in place")
	}
	entries, _ := os.ReadDir(pub)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// A name of the longest length CleanPublishedPath accepts publishes:
// the temp file beside it must fit the filesystem's name limit too.
func TestCopyPublishedLongestName(t *testing.T) {
	ws := t.TempDir()
	pub := t.TempDir()
	mustWrite(t, filepath.Join(ws, "a.md"), "x")
	long := "a" + strings.Repeat("é", PublishedNameMaxBytes/2) // 255 bytes, mid-rune at the cut
	dest, err := CleanPublishedPath("kit/" + long)
	if err != nil {
		t.Fatal(err)
	}
	written, err := CopyPublished(openTestRoot(t, ws), openTestRoot(t, pub), []PublishItem{{Src: "a.md", Dest: dest}}, copyLimits)
	if err != nil || len(written) != 1 {
		t.Fatalf("CopyPublished(%d-byte name) = %v, %v", len(long), written, err)
	}
	if got := mustRead(t, filepath.Join(pub, "kit", long)); got != "x" {
		t.Errorf("published = %q", got)
	}
	if b := tmpBase(long); len(b) > tmpBaseMaxBytes || !utf8.ValidString(b) {
		t.Errorf("tmpBase = %d bytes, valid UTF-8 %v", len(b), utf8.ValidString(b))
	}
}

func TestRemovePublished(t *testing.T) {
	pub := t.TempDir()
	mustWrite(t, filepath.Join(pub, "a.md"), "a")
	mustWrite(t, filepath.Join(pub, "kit/b.md"), "b")
	mustWrite(t, filepath.Join(pub, "kit/sub/c.csv"), "c")
	dst := openTestRoot(t, pub)
	gone, err := RemovePublished(dst, "kit")
	if err != nil || !reflect.DeepEqual(gone, []string{"kit/b.md", "kit/sub/c.csv"}) {
		t.Fatalf("RemovePublished(kit) = %v, %v", gone, err)
	}
	if _, err := os.Lstat(filepath.Join(pub, "kit")); err == nil {
		t.Error("kit/ still there")
	}
	gone, err = RemovePublished(dst, "a.md")
	if err != nil || !reflect.DeepEqual(gone, []string{"a.md"}) {
		t.Fatalf("RemovePublished(a.md) = %v, %v", gone, err)
	}
	if _, err := RemovePublished(dst, "a.md"); err == nil || !strings.Contains(err.Error(), "not published") {
		t.Errorf("removing twice: %v", err)
	}
}

func TestEmptyPublishedKeepsOwnerDirs(t *testing.T) {
	tmp := t.TempDir()
	victim := victimDir(t, tmp)
	mustWrite(t, filepath.Join(PublishedDir(tmp, "alice"), "kit/a.md"), "a")
	mustWrite(t, filepath.Join(PublishedDir(tmp, "alice"), "b.md"), "b")
	if err := os.Symlink(victim, filepath.Join(PublishedRoot(tmp), "bob")); err != nil {
		t.Fatal(err)
	}
	if err := EmptyPublished(tmp); err != nil {
		t.Fatal(err)
	}
	assertRealDir(t, PublishedDir(tmp, "alice"))
	if entries, _ := os.ReadDir(PublishedDir(tmp, "alice")); len(entries) != 0 {
		t.Errorf("public/alice not emptied: %d entries", len(entries))
	}
	if _, err := os.Lstat(PublishedDir(tmp, "bob")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a link at public/bob was kept: %v", err)
	}
	assertVictimUntouched(t, victim)
	if err := EmptyPublished(t.TempDir()); err != nil {
		t.Errorf("no public/ at all: %v", err)
	}
}

// Core reads body_path through a Backend on the agent's own tree;
// Mounts make /files/artifacts/public reach the published tree there,
// read-only, as the pod's mount does.
func TestBackendMountsServePublishedReadOnly(t *testing.T) {
	tmp := t.TempDir()
	d := Dispatcher{DataDir: tmp}
	storage := StorageRoot(filepath.Join(tmp, "agents", "alice"))
	if err := os.MkdirAll(filepath.Join(storage, "artifacts/public"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(PublishedDir(tmp, "alice"), "spec.md"), "published spec")
	mustWrite(t, filepath.Join(PublishedDir(tmp, "bob"), "b.md"), "bob's")
	read := d.ResolveBodyPathFor("alice")
	if got, err := read("/files/artifacts/public/spec.md"); err != nil || got != "published spec" {
		t.Errorf("own published file = %q, %v", got, err)
	}
	if got, err := read("/files/artifacts/shared/bob/b.md"); err != nil || got != "bob's" {
		t.Errorf("peer published file = %q, %v", got, err)
	}
	b := d.BackendFor("alice")
	body, isErr, err := Dispatch(b, ToolCreate, []byte(`{"path":"/files/artifacts/public/new.md","file_text":"x"}`))
	if err != nil || !isErr {
		t.Errorf("a write into the published tree = %q, %v, %v; want refused", body, isErr, err)
	}
	if _, err := os.Lstat(filepath.Join(PublishedDir(tmp, "alice"), "new.md")); err == nil {
		t.Error("file_create wrote into the published tree")
	}
}
