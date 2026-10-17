package browserpod

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kivali-ai/kivali/internal/clock"
)

// failsafe bounds how long a test waits for a message before failing
// with a useful error instead of hanging the package. Nothing waits it
// out in a passing test.
const failsafe = 60 * time.Second

// ─── fake core ──────────────────────────────────────────────────────

// fakeCore is core's end of the pod's WebSocket, served on a Unix
// socket the way core.sock is.
type fakeCore struct {
	t     *testing.T
	sock  string
	conns chan *websocket.Conn
	srv   *http.Server
	// path and token record the last connect request.
	mu    sync.Mutex
	path  string
	token string
}

func newFakeCore(t *testing.T) *fakeCore {
	t.Helper()
	// Unix socket paths are short on macOS; t.TempDir can be too long.
	dir, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	fc := &fakeCore{t: t, sock: filepath.Join(dir, "core.sock"), conns: make(chan *websocket.Conn, 1)}
	ln, err := net.Listen("unix", fc.sock)
	if err != nil {
		t.Fatal(err)
	}
	fc.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.path, fc.token = r.URL.Path, r.Header.Get(TokenHeader)
		fc.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		fc.conns <- c
	})}
	go func() { _ = fc.srv.Serve(ln) }()
	t.Cleanup(func() { _ = fc.srv.Close() })
	return fc
}

// coreSide reads what the pod sends and lets a test send to it.
type coreSide struct {
	t    *testing.T
	conn *websocket.Conn
	w    *wire
	in   chan received
	mu   sync.Mutex
	held []received
}

type received struct {
	Message
	data []byte
	err  error
}

func (fc *fakeCore) accept() *coreSide {
	fc.t.Helper()
	select {
	case c := <-fc.conns:
		cs := &coreSide{t: fc.t, conn: c, w: newWire(c), in: make(chan received, 1024)}
		go cs.readLoop()
		fc.t.Cleanup(func() { _ = c.CloseNow() })
		return cs
	case <-time.After(failsafe):
		fc.t.Fatal("the pod never connected")
		return nil
	}
}

func (cs *coreSide) readLoop() {
	for {
		msg, body, err := cs.w.readMessage(context.Background())
		if err != nil {
			cs.in <- received{err: err}
			return
		}
		data, err := io.ReadAll(body)
		cs.in <- received{Message: msg, data: data, err: err}
		if err != nil {
			return
		}
	}
}

func (cs *coreSide) send(msg Message) {
	cs.t.Helper()
	if err := cs.w.writeMessage(context.Background(), msg, nil); err != nil {
		cs.t.Fatalf("send %s: %v", msg.Type, err)
	}
}

func (cs *coreSide) sendBytes(msg Message, data []byte) {
	cs.t.Helper()
	if err := cs.w.sendBytes(context.Background(), msg, data); err != nil {
		cs.t.Fatalf("send %s: %v", msg.Type, err)
	}
}

// next returns the first message (held or arriving) that match
// accepts, holding the others for later calls.
func (cs *coreSide) next(what string, match func(Message) bool) received {
	cs.t.Helper()
	cs.mu.Lock()
	for i, r := range cs.held {
		if match(r.Message) {
			cs.held = append(cs.held[:i:i], cs.held[i+1:]...)
			cs.mu.Unlock()
			return r
		}
	}
	cs.mu.Unlock()
	deadline := time.After(failsafe)
	for {
		select {
		case r := <-cs.in:
			if r.err != nil {
				cs.t.Fatalf("waiting for %s: connection: %v", what, r.err)
			}
			if match(r.Message) {
				return r
			}
			cs.mu.Lock()
			cs.held = append(cs.held, r)
			cs.mu.Unlock()
		case <-deadline:
			cs.t.Fatalf("no %s arrived", what)
		}
	}
}

func (cs *coreSide) expectType(typ string) received {
	cs.t.Helper()
	return cs.next(typ, func(m Message) bool { return m.Type == typ })
}

func (cs *coreSide) expectState(state string) received {
	cs.t.Helper()
	return cs.next("state "+state, func(m Message) bool { return m.Type == TypeState && m.State == state })
}

var cmdSeq struct {
	sync.Mutex
	n int
}

// command sends a command and waits for its result.
func (cs *coreSide) command(group, tabID, name string, args any) Message {
	cs.t.Helper()
	id := cs.sendCommand(group, tabID, name, args)
	return cs.result(id)
}

func (cs *coreSide) sendCommand(group, tabID, name string, args any) string {
	cs.t.Helper()
	cmdSeq.Lock()
	cmdSeq.n++
	id := fmt.Sprintf("c%d", cmdSeq.n)
	cmdSeq.Unlock()
	raw, err := json.Marshal(args)
	if err != nil {
		cs.t.Fatal(err)
	}
	cs.send(Message{Type: TypeCommand, ID: id, Group: group, Tab: tabID, Name: name, Args: raw})
	return id
}

func (cs *coreSide) result(id string) Message {
	cs.t.Helper()
	return cs.next("result "+id, func(m Message) bool { return m.Type == TypeResult && m.ID == id }).Message
}

// ─── fake Chromium ──────────────────────────────────────────────────

// fakeCDP is a browser with pages that load instantly: enough of CDP
// for Runtime to be driven without Chromium.
type fakeCDP struct {
	mu       sync.Mutex
	subs     map[cdpKey][]*cdpHandler
	events   *serialQueue
	cancel   context.CancelFunc
	nextT    int
	sessions map[string]string // session → target
	urls     map[string]string // target → url
	calls    []string
	// contexts is the browser context each target was created in ("" for
	// the default one); disposed lists the contexts disposed of.
	nextC    int
	contexts map[string]string
	disposed []string
	// jar is what Storage.getCookies answers.
	jar []map[string]any
	// override answers a method before the built-in fake does.
	override func(ctx context.Context, session, method string, params json.RawMessage) (any, bool, error)
}

func newFakeCDP(t *testing.T) *fakeCDP {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeCDP{
		subs:     map[cdpKey][]*cdpHandler{},
		events:   newSerialQueue(),
		cancel:   cancel,
		sessions: map[string]string{},
		urls:     map[string]string{},
		contexts: map[string]string{},
	}
	go f.events.run(ctx)
	t.Cleanup(cancel)
	return f
}

func (f *fakeCDP) On(session, method string, fn func(json.RawMessage)) func() {
	h := &cdpHandler{fn: fn}
	k := cdpKey{session, method}
	f.mu.Lock()
	f.subs[k] = append(f.subs[k], h)
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		hs := f.subs[k]
		for i, x := range hs {
			if x == h {
				f.subs[k] = append(hs[:i:i], hs[i+1:]...)
				return
			}
		}
	}
}

// emit delivers an event on the event goroutine, after those before it.
func (f *fakeCDP) emit(session, method string, params any) {
	raw, _ := json.Marshal(params)
	f.events.push(func() {
		f.mu.Lock()
		hs := append([]*cdpHandler(nil), f.subs[cdpKey{session, method}]...)
		f.mu.Unlock()
		for _, h := range hs {
			h.fn(raw)
		}
	})
}

func (f *fakeCDP) setOverride(fn func(ctx context.Context, session, method string, params json.RawMessage) (any, bool, error)) {
	f.mu.Lock()
	f.override = fn
	f.mu.Unlock()
}

func (f *fakeCDP) called(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeCDP) Call(ctx context.Context, session, method string, params any) (json.RawMessage, error) {
	raw, _ := json.Marshal(params)
	f.mu.Lock()
	f.calls = append(f.calls, method+" "+string(raw))
	override := f.override
	f.mu.Unlock()
	if override != nil {
		res, ok, err := override(ctx, session, method, raw)
		if ok {
			if err != nil {
				return nil, err
			}
			out, _ := json.Marshal(res)
			return out, nil
		}
	}
	res, err := f.answer(session, method, raw)
	if err != nil {
		return nil, err
	}
	out, _ := json.Marshal(res)
	return out, nil
}

func (f *fakeCDP) answer(session, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "Target.createBrowserContext":
		f.mu.Lock()
		f.nextC++
		id := fmt.Sprintf("C%d", f.nextC)
		f.mu.Unlock()
		return map[string]any{"browserContextId": id}, nil
	case "Target.disposeBrowserContext":
		var p struct {
			ID string `json:"browserContextId"`
		}
		_ = json.Unmarshal(raw, &p)
		f.mu.Lock()
		f.disposed = append(f.disposed, p.ID)
		f.mu.Unlock()
		return map[string]any{}, nil
	case "Target.createTarget":
		var p struct {
			ContextID string `json:"browserContextId"`
		}
		_ = json.Unmarshal(raw, &p)
		f.mu.Lock()
		f.nextT++
		target, sess := fmt.Sprintf("T%d", f.nextT), fmt.Sprintf("S%d", f.nextT)
		f.sessions[sess] = target
		f.urls[target] = "about:blank"
		f.contexts[target] = p.ContextID
		f.mu.Unlock()
		f.emit("", "Target.attachedToTarget", map[string]any{
			"sessionId": sess, "targetInfo": map[string]any{"targetId": target, "type": "page", "url": "about:blank"},
		})
		return map[string]any{"targetId": target}, nil
	case "Target.closeTarget":
		var p struct {
			TargetID string `json:"targetId"`
		}
		_ = json.Unmarshal(raw, &p)
		f.emit("", "Target.targetDestroyed", map[string]any{"targetId": p.TargetID})
		return map[string]any{"success": true}, nil
	case "Target.getTargetInfo":
		var p struct {
			TargetID string `json:"targetId"`
		}
		_ = json.Unmarshal(raw, &p)
		f.mu.Lock()
		u := f.urls[p.TargetID]
		f.mu.Unlock()
		return map[string]any{"targetInfo": map[string]any{"targetId": p.TargetID, "type": "page", "url": u, "title": "Title of " + u}}, nil
	case "Page.navigate":
		var p struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(raw, &p)
		f.mu.Lock()
		target := f.sessions[session]
		f.urls[target] = p.URL
		f.nextT++
		loader := fmt.Sprintf("L%d", f.nextT)
		f.mu.Unlock()
		f.emit(session, "Page.frameStartedLoading", map[string]any{"frameId": target})
		f.emit(session, "Page.frameNavigated", map[string]any{"frame": map[string]any{"id": target, "loaderId": loader, "url": p.URL}})
		f.emit(session, "Page.loadEventFired", map[string]any{})
		f.emit(session, "Page.lifecycleEvent", map[string]any{"frameId": target, "loaderId": loader, "name": "networkIdle"})
		f.emit(session, "Page.frameStoppedLoading", map[string]any{"frameId": target})
		return map[string]any{"frameId": target, "loaderId": loader}, nil
	case "Storage.getCookies":
		f.mu.Lock()
		jar := f.jar
		f.mu.Unlock()
		if jar == nil {
			jar = []map[string]any{}
		}
		return map[string]any{"cookies": jar}, nil
	case "Page.createIsolatedWorld":
		return map[string]any{"executionContextId": 7}, nil
	case "Runtime.evaluate":
		return map[string]any{"result": map[string]any{"type": "boolean", "value": false}}, nil
	}
	return map[string]any{}, nil
}

// setJar is what the browser's cookies become.
func (f *fakeCDP) setJar(cookies ...map[string]any) {
	f.mu.Lock()
	f.jar = cookies
	f.mu.Unlock()
}

// fakeProc is a browserProcess around a fakeCDP.
type fakeProc struct {
	conn *fakeCDP
	done chan struct{}
	once sync.Once
}

func (p *fakeProc) Conn() cdpConn                   { return p.conn }
func (p *fakeProc) Done() <-chan struct{}           { return p.done }
func (p *fakeProc) Detail() string                  { return "fake" }
func (p *fakeProc) SandboxOff() bool                { return false }
func (p *fakeProc) Close(ctx context.Context) error { p.Kill(); return nil }
func (p *fakeProc) Kill()                           { p.once.Do(func() { close(p.done) }) }

// ─── a pod against both fakes ───────────────────────────────────────

type podHarness struct {
	t    *testing.T
	core *coreSide
	fc   *fakeCore
	cdp  *fakeCDP
	proc *fakeProc
	clk  *clock.Fake
	dir  string
	done chan error
}

type podOptions struct {
	site      string
	ephemeral bool
	profile   []byte
	cookies   []byte            // a cookie snapshot, sent before start
	jar       []map[string]any  // the browser's cookies from the start
	files     map[string]string // group/name → content, sent before start
}

// startPod runs a Runtime against a fake core and a fake Chromium and
// takes it through hello and start.
func startPod(t *testing.T, o podOptions) *podHarness {
	t.Helper()
	fc := newFakeCore(t)
	cdp := newFakeCDP(t)
	cdp.setJar(o.jar...)
	proc := &fakeProc{conn: cdp, done: make(chan struct{})}
	clk := clock.NewFake()
	dir := t.TempDir()
	rt := &Runtime{
		Slug: "ada", Session: "s1", Token: "tok", CoreToken: "ctok", CoreUDS: fc.sock, Dir: dir,
		Logger: log.New(io.Discard, "", 0), Clock: clk,
		launch: func(context.Context, chromeOptions) (browserProcess, error) { return proc, nil },
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
	core.sendBytes(Message{Type: TypeHello, CoreToken: "ctok", Site: o.site, Ephemeral: o.ephemeral}, o.profile)
	for key, content := range o.files {
		group, name, _ := strings.Cut(key, "/")
		core.sendBytes(Message{Type: TypeFile, Group: group, FileName: name}, []byte(content))
	}
	if o.cookies != nil {
		core.sendBytes(Message{Type: TypeCookies}, o.cookies)
	}
	core.send(Message{Type: TypeStart})
	core.expectState(StateStarting)
	core.expectState(StateRunning)
	return &podHarness{t: t, core: core, fc: fc, cdp: cdp, proc: proc, clk: clk, dir: dir, done: done}
}

func (h *podHarness) runResult() error {
	h.t.Helper()
	select {
	case err := <-h.done:
		h.done <- err
		return err
	case <-time.After(failsafe):
		h.t.Fatal("Run did not return")
		return nil
	}
}
