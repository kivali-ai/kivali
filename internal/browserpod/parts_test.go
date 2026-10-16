package browserpod

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/kivali-ai/kivali/internal/clock"
)

func TestFormatSnapshot(t *testing.T) {
	d := snapshotData{
		Title: "Plans", URL: "https://example.com/pricing", Width: 1280, Height: 800, ScrollY: 0, DocHeight: 2400,
		Lines: []snapLine{
			{0, `heading 2 "Plans"`},
			{0, "list"},
			{1, `listitem "Free"`},
			{1, `[e1] link "Pricing" → /pricing`},
		},
	}
	want := "Page: Plans — https://example.com/pricing\n" +
		"Viewport: 1280x800, scrolled 0 of 2400\n" +
		"heading 2 \"Plans\"\n" +
		"list\n" +
		"  listitem \"Free\"\n" +
		"  [e1] link \"Pricing\" → /pricing"
	if got := formatSnapshot(d, MaxSnapshotChars); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestFormatSnapshotCapsItsLength(t *testing.T) {
	d := snapshotData{Title: "Long", URL: "https://example.com/"}
	for i := 0; i < 5000; i++ {
		d.Lines = append(d.Lines, snapLine{1, strings.Repeat("x", 30)})
	}
	got := formatSnapshot(d, MaxSnapshotChars)
	lines := strings.Split(got, "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "… ") || !strings.HasSuffix(last, " more lines not shown; scroll, or wait for what you need, and snapshot again") {
		t.Fatalf("last line: %q", last)
	}
	body := strings.Join(lines[:len(lines)-1], "\n")
	if len(body) > MaxSnapshotChars {
		t.Fatalf("snapshot body is %d chars, over %d", len(body), MaxSnapshotChars)
	}
	shown := len(lines) - 3 // header lines and the cut line
	if !strings.Contains(last, "… "+itoa(5000-shown)+" more") {
		t.Fatalf("count: %d shown, last line %q", shown, last)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestSnapshotLinesDecode(t *testing.T) {
	var d snapshotData
	if err := json.Unmarshal([]byte(`{"title":"T","lines":[[0,"text \"a\""],[2,"[e1] button \"Go\""]],"refs":[null,{"role":"button","name":"Go"}]}`), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Lines) != 2 || d.Lines[1].Depth != 2 || d.Lines[1].Text != `[e1] button "Go"` || d.Refs[1].Name != "Go" || d.Refs[0] != nil {
		t.Fatalf("decoded: %+v", d)
	}
}

func TestCleanFileName(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":            "report.pdf",
		"Quarterly report.xlsx": "Quarterly-report.xlsx",
		"../../etc/passwd":      "passwd",
		`C:\Users\a\b.txt`:      "b.txt",
		"résumé (1).doc":        "rsum-1.doc",
		"...":                   "download.bin",
		"":                      "download.bin",
		"日本語":                   "download.bin",
		".hidden":               "hidden",
		"a/b/":                  "b",
	} {
		if got := cleanFileName(in); got != want {
			t.Errorf("cleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// testBrowser is a browser whose sends are recorded.
type sent struct {
	msg  Message
	data []byte
}

func testBrowser(t *testing.T) (*browser, *[]sent) {
	t.Helper()
	var out []sent
	b := newBrowser(newFakeCDP(t), clock.NewFake(), log.New(io.Discard, "", 0), "", t.TempDir(), func(m Message, body io.Reader) error {
		var data []byte
		if body != nil {
			data, _ = io.ReadAll(body)
		}
		out = append(out, sent{m, data})
		return nil
	})
	return b, &out
}

func TestDownloadsWithinTheLimitAreSent(t *testing.T) {
	b, out := testBrowser(t)
	tb := &tab{id: "t3", group: "g", changed: make(chan struct{})}
	path := filepath.Join(t.TempDir(), "guid-1")
	if err := os.WriteFile(path, []byte("PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.deliverDownload(&download{name: "My invoice.pdf", tab: tb}, path)
	if len(*out) != 1 {
		t.Fatalf("sent %d messages", len(*out))
	}
	m := (*out)[0]
	if m.msg.Type != TypeDownload || m.msg.Group != "g" || m.msg.Tab != "t3" || m.msg.FileName != "My-invoice.pdf" || m.msg.Bytes != 3 || string(m.data) != "PDF" {
		t.Fatalf("download: %+v %q", m.msg, m.data)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the downloaded file was kept")
	}
}

func TestDownloadsOverTheLimitAreDropped(t *testing.T) {
	b, out := testBrowser(t)
	tb := &tab{id: "t1", group: "g", changed: make(chan struct{})}
	path := filepath.Join(t.TempDir(), "guid-2")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Sparse: the size is what counts.
	if err := f.Truncate(MaxDownloadBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	b.deliverDownload(&download{name: "big.iso", tab: tb}, path)
	if len(*out) != 0 {
		t.Fatalf("sent %+v", (*out)[0].msg)
	}
	notes := tb.takeNotes()
	if len(notes) != 1 || !strings.Contains(notes[0], "was dropped: the limit is 50 MB") {
		t.Fatalf("notes: %q", notes)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the dropped file was kept")
	}
}

func TestProfileRoundTripLeavesOutCachesAndLocks(t *testing.T) {
	src := t.TempDir()
	files := map[string]string{
		"Local State":         "{}",
		"Default/Cookies":     "jar",
		"Default/Preferences": "{}",
		"Default/Local Storage/leveldb/000003.log":      "log",
		"Default/Local Storage/leveldb/000005.ldb":      "ldb",
		"Default/Cache/Cache_Data/f_000001":             "cache",
		"Default/Code Cache/js/index":                   "code",
		"Default/Service Worker/CacheStorage/x":         "sw cache",
		"Default/Service Worker/ScriptCache/y":          "sw script",
		"Default/Service Worker/Database/MANIFEST":      "sw db",
		"GPUCache/data_0":                               "gpu",
		"GrShaderCache/data":                            "gr",
		"ShaderCache/data":                              "shader",
		"Default/DawnCache/data":                        "dawn",
		"chrome_debug.log":                              "debug",
		"lockfile":                                      "",
		"RunningChromeVersion":                          "1",
		"optimization_guide_model_store/1/model.tflite": "model",
	}
	for name, content := range files {
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(src, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(src, "Default", "link")); err != nil {
		t.Fatal(err)
	}

	var packed bytes.Buffer
	if err := packProfile(src, &packed); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := unpackProfile(bytes.NewReader(packed.Bytes()), dst); err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dst, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	want := map[string]bool{
		"Local State": true, "Default/Cookies": true, "Default/Preferences": true,
		"Default/Local Storage/leveldb/000005.ldb": true, "Default/Service Worker/Database/MANIFEST": true,
	}
	if len(got) != len(want) {
		t.Fatalf("unpacked %q", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("unpacked %q, which should be left out", g)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "Default", "Cookies")); string(b) != "jar" {
		t.Fatalf("Cookies: %q", b)
	}
}

func TestUnpackProfileSkipsUnsafeEntries(t *testing.T) {
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(h *tar.Header, body string) {
		h.Size = int64(len(body))
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(tw, body)
	}
	add(&tar.Header{Name: "../escape", Mode: 0o600}, "x")
	add(&tar.Header{Name: "a/../../escape2", Mode: 0o600}, "x")
	add(&tar.Header{Name: "/abs", Mode: 0o600}, "x")
	add(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc"}, "")
	add(&tar.Header{Name: "ok/file", Mode: 0o600}, "fine")
	_ = tw.Close()
	_ = zw.Close()

	parent := t.TempDir()
	dst := filepath.Join(parent, "profile")
	if err := unpackProfile(&buf, dst); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(parent, "escape"), filepath.Join(parent, "escape2"), filepath.Join(dst, "link"), filepath.Join(dst, "abs")} {
		if _, err := os.Lstat(p); err == nil {
			t.Fatalf("%s was written", p)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "ok", "file")); string(b) != "fine" {
		t.Fatalf("ok/file: %q", b)
	}
}

func TestParseChord(t *testing.T) {
	cases := []struct {
		in   string
		mods int
		key  keyDef
	}{
		{"Enter", 0, keyDef{"Enter", "Enter", 13, "\r"}},
		{"escape", 0, keyDef{"Escape", "Escape", 27, ""}},
		{"a", 0, keyDef{"a", "KeyA", 'A', "a"}},
		{"Shift+a", modShift, keyDef{"A", "KeyA", 'A', "A"}},
		{"Control+a", modCtrl, keyDef{"a", "KeyA", 'A', ""}},
		{"Meta+Shift+z", modMeta | modShift, keyDef{"Z", "KeyZ", 'Z', ""}},
		{"7", 0, keyDef{"7", "Digit7", '7', "7"}},
		{"Space", 0, keyDef{" ", "Space", 32, " "}},
		{"Control++", modCtrl, keyDef{"+", "", 0, ""}},
	}
	for _, c := range cases {
		got, err := parseChord(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got.mods != c.mods || got.key != c.key {
			t.Errorf("%q: got %+v, want mods %d key %+v", c.in, got, c.mods, c.key)
		}
	}
	for _, bad := range []string{"Hyper+a", "Enterr", "F13"} {
		if _, err := parseChord(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestOnSite(t *testing.T) {
	for host, want := range map[string]bool{
		"example.com": true, "www.example.com": true, "a.b.example.com": true, "EXAMPLE.com.": true,
		"notexample.com": false, "example.com.evil.test": false, "com": false,
	} {
		if got := onSite("example.com", host); got != want {
			t.Errorf("onSite(%q) = %v", host, got)
		}
	}
}

func TestChromeArgs(t *testing.T) {
	o := chromeOptions{Dir: "/w", Proxy: "http://proxy:3128"}
	args := strings.Join(chromeArgs(o, false), " ")
	for _, want := range []string{"--remote-debugging-pipe", "--user-data-dir=/w/profile", "--headless=new", "--proxy-server=http://proxy:3128", "--window-size=1280,800"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %s: %s", want, args)
		}
	}
	if strings.Contains(args, "--no-sandbox") {
		t.Error("sandbox off by default")
	}
	o.Headful, o.Proxy = true, ""
	args = strings.Join(chromeArgs(o, true), " ")
	if strings.Contains(args, "--headless") || strings.Contains(args, "--proxy-server") || !strings.Contains(args, "--no-sandbox") {
		t.Errorf("headful without proxy, sandbox off: %s", args)
	}
	if !isSandboxFailure("[0101/000000.0:FATAL:zygote_host_impl_linux.cc(127)] No usable sandbox! Update your kernel") || isSandboxFailure("some other crash") {
		t.Error("isSandboxFailure")
	}
}
