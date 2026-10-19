package browserpod

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// TestRealChromiumSiteHold drives a real browser through a group held
// to one site (localhost), with another site (127.0.0.1) embedded in
// and linked from its pages: the hold keeps the task's own actions and
// its tabs on the site, and lets the other site's frame load. Like
// TestRealChromium it runs only when KIVALI_CHROMIUM names a browser.
func TestRealChromiumSiteHold(t *testing.T) {
	bin := os.Getenv("KIVALI_CHROMIUM")
	if bin == "" {
		t.Skip("KIVALI_CHROMIUM is not set")
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<!doctype html><title>Other site</title><input aria-label="Elsewhere"><p>OTHER-SITE</p>`)
	}))
	defer other.Close()
	page := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = io.WriteString(w, `<!doctype html><title>Held site</title>`+strings.ReplaceAll(body, "OTHER", other.URL))
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", page(`<label for="name">Your name</label> <input id="name">
		<iframe id="f" src="OTHER/frame" width="300" height="80"></iframe>
		<button onclick="window.open('OTHER/', '_blank')">Pop other</button>
		<a href="OTHER/">Go elsewhere</a>`))
	mux.HandleFunc("/cover", page(`<button>Under the frame</button>
		<iframe src="OTHER/" style="position: fixed; left: 0; top: 0; width: 100%; height: 100%; border: 0"></iframe>`))
	// The other site's frame inside the element clicked, and inside an
	// open shadow root, focused and covering a button.
	mux.HandleFunc("/wrapped", page(`<a href="#" aria-label="Wrapped frame" style="display: inline-block"><iframe src="OTHER/" width="300" height="120" style="border: 0"></iframe></a>`))
	mux.HandleFunc("/shadow", page(`<button>Under the shadow</button>
		<label for="name">Your name</label> <input id="name">
		<div id="host"></div>
		<script>
		const root = document.getElementById('host').attachShadow({mode: 'open'});
		root.innerHTML = '<iframe src="OTHER/" style="position: fixed; left: 0; top: 0; width: 100%; height: 60%; border: 0"></iframe>';
		</script>`))
	held := httptest.NewServer(mux)
	defer held.Close()
	home := strings.Replace(held.URL, "127.0.0.1", "localhost", 1)

	fc := newFakeCore(t)
	rt := &Runtime{
		Slug: "ada", Session: "web", Token: "tok", CoreUDS: fc.sock,
		Chromium: bin, Dir: t.TempDir(),
		Logger: log.New(os.Stderr, "browserpod: ", log.Lmicroseconds),
		Clock:  clock.New(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
		}
	}()
	core := fc.accept()
	core.send(Message{Type: TypeHello})
	core.send(Message{Type: TypeStart})
	core.expectState(StateRunning)

	n := 0
	cmd := func(name string, args any) Message {
		t.Helper()
		n++
		id := fmt.Sprintf("real-hold-%d", n)
		raw, _ := json.Marshal(args)
		core.send(Message{Type: TypeCommand, ID: id, Group: "up", Name: name, Args: raw, Site: "localhost"})
		res := core.result(id)
		t.Logf("%s: ok=%v %s%s", name, res.OK, firstLines(res.Text, 2), res.Error)
		return res
	}
	refIn := func(snap, pattern string) string {
		t.Helper()
		m := regexp.MustCompile(`\[(e\d+)\] ` + pattern).FindStringSubmatch(snap)
		if m == nil {
			t.Fatalf("no ref matching %q in:\n%s", pattern, snap)
		}
		return m[1]
	}

	if res := cmd(CommandNavigate, NavigateArgs{URL: other.URL + "/"}); res.Code != CodeOffSite {
		t.Fatalf("navigate off the site: %+v", res)
	}
	if res := cmd(CommandNavigate, NavigateArgs{URL: home + "/"}); !res.OK {
		t.Fatalf("navigate on the site: %+v", res)
	}
	snap := cmd(CommandSnapshot, nil).Text

	// Typing on the page itself works; with the focus moved into the
	// other site's frame, a key is refused.
	if res := cmd(CommandType, TypeArgs{Ref: refIn(snap, `textbox "Your name"`), Text: "Grace"}); !res.OK {
		t.Fatalf("type on the page: %+v", res)
	}
	if v := cmd(CommandEval, EvalArgs{Script: `document.getElementById('name').value`}).Text; v != `"Grace"` {
		t.Errorf("typed value = %s", v)
	}
	// The page moves the focus into the other site's frame as soon as
	// the field takes it: the text still goes into the field.
	cmd(CommandEval, EvalArgs{Script: `document.getElementById('name').addEventListener('focus', () => document.getElementById('f').focus()), 1`})
	if res := cmd(CommandType, TypeArgs{Ref: refIn(snap, `textbox "Your name"`), Text: "Ada"}); !res.OK {
		t.Errorf("type with the focus taken: %+v", res)
	}
	if v := cmd(CommandEval, EvalArgs{Script: `document.getElementById('name').value`}).Text; v != `"Ada"` {
		t.Errorf("value after a focus theft = %s", v)
	}
	cmd(CommandEval, EvalArgs{Script: `document.getElementById('f').focus(), document.activeElement.tagName`})
	if res := cmd(CommandPress, PressArgs{Key: "a"}); res.Code != CodeOffSite || !strings.Contains(res.Error, "embedded frame from another site") {
		t.Errorf("a key into the other site's frame: %+v", res)
	}

	// A popup to the other site is closed as it heads there: window.open
	// attaches it blank, and it never holds the other site's page.
	cmd(CommandClick, ClickArgs{Ref: refIn(snap, `button "Pop other"`)})
	core.next("the popup closed", func(m Message) bool { return m.Type == TypeTabClosed && m.Group == "up" })
	if list := cmd(CommandTabs, TabsArgs{Action: TabsList}).Text; strings.Contains(list, "127.0.0.1") || strings.Contains(list, "t2") {
		t.Errorf("the off-site popup became a tab:\n%s", list)
	}

	// A link off the site does not take the tab there.
	if res := cmd(CommandClick, ClickArgs{Ref: refIn(snap, `link "Go elsewhere"`)}); strings.Contains(res.Text, "OTHER-SITE") || strings.Contains(res.Text, "now at "+other.URL) {
		t.Errorf("the link left the site: %+v", res)
	}

	// A click whose point the other site's frame covers is refused.
	if res := cmd(CommandNavigate, NavigateArgs{URL: home + "/cover"}); !res.OK {
		t.Fatalf("navigate to the covered page: %+v", res)
	}
	snap = cmd(CommandSnapshot, nil).Text
	if res := cmd(CommandClick, ClickArgs{Ref: refIn(snap, `button "Under the frame"`)}); res.Code != CodeOffSite {
		t.Errorf("a click under the other site's frame: %+v", res)
	}

	// A frame inside the element clicked is no exemption.
	if res := cmd(CommandNavigate, NavigateArgs{URL: home + "/wrapped"}); !res.OK {
		t.Fatalf("navigate to the wrapped frame: %+v", res)
	}
	snap = cmd(CommandSnapshot, nil).Text
	if res := cmd(CommandClick, ClickArgs{Ref: refIn(snap, `link "Wrapped frame"`)}); res.Code != CodeOffSite {
		t.Errorf("a click on a frame inside the link: %+v", res)
	}

	// A frame inside a shadow root: covering a button, and with the focus.
	if res := cmd(CommandNavigate, NavigateArgs{URL: home + "/shadow"}); !res.OK {
		t.Fatalf("navigate to the shadow page: %+v", res)
	}
	snap = cmd(CommandSnapshot, nil).Text
	if res := cmd(CommandClick, ClickArgs{Ref: refIn(snap, `button "Under the shadow"`)}); res.Code != CodeOffSite {
		t.Errorf("a click under a frame in a shadow root: %+v", res)
	}
	cmd(CommandEval, EvalArgs{Script: `document.getElementById('host').shadowRoot.querySelector('iframe').focus(), document.activeElement.id`})
	if res := cmd(CommandPress, PressArgs{Key: "a"}); res.Code != CodeOffSite {
		t.Errorf("a key into a frame in a shadow root: %+v", res)
	}
}
