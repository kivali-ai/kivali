package web

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// fakeBrowserProv stands in for the pod provisioner: it records starts
// and stops and signals each start so a test can play the pod.
type fakeBrowserProv struct {
	mu         sync.Mutex
	enabled    bool
	startErr   error
	started    []string // slug/session/token
	coreTokens []string
	stopped    []string
	stopAll    int
	startedC   chan string
	stoppedC   chan string
	// stopGate, when set, holds every Stop until it is closed.
	stopGate chan struct{}
	// startGate, when set, holds every Start, as a slow create does,
	// until it is closed or Start's context ends; startingC hears each
	// Start that is held.
	startGate chan struct{}
	startingC chan string
}

func newFakeBrowserProv() *fakeBrowserProv {
	return &fakeBrowserProv{enabled: true, startedC: make(chan string, 8), stoppedC: make(chan string, 8)}
}

// awaitStop blocks until the provisioner has been asked to stop a pod
// and returns which; the deadline is a deadlock diagnostic.
func (p *fakeBrowserProv) awaitStop(t *testing.T) string {
	t.Helper()
	select {
	case s := <-p.stoppedC:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("the provisioner was not asked to stop a pod")
		return ""
	}
}

func (p *fakeBrowserProv) Enabled() bool { return p.enabled }
func (p *fakeBrowserProv) Start(ctx context.Context, slug, session, token, coreToken string) error {
	p.mu.Lock()
	gate := p.startGate
	p.mu.Unlock()
	if gate != nil {
		p.startingC <- token
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.startErr != nil {
		return p.startErr
	}
	p.started = append(p.started, slug+"/"+session+"/"+token)
	p.coreTokens = append(p.coreTokens, coreToken)
	p.startedC <- token
	return nil
}
func (p *fakeBrowserProv) Stop(_ context.Context, slug, session string) error {
	p.mu.Lock()
	p.stopped = append(p.stopped, slug+"/"+session)
	gate := p.stopGate
	p.mu.Unlock()
	p.stoppedC <- slug + "/" + session
	if gate != nil {
		// A slow delete: the test says when it returns.
		<-gate
	}
	return nil
}
func (p *fakeBrowserProv) StopAll(context.Context) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopAll++
	return 0, nil
}

// sentFrame is one message a fake pod received, with its bytes.
type sentFrame struct {
	msg  browserpod.Message
	body []byte
}

// fakeLink records what core sends and signals each frame.
type fakeLink struct {
	mu     sync.Mutex
	frames []sentFrame
	sentC  chan sentFrame
	closed atomic.Bool
	err    error
}

func newFakeLink() *fakeLink { return &fakeLink{sentC: make(chan sentFrame, 64)} }

func (l *fakeLink) send(_ time.Duration, msg browserpod.Message, body io.Reader) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recordLocked(msg, body)
}

// recordLocked notes a frame the pod received. l.mu is held.
func (l *fakeLink) recordLocked(msg browserpod.Message, body io.Reader) error {
	if l.err != nil {
		return l.err
	}
	f := sentFrame{msg: msg}
	if body != nil && msg.Bytes > 0 {
		b, err := io.ReadAll(io.LimitReader(body, msg.Bytes))
		if err != nil {
			return err
		}
		f.body = b
	}
	l.frames = append(l.frames, f)
	l.sentC <- f
	return nil
}

// close records the close. Like the real link's, it does not take the
// write lock: a writer the pod has stopped reading from may hold it.
func (l *fakeLink) close() { l.closed.Store(true) }

// next is the next frame the pod received, of the given type, or a
// test failure after the deadline (a deadlock diagnostic, not a wait).
func (l *fakeLink) next(t *testing.T, typ string) sentFrame {
	t.Helper()
	for {
		select {
		case f := <-l.sentC:
			if f.msg.Type == typ {
				return f
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no %s frame arrived", typ)
		}
	}
}

// browserFixture is a registry over a real store with a fake
// provisioner, a fake clock and an overlay for downloads.
type browserFixture struct {
	t    *testing.T
	st   *store.FSStore
	prov *fakeBrowserProv
	clk  *clock.Fake
	reg  *browserRegistry
	root string
}

func newBrowserFixture(t *testing.T) *browserFixture {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "role"); err != nil {
		t.Fatal(err)
	}
	if err := files.Sync(files.BootstrapOptions{AgentRoot: filepath.Join(st.Root(), "agents", "alice")}); err != nil {
		t.Fatal(err)
	}
	prov := newFakeBrowserProv()
	clk := clock.NewFakeAt(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	root := subagentFilesRoot(st, "alice", "job1")
	if err := files.BuildSubagentOverlay(files.SubagentOverlay{Root: root}); err != nil {
		t.Fatal(err)
	}
	reg := newBrowserRegistry(st, prov, clk, 3, t.Logf, func(slug, id string) string { return subagentFilesRoot(st, slug, id) })
	reg.markBooted()
	return &browserFixture{t: t, st: st, prov: prov, clk: clk, reg: reg, root: root}
}

// connect plays the pod's connection for the vendor.example browser
// the last Start asked for: claims it with the token, attaches a fake
// link and sends the handshake. It returns the browser and the link.
func (f *browserFixture) connect(ctx context.Context) (*browser, *fakeLink) {
	f.t.Helper()
	link := newFakeLink()
	return f.connectWith("vendor.example", link), link
}

// connectWith is connect for the browser of session, over link.
func (f *browserFixture) connectWith(session string, link browserLink) *browser {
	f.t.Helper()
	var token string
	select {
	case token = <-f.prov.startedC:
	case <-time.After(10 * time.Second):
		f.t.Fatal("the provisioner was not asked to start a pod")
	}
	b := f.reg.claim("alice", session, token, link)
	if b == nil {
		f.t.Fatal("claim found no waiting browser")
	}
	if err := f.reg.attach(b); err != nil {
		f.t.Fatalf("attach: %v", err)
	}
	return b
}

// stopped waits for the provisioner to be asked to stop a pod and for
// the registry to let the record go, and returns which pod stopped.
func (f *browserFixture) stopped(t *testing.T) string {
	t.Helper()
	s := f.prov.awaitStop(t)
	f.reg.awaitStops()
	return s
}

// running starts a browser for group with one command, plays the pod
// up to running, answers that command and returns the browser and its
// link.
func (f *browserFixture) running(ctx context.Context, group string) (*browser, *fakeLink) {
	f.t.Helper()
	errs := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", group, browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	b, link := f.connect(ctx)
	link.next(f.t, browserpod.TypeStart)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	cmd := link.next(f.t, browserpod.TypeCommand)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "ok"}, nil)
	if err := <-errs; err != nil {
		f.t.Fatalf("first command: %v", err)
	}
	return b, link
}

// TestBrowserRegistryStartsRelaysAndReaps follows one browser through
// its life: the first command starts the pod, the handshake carries
// the saved profile, commands and results pair up under the group,
// the group's facts and downloads are kept, releasing the group makes
// the browser idle, the reap asks for the profile and stores it, and
// the next start hands that profile over.
func TestBrowserRegistryStartsRelaysAndReaps(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()

	type outcome struct {
		res browserpod.Message
		err error
	}
	got := make(chan outcome, 1)
	go func() {
		res, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		got <- outcome{res, err}
	}()
	b, link := f.connect(ctx)
	hello := link.next(t, browserpod.TypeHello)
	// A site session: the pod is told the site and runs headed.
	if hello.msg.Bytes != 0 || hello.msg.Site != "vendor.example" || !hello.msg.Headful || hello.msg.Ephemeral {
		t.Errorf("first hello = %+v, want no profile, the site and headful", hello.msg)
	}
	link.next(t, browserpod.TypeStart)
	if states := f.reg.states(); states["alice/vendor.example"] != string(browserStarting) {
		t.Errorf("state before running = %v", states)
	}
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)

	cmd := link.next(t, browserpod.TypeCommand)
	if cmd.msg.Name != browserpod.CommandSnapshot || cmd.msg.Group != "job1" || cmd.msg.ID == "" {
		t.Fatalf("command frame = %+v", cmd.msg)
	}
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeTab, Group: "job1", Tab: "t1", URL: "about:blank"}, nil)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeNavigated, Group: "job1", Tab: "t1", URL: "https://vendor.example/pricing?x=1", Title: "Pricing"}, nil)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "Page: Pricing"}, nil)
	out := <-got
	if out.err != nil || !out.res.OK || out.res.Text != "Page: Pricing" {
		t.Fatalf("Command = %+v, %v", out.res, out.err)
	}

	// A download lands in the group's untrusted-downloads/.
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeDownload, Group: "job1", Tab: "t1", FileName: "quote (1).pdf", Bytes: 9}, strings.NewReader("pdf bytes"))
	if body, err := os.ReadFile(filepath.Join(f.root, files.UntrustedDownloadsDir, "quote-1.pdf")); err != nil || string(body) != "pdf bytes" {
		t.Errorf("download on disk = %q, %v", body, err)
	}

	facts, downloads := f.reg.ReleaseGroup("alice", "vendor.example", "job1")
	if facts.Pages != 1 || facts.Origin != "https://vendor.example" || len(downloads) != 1 || downloads[0] != "quote-1.pdf" {
		t.Errorf("ReleaseGroup = %+v, %v", facts, downloads)
	}
	if cg := link.next(t, browserpod.TypeCloseGroup); cg.msg.Group != "job1" {
		t.Errorf("close-group = %+v", cg.msg)
	}
	if states := f.reg.states(); states["alice/vendor.example"] != string(browserIdle) {
		t.Fatalf("state after release = %v, want idle", states)
	}

	// Short of the idle limit nothing happens; at it the reap begins.
	f.clk.Advance(browserIdleAfter - time.Second)
	next := f.reg.sweep(f.clk.Now())
	if next.IsZero() || f.reg.states()["alice/vendor.example"] != string(browserIdle) {
		t.Errorf("early sweep: next=%v states=%v", next, f.reg.states())
	}
	f.clk.Advance(time.Second)
	f.clk.Advance(browserIdleAfter)
	f.reg.sweep(f.clk.Now())
	link.next(t, browserpod.TypeSave)
	if states := f.reg.states(); states["alice/vendor.example"] != string(browserSaving) {
		t.Fatalf("state after reap = %v, want saving", states)
	}
	profile := bytes.Repeat([]byte("p"), 5000)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeProfile, Bytes: int64(len(profile))}, bytes.NewReader(profile))
	// The pod's delete is held so the record can be seen in between.
	gate := make(chan struct{})
	f.prov.mu.Lock()
	f.prov.stopGate = gate
	f.prov.mu.Unlock()
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeSaved}, nil)
	if states := f.reg.states(); states["alice/vendor.example"] != string(browserStopped) {
		t.Errorf("state while the pod is deleted = %v, want stopped", states)
	}
	close(gate)
	if stopped := f.stopped(t); stopped != "alice/vendor.example" {
		t.Errorf("pod stop = %q", stopped)
	}
	if states := f.reg.states(); len(states) != 0 {
		t.Errorf("browser still registered after save: %v", states)
	}
	link.mu.Lock()
	closed := link.closed.Load()
	link.mu.Unlock()
	if !closed {
		t.Error("link not closed after save")
	}
	if r, n, err := f.st.OpenBrowserProfile("alice", "vendor.example"); err != nil || n != int64(len(profile)) {
		t.Fatalf("stored profile = %d, %v", n, err)
	} else {
		_ = r.Close()
	}

	// The next browser starts from that profile.
	go func() {
		res, err := f.reg.Command(ctx, "alice", "vendor.example", "job2", browserpod.Message{Name: browserpod.CommandSnapshot})
		got <- outcome{res, err}
	}()
	b2, link2 := f.connect(ctx)
	hello2 := link2.next(t, browserpod.TypeHello)
	if hello2.msg.Bytes != int64(len(profile)) || !bytes.Equal(hello2.body, profile) {
		t.Errorf("second hello carried %d bytes, want the saved profile", len(hello2.body))
	}
	f.reg.onMessage(b2, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	cmd2 := link2.next(t, browserpod.TypeCommand)
	f.reg.onMessage(b2, browserpod.Message{Type: browserpod.TypeResult, ID: cmd2.msg.ID, OK: false, Error: "no such ref", Code: browserpod.CodeNotFound}, nil)
	if out := <-got; out.err != nil || out.res.OK || out.res.Error != "no such ref" {
		t.Errorf("second Command = %+v, %v", out.res, out.err)
	}
}

// TestBrowserRegistryStartTimeoutLostAndCap: a pod that never connects
// fails the waiting command at the start timeout and is deleted; a
// connection lost mid-command fails that command; the team cap refuses
// a browser beyond it; a disabled provisioner refuses everything.
func TestBrowserRegistryStartTimeoutLostAndCap(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	errs := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	<-f.prov.startedC
	f.clk.Advance(browserStartTimeout)
	f.reg.sweep(f.clk.Now())
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "did not start in time") {
		t.Errorf("start timeout err = %v", err)
	}
	f.stopped(t)

	// Lost mid-command.
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	b, link := f.connect(ctx)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	link.next(t, browserpod.TypeCommand)
	f.reg.onLost(b)
	if err := <-errs; err == nil || !strings.Contains(err.Error(), "lost") {
		t.Errorf("lost err = %v", err)
	}
	f.stopped(t)
	if len(f.reg.states()) != 0 {
		t.Errorf("lost browser still registered: %v", f.reg.states())
	}

	// The cap.
	f.reg.cap = 1
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	<-f.prov.startedC
	if _, err := f.reg.Command(ctx, "alice", "vendor.com", "job2", browserpod.Message{Name: browserpod.CommandSnapshot}); err == nil || !strings.Contains(err.Error(), "all 1 browsers are in use") {
		t.Errorf("cap err = %v", err)
	}
	f.clk.Advance(browserStartTimeout)
	f.reg.sweep(f.clk.Now())
	<-errs
	f.stopped(t)

	// Disabled.
	f.prov.enabled = false
	if _, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{}); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("disabled err = %v", err)
	}
}

// TestBrowserRegistryTellsAStartFailureApart guards how a web task
// learns its browser could not start: every way a start fails must
// read as a start failure, which ends the task, or the subagent is
// left to name a cause it cannot see (it once reported a Chromium
// crash as the site being unreachable). A refusal worth retrying, or a
// browser lost after it ran, must not, or it would end a task that
// could go on.
func TestBrowserRegistryTellsAStartFailureApart(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	command := func(group string) <-chan error {
		errs := make(chan error, 1)
		go func() {
			_, err := f.reg.Command(ctx, "alice", "vendor.example", group, browserpod.Message{Name: browserpod.CommandSnapshot})
			errs <- err
		}()
		return errs
	}
	wantStart := func(name string, err error, text string) {
		t.Helper()
		if err == nil || !isBrowserStartError(err) || !strings.Contains(err.Error(), text) {
			t.Errorf("%s: err = %v, want a start failure naming %q", name, err, text)
		}
		f.stopped(t)
	}

	// The pod could not be made.
	f.prov.mu.Lock()
	f.prov.startErr = errors.New("quota exceeded")
	f.prov.mu.Unlock()
	wantStart("pod not made", <-command("job1"), "start the browser pod: quota exceeded")
	f.prov.mu.Lock()
	f.prov.startErr = nil
	f.prov.mu.Unlock()

	// The pod reported stopped while starting: Chromium did not come up.
	errs := command("job1")
	b, link := f.connect(ctx)
	link.next(t, browserpod.TypeStart)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateStopped, Detail: "chromium exited at start: exit status 13"}, nil)
	wantStart("chromium exited", <-errs, "the browser could not start: chromium exited at start")

	// The pod never connected.
	errs = command("job1")
	<-f.prov.startedC
	f.clk.Advance(browserStartTimeout)
	f.reg.sweep(f.clk.Now())
	wantStart("start timeout", <-errs, "the browser did not start in time")

	// The connection was lost before the pod reported running.
	errs = command("job1")
	b, _ = f.connect(ctx)
	f.reg.onLost(b)
	wantStart("lost while starting", <-errs, "the browser was lost before it started")

	// The cap: try again shortly.
	f.reg.cap = 0
	if err := <-command("job1"); err == nil || isBrowserStartError(err) {
		t.Errorf("cap: err = %v, want a refusal that is not a start failure", err)
	}
	f.reg.cap = 3

	// Lost after it ran: the next use starts afresh.
	b, link = f.running(ctx, "job1")
	errs = command("job1")
	link.next(t, browserpod.TypeCommand)
	f.reg.onLost(b)
	if err := <-errs; err == nil || isBrowserStartError(err) {
		t.Errorf("lost mid-command: err = %v, want an error that is not a start failure", err)
	}
	f.stopped(t)
}

// TestBrowserRegistryStartOutlivesItsFirstCaller guards the tasks that
// share a starting browser. The pod's start ran under the first
// caller's request, so a first caller that went away while the pod was
// being made (a window closed, a task stopped) failed the start for
// every other waiter, and a start failure ends a web task: one task's
// cancel ended its siblings as "the browser could not start".
func TestBrowserRegistryStartOutlivesItsFirstCaller(t *testing.T) {
	f := newBrowserFixture(t)
	gate := make(chan struct{})
	f.prov.mu.Lock()
	f.prov.startGate, f.prov.startingC = gate, make(chan string, 1)
	f.prov.mu.Unlock()

	// A window starts the browser and is closed while the pod is made.
	window, closeWindow := context.WithCancel(context.Background())
	windowErr := make(chan error, 1)
	go func() {
		_, err := f.reg.Watch(window, "alice", "vendor.example", "job1", false)
		windowErr <- err
	}()
	select {
	case <-f.prov.startingC:
	case <-time.After(10 * time.Second):
		t.Fatal("the provisioner was not asked to start a pod")
	}
	task := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(context.Background(), "alice", "vendor.example", "job2", browserpod.Message{Name: browserpod.CommandSnapshot})
		task <- err
	}()
	closeWindow()
	if err := <-windowErr; !errors.Is(err, context.Canceled) {
		t.Errorf("the closed window's wait = %v, want its own cancel", err)
	}

	// The pod is made and comes up: the task's command goes through.
	close(gate)
	b, link := f.connect(context.Background())
	link.next(t, browserpod.TypeStart)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	cmd := link.next(t, browserpod.TypeCommand)
	if cmd.msg.Group != "job2" {
		t.Fatalf("command frame = %+v, want the task's", cmd.msg)
	}
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "ok"}, nil)
	if err := <-task; err != nil {
		t.Errorf("the task's command = %v, want its result", err)
	}
	f.prov.mu.Lock()
	stopped := len(f.prov.stopped)
	f.prov.mu.Unlock()
	if stopped != 0 {
		t.Errorf("the browser was stopped %d time(s) for a caller that went away", stopped)
	}
}

// TestBrowserRegistryResetGivesAWaiterTheNextBrowser guards a task that
// asked for a browser just as its session was reset: the reset ended
// the starting browser with a start failure, which ends a web task as
// "the browser could not start". The task asked for a browser; it must
// be given the session's next one, started once the reset browser's
// pod is gone.
func TestBrowserRegistryResetGivesAWaiterTheNextBrowser(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	task := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		task <- err
	}()
	first := <-f.prov.startedC
	if err := f.reg.Reset("alice", "vendor.example"); err != nil {
		t.Fatal(err)
	}
	if stopped := f.prov.awaitStop(t); stopped != "alice/vendor.example" {
		t.Fatalf("stopped %q", stopped)
	}
	b, link := f.connect(ctx)
	if b.token == first {
		t.Fatal("the reset browser was connected, not the next one")
	}
	link.next(t, browserpod.TypeStart)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	cmd := link.next(t, browserpod.TypeCommand)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeResult, ID: cmd.msg.ID, OK: true, Text: "ok"}, nil)
	if err := <-task; err != nil {
		t.Errorf("the waiting task's command = %v, want the next browser's result", err)
	}
}

// TestBrowserRegistryClaimNeedsTheRightToken: a connection is accepted
// only for the browser waiting for this token, once.
func TestBrowserRegistryClaimNeedsTheRightToken(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	go func() {
		_, _ = f.reg.Command(ctx, "alice", "vendor.example", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
	}()
	token := <-f.prov.startedC
	if f.reg.claim("alice", "vendor.example", "wrong", newFakeLink()) != nil || f.reg.claim("bob", "vendor.example", token, newFakeLink()) != nil || f.reg.claim("alice", "vendor.example", "", newFakeLink()) != nil {
		t.Error("a wrong token, slug or empty token claimed a browser")
	}
	b := f.reg.claim("alice", "vendor.example", token, newFakeLink())
	if b == nil {
		t.Fatal("the right token did not claim")
	}
	if err := f.reg.attach(b); err != nil {
		t.Fatal(err)
	}
	if f.reg.claim("alice", "vendor.example", token, newFakeLink()) != nil {
		t.Error("a browser with a link was claimed again")
	}
	f.reg.finish(b)
	if !errors.Is(ctx.Err(), nil) {
		t.Fatal("unreachable")
	}
}
