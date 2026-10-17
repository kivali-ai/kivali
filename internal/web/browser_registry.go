// Package web (browser_registry.go) — the browsers an agent's web
// subagents run in, as core sees them.
//
// One record per (agent, session) with a browser pod: its state, the
// pod's connection, the tab groups (one per web subagent using it) and
// the deadline the lifecycle timer watches. Core owns the lifecycle. A
// web subagent's first command starts the pod and hands it the saved
// profile; commands are relayed under the subagent's group; when no
// subagent uses the browser it is idle, and after browserIdleAfter core
// asks for the profile back, writes it to the store and deletes the
// pod. The open session is ephemeral (ephemeralSession): its browser
// starts empty, each task's tabs live in a browser context of their
// own, and no profile is asked for or stored.
// The registry is in memory: on boot every browser pod is deleted
// and the saved profiles are what persists.
//
// The pod is the least trusted thing core talks to: it runs Chromium
// on whatever the web serves. Everything it sends is checked against
// what core itself recorded: a download must name a group core opened,
// a profile arrives only while core asked for one, and sizes are
// bounded. Nothing the pod sends becomes a path.
//
// All timers use the injected clock and one timer re-armed to the
// earliest deadline, as runAutoRelease does.

package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/structured"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Browser lifecycle limits.
const (
	// browserIdleAfter is how long a browser with no subagent and no
	// window stays up before core saves its profile and deletes it.
	browserIdleAfter = 10 * time.Minute
	// browserLifetime is how long a browser runs before it is saved and
	// started afresh: Chromium's memory grows with use. A browser past
	// it takes no new task and is saved as soon as its last task ends,
	// never under a task or a window.
	browserLifetime = 2 * time.Hour
	// browserStartTimeout bounds a pod's start: the first use pulls
	// the image.
	browserStartTimeout = 5 * time.Minute
	// browserSaveTimeout bounds a save: a clean close and the profile
	// upload. Longer than the pod's stop grace so a slow upload is not
	// cut short by core.
	browserSaveTimeout = 90 * time.Second
	// browserCommandTimeout bounds a relayed command; the long one
	// covers navigate and wait, which the pod itself allows 45 s.
	browserCommandTimeout     = 30 * time.Second
	browserLongCommandTimeout = 60 * time.Second
	// browserSendTimeout bounds one write to the pod; a pod that stops
	// reading for that long is hung, and the write's failure ends it.
	// File sends get longer for their size.
	browserSendTimeout     = 15 * time.Second
	browserFileSendTimeout = 2 * time.Minute
	// browserSendChunk is the size of one binary frame on the way to
	// the pod.
	browserSendChunk = 64 << 10
	// Downloads one group may produce: the count and the bytes.
	maxDownloadsPerGroup     = 20
	maxDownloadBytesPerGroup = 200 << 20
	// browserWindowGrace is how long a group's screencast stays on
	// after its last window has gone: a reload or a brief disconnect
	// does not count as closing the window.
	browserWindowGrace = 10 * time.Second
	// browserMaxFrameBytes bounds one screencast frame from the pod.
	browserMaxFrameBytes = 4 << 20
	// browserWatcherEvents is how many unread text messages a window
	// may hold before the oldest is dropped.
	browserWatcherEvents = 16
)

// browserState is where a browser is in its life.
type browserState string

const (
	browserStarting browserState = "starting"
	browserRunning  browserState = "running"
	browserIdle     browserState = "idle"
	browserSaving   browserState = "saving"
	// browserStopped: the pod is being deleted; the record stays, and
	// counts toward the cap, until the delete returns, so a new start
	// under the same name cannot be killed by the old delete.
	browserStopped browserState = "stopped"
)

// BrowserProvisioner starts and stops browser pods.
// browserpod.Provisioner implements it; tests inject a fake.
type BrowserProvisioner interface {
	Enabled() bool
	Start(ctx context.Context, slug, session, token string) error
	Stop(ctx context.Context, slug, session string) error
	StopAll(ctx context.Context) (int, error)
}

// browserLink is a connected pod as the registry uses it: it sends a
// message, with the bytes that follow it, and closes. The WebSocket
// handler in browser_socket.go implements it; tests inject a fake.
type browserLink interface {
	send(ctx context.Context, msg browserpod.Message, body io.Reader) error
	close()
}

// browserGroup is one web subagent's use of a browser: its tabs, what
// core saw of its run, and the windows watching it.
type browserGroup struct {
	tabs          map[string]bool
	pages         int
	lastURL       string
	downloads     []string
	downloadBytes int64
	// tabInfo is each tab's last URL and title, for the windows.
	tabInfo map[string]browserTabInfo
	// watchers are the windows on this group. watching is whether the
	// pod screencasts the group; it stays on for browserWindowGrace
	// after the last watcher left, closeAt being when that grace ends.
	// shownTab is the tab the last frame came from.
	watchers map[*browserWatcher]struct{}
	watching bool
	closeAt  time.Time
	shownTab string
}

// browserTabInfo is what core knows of a tab: its last URL and title.
type browserTabInfo struct {
	url, title string
}

func (b *browser) groupLocked(name string) *browserGroup {
	g, ok := b.groups[name]
	if !ok {
		g = &browserGroup{tabs: map[string]bool{}, tabInfo: map[string]browserTabInfo{}, watchers: map[*browserWatcher]struct{}{}}
		b.groups[name] = g
	}
	return g
}

// browserWatcher is one browser window on a group. frames holds the
// latest frame only: a window that cannot keep up sees the newest frame
// when it next reads, never a backlog, and never slows the pod or
// another window. events are the rare text messages. done closes when
// the window is over, after its end event was queued.
type browserWatcher struct {
	b      *browser
	group  string
	frames chan []byte
	events chan apitypes.BrowserWindowEvent
	done   chan struct{}
	once   sync.Once
}

func newBrowserWatcher(b *browser, group string) *browserWatcher {
	return &browserWatcher{
		b: b, group: group,
		frames: make(chan []byte, 1),
		events: make(chan apitypes.BrowserWindowEvent, browserWatcherEvents),
		done:   make(chan struct{}),
	}
}

// Frames, Events and Done are what the window's handler reads.
func (w *browserWatcher) Frames() <-chan []byte                      { return w.frames }
func (w *browserWatcher) Events() <-chan apitypes.BrowserWindowEvent { return w.events }
func (w *browserWatcher) Done() <-chan struct{}                      { return w.done }

// offer queues a frame, replacing one the window has not read yet.
// Frames of one browser come from its one connection goroutine, so the
// slot cannot be refilled by anyone else between the drain and the send.
func (w *browserWatcher) offer(frame []byte) {
	select {
	case w.frames <- frame:
		return
	default:
	}
	select {
	case <-w.frames:
	default:
	}
	select {
	case w.frames <- frame:
	default:
	}
}

// event queues a text message; a window that has not read the last
// browserWatcherEvents of them loses the oldest.
func (w *browserWatcher) event(ev apitypes.BrowserWindowEvent) {
	select {
	case w.events <- ev:
		return
	default:
	}
	select {
	case <-w.events:
	default:
	}
	select {
	case w.events <- ev:
	default:
	}
}

// end queues the window's end event, with reason, and closes done.
func (w *browserWatcher) end(reason string) {
	w.once.Do(func() {
		w.event(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeEnd, Reason: reason})
		close(w.done)
	})
}

// tabEvent is the window message for a tab.
func tabEvent(id string, info browserTabInfo) apitypes.BrowserWindowEvent {
	return apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeTab, Tab: &apitypes.BrowserWindowTab{ID: id, URL: info.url, Title: info.title}}
}

// wireState is a browser's state as the API reports it: idle is still
// running (the pod is up), saving and stopped are stopped.
func wireState(s browserState) apitypes.BrowserState {
	switch s {
	case browserStarting:
		return apitypes.BrowserStateStarting
	case browserRunning, browserIdle:
		return apitypes.BrowserStateRunning
	default:
		return apitypes.BrowserStateStopped
	}
}

// browser is one (agent, session) with a pod.
type browser struct {
	slug, session, site string
	token               string
	state               browserState
	link                browserLink
	// ready is closed when the pod reports running, or when the start
	// failed; startErr says which.
	ready    chan struct{}
	startErr error

	startedAt time.Time
	lastUsed  time.Time
	// deadline is the browser's next lifecycle moment: the start
	// timeout while starting, the reap while idle, the save timeout
	// while saving. Zero while running.
	deadline time.Time

	groups map[string]*browserGroup
	// released holds the groups whose task has ended: a command for
	// one is refused, so a call still in flight cannot reopen it.
	released map[string]bool
	tabs     map[string]string // tab id → group
	windows  int

	pending map[string]chan browserpod.Message
	seq     int
	// profileSaved is set once a save's profile has been written.
	profileSaved bool
	// offboarded: the agent is gone; a profile that lands after this
	// is deleted again, not kept.
	offboarded bool
}

func (b *browser) key() string { return b.slug + "/" + b.session }

func browserKey(slug, session string) string { return slug + "/" + session }

// ephemeralSession reports whether session keeps nothing between
// tasks: the open session. Its browser starts empty, each task's tabs
// live in a browser context of their own, and no profile is ever asked
// for or stored.
func ephemeralSession(session string) bool { return session == agentpod.OpenWebSession }

// groupID is the shape of a group name core would have minted: one
// path element of plain characters. A name the pod sends that does not
// fit is not even looked up; one that fits must still be a group core
// opened on that browser.
var groupID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// browserRegistry holds the browsers and runs their lifecycle.
type browserRegistry struct {
	mu       sync.Mutex
	browsers map[string]*browser

	store *store.FSStore
	prov  BrowserProvisioner
	clk   clock.Clock
	// cap is the team's browser count; a start beyond it is refused.
	cap  int
	logf func(format string, args ...any)
	// downloadsRoot is where a group's downloads are written: the
	// subagent's overlay root on disk.
	downloadsRoot func(slug, subagentID string) string
	// OnState, when set, hears every change of a browser's wire state,
	// outside the registry's lock: starting, running, stopped.
	OnState func(slug, session string, state apitypes.BrowserState)
	// OnWindowsClosed, when set, hears that a group's last window has
	// been gone for the grace, outside the lock: a task waiting for a
	// person in that window resumes.
	OnWindowsClosed func(slug, session, group string)

	wake chan struct{}
	// booted is closed once the boot cleanup has run; a start waits
	// for it, so the cleanup cannot delete a pod it just created.
	booted   chan struct{}
	bootOnce sync.Once
	stopWait sync.WaitGroup
}

// newBrowserRegistry wires a registry. The lifecycle runs once Run is
// started; without it browsers start and relay but are never reaped,
// and starts wait for a boot that never comes: tests call markBooted.
func newBrowserRegistry(st *store.FSStore, prov BrowserProvisioner, clk clock.Clock, teamCap int, logf func(string, ...any), downloadsRoot func(slug, subagentID string) string) *browserRegistry {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if teamCap < 1 {
		teamCap = 1
	}
	return &browserRegistry{
		browsers:      map[string]*browser{},
		store:         st,
		prov:          prov,
		clk:           clk,
		cap:           teamCap,
		logf:          logf,
		downloadsRoot: downloadsRoot,
		wake:          make(chan struct{}, 1),
		booted:        make(chan struct{}),
	}
}

// Enabled reports whether browsers can run here at all.
func (r *browserRegistry) Enabled() bool {
	return r != nil && r.prov != nil && r.prov.Enabled()
}

// StartBrowsers runs the browser lifecycle until the returned stop
// function is called. A no-op without a registry.
func (s *Server) StartBrowsers() func() {
	if s.Browsers == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Browsers.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// markBooted lets starts proceed.
func (r *browserRegistry) markBooted() {
	r.bootOnce.Do(func() { close(r.booted) })
}

// Run deletes every browser pod left from before this start, then
// runs the lifecycle timer until ctx ends: one timer, re-armed to the
// earliest deadline across browsers, woken early whenever a deadline
// changes.
func (r *browserRegistry) Run(ctx context.Context) {
	if r.Enabled() {
		if n, err := r.prov.StopAll(ctx); err != nil {
			r.logf("browser: boot cleanup: %v", err)
		} else if n > 0 {
			r.logf("browser: boot cleanup: deleted %d browser pod(s)", n)
		}
	}
	r.markBooted()
	timer := r.clk.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		next := r.sweep(r.clk.Now())
		wait := time.Hour
		if !next.IsZero() {
			wait = max(next.Sub(r.clk.Now()), 0)
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-timer.C():
		}
	}
}

// kick wakes Run so it re-reads the deadlines.
func (r *browserRegistry) kick() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// sweep acts on every browser whose deadline has passed and returns
// the earliest deadline left, or zero. Nothing here waits on a pod:
// what it starts runs on its own goroutine.
func (r *browserRegistry) sweep(now time.Time) time.Time {
	r.mu.Lock()
	type action struct {
		b    *browser
		kind browserState
	}
	type window struct {
		b     *browser
		group string
	}
	var due []action
	var closes []window
	var next time.Time
	earliest := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, b := range r.browsers {
		for name, g := range b.groups {
			if !g.watching || len(g.watchers) != 0 || g.closeAt.IsZero() {
				continue
			}
			if !g.closeAt.After(now) {
				closes = append(closes, window{b, name})
			} else {
				earliest(g.closeAt)
			}
		}
		if b.deadline.IsZero() {
			continue
		}
		if !b.deadline.After(now) {
			due = append(due, action{b, b.state})
			continue
		}
		earliest(b.deadline)
	}
	r.mu.Unlock()
	for _, c := range closes {
		r.closeWindow(c.b, c.group)
	}
	for _, a := range due {
		switch a.kind {
		case browserStarting:
			r.failStart(a.b, errors.New("the browser did not start in time"))
		case browserIdle:
			r.beginSave(a.b)
		case browserSaving:
			r.logf("browser %s: save did not finish in time", a.b.key())
			r.finish(a.b)
		}
	}
	if len(due) > 0 || len(closes) > 0 {
		return r.sweep(now)
	}
	return next
}

// sendCtx is the context a write to the pod runs under: detached from
// the caller, bounded by d. The WebSocket library closes the
// connection when a write's context ends, so a caller's cancellation
// must never reach a write that other groups share.
func sendCtx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// Command relays one web tool call for group to the browser of
// (slug, session), starting the browser when none runs, and returns
// the pod's result. An error is core's refusal or a lost browser; a
// result with OK false is the pod's refusal, which the model reads.
func (r *browserRegistry) Command(ctx context.Context, slug, session, group string, msg browserpod.Message) (browserpod.Message, error) {
	b, err := r.acquire(ctx, slug, session)
	if err != nil {
		return browserpod.Message{}, err
	}
	r.mu.Lock()
	if b.state != browserRunning && b.state != browserIdle {
		r.mu.Unlock()
		return browserpod.Message{}, errors.New("the browser is not running")
	}
	if b.released[group] {
		r.mu.Unlock()
		return browserpod.Message{}, errors.New("this task has ended")
	}
	if _, has := b.groups[group]; !has && r.expiredLocked(b) {
		r.mu.Unlock()
		return browserpod.Message{}, errBrowserLifetime
	}
	b.state = browserRunning
	b.deadline = time.Time{}
	b.lastUsed = r.clk.Now()
	b.groupLocked(group)
	b.seq++
	id := "c" + strconv.Itoa(b.seq)
	ch := make(chan browserpod.Message, 1)
	b.pending[id] = ch
	link := b.link
	r.mu.Unlock()

	msg.Type = browserpod.TypeCommand
	msg.ID = id
	msg.Group = group
	sctx, cancel := sendCtx(browserSendTimeout)
	err = link.send(sctx, msg, nil)
	cancel()
	if err != nil {
		r.dropPending(b, id)
		return browserpod.Message{}, fmt.Errorf("the browser connection failed: %w", err)
	}
	timeout := browserCommandTimeout
	if msg.Name == browserpod.CommandNavigate || msg.Name == browserpod.CommandWait {
		timeout = browserLongCommandTimeout
	}
	t := r.clk.NewTimer(timeout)
	defer t.Stop()
	select {
	case res, ok := <-ch:
		if !ok {
			return browserpod.Message{}, errors.New("the browser was lost")
		}
		return res, nil
	case <-t.C():
		r.dropPending(b, id)
		return browserpod.Message{}, errors.New("the browser did not answer in time")
	case <-ctx.Done():
		r.dropPending(b, id)
		return browserpod.Message{}, ctx.Err()
	}
}

// SendFile hands the pod a file group may upload.
func (r *browserRegistry) SendFile(ctx context.Context, slug, session, group, name string, body io.Reader, size int64) error {
	b, err := r.acquire(ctx, slug, session)
	if err != nil {
		return err
	}
	r.mu.Lock()
	link := b.link
	released := b.released[group]
	r.mu.Unlock()
	if link == nil {
		return errors.New("the browser is not running")
	}
	if released {
		return errors.New("this task has ended")
	}
	sctx, cancel := sendCtx(browserFileSendTimeout)
	defer cancel()
	return link.send(sctx, browserpod.Message{Type: browserpod.TypeFile, Group: group, FileName: name, Bytes: size}, body)
}

func (r *browserRegistry) dropPending(b *browser, id string) {
	r.mu.Lock()
	delete(b.pending, id)
	r.mu.Unlock()
}

// acquire returns the running browser for (slug, session), starting
// one when none exists and waiting for it to report running.
func (r *browserRegistry) acquire(ctx context.Context, slug, session string) (*browser, error) {
	if !r.Enabled() {
		return nil, errors.New("browsers are not available in this deployment")
	}
	select {
	case <-r.booted:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r.mu.Lock()
	b, ok := r.browsers[browserKey(slug, session)]
	switch {
	case ok && b.state == browserSaving:
		// A reap in progress: the next browser starts from the profile
		// this one is saving.
		r.mu.Unlock()
		return nil, errors.New("the browser is being saved; try again in a moment")
	case ok && b.state == browserStopped:
		r.mu.Unlock()
		return nil, errors.New("the previous browser is still stopping; try again in a moment")
	}
	if !ok {
		if len(r.browsers) >= r.cap {
			// An idle browser holds a slot another agent needs: save
			// it now rather than at its idle deadline.
			victim := r.oldestIdleLocked()
			r.mu.Unlock()
			if victim != nil {
				r.beginSave(victim)
				return nil, fmt.Errorf("all %d browsers are in use; an idle one is being saved to make room, try again shortly", r.cap)
			}
			return nil, fmt.Errorf("all %d browsers are in use; try again shortly", r.cap)
		}
		token, err := newBrowserToken()
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		now := r.clk.Now()
		b = &browser{
			slug: slug, session: session, token: token,
			state: browserStarting, ready: make(chan struct{}),
			startedAt: now, lastUsed: now, deadline: now.Add(browserStartTimeout),
			groups: map[string]*browserGroup{}, released: map[string]bool{},
			tabs: map[string]string{}, pending: map[string]chan browserpod.Message{},
		}
		ephemeral := ephemeralSession(session)
		if !ephemeral {
			// A site session's name is its site: the registrable domain
			// its browser is locked to.
			b.site = session
		}
		r.browsers[b.key()] = b
		r.mu.Unlock()
		r.kick()
		r.notify(b, apitypes.BrowserStateStarting)
		if ephemeral {
			// Nothing of this session is kept; a record or profile
			// stored for it goes too.
			if err := r.store.DeleteBrowserSession(slug, session); err != nil {
				r.logf("browser %s: delete the stored session: %v", b.key(), err)
			}
		} else if _, err := r.store.EnsureBrowserSession(slug, session, session, r.clk.Now()); err != nil {
			r.failStart(b, fmt.Errorf("browser session: %w", err))
			return nil, err
		}
		if err := r.prov.Start(ctx, slug, session, token); err != nil {
			r.failStart(b, fmt.Errorf("start the browser pod: %w", err))
			return nil, err
		}
	} else {
		r.mu.Unlock()
	}
	select {
	case <-b.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r.mu.Lock()
	err := b.startErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return b, nil
}

// notify reports a browser's wire state to OnState, outside the lock.
func (r *browserRegistry) notify(b *browser, state apitypes.BrowserState) {
	if r.OnState != nil {
		r.OnState(b.slug, b.session, state)
	}
}

// StateFor is the wire state of (slug, session)'s browser: stopped
// when none is registered.
func (r *browserRegistry) StateFor(slug, session string) apitypes.BrowserState {
	if r == nil {
		return apitypes.BrowserStateStopped
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.browsers[browserKey(slug, session)]
	if !ok {
		return apitypes.BrowserStateStopped
	}
	return wireState(b.state)
}

// Watch attaches a browser window to group's tabs in the browser of
// (slug, session), starting the browser when none runs, and returns
// the watcher the window reads from. input is whether the window takes
// input now; the first state event says so. The first window on a
// group turns the group's screencast on; the browser counts the group
// as a window, which keeps it from going idle, until the last window
// has been gone for browserWindowGrace.
func (r *browserRegistry) Watch(ctx context.Context, slug, session, group string, input bool) (*browserWatcher, error) {
	b, err := r.acquire(ctx, slug, session)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if b.state != browserRunning && b.state != browserIdle {
		r.mu.Unlock()
		return nil, errors.New("the browser is not running")
	}
	if b.released[group] {
		r.mu.Unlock()
		return nil, errors.New("this task has ended")
	}
	if _, has := b.groups[group]; !has && r.expiredLocked(b) {
		r.mu.Unlock()
		return nil, errBrowserLifetime
	}
	b.state = browserRunning
	b.deadline = time.Time{}
	b.lastUsed = r.clk.Now()
	g := b.groupLocked(group)
	w := newBrowserWatcher(b, group)
	g.watchers[w] = struct{}{}
	g.closeAt = time.Time{}
	start := !g.watching
	if start {
		g.watching = true
		b.windows++
	}
	link := b.link
	w.event(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeState, Browser: apitypes.BrowserStateRunning, Input: input, Site: b.site})
	if g.shownTab != "" {
		w.event(tabEvent(g.shownTab, g.tabInfo[g.shownTab]))
	}
	r.mu.Unlock()
	if start && link != nil {
		go func() {
			sctx, cancel := sendCtx(browserSendTimeout)
			defer cancel()
			_ = link.send(sctx, browserpod.Message{Type: browserpod.TypeScreencast, Group: group, OK: true}, nil)
		}()
	}
	r.kick()
	return w, nil
}

// Unwatch is a window leaving its group. The group's screencast stays
// on for browserWindowGrace, so a reload or a brief disconnect costs
// nothing; the sweep turns it off when no window has come back.
func (r *browserRegistry) Unwatch(w *browserWatcher) {
	r.mu.Lock()
	if g, ok := w.b.groups[w.group]; ok {
		delete(g.watchers, w)
		if len(g.watchers) == 0 && g.watching {
			g.closeAt = r.clk.Now().Add(browserWindowGrace)
		}
	}
	r.mu.Unlock()
	r.kick()
}

// closeWindow ends a group's screencast once its grace has run out
// with no window back on it.
func (r *browserRegistry) closeWindow(b *browser, group string) {
	r.mu.Lock()
	g, ok := b.groups[group]
	if !ok || !g.watching || len(g.watchers) != 0 || g.closeAt.IsZero() || g.closeAt.After(r.clk.Now()) {
		r.mu.Unlock()
		return
	}
	g.watching = false
	g.closeAt = time.Time{}
	b.windows--
	link := b.link
	r.mu.Unlock()
	if link != nil {
		snapshot := !ephemeralSession(b.session)
		go func() {
			sctx, cancel := sendCtx(browserSendTimeout)
			defer cancel()
			if err := link.send(sctx, browserpod.Message{Type: browserpod.TypeScreencast, Group: group, OK: false}, nil); err != nil {
				return
			}
			if snapshot {
				// A person may have signed in through the window: its
				// cookies are worth keeping now, not in minutes.
				_ = link.send(sctx, browserpod.Message{Type: browserpod.TypeSnapshot}, nil)
			}
		}()
	}
	if r.OnWindowsClosed != nil {
		r.OnWindowsClosed(b.slug, b.session, group)
	}
}

// Input sends one event from a window to the tab it shows. Only the
// window's handler calls it, and only while the window takes input.
func (r *browserRegistry) Input(w *browserWatcher, in apitypes.BrowserInput) error {
	r.mu.Lock()
	var tab string
	if g, ok := w.b.groups[w.group]; ok {
		tab = g.shownTab
	}
	link := w.b.link
	r.mu.Unlock()
	if link == nil {
		return errors.New("the browser is not running")
	}
	if tab == "" {
		return errors.New("no tab is shown yet")
	}
	args, err := json.Marshal(in)
	if err != nil {
		return err
	}
	sctx, cancel := sendCtx(browserSendTimeout)
	defer cancel()
	return link.send(sctx, browserpod.Message{Type: browserpod.TypeInput, Group: w.group, Tab: tab, Args: args}, nil)
}

// BroadcastState sends every window on a group the browser's state
// again, with whether the window takes input now: the service calls it
// when the group's task starts or stops waiting for a person.
func (r *browserRegistry) BroadcastState(slug, session, group string, input bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.browsers[browserKey(slug, session)]
	if !ok {
		return
	}
	g, ok := b.groups[group]
	if !ok {
		return
	}
	ev := apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeState, Browser: wireState(b.state), Input: input, Site: b.site}
	for w := range g.watchers {
		w.event(ev)
	}
}

// Reset ends a session's browser, whatever it is doing and without
// saving, and deletes what the store holds for the session. The
// service cancels the session's tasks first.
func (r *browserRegistry) Reset(slug, session string) error {
	r.mu.Lock()
	b, ok := r.browsers[browserKey(slug, session)]
	if ok {
		// A profile that lands from a save already in flight is
		// deleted again (onProfile).
		b.offboarded = true
	}
	r.mu.Unlock()
	if ok {
		r.finish(b)
	}
	return r.store.DeleteBrowserSession(slug, session)
}

// onFrame hands a screencast frame to the windows on its group. The
// bytes are read only when a window is there to see them; otherwise the
// body is left for the caller to drain. One buffer serves every window.
func (r *browserRegistry) onFrame(b *browser, msg browserpod.Message, body io.Reader) {
	if body == nil || msg.Bytes <= 0 || msg.Bytes > browserMaxFrameBytes {
		return
	}
	r.mu.Lock()
	g, ok := b.groups[msg.Group]
	watched := ok && len(g.watchers) > 0
	r.mu.Unlock()
	if !watched {
		return
	}
	buf := make([]byte, msg.Bytes)
	if _, err := io.ReadFull(body, buf); err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok = b.groups[msg.Group]
	if !ok {
		return
	}
	if g.shownTab != msg.Tab {
		g.shownTab = msg.Tab
		ev := tabEvent(msg.Tab, g.tabInfo[msg.Tab])
		for w := range g.watchers {
			w.event(ev)
		}
	}
	for w := range g.watchers {
		w.offer(buf)
	}
}

// errBrowserLifetime refuses a new task on a browser past its lifetime.
var errBrowserLifetime = errors.New("the browser has run for two hours and restarts when its current tasks end; try again shortly")

// expiredLocked reports whether b has run past browserLifetime.
func (r *browserRegistry) expiredLocked(b *browser) bool {
	return r.clk.Now().Sub(b.startedAt) >= browserLifetime
}

// idleDeadlineLocked is when an idle b is saved: after browserIdleAfter,
// or at once past its lifetime.
func (r *browserRegistry) idleDeadlineLocked(b *browser) time.Time {
	if r.expiredLocked(b) {
		return r.clk.Now()
	}
	return r.clk.Now().Add(browserIdleAfter)
}

// oldestIdleLocked is the idle browser unused the longest, or nil.
func (r *browserRegistry) oldestIdleLocked() *browser {
	var oldest *browser
	for _, b := range r.browsers {
		if b.state != browserIdle {
			continue
		}
		if oldest == nil || b.lastUsed.Before(oldest.lastUsed) {
			oldest = b
		}
	}
	return oldest
}

// claim is the connect handler's first step: it binds link to the
// browser that is waiting for a pod with this token, once, and returns
// it, or nil when no browser is waiting for that token. Binding and
// checking happen under one lock, so two connections with one token
// cannot both be taken.
func (r *browserRegistry) claim(slug, session, token string, link browserLink) *browser {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.browsers[browserKey(slug, session)]
	if !ok || b.token == "" || b.state != browserStarting || b.link != nil {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(b.token), []byte(token)) != 1 {
		return nil
	}
	b.link = link
	return b
}

// attach sends the claimed pod its session: hello with the saved
// profile, then start. The pod's running state, which closes ready,
// comes back through onMessage.
func (r *browserRegistry) attach(ctx context.Context, b *browser) error {
	r.mu.Lock()
	link, site := b.link, b.site
	r.mu.Unlock()
	if link == nil {
		return errors.New("no link")
	}
	hello := browserpod.Message{Type: browserpod.TypeHello, Site: site}
	var body io.Reader
	if ephemeralSession(b.session) {
		hello.Ephemeral = true
	} else {
		// A site session runs headed, under Xvfb: some sites refuse a
		// headless browser's sign-in, and a person may be asked to sign
		// in through the window.
		hello.Headful = true
		profile, size, err := r.store.OpenBrowserProfile(b.slug, b.session)
		if err != nil {
			return fmt.Errorf("open profile: %w", err)
		}
		if profile != nil {
			defer func() { _ = profile.Close() }()
			body = profile
			hello.Bytes = size
		}
	}
	sctx, cancel := context.WithTimeout(ctx, browserFileSendTimeout)
	defer cancel()
	if err := link.send(sctx, hello, body); err != nil {
		return err
	}
	if !ephemeralSession(b.session) {
		// The cookies reported since that profile was saved, if any:
		// the pod sets them once Chromium is up.
		cookies, size, err := r.store.OpenBrowserCookies(b.slug, b.session)
		if err != nil {
			return fmt.Errorf("open cookies: %w", err)
		}
		if cookies != nil {
			defer func() { _ = cookies.Close() }()
			if err := link.send(sctx, browserpod.Message{Type: browserpod.TypeCookies, Bytes: size}, cookies); err != nil {
				return err
			}
		}
	}
	return link.send(sctx, browserpod.Message{Type: browserpod.TypeStart}, nil)
}

// onMessage handles one message from the pod. body carries the bytes
// announced by msg.Bytes and must be read before onMessage returns;
// what is left unread the caller drains.
func (r *browserRegistry) onMessage(b *browser, msg browserpod.Message, body io.Reader) {
	switch msg.Type {
	case browserpod.TypeState:
		r.onState(b, msg)
	case browserpod.TypeResult:
		r.mu.Lock()
		ch, ok := b.pending[msg.ID]
		delete(b.pending, msg.ID)
		r.mu.Unlock()
		if ok {
			ch <- msg
		}
	case browserpod.TypeTab:
		r.mu.Lock()
		if g, ok := b.groups[msg.Group]; ok && msg.Tab != "" {
			g.tabs[msg.Tab] = true
			b.tabs[msg.Tab] = msg.Group
			g.tabInfo[msg.Tab] = browserTabInfo{url: msg.URL, title: msg.Title}
		}
		r.mu.Unlock()
	case browserpod.TypeTabClosed:
		r.mu.Lock()
		if g, ok := b.groups[msg.Group]; ok {
			delete(g.tabs, msg.Tab)
			delete(g.tabInfo, msg.Tab)
			if g.shownTab == msg.Tab {
				g.shownTab = ""
			}
		}
		delete(b.tabs, msg.Tab)
		r.mu.Unlock()
	case browserpod.TypeNavigated:
		r.mu.Lock()
		if g, ok := b.groups[msg.Group]; ok {
			g.pages++
			g.lastURL = msg.URL
			info := browserTabInfo{url: msg.URL, title: msg.Title}
			g.tabInfo[msg.Tab] = info
			if msg.Tab == g.shownTab {
				ev := tabEvent(msg.Tab, info)
				for w := range g.watchers {
					w.event(ev)
				}
			}
		}
		r.mu.Unlock()
	case browserpod.TypeFrame:
		r.onFrame(b, msg, body)
	case browserpod.TypeDownload:
		r.onDownload(b, msg, body)
	case browserpod.TypeProfile:
		r.onProfile(b, msg, body)
	case browserpod.TypeSaved:
		r.finish(b)
	case browserpod.TypeCookies:
		r.onCookies(b, msg, body)
	}
}

// onCookies keeps a cookie snapshot the pod sent: the session's
// cookies since its profile was last saved, for the next browser to
// start from should this one not get to save. Only a site session's,
// only while its browser runs, and only within bounds; anything else
// is left for the caller to drain.
func (r *browserRegistry) onCookies(b *browser, msg browserpod.Message, body io.Reader) {
	r.mu.Lock()
	running := (b.state == browserRunning || b.state == browserIdle) && !b.offboarded
	r.mu.Unlock()
	if body == nil || !running || ephemeralSession(b.session) || msg.Bytes <= 0 || msg.Bytes > browserpod.MaxCookiesBytes {
		r.logf("browser %s: cookies of %d bytes refused (running: %v)", b.key(), msg.Bytes, running)
		return
	}
	n, err := r.store.WriteBrowserCookies(b.slug, b.session, io.LimitReader(body, msg.Bytes))
	if err != nil {
		r.logf("browser %s: write cookies: %v", b.key(), err)
		return
	}
	r.mu.Lock()
	offboarded := b.offboarded
	r.mu.Unlock()
	if offboarded {
		// The agent was offboarded while this was being written and
		// its sessions deleted; this one must not come back.
		if err := r.store.DeleteBrowserSession(b.slug, b.session); err != nil {
			r.logf("browser %s: delete cookies of an offboarded agent: %v", b.key(), err)
		}
		return
	}
	r.logf("browser %s: cookies kept (%d bytes)", b.key(), n)
}

func (r *browserRegistry) onState(b *browser, msg browserpod.Message) {
	switch msg.State {
	case browserpod.StateRunning:
		r.mu.Lock()
		became := false
		if b.state == browserStarting {
			became = true
			b.state = browserRunning
			b.deadline = time.Time{}
			// Nobody may be waiting any more: the command that started
			// it was cancelled. Then the browser is idle from the start
			// and is reaped like any other.
			if len(b.groups) == 0 {
				b.state = browserIdle
				b.deadline = r.idleDeadlineLocked(b)
			}
			close(b.ready)
		}
		r.mu.Unlock()
		if msg.Detail != "" {
			r.logf("browser %s: running (%s)", b.key(), msg.Detail)
		}
		if became {
			r.notify(b, apitypes.BrowserStateRunning)
		}
		r.kick()
	case browserpod.StateStopped:
		r.mu.Lock()
		starting := b.state == browserStarting
		r.mu.Unlock()
		if starting {
			r.failStart(b, fmt.Errorf("the browser could not start: %s", msg.Detail))
			return
		}
		r.logf("browser %s: stopped: %s", b.key(), msg.Detail)
		r.finish(b)
	}
}

// onDownload writes a download the pod sent into the group's
// untrusted-downloads/ and remembers its name for the task's result.
// The group must be one core opened on this browser and still within
// its download bounds; anything else is left unread for the caller to
// drain.
func (r *browserRegistry) onDownload(b *browser, msg browserpod.Message, body io.Reader) {
	if body == nil || r.downloadsRoot == nil || !groupID.MatchString(msg.Group) {
		return
	}
	r.mu.Lock()
	g, ok := b.groups[msg.Group]
	if !ok || len(g.downloads) >= maxDownloadsPerGroup || g.downloadBytes+msg.Bytes > maxDownloadBytesPerGroup || msg.Bytes > browserpod.MaxDownloadBytes {
		r.mu.Unlock()
		r.logf("browser %s: download %q for %q refused", b.key(), msg.FileName, msg.Group)
		return
	}
	g.downloadBytes += msg.Bytes
	r.mu.Unlock()
	root := r.downloadsRoot(b.slug, msg.Group)
	name, err := files.WriteUntrustedDownload(root, msg.FileName, io.LimitReader(body, msg.Bytes), browserpod.MaxDownloadBytes)
	if err != nil {
		r.logf("browser %s: download %q for %s: %v", b.key(), msg.FileName, msg.Group, err)
		return
	}
	r.mu.Lock()
	if g, ok := b.groups[msg.Group]; ok {
		g.downloads = append(g.downloads, name)
	}
	r.mu.Unlock()
}

// onProfile writes the profile a saving pod sent to the store. A
// profile at any other time, or one beyond the size the pod itself
// caps, is left unread.
func (r *browserRegistry) onProfile(b *browser, msg browserpod.Message, body io.Reader) {
	r.mu.Lock()
	saving := b.state == browserSaving
	r.mu.Unlock()
	if body == nil || !saving || msg.Bytes <= 0 || msg.Bytes > browserpod.MaxProfileBytes {
		r.logf("browser %s: profile of %d bytes refused (saving: %v)", b.key(), msg.Bytes, saving)
		return
	}
	n, err := r.store.WriteBrowserProfile(b.slug, b.session, io.LimitReader(body, msg.Bytes), r.clk.Now())
	if err != nil {
		r.logf("browser %s: write profile: %v", b.key(), err)
		return
	}
	r.mu.Lock()
	b.profileSaved = true
	offboarded := b.offboarded
	r.mu.Unlock()
	if offboarded {
		// The agent was offboarded while this was being written and
		// its sessions deleted; this one must not come back.
		if err := r.store.DeleteBrowserSession(b.slug, b.session); err != nil {
			r.logf("browser %s: delete profile of an offboarded agent: %v", b.key(), err)
		}
		return
	}
	r.logf("browser %s: profile saved (%d bytes)", b.key(), n)
}

// ReleaseGroup ends a subagent's use of its browser: its tabs are
// closed, and what core saw of its run is returned. A browser with no
// group and no window left goes idle, to be saved and deleted after
// browserIdleAfter.
func (r *browserRegistry) ReleaseGroup(slug, session, group string) (structured.WebFacts, []string) {
	var facts structured.WebFacts
	r.mu.Lock()
	b, ok := r.browsers[browserKey(slug, session)]
	if !ok {
		r.mu.Unlock()
		return facts, nil
	}
	var downloads []string
	if g, found := b.groups[group]; found {
		facts.Pages = g.pages
		facts.Origin = originOf(g.lastURL)
		downloads = append(downloads, g.downloads...)
		for tab := range g.tabs {
			delete(b.tabs, tab)
		}
		// The windows on it end with it; close-group ends its cast.
		for w := range g.watchers {
			w.end("The task has ended.")
		}
		if g.watching {
			b.windows--
		}
		delete(b.groups, group)
	}
	b.released[group] = true
	link := b.link
	idle := len(b.groups) == 0 && b.windows == 0 && b.state == browserRunning
	if idle {
		b.state = browserIdle
		b.deadline = r.idleDeadlineLocked(b)
	}
	r.mu.Unlock()
	if link != nil {
		// Off this goroutine: a job's end must not wait on the pod.
		go func() {
			sctx, cancel := sendCtx(browserSendTimeout)
			defer cancel()
			_ = link.send(sctx, browserpod.Message{Type: browserpod.TypeCloseGroup, Group: group}, nil)
		}()
	}
	if idle {
		r.kick()
	}
	return facts, downloads
}

// beginSave asks an idle browser's pod for its profile.
func (r *browserRegistry) beginSave(b *browser) {
	r.mu.Lock()
	if b.state != browserIdle {
		r.mu.Unlock()
		return
	}
	if ephemeralSession(b.session) {
		// Nothing to save: the session keeps nothing.
		r.mu.Unlock()
		r.finish(b)
		return
	}
	b.state = browserSaving
	b.deadline = r.clk.Now().Add(browserSaveTimeout)
	link := b.link
	r.mu.Unlock()
	r.notify(b, apitypes.BrowserStateStopped)
	r.kick()
	if link == nil {
		r.finish(b)
		return
	}
	// Off the lifecycle goroutine: a pod that has stopped reading must
	// not stop every other browser's timers with it.
	go func() {
		sctx, cancel := sendCtx(browserSendTimeout)
		defer cancel()
		if err := link.send(sctx, browserpod.Message{Type: browserpod.TypeSave}, nil); err != nil {
			r.logf("browser %s: ask for save: %v", b.key(), err)
			r.finish(b)
		}
	}()
}

// failStart ends a browser that never became running: waiters get
// err, the pod is deleted, the record goes.
func (r *browserRegistry) failStart(b *browser, err error) {
	r.mu.Lock()
	if b.state != browserStarting {
		r.mu.Unlock()
		return
	}
	b.startErr = err
	r.mu.Unlock()
	r.logf("browser %s: %v", b.key(), err)
	r.finish(b)
}

// finish closes a browser out, whatever it was doing: pending commands
// fail, the link closes, the pod is deleted. The record stays, as
// stopped, until the delete has returned, and only then goes, so the
// next start under the same name cannot be killed by this delete and
// the cap counts the pod until it is gone.
func (r *browserRegistry) finish(b *browser) {
	r.mu.Lock()
	if b.state == browserStopped {
		r.mu.Unlock()
		return
	}
	if b.state == browserStarting {
		if b.startErr == nil {
			b.startErr = errors.New("the browser was lost before it started")
		}
		close(b.ready)
	}
	wasSaving := b.state == browserSaving
	b.state = browserStopped
	b.deadline = time.Time{}
	for id, ch := range b.pending {
		close(ch)
		delete(b.pending, id)
	}
	for _, g := range b.groups {
		for w := range g.watchers {
			w.end("The browser stopped.")
		}
	}
	link := b.link
	b.link = nil
	r.mu.Unlock()
	if link != nil {
		link.close()
	}
	if !wasSaving {
		// A save already reported stopped when it began.
		r.notify(b, apitypes.BrowserStateStopped)
	}
	r.stopWait.Add(1)
	go func() {
		defer r.stopWait.Done()
		if r.Enabled() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := r.prov.Stop(ctx, b.slug, b.session); err != nil {
				r.logf("browser %s: stop pod: %v", b.key(), err)
			}
		}
		r.mu.Lock()
		if r.browsers[b.key()] == b {
			delete(r.browsers, b.key())
		}
		r.mu.Unlock()
		r.kick()
	}()
	r.kick()
}

// Offboard ends every browser of slug's, whatever it is doing, without
// saving: the agent is gone and its profiles are being deleted.
func (r *browserRegistry) Offboard(slug string) {
	r.mu.Lock()
	var gone []*browser
	for _, b := range r.browsers {
		if b.slug == slug {
			b.offboarded = true
			gone = append(gone, b)
		}
	}
	r.mu.Unlock()
	for _, b := range gone {
		r.finish(b)
	}
}

// onLost is the connect handler's exit: the pod's connection ended.
func (r *browserRegistry) onLost(b *browser) {
	r.mu.Lock()
	gone := b.state == browserStopped
	r.mu.Unlock()
	if gone {
		return
	}
	r.logf("browser %s: connection lost", b.key())
	r.finish(b)
}

// states describes the registry's browsers for logs and tests.
func (r *browserRegistry) states() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for k, b := range r.browsers {
		out[k] = string(b.state)
	}
	return out
}

// awaitStops blocks until every pod delete in flight has returned.
// For tests, which must not read the registry while a stop races it.
func (r *browserRegistry) awaitStops() { r.stopWait.Wait() }

func newBrowserToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// originOf is a URL's scheme and host, for the result's facts.
func originOf(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
