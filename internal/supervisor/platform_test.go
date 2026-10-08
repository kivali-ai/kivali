package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

// The backup confinement holds with either separator: a relative path
// that climbs out is refused whatever the OS writes between the dots.
func TestWithin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	sep := string(filepath.Separator)
	for dir, want := range map[string]bool{
		root:                          true,
		filepath.Join(root, "a", "b"): true,
		filepath.Join(root, "..data"): true,
		filepath.Dir(root):            false,
		filepath.Join(filepath.Dir(root), "other"): false,
		filepath.Join(filepath.Dir(root), "root2"): false,
		sep: false,
	} {
		if got := within(root, dir); got != want {
			t.Errorf("within(%s, %s) = %v, want %v", root, dir, got, want)
		}
	}
}

func TestFileURLPath(t *testing.T) {
	cases := []struct{ url, goos, want string }{
		{"file:///Users/x/release.json", "darwin", "/Users/x/release.json"},
		{"file:///C:/x/release.json", "windows", "C:/x/release.json"},
		{"file:///c:/x", "windows", "c:/x"},
		{"file://server/share/release.json", "windows", "//server/share/release.json"},
		{"file://localhost/C:/x", "windows", "C:/x"},
	}
	for _, c := range cases {
		u, err := url.Parse(c.url)
		if err != nil {
			t.Fatal(err)
		}
		if got := fileURLPath(u, c.goos); got != c.want {
			t.Errorf("fileURLPath(%s, %s) = %q, want %q", c.url, c.goos, got, c.want)
		}
	}
	p := filepath.Join(t.TempDir(), "release.json")
	got, ok := isLocal("file://" + filepath.ToSlash(p))
	if filepath.VolumeName(p) != "" {
		got, ok = isLocal("file:///" + filepath.ToSlash(p))
	}
	if !ok || got != p {
		t.Fatalf("isLocal of a file URL of %s = %q, %v", p, got, ok)
	}
	if _, ok := isLocal("https://example.com/release.json"); ok {
		t.Fatal("an https feed is local")
	}
}

func TestAssetNames(t *testing.T) {
	feed := filepath.Join(t.TempDir(), "release.json")
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, `..\x`, "../x"} {
		if _, err := assetRef(feed, Asset{Name: bad}); err == nil {
			t.Errorf("asset name %q accepted", bad)
		}
	}
	got, err := assetRef(feed, Asset{Name: "kivali-0.16.0.tgz"})
	if err != nil || got != filepath.Join(filepath.Dir(feed), "kivali-0.16.0.tgz") {
		t.Fatalf("assetRef = %q, %v", got, err)
	}
	// A URL asset's name is still the downloaded file's name.
	h := newHarness(t)
	_, err = h.sup.download(context.Background(), feed, Asset{Name: `..\evil`, URL: "file:///nonexistent", SHA256: strings.Repeat("0", 64)}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "asset name") {
		t.Fatalf("download with a separator in its name: %v", err)
	}
}

// The release's image bundle is chosen by the image's architecture,
// never a fixed one.
func TestUpgradeNeedsTheImagesArchitecture(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	dir := filepath.Join(t.TempDir(), "rel")
	feed := writeRelease(t, dir, "0.16.0", "v0.16.0")
	b, err := os.ReadFile(feed)
	if err != nil {
		t.Fatal(err)
	}
	var rel Release
	if err := json.Unmarshal(b, &rel); err != nil {
		t.Fatal(err)
	}
	rel.Images = map[string]Asset{"linux/other": rel.Images["linux/"+fakeArch]}
	b, _ = json.Marshal(rel)
	if err := os.WriteFile(feed, b, 0o644); err != nil {
		t.Fatal(err)
	}
	err = h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
	if err == nil || !strings.Contains(err.Error(), "no images for linux/"+fakeArch) {
		t.Fatalf("upgrade: %v", err)
	}
}

func TestNewNeedsABackend(t *testing.T) {
	if _, err := New(Options{ConfigDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "backend") {
		t.Fatalf("New without a backend: %v", err)
	}
}

// The snapshot sits next to the data disk, wherever the backend keeps
// it, and an upgrade's free-space check measures that directory.
func TestSnapshotFollowsTheDataDisk(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	var snapped string
	h.snapshot = func(src, dst string) error {
		snapped = dst
		return h.sup.o.Host.(testHost).Host.Snapshot(src, dst)
	}
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")}, h.logf()); err != nil {
		t.Fatal(err)
	}
	if snapped != h.snapPath() || filepath.Dir(snapped) != filepath.Dir(h.dataPath()) {
		t.Fatalf("snapshot at %s, data disk at %s", snapped, h.dataPath())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.freeDirs) != 1 || h.freeDirs[0] != filepath.Dir(h.dataPath()) {
		t.Fatalf("free space measured in %v, want the data disk's directory %s", h.freeDirs, filepath.Dir(h.dataPath()))
	}
}

// Every write of local.json (the journal) and the rollback's swap go
// through the host's durable Rename.
func TestJournalWritesAreDurableRenames(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	count := func(target string) int {
		h.mu.Lock()
		defer h.mu.Unlock()
		n := 0
		for _, r := range h.renamed {
			if r == target {
				n++
			}
		}
		h.renamed = nil
		return n
	}
	local := filepath.Join(h.dir, "local.json")
	if count(local) == 0 {
		t.Fatal("up saved local.json without the host's Rename")
	}
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "a"), "0.16.0", "v0.16.0")}, h.logf()); err != nil {
		t.Fatal(err)
	}
	// prepared, quiescing, snapshotting, snapshotted, applying,
	// committed, cleared: at least one durable write each.
	if n := count(local); n < 7 {
		t.Fatalf("%d durable local.json writes during an upgrade", n)
	}
	h.vm.broken["v0.17.0"] = true
	if err := h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: writeRelease(t, filepath.Join(t.TempDir(), "b"), "0.17.0", "v0.17.0")}, h.logf()); err == nil {
		t.Fatal("broken upgrade succeeded")
	}
	if count(h.dataPath()) != 1 {
		t.Fatal("the rollback did not swap the snapshot back through the host's Rename")
	}
}

// local.json carries a key nothing reads and an open journal that names
// its snapshot at a path of its own. Recovery follows the journal's
// path, and the next save drops the unknown key.
func TestRecoverFollowsTheJournalsSnapshotPath(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	before := h.disk()[ManifestRel]
	if err := h.sup.Down(context.Background(), h.logf()); err != nil {
		t.Fatal(err)
	}
	oldSnap := filepath.Join(h.dir, "data.img.upgrade")
	if err := host.Default().Snapshot(h.dataPath(), oldSnap); err != nil {
		t.Fatal(err)
	}
	fs := h.disk()
	fs[ManifestRel] = "half-applied"
	if err := writeDisk(h.dataPath(), fs); err != nil {
		t.Fatal(err)
	}
	old := fmt.Sprintf(`{"vm_image":"0.15.0","kivali":"0.16.0","unknown_key":"data.img","data_disk_formatted":true,`+
		`"memory_mb":4096,"cpus":4,"port":%d,"feed":"https://example.com/release.json","last_check":null,"last_check_ok":false,`+
		`"upgrade":{"from":"0.15.0","to":"0.16.0","step":"applying","snapshot":%q,"snapshot_complete":true,"started":"2026-10-01T12:00:00Z"}}`,
		h.port, oldSnap)
	local := filepath.Join(h.dir, "local.json")
	if err := os.WriteFile(local, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	h.restart()
	h.mustUp(UpOptions{})
	st := h.sup.State()
	if exists(oldSnap) || st.Upgrade != nil || st.Kivali != "0.15.0" || h.disk()[ManifestRel] != before {
		t.Fatalf("after recovery: snapshot %v, state %+v", exists(oldSnap), st)
	}
	b, err := os.ReadFile(local)
	if err != nil || strings.Contains(string(b), `"unknown_key":`) || !strings.Contains(string(b), `"data_disk_formatted": true`) {
		t.Fatalf("local.json after recovery: %s %v", b, err)
	}
}
