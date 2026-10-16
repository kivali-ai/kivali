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
	mu       sync.Mutex
	enabled  bool
	startErr error
	started  []string // slug/session/token
	stopped  []string
	stopAll  int
	startedC chan string
	stoppedC chan string
	// stopGate, when set, holds every Stop until it is closed.
	stopGate chan struct{}
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
func (p *fakeBrowserProv) Start(_ context.Context, slug, session, token string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.startErr != nil {
		return p.startErr
	}
	p.started = append(p.started, slug+"/"+session+"/"+token)
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
	closed bool
	err    error
}

func newFakeLink() *fakeLink { return &fakeLink{sentC: make(chan sentFrame, 64)} }

func (l *fakeLink) send(_ context.Context, msg browserpod.Message, body io.Reader) error {
	l.mu.Lock()
	defer l.mu.Unlock()
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

func (l *fakeLink) close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

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

// connect plays the pod's connection for the browser the last Start
// asked for: claims it with the token, attaches a fake link and
// reports running. It returns the browser and the link.
func (f *browserFixture) connect(ctx context.Context) (*browser, *fakeLink) {
	f.t.Helper()
	var token string
	select {
	case token = <-f.prov.startedC:
	case <-time.After(10 * time.Second):
		f.t.Fatal("the provisioner was not asked to start a pod")
	}
	link := newFakeLink()
	b := f.reg.claim("alice", "open", token, link)
	if b == nil {
		f.t.Fatal("claim found no waiting browser")
	}
	if err := f.reg.attach(ctx, b); err != nil {
		f.t.Fatalf("attach: %v", err)
	}
	return b, link
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
		_, err := f.reg.Command(ctx, "alice", "open", group, browserpod.Message{Name: browserpod.CommandSnapshot})
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
		res, err := f.reg.Command(ctx, "alice", "open", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		got <- outcome{res, err}
	}()
	b, link := f.connect(ctx)
	hello := link.next(t, browserpod.TypeHello)
	if hello.msg.Bytes != 0 || hello.msg.Site != "" {
		t.Errorf("first hello = %+v, want no profile and no site", hello.msg)
	}
	link.next(t, browserpod.TypeStart)
	if states := f.reg.states(); states["alice/open"] != string(browserStarting) {
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

	facts, downloads := f.reg.ReleaseGroup("alice", "open", "job1")
	if facts.Pages != 1 || facts.Origin != "https://vendor.example" || len(downloads) != 1 || downloads[0] != "quote-1.pdf" {
		t.Errorf("ReleaseGroup = %+v, %v", facts, downloads)
	}
	if cg := link.next(t, browserpod.TypeCloseGroup); cg.msg.Group != "job1" {
		t.Errorf("close-group = %+v", cg.msg)
	}
	if states := f.reg.states(); states["alice/open"] != string(browserIdle) {
		t.Fatalf("state after release = %v, want idle", states)
	}

	// Short of the idle limit nothing happens; at it the reap begins.
	f.clk.Advance(browserIdleAfter - time.Second)
	next := f.reg.sweep(f.clk.Now())
	if next.IsZero() || f.reg.states()["alice/open"] != string(browserIdle) {
		t.Errorf("early sweep: next=%v states=%v", next, f.reg.states())
	}
	f.clk.Advance(time.Second)
	f.clk.Advance(browserIdleAfter)
	f.reg.sweep(f.clk.Now())
	link.next(t, browserpod.TypeSave)
	if states := f.reg.states(); states["alice/open"] != string(browserSaving) {
		t.Fatalf("state after reap = %v, want saving", states)
	}
	profile := bytes.Repeat([]byte("p"), 5000)
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeProfile, Bytes: int64(len(profile))}, bytes.NewReader(profile))
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeSaved}, nil)
	if states := f.reg.states(); states["alice/open"] != string(browserStopped) {
		t.Errorf("state while the pod is deleted = %v, want stopped", states)
	}
	if stopped := f.stopped(t); stopped != "alice/open" {
		t.Errorf("pod stop = %q", stopped)
	}
	if states := f.reg.states(); len(states) != 0 {
		t.Errorf("browser still registered after save: %v", states)
	}
	link.mu.Lock()
	closed := link.closed
	link.mu.Unlock()
	if !closed {
		t.Error("link not closed after save")
	}
	if r, n, err := f.st.OpenBrowserProfile("alice", "open"); err != nil || n != int64(len(profile)) {
		t.Fatalf("stored profile = %d, %v", n, err)
	} else {
		_ = r.Close()
	}

	// The next browser starts from that profile.
	go func() {
		res, err := f.reg.Command(ctx, "alice", "open", "job2", browserpod.Message{Name: browserpod.CommandSnapshot})
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
		_, err := f.reg.Command(ctx, "alice", "open", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
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
		_, err := f.reg.Command(ctx, "alice", "open", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
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
		_, err := f.reg.Command(ctx, "alice", "open", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
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
	if _, err := f.reg.Command(ctx, "alice", "open", "job1", browserpod.Message{}); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("disabled err = %v", err)
	}
}

// TestBrowserRegistryClaimNeedsTheRightToken: a connection is accepted
// only for the browser waiting for this token, once.
func TestBrowserRegistryClaimNeedsTheRightToken(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	go func() {
		_, _ = f.reg.Command(ctx, "alice", "open", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
	}()
	token := <-f.prov.startedC
	if f.reg.claim("alice", "open", "wrong", newFakeLink()) != nil || f.reg.claim("bob", "open", token, newFakeLink()) != nil || f.reg.claim("alice", "open", "", newFakeLink()) != nil {
		t.Error("a wrong token, slug or empty token claimed a browser")
	}
	b := f.reg.claim("alice", "open", token, newFakeLink())
	if b == nil {
		t.Fatal("the right token did not claim")
	}
	if err := f.reg.attach(ctx, b); err != nil {
		t.Fatal(err)
	}
	if f.reg.claim("alice", "open", token, newFakeLink()) != nil {
		t.Error("a browser with a link was claimed again")
	}
	f.reg.finish(b)
	if !errors.Is(ctx.Err(), nil) {
		t.Fatal("unreachable")
	}
}
