package files

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// confinedFixture lays out two agents' storage roots side by side, the
// way core sees them, plus a "credentials" file outside both.
//
//	<tmp>/agents/alice/memory/artifacts/private/
//	<tmp>/agents/bob/memory/artifacts/private/secret.txt
//	<tmp>/claude-home/.credentials.json
func confinedFixture(t *testing.T) (alice, bob, creds string) {
	t.Helper()
	tmp := t.TempDir()
	alice = filepath.Join(tmp, "agents", "alice", "memory")
	bob = filepath.Join(tmp, "agents", "bob", "memory")
	for _, d := range []string{
		filepath.Join(alice, "artifacts", "private"),
		filepath.Join(bob, "artifacts", "private"),
		filepath.Join(tmp, "claude-home"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bob, "artifacts", "private", "secret.txt"), []byte("bob's secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	creds = filepath.Join(tmp, "claude-home", ".credentials.json")
	if err := os.WriteFile(creds, []byte("oauth-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	return alice, bob, creds
}

func readAll(t *testing.T, f *os.File) string {
	t.Helper()
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOpenNoFollowReadsARegularFile(t *testing.T) {
	alice, _, _ := confinedFixture(t)
	if err := os.WriteFile(filepath.Join(alice, "artifacts", "private", "a.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenNoFollow(alice, "artifacts/private/a.md")
	if err != nil {
		t.Fatalf("OpenNoFollow: %v", err)
	}
	if got := readAll(t, f); got != "mine" {
		t.Errorf("read %q, want %q", got, "mine")
	}
	b, err := ReadFileNoFollow(alice, "artifacts/private/a.md")
	if err != nil || string(b) != "mine" {
		t.Errorf("ReadFileNoFollow = %q, %v", b, err)
	}
}

// Every way a link can sit on the path is refused with ErrSymlink,
// whether it points at a sibling agent (relative, inside the volume)
// or at the credentials (absolute), and whether it is the file or a
// directory above it.
func TestOpenNoFollowRefusesSymlinks(t *testing.T) {
	alice, bob, creds := confinedFixture(t)
	priv := filepath.Join(alice, "artifacts", "private")
	// Relative link to the sibling agent's private file.
	if err := os.Symlink("../../../../bob/memory/artifacts/private/secret.txt", filepath.Join(priv, "x.txt")); err != nil {
		t.Fatal(err)
	}
	// Absolute link to the credentials.
	if err := os.Symlink(creds, filepath.Join(priv, "creds.json")); err != nil {
		t.Fatal(err)
	}
	// A symlinked intermediate directory.
	if err := os.Symlink(filepath.Join(bob, "artifacts", "private"), filepath.Join(priv, "bobdir")); err != nil {
		t.Fatal(err)
	}
	// A link that stays inside alice's own tree is still a link.
	if err := os.WriteFile(filepath.Join(priv, "real.md"), []byte("r"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.md", filepath.Join(priv, "own.md")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"artifacts/private/x.txt",
		"artifacts/private/creds.json",
		"artifacts/private/bobdir/secret.txt",
		"artifacts/private/own.md",
	} {
		f, err := OpenNoFollow(alice, rel)
		if err == nil {
			t.Errorf("%s: opened (%q), want ErrSymlink", rel, readAll(t, f))
			continue
		}
		if !errors.Is(err, ErrSymlink) {
			t.Errorf("%s: err = %v, want ErrSymlink", rel, err)
		}
	}
}

// A symlinked directory at the top of the walk (artifacts/ itself) is
// refused too, by the directory opener as much as the file opener.
func TestOpenDirNoFollowRefusesSymlinkedComponent(t *testing.T) {
	alice, bob, _ := confinedFixture(t)
	if err := os.Rename(filepath.Join(alice, "artifacts"), filepath.Join(alice, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(bob, "artifacts"), filepath.Join(alice, "artifacts")); err != nil {
		t.Fatal(err)
	}
	if r, err := OpenDirNoFollow(alice, "artifacts/private"); err == nil {
		_ = r.Close()
		t.Fatal("OpenDirNoFollow opened a path through a symlinked artifacts/")
	} else if !errors.Is(err, ErrSymlink) {
		t.Errorf("err = %v, want ErrSymlink", err)
	}
	if _, err := OpenNoFollow(alice, "artifacts/private/secret.txt"); !errors.Is(err, ErrSymlink) {
		t.Errorf("OpenNoFollow err = %v, want ErrSymlink", err)
	}
}

func TestOpenNoFollowRefusesDirectoriesAndEscapes(t *testing.T) {
	alice, _, _ := confinedFixture(t)
	if _, err := OpenNoFollow(alice, "artifacts/private"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("directory: err = %v, want ErrNotRegular", err)
	}
	if _, err := OpenNoFollow(alice, "artifacts/nope.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: err = %v, want ErrNotExist", err)
	}
	for _, rel := range []string{"../bob/memory/artifacts/private/secret.txt", "artifacts/../../../bob/memory/artifacts/private/secret.txt", "/etc/passwd"} {
		if f, err := OpenNoFollow(alice, rel); err == nil {
			t.Errorf("%s: opened (%q), want an error", rel, readAll(t, f))
		}
	}
}
