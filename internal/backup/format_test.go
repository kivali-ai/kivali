package backup

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// writeArchive backs src up into a zip file, as the download leaves one.
func writeArchive(t *testing.T, src string) (string, *Manifest) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	m, err := WriteZip(src, f, "test")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	return path, m
}

func restoreFile(t *testing.T, path string, opt Options) (string, *Result, error) {
	t.Helper()
	src, closeSrc, err := OpenArchive(path, opt)
	if err != nil {
		return "", nil, err
	}
	defer closeSrc()
	dst := t.TempDir()
	res, err := Restore(dst, src, opt)
	return dst, res, err
}

// members lists an archive's member names in stored order.
func members(t *testing.T, path string) []string {
	t.Helper()
	src, closeSrc, err := OpenArchive(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeSrc()
	var out []string
	for {
		e, err := src.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.TrimSuffix(e.Name, "/"))
	}
}

// A restore streams entries in order and checks the manifest at the
// end, and any reader of the format relies on the same shape:
// directories before what they hold, lexical order, the manifest last,
// every member once.
func TestArchiveLayoutIsWhatRestoreReads(t *testing.T) {
	src := writeTree(t, tree)
	if err := os.MkdirAll(filepath.Join(src, "agents", "alice", "files", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, m := writeArchive(t, src)
	names := members(t, path)
	if names[len(names)-1] != ManifestName {
		t.Fatalf("last member = %s, want the manifest", names[len(names)-1])
	}
	body := names[:len(names)-1]
	if !sort.StringsAreSorted(body) {
		t.Errorf("members are not in lexical order: %v", body)
	}
	seen := map[string]bool{}
	for _, n := range body {
		if seen[n] {
			t.Errorf("%s stored twice", n)
		}
		seen[n] = true
		if dir := filepath.ToSlash(filepath.Dir(n)); dir != "." && !seen[dir] {
			t.Errorf("%s stored before its directory %s", n, dir)
		}
	}
	for _, f := range m.Files {
		if !seen[f.Path] {
			t.Errorf("manifest lists %s, the archive does not hold it", f.Path)
		}
	}
	if !seen["agents/alice/files/empty"] {
		t.Error("an empty directory was left out")
	}
}

// A backup of a restore is the backup it came from: nothing is lost,
// added or changed on the way round, and file modes survive it.
func TestABackupOfARestoreIsTheSameBackup(t *testing.T) {
	src := writeTree(t, tree)
	if err := os.MkdirAll(filepath.Join(src, "skills", "tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "skills", "tool", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	first, firstM := writeArchive(t, src)
	dst, _, err := restoreFile(t, first, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, again := writeArchive(t, dst)
	if d := Diff(firstM.Files, again.Files); len(d) > 0 {
		t.Errorf("the second backup differs from the first: %v", d)
	}
	fi, err := os.Stat(filepath.Join(dst, "skills", "tool", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("a skill script came back %v, want its exec bit", fi.Mode().Perm())
	}
}

// rewriteZip copies a zip member by member, raw, except where edit
// returns new content for a member; the manifest is copied as it was.
func rewriteZip(t *testing.T, path string, edit func(name string, body []byte) ([]byte, bool)) string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	out := filepath.Join(t.TempDir(), "edited.zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, m := range zr.File {
		var body []byte
		if !m.Mode().IsDir() {
			rc, err := m.Open()
			if err != nil {
				t.Fatal(err)
			}
			body, _ = io.ReadAll(rc)
			_ = rc.Close()
		}
		if nb, ok := edit(m.Name, body); ok {
			hdr := m.FileHeader
			w, err := zw.CreateHeader(&hdr)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(nb)
			continue
		}
		if err := zw.Copy(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return out
}

// A zip whose names and sizes match its manifest but whose content does
// not is refused by the check, which reads every member, so nothing is
// written: a restore that failed half way through writing would leave a
// deployment half restored and not fresh enough to try again.
func TestCheckZipRefusesDamagedContentBeforeAnythingIsWritten(t *testing.T) {
	src := writeTree(t, tree)
	good, _ := writeArchive(t, src)
	cases := map[string]struct {
		path string
		want string
	}{
		"same size, other bytes": {rewriteZip(t, good, func(n string, b []byte) ([]byte, bool) {
			if n == "usage.jsonl" {
				return bytes.Replace(b, []byte("3"), []byte("9"), 1), true
			}
			return nil, false
		}), "content differs: usage.jsonl"},
		"a member stored twice": {rewriteZip(t, good, func(n string, b []byte) ([]byte, bool) {
			if n == "usage.jsonl" {
				return b, true // and below, again
			}
			return nil, false
		}), ""},
	}
	// A bit flipped in a member's compressed bytes, as a bad disk or a
	// cut download leaves: the zip's own CRC catches it.
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	at := int64(-1)
	for _, f := range zr.File {
		if f.Name == "handbook.md" {
			off, err := f.DataOffset()
			if err != nil {
				t.Fatal(err)
			}
			at = off + int64(f.CompressedSize64)/2
		}
	}
	if at < 0 {
		t.Fatal("member not found")
	}
	flipped := append([]byte(nil), raw...)
	flipped[at] ^= 0xff
	flippedPath := filepath.Join(t.TempDir(), "flipped.zip")
	if err := os.WriteFile(flippedPath, flipped, 0o644); err != nil {
		t.Fatal(err)
	}
	cases["a flipped bit"] = struct {
		path string
		want string
	}{flippedPath, "handbook.md"}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "a member stored twice" {
				c.path = duplicateMember(t, good, "usage.jsonl")
				c.want = "appears twice"
			}
			zr, err := zip.OpenReader(c.path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = zr.Close() }()
			if err := CheckZip(&zr.Reader, Options{}); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("CheckZip err = %v, want it to say %q", err, c.want)
			}
			// OpenArchive runs the check, so nothing reaches Restore.
			if _, _, err := OpenArchive(c.path, Options{}); err == nil {
				t.Error("OpenArchive accepted the archive")
			}
		})
	}
}

// duplicateMember copies a zip and stores one member a second time.
func duplicateMember(t *testing.T, path, name string) string {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	out := filepath.Join(t.TempDir(), "dup.zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, m := range zr.File {
		if err := zw.Copy(m); err != nil {
			t.Fatal(err)
		}
		if m.Name == name {
			if err := zw.Copy(m); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = zw.Close()
	_ = f.Close()
	return out
}

// An archive without a manifest is refused, by the check before
// anything is written and by Restore itself, and the message says so.
func TestAnArchiveWithoutAManifestIsRefused(t *testing.T) {
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("handbook.md")
	_, _ = w.Write([]byte("# rules\n"))
	_ = zw.Close()
	path := filepath.Join(t.TempDir(), "old.zip")
	if err := os.WriteFile(path, zbuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenArchive(path, Options{}); err == nil || !strings.Contains(err.Error(), "no manifest") {
		t.Errorf("OpenArchive err = %v, want it to name the missing manifest", err)
	}
	if _, err := restoreBytes(t, zbuf.Bytes(), Options{}); err == nil || !strings.Contains(err.Error(), "no manifest") {
		t.Errorf("Restore err = %v, want it to name the missing manifest", err)
	}
}

// Anything but a zip, such as a tarball, is refused as not a backup,
// and the message says what one is.
func TestOpenArchiveRefusesWhatIsNotAZip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.tar.gz")
	if err := os.WriteFile(path, []byte("\x1f\x8b\x08\x00not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := OpenArchive(path, Options{}); err == nil || !strings.Contains(err.Error(), "the .zip the app downloads") {
		t.Errorf("err = %v, want it to say a backup is the .zip the app downloads", err)
	}
}

// The manifest is the one part of the format a later version is most
// likely to extend. Fields this version does not know are ignored, so
// a backup from a later patch release with one more field still
// restores; a different format name is refused by name.
func TestManifestReadsFieldsItDoesNotKnowAndRefusesAnotherFormat(t *testing.T) {
	sum := strings.Repeat("a", 64)
	body := `{"format":"kivali-backup-manifest/1","created":"2026-10-01T00:00:00Z","version":"v0.99.0","compression":"zstd"}` + "\n" +
		`{"path":"a.md","size":1,"sha256":"` + sum + `","mode":"0644"}` + "\n"
	m, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("a manifest with extra fields: %v", err)
	}
	if m.Version != "v0.99.0" || len(m.Files) != 1 || m.Files[0].Path != "a.md" {
		t.Errorf("parsed %+v", m)
	}
	_, err = ParseManifest([]byte(strings.Replace(body, "manifest/1", "manifest/2", 1)))
	if err == nil || !strings.Contains(err.Error(), "kivali-backup-manifest/2") {
		t.Errorf("format /2: err = %v, want the format named", err)
	}
	// What Marshal writes, ParseManifest reads back, and it is the
	// format this version writes.
	again, err := ParseManifest(m.Marshal())
	if err != nil || again.Format != ManifestFormat || len(again.Files) != 1 {
		t.Errorf("round trip: %+v, %v", again, err)
	}
}

// changingSink is the zip sink with a hook that runs when the walk
// reaches a file, before its bytes are copied: the server keeps
// writing while a backup runs.
type changingSink struct {
	*zipSink
	before map[string]func()
}

func (c *changingSink) file(rel string, info fs.FileInfo, r io.Reader) error {
	if f := c.before[rel]; f != nil {
		f()
	}
	return c.zipSink.file(rel, info, r)
}

// A running server deletes, replaces and appends to files while a
// backup walks the tree. Each change leaves an archive that matches its
// manifest and restores: a deleted file is left out, a replaced one is
// carried as whichever version was opened, an appended log is cut at
// a whole prefix, and a path swapped for a link is never followed.
func TestABackupWrittenWhileFilesChangeStillRestores(t *testing.T) {
	src := writeTree(t, map[string]string{
		"a/trigger.md":  "t",
		"b/deleted.md":  "gone soon",
		"c/linked.md":   "becomes a link",
		"d/replaced.md": "old",
		"e/chat.jsonl":  "line 1\n",
	})
	secret := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(secret, []byte("TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := func(rel string) string { return filepath.Join(src, filepath.FromSlash(rel)) }
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	s := &changingSink{zipSink: &zipSink{zw: zw}, before: map[string]func(){
		// The walk listed these three before the first copy began.
		"a/trigger.md": func() {
			_ = os.Remove(p("b/deleted.md"))
			_ = os.Remove(p("c/linked.md"))
			_ = os.Symlink(secret, p("c/linked.md"))
			tmp := p("d/.tmp-1")
			_ = os.WriteFile(tmp, []byte("new and longer"), 0o644)
			_ = os.Rename(tmp, p("d/replaced.md"))
		},
		// Opened and sized already: this append lands past the cut.
		"e/chat.jsonl": func() {
			f, _ := os.OpenFile(p("e/chat.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.WriteString("line 2\n")
			_ = f.Close()
		},
	}}
	m, err := write(src, s, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range m.Files {
		got = append(got, f.Path)
	}
	if strings.Join(got, " ") != "a/trigger.md d/replaced.md e/chat.jsonl" {
		t.Errorf("manifest lists %v", got)
	}
	dst, err := restoreBytes(t, buf.Bytes(), Options{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	for rel, want := range map[string]string{"d/replaced.md": "new and longer", "e/chat.jsonl": "line 1\n"} {
		if b, _ := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel))); string(b) != want {
			t.Errorf("%s = %q, want %q", rel, b, want)
		}
	}
	// Members are deflated, so look for the secret in what came out.
	for rel, v := range snapshot(t, dst) {
		if strings.Contains(v, "TOKEN") {
			t.Errorf("the archive carries what a swapped-in link pointed at, as %s", rel)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "c", "linked.md")); err == nil {
		t.Error("the path swapped for a link was restored")
	}
}

// fileInfo is a regular file's fs.FileInfo for a sink fed without a disk.
type fileInfo struct {
	name string
	size int64
}

func (f fileInfo) Name() string       { return f.name }
func (f fileInfo) Size() int64        { return f.size }
func (f fileInfo) Mode() fs.FileMode  { return 0o644 }
func (f fileInfo) ModTime() time.Time { return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) }
func (f fileInfo) IsDir() bool        { return false }
func (f fileInfo) Sys() any           { return nil }

// One message is one file, so a long-lived org passes 65,535 files, the
// most a zip without zip64 records can hold. The zip is built in memory
// through the download's own sink (70,000 files on disk take most of a
// minute to write and read back), then put through what a restore runs
// on it: the whole-archive check and the entry stream.
func TestAZipOfMoreThan65535FilesIsReadWhole(t *testing.T) {
	const n = 70_000
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	s := &zipSink{zw: zw}
	m := &Manifest{Format: ManifestFormat, Created: time.Now().UTC(), Version: "test"}
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("messages/2026-09-01/%05d.md", i)
		body := []byte(rel)
		if err := s.file(rel, fileInfo{filepath.Base(rel), int64(len(body))}, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		m.Files = append(m.Files, File{Path: rel, Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:])})
	}
	if err := s.manifest(m.Marshal(), m.Created); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != n+1 {
		t.Fatalf("the zip reads back %d members, want %d", len(zr.File), n+1)
	}
	if err := CheckZip(zr, Options{}); err != nil {
		t.Fatal(err)
	}
	src := NewZipSource(zr)
	count := 0
	for {
		e, err := src.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !e.Dir && e.Name != ManifestName {
			count++
		}
	}
	if count != n {
		t.Errorf("the restore's stream yields %d files, want %d", count, n)
	}
}

// Backup and restore stream: the memory either takes does not grow with
// the size of a file. A file several times the bound passes through
// both, and the bytes allocated stay under it.
func TestBackupAndRestoreStreamLargeFiles(t *testing.T) {
	const size = 96 << 20
	const bound = 24 << 20
	src := t.TempDir()
	f, err := os.Create(filepath.Join(src, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(`{"ts":"2026-09-01T00:00:00Z","model":"m","input_tokens":3,"output_tokens":4}` + "\n")
	for written := 0; written < size; written += len(line) {
		_, _ = f.Write(line)
	}
	_ = f.Close()
	alloc := func(do func()) uint64 {
		var a, b runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&a)
		do()
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc
	}
	var path string
	if got := alloc(func() { path, _ = writeArchive(t, src) }); got > bound {
		t.Errorf("backup allocated %d MB for a %d MB file", got>>20, size>>20)
	}
	if got := alloc(func() { _, _, err = restoreFile(t, path, Options{}) }); got > bound {
		t.Errorf("restore allocated %d MB for a %d MB file", got>>20, size>>20)
	}
	if err != nil {
		t.Fatal(err)
	}
}
