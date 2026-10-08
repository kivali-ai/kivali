package files

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newBackend(t *testing.T) *Backend {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"project", "past-chats", "notes"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &Backend{Root: root}
}

func TestResolveAcceptsAllPathForms(t *testing.T) {
	b := newBackend(t)
	cases := []string{"/files/notes/a.md", "files/notes/a.md", "/notes/a.md", "notes/a.md"}
	for _, in := range cases {
		got, err := b.Resolve(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if want := filepath.Join(b.Root, "notes", "a.md"); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func TestResolveRejectsTraversal(t *testing.T) {
	b := newBackend(t)
	bad := []string{"/files/../outside", "notes/../../outside", "../escape"}
	for _, p := range bad {
		if _, err := b.Resolve(p); err == nil {
			t.Errorf("%q: expected error", p)
		}
	}
}

func TestCreateAndView(t *testing.T) {
	b := newBackend(t)
	if err := b.Create("/files/notes/hello.md", "hello\nworld\n"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	text, err := b.View("/files/notes/hello.md", ViewOptions{})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	body := stripHeader(text)
	if body != "hello\nworld\n" {
		t.Errorf("body = %q", body)
	}
	// Header should announce both line count and byte size.
	if !strings.HasPrefix(text, "[file: 2 lines, 12 bytes]") {
		t.Errorf("missing/wrong header: %q", text)
	}
}

func TestViewLineRange(t *testing.T) {
	b := newBackend(t)
	_ = b.Create("/files/notes/x.md", "one\ntwo\nthree\nfour\n")
	got, err := b.View("/files/notes/x.md", ViewOptions{ViewRange: []int{2, 3}})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if stripHeader(got) != "two\nthree" {
		t.Errorf("body = %q", got)
	}
	if !strings.Contains(got, "showing 2-3") {
		t.Errorf("header missing range: %q", got)
	}
}

func TestViewOffsetLimit(t *testing.T) {
	b := newBackend(t)
	_ = b.Create("/files/notes/x.md", "one\ntwo\nthree\nfour\nfive\n")
	got, err := b.View("/files/notes/x.md", ViewOptions{Offset: 2, Limit: 2})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if stripHeader(got) != "two\nthree" {
		t.Errorf("body = %q", got)
	}
	if !strings.Contains(got, "showing 2-3") {
		t.Errorf("header missing range: %q", got)
	}
	// Offset alone (no limit) reads to end.
	got2, _ := b.View("/files/notes/x.md", ViewOptions{Offset: 4})
	if !strings.Contains(stripHeader(got2), "four\nfive") {
		t.Errorf("offset-only: %q", got2)
	}
}

func TestViewOffsetPastEnd(t *testing.T) {
	b := newBackend(t)
	_ = b.Create("/files/notes/x.md", "one\ntwo\n")
	got, err := b.View("/files/notes/x.md", ViewOptions{Offset: 99, Limit: 5})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if stripHeader(got) != "" {
		t.Errorf("body = %q, want empty", got)
	}
	if !strings.Contains(got, "empty range") {
		t.Errorf("header missing empty marker: %q", got)
	}
}

func TestViewGrep(t *testing.T) {
	b := newBackend(t)
	_ = b.Create("/files/notes/x.md", "alpha\nbeta\ngamma\nbetatest\ndelta\n")
	got, err := b.View("/files/notes/x.md", ViewOptions{Grep: "^beta"})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	body := stripHeader(got)
	if body != "2:beta\n4:betatest" {
		t.Errorf("body = %q", body)
	}
	if !strings.Contains(got, `grep "^beta": 2 matches`) {
		t.Errorf("header missing match count: %q", got)
	}
}

func TestViewGrepContext(t *testing.T) {
	b := newBackend(t)
	_ = b.Create("/files/notes/x.md", "a\nb\nMATCH\nc\nd\ne\nMATCH2\nf\n")
	got, err := b.View("/files/notes/x.md", ViewOptions{Grep: "MATCH", GrepContext: 1})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	body := stripHeader(got)
	// First hunk (lines 2-4) and second hunk (lines 6-8) are
	// non-adjacent, so a "--" separator is expected.
	want := "2:b\n3:MATCH\n4:c\n--\n6:e\n7:MATCH2\n8:f"
	if body != want {
		t.Errorf("body = %q\nwant  %q", body, want)
	}
}

func TestViewGrepInvalidRegex(t *testing.T) {
	b := newBackend(t)
	_ = b.Create("/files/notes/x.md", "a\nb\n")
	if _, err := b.View("/files/notes/x.md", ViewOptions{Grep: "[unclosed"}); err == nil {
		t.Error("expected error for invalid regex")
	}
}

func TestViewDirListing(t *testing.T) {
	b := newBackend(t)
	_ = b.Create("/files/notes/a.md", "a")
	_ = b.Create("/files/notes/b.md", "b")
	text, err := b.View("/files/notes/", ViewOptions{})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if !strings.Contains(text, "a.md") || !strings.Contains(text, "b.md") {
		t.Errorf("listing missing entries: %q", text)
	}
}

// stripHeader removes the one-line "[file: ...]\n" preamble so existing
// tests can still assert against raw bodies.
func stripHeader(s string) string {
	if !strings.HasPrefix(s, "[file:") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return ""
}

func TestReadOnlyEnforcement(t *testing.T) {
	b := newBackend(t)
	for _, p := range []string{"/files/project/foo", "/files/past-chats/x/chat.jsonl"} {
		if err := b.Create(p, "x"); !errors.Is(err, ErrReadOnly) {
			t.Errorf("Create(%q) err = %v, want ErrReadOnly", p, err)
		}
		if err := b.Delete(p); !errors.Is(err, ErrReadOnly) {
			t.Errorf("Delete(%q) err = %v, want ErrReadOnly", p, err)
		}
	}
	// Cross-subtree rename is also blocked.
	_ = b.Create("/files/notes/a.md", "x")
	if err := b.Rename("/files/notes/a.md", "/files/project/a.md"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("cross-RO rename err = %v", err)
	}
}

func TestStrReplaceUniqueOnly(t *testing.T) {
	b := newBackend(t)
	_ = b.Create("/files/notes/x.md", "hello world hello")
	if err := b.StrReplace("/files/notes/x.md", "hello", "hi"); err == nil {
		t.Error("expected error for non-unique match")
	}
	if err := b.StrReplace("/files/notes/x.md", "world", "earth"); err != nil {
		t.Fatalf("StrReplace: %v", err)
	}
	got, _ := b.View("/files/notes/x.md", ViewOptions{})
	if stripHeader(got) != "hello earth hello" {
		t.Errorf("got %q", got)
	}
}

func TestInsertAtLine(t *testing.T) {
	b := newBackend(t)
	_ = b.Create("/files/notes/x.md", "a\nb\nc")
	if err := b.Insert("/files/notes/x.md", 1, "inserted"); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, _ := b.View("/files/notes/x.md", ViewOptions{})
	if stripHeader(got) != "a\ninserted\nb\nc" {
		t.Errorf("got %q", got)
	}
}

func TestDeleteRejectsDirectory(t *testing.T) {
	b := newBackend(t)
	_ = os.MkdirAll(filepath.Join(b.Root, "notes", "sub"), 0o755)
	if err := b.Delete("/files/notes/sub"); err == nil {
		t.Error("expected error on directory delete")
	}
}

// Agents trained on shell paths sometimes pre-escape spaces or wrap
// the path in quotes. resolveExisting falls back to unescapeShellPath
// when the literal lookup misses, so the file_view UX matches the
// agent's intent rather than failing with "files: not found".
func TestViewWithShellEscapedSpaces(t *testing.T) {
	b := newBackend(t)
	name := "Screenshot 2026-05-06 at 9.32.27 PM.png"
	if err := b.Create("/files/notes/"+name, "data"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cases := []string{
		`/files/notes/Screenshot\ 2026-05-06\ at\ 9.32.27\ PM.png`,
		`"/files/notes/Screenshot 2026-05-06 at 9.32.27 PM.png"`,
		`'/files/notes/Screenshot 2026-05-06 at 9.32.27 PM.png'`,
	}
	for _, p := range cases {
		got, err := b.View(p, ViewOptions{})
		if err != nil {
			t.Errorf("View(%q): %v", p, err)
			continue
		}
		if !strings.Contains(got, "data") {
			t.Errorf("View(%q) body = %q", p, got)
		}
	}
}

// Literal-first: a real file whose name actually contains "\<space>"
// keeps resolving to itself, even though unescapeShellPath would
// rewrite it. This is the safety case the fallback design protects.
// Files have to be planted via os.WriteFile here because Create
// applies unescapeShellPath up-front.
func TestViewLiteralBackslashWins(t *testing.T) {
	b := newBackend(t)
	literal := filepath.Join(b.Root, "notes", `weird\ name.txt`)
	if err := os.WriteFile(literal, []byte("literal"), 0o644); err != nil {
		t.Fatalf("write literal: %v", err)
	}
	unescaped := filepath.Join(b.Root, "notes", "weird name.txt")
	if err := os.WriteFile(unescaped, []byte("unescaped"), 0o644); err != nil {
		t.Fatalf("write unescaped: %v", err)
	}
	got, err := b.View(`/files/notes/weird\ name.txt`, ViewOptions{})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if !strings.Contains(got, "literal") {
		t.Errorf("expected literal-first to win, body = %q", got)
	}
}

// Agents that see /files/attachments/Foo Bar.png in the prompt sometimes
// round-trip it as /files/attachments/Foo%20Bar.png — the path visually
// resembles a URL, and percent-encoding the space is the model's
// well-meaning "fix." resolveExisting falls back to url.PathUnescape so
// these calls land on the same on-disk symlink as the literal form.
func TestViewWithURLEncodedSpaces(t *testing.T) {
	b := newBackend(t)
	name := "Screenshot 2026-05-06 at 9.32.27 PM.png"
	if err := b.Create("/files/notes/"+name, "data"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cases := []string{
		"/files/notes/Screenshot%202026-05-06%20at%209.32.27%20PM.png",
		// Mixed encoding: some spaces literal, some encoded.
		"/files/notes/Screenshot 2026-05-06 at 9.32.27%20PM.png",
	}
	for _, p := range cases {
		got, err := b.View(p, ViewOptions{})
		if err != nil {
			t.Errorf("View(%q): %v", p, err)
			continue
		}
		if !strings.Contains(got, "data") {
			t.Errorf("View(%q) body = %q", p, got)
		}
	}
}

// Literal-first protects a real file whose name contains "%20" as
// literal characters: the on-disk name wins, even though
// unescapeShellPath would decode it. Files have to be planted via
// os.WriteFile here because Create applies unescapeShellPath up-front.
func TestViewLiteralPercentEncodingWins(t *testing.T) {
	b := newBackend(t)
	literal := filepath.Join(b.Root, "notes", "weird%20name.txt")
	if err := os.WriteFile(literal, []byte("literal"), 0o644); err != nil {
		t.Fatalf("write literal: %v", err)
	}
	decoded := filepath.Join(b.Root, "notes", "weird name.txt")
	if err := os.WriteFile(decoded, []byte("decoded"), 0o644); err != nil {
		t.Fatalf("write decoded: %v", err)
	}
	got, err := b.View("/files/notes/weird%20name.txt", ViewOptions{})
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	if !strings.Contains(got, "literal") {
		t.Errorf("expected literal-first to win, body = %q", got)
	}
}

// File creation unescapes up-front so file_view of the same
// shell-escaped form (or the natural-spaces form) round-trips.
func TestCreateUnescapesShellPath(t *testing.T) {
	b := newBackend(t)
	if err := b.Create(`/files/notes/Foo\ Bar.txt`, "x"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.Root, "notes", "Foo Bar.txt")); err != nil {
		t.Errorf("expected unescaped file on disk: %v", err)
	}
}

// Copy mirrors the read-from-anywhere / write-to-writable split: the
// source can sit under any readable subtree (including read-only
// project/, attachments/, etc.), but the destination must be writable.
func TestCopyReadOnlySourceToWritableDest(t *testing.T) {
	b := newBackend(t)
	_ = os.MkdirAll(filepath.Join(b.Root, "project"), 0o755)
	_ = os.MkdirAll(filepath.Join(b.Root, "artifacts", "private"), 0o755)
	if err := os.WriteFile(filepath.Join(b.Root, "project", "src.csv"), []byte("rows"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Copy("/files/project/src.csv", "/files/artifacts/private/src.csv"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(b.Root, "artifacts", "private", "src.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "rows" {
		t.Errorf("dest body = %q", got)
	}
	// Source must remain.
	if _, err := os.Stat(filepath.Join(b.Root, "project", "src.csv")); err != nil {
		t.Errorf("source disappeared: %v", err)
	}
}

func TestCopyToReadOnlyRejected(t *testing.T) {
	b := newBackend(t)
	_ = b.Create("/files/notes/a.md", "x")
	if err := b.Copy("/files/notes/a.md", "/files/project/a.md"); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Copy to RO err = %v, want ErrReadOnly", err)
	}
}

func TestCopyMissingSource(t *testing.T) {
	b := newBackend(t)
	if err := b.Copy("/files/notes/missing.md", "/files/notes/dest.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Copy missing source err = %v, want ErrNotFound", err)
	}
}

func TestCopyRejectsDirectory(t *testing.T) {
	b := newBackend(t)
	_ = os.MkdirAll(filepath.Join(b.Root, "notes", "sub"), 0o755)
	if err := b.Copy("/files/notes/sub", "/files/notes/sub-copy"); err == nil {
		t.Error("expected error on directory copy")
	}
}

func TestCopyOverwritesExistingDest(t *testing.T) {
	b := newBackend(t)
	_ = b.Create("/files/notes/src.md", "new")
	_ = b.Create("/files/notes/dst.md", "old")
	if err := b.Copy("/files/notes/src.md", "/files/notes/dst.md"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(b.Root, "notes", "dst.md"))
	if string(got) != "new" {
		t.Errorf("dst = %q, want %q", got, "new")
	}
}

// A symlinked source (the shape of /files/project/, which is a
// symlink farm) must produce a real file at the destination, not a
// link to the original.
func TestCopyFollowsSymlinkSource(t *testing.T) {
	b := newBackend(t)
	target := filepath.Join(b.Root, "notes", "real.md")
	if err := os.WriteFile(target, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(b.Root, "notes", "link.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := b.Copy("/files/notes/link.md", "/files/notes/copy.md"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	info, err := os.Lstat(filepath.Join(b.Root, "notes", "copy.md"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Error("destination is a symlink; expected a real file")
	}
	got, _ := os.ReadFile(filepath.Join(b.Root, "notes", "copy.md"))
	if string(got) != "body" {
		t.Errorf("copied body = %q", got)
	}
}

func TestUnescapeShellPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{`Foo\ Bar.png`, "Foo Bar.png"},
		{`"Foo Bar.png"`, "Foo Bar.png"},
		{`'Foo Bar.png'`, "Foo Bar.png"},
		{"`Foo Bar.png`", "Foo Bar.png"},
		{`Foo Bar.png`, "Foo Bar.png"},
		{`/files/a/Foo\ Bar/baz\ qux.png`, "/files/a/Foo Bar/baz qux.png"},
		{`"/files/a/Foo Bar"`, "/files/a/Foo Bar"},
		// Mismatched quotes are left alone.
		{`"Foo Bar`, `"Foo Bar`},
		{`Foo Bar"`, `Foo Bar"`},
	}
	for _, c := range cases {
		if got := unescapeShellPath(c.in); got != c.want {
			t.Errorf("unescapeShellPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
