package browserpod

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/clock"
)

// hiddenMarkers are on the test page in ways a reader cannot see.
// None may appear in a snapshot.
var hiddenMarkers = []string{
	"HIDDEN-DISPLAY", "HIDDEN-VISIBILITY", "HIDDEN-OPACITY", "HIDDEN-ARIA",
	"HIDDEN-ATTR", "HIDDEN-ZERO", "HIDDEN-SRONLY", "HIDDEN-OFFSCREEN",
	"HIDDEN-FONTSIZE", "HIDDEN-COLOR", "HIDDEN-INDENT", "HIDDEN-NOSCRIPT",
	"HIDDEN-TEMPLATE", "HIDDEN-SCRIPT", "HIDDEN-STYLE", "HIDDEN-CLIPPATH",
	"HIDDEN-NESTED",
}

const testPageOne = `<!doctype html>
<html><head><title>Kivali test page</title>
<style>
  .sr { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0, 0, 0, 0); white-space: nowrap; }
  .cp { position: absolute; width: 1px; height: 1px; overflow: hidden; clip-path: inset(100%); }
  .x::after { content: "HIDDEN-STYLE"; display: none; }
</style></head>
<body style="background: #fff">
  <h1>Visible heading</h1>
  <p>VISIBLE-MARKER is on the page.</p>
  <div style="display: none">HIDDEN-DISPLAY</div>
  <div style="visibility: hidden">HIDDEN-VISIBILITY</div>
  <div style="opacity: 0">HIDDEN-OPACITY</div>
  <div aria-hidden="true">HIDDEN-ARIA</div>
  <div hidden>HIDDEN-ATTR</div>
  <div style="height: 0; overflow: hidden">HIDDEN-ZERO</div>
  <span class="sr">HIDDEN-SRONLY</span>
  <span class="cp">HIDDEN-CLIPPATH</span>
  <div style="position: absolute; left: -9999px; top: 0">HIDDEN-OFFSCREEN</div>
  <div style="font-size: 0">HIDDEN-FONTSIZE</div>
  <div style="background: #ffffff; color: rgb(255, 255, 255)">HIDDEN-COLOR</div>
  <p style="text-indent: -9999px; overflow: hidden">HIDDEN-INDENT</p>
  <div style="display: none"><p><b>HIDDEN-NESTED</b></p></div>
  <noscript>HIDDEN-NOSCRIPT</noscript>
  <template>HIDDEN-TEMPLATE</template>
  <script>var marker = "HIDDEN-SCRIPT";</script>
  <style>.y { content: "HIDDEN-STYLE"; }</style>
  <ul><li>First item</li><li>Second item</li></ul>
  <p><a href="/two">Go to page two</a></p>
  <label for="name">Your name</label> <input id="name" placeholder="Ada">
  <label for="color">Colour</label>
  <select id="color"><option value="r">Red</option><option value="g">Green</option><option value="b">Blue</option></select>
  <label for="doc">Document</label> <input type="file" id="doc">
  <button onclick="window.open('/two', '_blank')">Open popup</button>
  <a href="/report" download>Download report</a>
  <iframe id="export" style="display: none" src="about:blank"></iframe>
</body></html>`

const testPageTwo = `<!doctype html><html><head><title>Page two</title></head>
<body><p>SECOND-PAGE</p><a href="/">Back home</a></body></html>`

// realPod is a pod running a real browser against a fake core, for the
// tests that need Chromium.
type realPod struct {
	t    *testing.T
	core *coreSide
	url  string
	done chan error
}

// realChromium starts a pod on the browser KIVALI_CHROMIUM names, or
// skips the test when it is not set. The pod serves its pages from mux
// and is stopped with the test.
func realChromium(t *testing.T, mux *http.ServeMux) *realPod {
	t.Helper()
	bin := os.Getenv("KIVALI_CHROMIUM")
	if bin == "" {
		t.Skip("KIVALI_CHROMIUM is not set")
	}
	site := httptest.NewServer(mux)
	t.Cleanup(site.Close)

	fc := newFakeCore(t)
	rt := &Runtime{
		Slug: "ada", Session: "web", Token: "tok", CoreUDS: fc.sock,
		Chromium: bin, Dir: t.TempDir(),
		Logger: log.New(os.Stderr, "browserpod: ", log.Lmicroseconds),
		Clock:  clock.New(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(failsafe):
			t.Error("Run did not return")
		}
	})

	core := fc.accept()
	core.send(Message{Type: TypeHello})
	core.send(Message{Type: TypeStart})
	core.expectState(StateStarting)
	running := core.expectState(StateRunning)
	t.Logf("running (detail %q)", running.Detail)
	return &realPod{t: t, core: core, url: site.URL, done: done}
}

// step runs a command in group g that must succeed, logging its answer
// and how long it took.
func (p *realPod) step(name, tabID, cmd string, args any) Message {
	p.t.Helper()
	start := time.Now()
	res := p.core.command("g", tabID, cmd, args)
	if !res.OK {
		p.t.Fatalf("%s: %s (%s)", name, res.Error, res.Code)
	}
	p.t.Logf("%s (%s): %s", name, time.Since(start).Round(time.Millisecond), firstLines(res.Text, 3))
	return res
}

// eval runs script and returns its value, without the page content
// lines around it.
func (p *realPod) eval(script string) string {
	p.t.Helper()
	return pageBody(p.t, p.step("eval "+script, "", CommandEval, EvalArgs{Script: script}).Text)
}

// findRef is the ref of the line matching pattern in a snapshot.
func findRef(t *testing.T, snap, pattern string) string {
	t.Helper()
	m := regexp.MustCompile(`\[(e\d+)\] ` + pattern).FindStringSubmatch(snap)
	if m == nil {
		t.Fatalf("no ref matching %q in snapshot:\n%s", pattern, snap)
	}
	return m[1]
}

func htmlPage(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, body)
	}
}

// TestRealChromium drives a real browser through the whole runtime.
// It runs only when KIVALI_CHROMIUM names a Chromium or Chrome binary,
// for example:
//
//	KIVALI_CHROMIUM="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
//	  go test ./internal/browserpod/ -run TestRealChromium -v
func TestRealChromium(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", htmlPage(testPageOne))
	mux.HandleFunc("/two", htmlPage(testPageTwo))
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="quarterly report.txt"`)
		_, _ = io.WriteString(w, "REPORT-CONTENT")
	})
	mux.HandleFunc("/export", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="export.csv"`)
		_, _ = io.WriteString(w, "a,b")
	})
	p := realChromium(t, mux)
	core, site, done := p.core, p.url, p.done
	core.sendBytes(Message{Type: TypeFile, Group: "g", FileName: "hello.txt"}, []byte("hello"))
	step, eval := p.step, p.eval

	// Navigate.
	res := step("navigate", "", CommandNavigate, NavigateArgs{URL: site + "/"})
	if !strings.Contains(res.Text, "Loaded Kivali test page — "+site) {
		t.Fatalf("navigate text: %q", res.Text)
	}
	if tab := core.expectType(TypeTab); tab.Tab != "t1" || tab.Group != "g" {
		t.Fatalf("tab: %+v", tab.Message)
	}
	if nav := core.expectType(TypeNavigated); nav.Title != "Kivali test page" {
		t.Fatalf("navigated: %+v", nav.Message)
	}

	// Snapshot: visible text in, hidden text out.
	snap := step("snapshot", "", CommandSnapshot, nil).Text
	t.Logf("snapshot:\n%s", snap)
	for _, want := range []string{"Page: Kivali test page — ", "Viewport: ", `heading 1 "Visible heading"`, "VISIBLE-MARKER", `listitem "First item"`} {
		if !strings.Contains(snap, want) {
			t.Errorf("snapshot lacks %q", want)
		}
	}
	for _, m := range hiddenMarkers {
		if strings.Contains(snap, m) {
			t.Errorf("snapshot shows hidden %s", m)
		}
	}
	ref := func(pattern string) string { t.Helper(); return findRef(t, snap, pattern) }
	linkRef := ref(`link "Go to page two" → /two`)
	nameRef := ref(`textbox "Your name" value="" placeholder="Ada"`)
	colorRef := ref(`combobox "Colour" value="r" selected="Red" options=3`)
	fileRef := ref(`file "Document"`)
	popupRef := ref(`button "Open popup"`)
	downloadRef := ref(`link "Download report"`)

	// Type, select, upload.
	step("type", "", CommandType, TypeArgs{Ref: nameRef, Text: "Grace"})
	if v := eval(`document.getElementById('name').value`); v != `"Grace"` {
		t.Fatalf("typed value: %s", v)
	}
	res = step("select", "", CommandSelect, SelectArgs{Ref: colorRef, Value: "Green"})
	if !strings.Contains(res.Text, `"Green"`) {
		t.Fatalf("select text: %q", res.Text)
	}
	if v := eval(`document.getElementById('color').value`); v != `"g"` {
		t.Fatalf("selected value: %s", v)
	}
	if res := core.command("g", "", CommandSelect, SelectArgs{Ref: colorRef, Value: "Purple"}); res.Code != CodeNotFound || !strings.Contains(res.Error, "Blue") {
		t.Fatalf("unknown option: %+v", res)
	}
	step("upload", "", CommandUpload, UploadArgs{Ref: fileRef, File: "hello.txt"})
	if v := eval(`document.getElementById('doc').files[0].name`); v != `"hello.txt"` {
		t.Fatalf("uploaded file: %s", v)
	}

	// Screenshot.
	res = step("screenshot", "", CommandScreenshot, nil)
	if res.Image == nil || res.Image.MIME != "image/png" || !bytes.HasPrefix(res.Image.Data, []byte("\x89PNG")) {
		t.Fatalf("screenshot image: %+v", res.Image != nil)
	}
	t.Logf("screenshot: %d bytes", len(res.Image.Data))

	// A popup joins the group.
	step("click popup", "", CommandClick, ClickArgs{Ref: popupRef})
	popup := core.next("popup tab", func(m Message) bool { return m.Type == TypeTab && m.Opener == "t1" })
	if popup.Group != "g" {
		t.Fatalf("popup: %+v", popup.Message)
	}
	core.next("popup navigated", func(m Message) bool {
		return m.Type == TypeNavigated && m.Tab == popup.Tab && strings.HasSuffix(m.URL, "/two")
	})
	// Download.
	step("click download", "", CommandClick, ClickArgs{Ref: downloadRef})
	dl := core.expectType(TypeDownload)
	if dl.FileName != "quarterly-report.txt" || string(dl.data) != "REPORT-CONTENT" || dl.Tab != "t1" {
		t.Fatalf("download: %+v %q", dl.Message, dl.data)
	}
	// A download a hidden iframe starts is the tab's too.
	eval(`document.getElementById('export').src = '/export'; 'exporting'`)
	dl = core.expectType(TypeDownload)
	if dl.FileName != "export.csv" || string(dl.data) != "a,b" || dl.Tab != "t1" {
		t.Fatalf("download from an iframe: %+v %q", dl.Message, dl.data)
	}

	list := step("tabs list", "", CommandTabs, TabsArgs{Action: TabsList}).Text
	if !strings.Contains(list, "* t1 ") || !strings.Contains(list, popup.Tab+" "+site+"/two") {
		t.Fatalf("tabs list: %q", list)
	}
	step("close popup", "", CommandTabs, TabsArgs{Action: TabsClose, Tab: popup.Tab})
	core.next("popup closed", func(m Message) bool { return m.Type == TypeTabClosed && m.Tab == popup.Tab })

	// Open, switch, close.
	res = step("tabs open", "", CommandTabs, TabsArgs{Action: TabsOpen, URL: site + "/two"})
	opened := core.next("opened tab", func(m Message) bool { return m.Type == TypeTab && m.Opener == "" && m.Tab != "t1" })
	if !strings.Contains(res.Text, "Page two") {
		t.Fatalf("tabs open: %q", res.Text)
	}
	if v := eval("document.title"); v != `"Page two"` {
		t.Fatalf("the opened tab is not current: %s", v)
	}
	step("tabs switch", "", CommandTabs, TabsArgs{Action: TabsSwitch, Tab: "t1"})
	if v := eval("document.title"); v != `"Kivali test page"` {
		t.Fatalf("after switch: %s", v)
	}
	step("tabs close", "", CommandTabs, TabsArgs{Action: TabsClose, Tab: opened.Tab})
	core.next("opened tab closed", func(m Message) bool { return m.Type == TypeTabClosed && m.Tab == opened.Tab })

	// Click a link: the tab navigates and core hears of it.
	res = step("click link", "", CommandClick, ClickArgs{Ref: linkRef})
	if !strings.Contains(res.Text, `Clicked link "Go to page two"; now at `+site+"/two — Page two") {
		t.Fatalf("click text: %q", res.Text)
	}
	core.next("navigated to two", func(m Message) bool {
		return m.Type == TypeNavigated && m.Tab == "t1" && m.URL == site+"/two" && m.Title == "Page two"
	})
	if res := core.command("g", "", CommandClick, ClickArgs{Ref: nameRef}); res.Code != CodeNotFound {
		t.Fatalf("stale ref: %+v", res)
	}
	res = step("wait text", "", CommandWait, WaitArgs{For: WaitText, Value: "SECOND-PAGE", TimeoutMS: 2000})
	if !strings.Contains(res.Text, "SECOND-PAGE") {
		t.Fatalf("wait: %q", res.Text)
	}
	res = step("back", "", CommandBack, nil)
	if !strings.Contains(res.Text, "Kivali test page") {
		t.Fatalf("back: %q", res.Text)
	}
	res = step("press", "", CommandPress, PressArgs{Key: "End"})
	_ = res
	res = step("scroll", "", CommandScroll, ScrollArgs{Direction: "down"})
	_ = res

	// Save: the profile comes back and unpacks.
	core.send(Message{Type: TypeSave})
	core.expectState(StateSaving)
	prof := core.expectType(TypeProfile)
	core.expectType(TypeSaved)
	core.expectState(StateStopped)
	out := t.TempDir()
	if err := unpackProfile(bytes.NewReader(prof.data), out); err != nil {
		t.Fatalf("unpack saved profile: %v", err)
	}
	entries, _ := os.ReadDir(out)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("saved profile: %d bytes packed, top level %v", len(prof.data), names)
	if os.Getenv("KIVALI_PROFILE_SIZES") != "" {
		_ = filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if fi, err := d.Info(); err == nil && fi.Size() > 200<<10 {
					t.Logf("  %8d KiB %s", fi.Size()>>10, strings.TrimPrefix(p, out))
				}
			}
			return nil
		})
	}
	if len(names) == 0 {
		t.Fatal("the saved profile is empty")
	}
	_ = core.conn.Close(websocket.StatusNormalClosure, "")
	select {
	case err := <-done:
		// The pod's exit goes back for the helper's cleanup to see.
		done <- err
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(failsafe):
		t.Fatal("Run did not return after save")
	}
}

// TestRealChromiumSiteSession drives a real browser in a site session,
// twice: the first browser signs the test page in with a value in local
// storage and saves its profile; the second starts from that profile
// and must still find the value. It runs like TestRealChromium, when
// KIVALI_CHROMIUM names a browser.
func TestRealChromiumSiteSession(t *testing.T) {
	bin := os.Getenv("KIVALI_CHROMIUM")
	if bin == "" {
		t.Skip("KIVALI_CHROMIUM is not set")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, testPageOne)
	})
	mux.HandleFunc("/two", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, testPageTwo)
	})
	// The server answers on 127.0.0.1, the session's site; the same
	// server as localhost is another host, off the site.
	site := httptest.NewServer(mux)
	defer site.Close()

	// run takes one browser through hello (with profile), start, fn and
	// save, and returns the saved profile.
	run := func(profile []byte, fn func(core *coreSide, eval func(string) string)) []byte {
		t.Helper()
		fc := newFakeCore(t)
		rt := &Runtime{
			Slug: "ada", Session: "127.0.0.1", Token: "tok", CoreUDS: fc.sock,
			Chromium: bin, Dir: t.TempDir(),
			Logger: log.New(os.Stderr, "browserpod: ", log.Lmicroseconds),
			Clock:  clock.New(),
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- rt.Run(ctx) }()
		core := fc.accept()
		core.sendBytes(Message{Type: TypeHello, Site: "127.0.0.1"}, profile)
		core.send(Message{Type: TypeStart})
		core.expectState(StateStarting)
		core.expectState(StateRunning)
		eval := func(script string) string {
			t.Helper()
			res := core.command("g", "", CommandEval, EvalArgs{Script: script})
			if !res.OK {
				t.Fatalf("eval %s: %s (%s)", script, res.Error, res.Code)
			}
			return res.Text
		}
		fn(core, eval)
		core.send(Message{Type: TypeSave})
		core.expectState(StateSaving)
		prof := core.expectType(TypeProfile)
		core.expectType(TypeSaved)
		core.expectState(StateStopped)
		_ = core.conn.Close(websocket.StatusNormalClosure, "")
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
		case <-time.After(failsafe):
			t.Fatal("Run did not return after save")
		}
		return prof.data
	}

	saved := run(nil, func(core *coreSide, eval func(string) string) {
		if res := core.command("g", "", CommandNavigate, NavigateArgs{URL: site.URL + "/"}); !res.OK {
			t.Fatalf("navigate: %+v", res)
		}
		eval(`localStorage.setItem('kivali-session', 'kept'); 'set'`)
	})
	t.Logf("saved profile: %d bytes packed", len(saved))

	run(saved, func(core *coreSide, eval func(string) string) {
		if res := core.command("g", "", CommandNavigate, NavigateArgs{URL: site.URL + "/"}); !res.OK {
			t.Fatalf("navigate: %+v", res)
		}
		if v := eval(`localStorage.getItem('kivali-session')`); v != `"kept"` {
			t.Fatalf("local storage after the profile round trip: %s", v)
		}
	})
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		return strings.Join(lines[:n], " | ") + fmt.Sprintf(" | … (%d chars)", len(s))
	}
	return strings.Join(lines, " | ")
}
