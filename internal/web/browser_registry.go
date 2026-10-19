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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	// browserInputTextMax bounds the text of one input event from a
	// window, a paste: the message that carries it to the pod must fit
	// the pod's text frame limit with room to spare.
	browserInputTextMax = 256 << 10
	// browserTabTextMax bounds a tab's URL and title as the pod reports
	// them and core keeps them for the windows.
	browserTabTextMax = 2048
	// browserReleasedPerSession bounds the released groups remembered
	// for one session (see browserRegistry.released); the oldest go
	// first. Group ids are random, so a forgotten one is never reused.
	browserReleasedPerSession = 1024
	// browserShutdownSnapshotGrace is how long Shutdown waits for a
	// running browser's cookie snapshot. The pod sends one only when
	// the cookies have changed since its last, so a browser with
	// nothing new to report is given up on after this.
	browserShutdownSnapshotGrace = 5 * time.Second
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
	Start(ctx context.Context, slug, session, token, coreToken string) error
	Stop(ctx context.Context, slug, session string) error
	StopAll(ctx context.Context) (int, error)
}

// browserLink is a connected pod as the registry uses it: it sends a
// message, with the bytes that follow it, and closes. A send's limit
// counts from when its write begins, after the writes queued ahead of
// it; a pod that stops reading fails the write at the limit. close
// ends the connection at once, whatever a writer is doing. The
// WebSocket handler in browser_socket.go implements it; tests inject a
// fake.
type browserLink interface {
	send(limit time.Duration, msg browserpod.Message, body io.Reader) error
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
	// input is whether the group's windows take input: its task waits
	// for a person (Watch, BroadcastState). unlocked is the site lock
	// the pod was last told for the group (syncSiteLock).
	input    bool
	unlocked bool
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
	coreToken           string
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

	groups  map[string]*browserGroup
	tabs    map[string]string // tab id → group
	windows int

	pending map[string]chan browserpod.Message
	seq     int
	// lockMu orders the site-lock messages to the pod (syncSiteLock).
	lockMu sync.Mutex
	// profileSaved is set once a save's profile has been written;
	// cookieGen counts the cookie snapshots written.
	profileSaved bool
	// sandboxOff: the pod reported Chromium running with --no-sandbox
	// (the detail "sandbox: off" on its running state).
	sandboxOff bool
	cookieGen  int
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

// tabID is the shape of a tab id the pod mints: "t" and a number
// (browserpod's registerLocked). A tab the pod names otherwise is not
// recorded.
var tabID = regexp.MustCompile(`^t[0-9]{1,9}$`)

// releasedGroups are the groups of one (agent, session) whose task has
// ended, in order of release, bounded by browserReleasedPerSession.
type releasedGroups struct {
	set   map[string]bool
	order []string
}

func (g *releasedGroups) add(group string) {
	if g.set[group] {
		return
	}
	g.set[group] = true
	g.order = append(g.order, group)
	for len(g.order) > browserReleasedPerSession {
		delete(g.set, g.order[0])
		g.order = g.order[1:]
	}
}

// browserRegistry holds the browsers and runs their lifecycle.
type browserRegistry struct {
	mu       sync.Mutex
	browsers map[string]*browser
	// released holds, per (agent, session) key, the groups whose task
	// has ended: a command or window for one is refused, so a call
	// still in flight at a task's end cannot open its group on this
	// browser or the next. Kept apart from the browser records, which
	// come and go: a release with no browser running must still be
	// remembered by the browser a late call would start.
	released map[string]*releasedGroups

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
	// changed is closed, and replaced, under mu whenever a browser
	// stops or a cookie snapshot is written: what Shutdown waits on.
	changed chan struct{}
	// booted is closed once the boot cleanup has run; a start waits
	// for it, so the cleanup cannot delete a pod it just created.
	booted   chan struct{}
	bootOnce sync.Once
	stopWait sync.WaitGroup
	// siteLocks counts site-lock syncs in flight (awaitSiteLocks).
	siteLocks sync.WaitGroup
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
		released:      map[string]*releasedGroups{},
		store:         st,
		prov:          prov,
		clk:           clk,
		cap:           teamCap,
		logf:          logf,
		downloadsRoot: downloadsRoot,
		wake:          make(chan struct{}, 1),
		changed:       make(chan struct{}),
		booted:        make(chan struct{}),
	}
}

// changedLocked wakes whoever waits on changed. r.mu is held.
func (r *browserRegistry) changedLocked() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// Enabled reports whether browsers can run here at all.
func (r *browserRegistry) Enabled() bool {
	return r != nil && r.prov != nil && r.prov.Enabled()
}

// ShutdownBrowsers keeps what the running browsers can still give
// before the process exits (browserRegistry.Shutdown); it is called
// before the browser socket closes and the lifecycle stops. A no-op
// without a registry.
func (s *Server) ShutdownBrowsers(ctx context.Context) {
	if s.Browsers != nil {
		s.Browsers.Shutdown(ctx)
	}
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

// Shutdown keeps what it can of the site sessions before the process
// exits, since the next boot deletes every pod: each idle browser
// begins its save, each running browser is asked for a cookie
// snapshot, and Shutdown returns once the saves have finished and each
// snapshot has landed or had browserShutdownSnapshotGrace, or when ctx
// ends. A browser still starting, and the open session's, have
// nothing to keep. Run must be running, and the browser socket open:
// the saves' timers are Run's and the profiles arrive on the socket.
func (r *browserRegistry) Shutdown(ctx context.Context) {
	r.mu.Lock()
	var saves, snaps []*browser
	before := map[*browser]int{}
	for _, b := range r.browsers {
		if ephemeralSession(b.session) {
			continue
		}
		switch b.state {
		case browserIdle, browserSaving:
			saves = append(saves, b)
		case browserRunning:
			snaps = append(snaps, b)
			before[b] = b.cookieGen
		}
	}
	r.mu.Unlock()
	if len(saves)+len(snaps) == 0 {
		return
	}
	r.logf("browser: shutdown: saving %d idle browser(s), asking %d running one(s) for their cookies", len(saves), len(snaps))
	for _, b := range saves {
		r.beginSave(b)
	}
	for _, b := range snaps {
		r.mu.Lock()
		link := b.link
		r.mu.Unlock()
		if link != nil {
			go func() {
				_ = link.send(browserSendTimeout, browserpod.Message{Type: browserpod.TypeSnapshot}, nil)
			}()
		}
	}
	grace := r.clk.NewTimer(browserShutdownSnapshotGrace)
	defer grace.Stop()
	graceOver := false
	for {
		r.mu.Lock()
		done := true
		for _, b := range saves {
			if b.state != browserStopped {
				done = false
			}
		}
		if !graceOver {
			for _, b := range snaps {
				if b.cookieGen == before[b] && b.state != browserStopped {
					done = false
				}
			}
		}
		changed := r.changed
		r.mu.Unlock()
		if done {
			r.logf("browser: shutdown: done")
			return
		}
		select {
		case <-changed:
		case <-grace.C():
			graceOver = true
		case <-ctx.Done():
			r.logf("browser: shutdown: %v before every browser was saved", ctx.Err())
			return
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
// the earliest deadline left, or zero. A deadline that is due is taken
// under the lock, so no later sweep acts on it again, and what it
// calls for runs on a goroutine of its own: nothing on the lifecycle
// goroutine waits on a pod, a store or a listener, so one stuck pod
// holds up no other browser's timer. Each action kicks Run when it
// has set a new deadline.
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
				// Decided here; closeWindow carries it out. A window
				// back before then is seen by its watcher, a new grace
				// by its closeAt.
				g.closeAt = time.Time{}
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
			b.deadline = time.Time{}
			continue
		}
		earliest(b.deadline)
	}
	r.mu.Unlock()
	for _, c := range closes {
		go r.closeWindow(c.b, c.group)
	}
	for _, a := range due {
		switch a.kind {
		case browserStarting:
			go r.failStart(a.b, errors.New("the browser did not start in time"))
		case browserIdle:
			go r.beginSave(a.b)
		case browserSaving:
			go func(b *browser) {
				r.logf("browser %s: save did not finish in time", b.key())
				r.finish(b)
			}(a.b)
		}
	}
	return next
}

// errTaskEnded refuses a call for a group whose task has ended.
var errTaskEnded = errors.New("this task has ended")

// releasedLocked reports whether group's task on (slug, session) has
// ended. r.mu is held.
func (r *browserRegistry) releasedLocked(slug, session, group string) bool {
	g, ok := r.released[browserKey(slug, session)]
	return ok && g.set[group]
}

// released reports releasedLocked under the lock.
func (r *browserRegistry) isReleased(slug, session, group string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releasedLocked(slug, session, group)
}

// takeGroupLocked opens group on b for a call, or returns why not:
// the browser is not running, the group's task has ended, or the
// browser is past its lifetime and group is new to it. A new group is
// the task's first use of the session, which the caller records on
// the session (touch) once off the lock. r.mu is held.
func (r *browserRegistry) takeGroupLocked(b *browser, group string) (touch bool, err error) {
	if b.state != browserRunning && b.state != browserIdle {
		return false, errors.New("the browser is not running")
	}
	if r.releasedLocked(b.slug, b.session, group) {
		return false, errTaskEnded
	}
	_, had := b.groups[group]
	if !had && r.expiredLocked(b) {
		return false, errBrowserLifetime
	}
	b.state = browserRunning
	b.deadline = time.Time{}
	b.lastUsed = r.clk.Now()
	b.groupLocked(group)
	return !had && !ephemeralSession(b.session), nil
}

// touch records on the session's stored record that a task has taken
// it now. Once per task, not per command.
func (r *browserRegistry) touch(b *browser) {
	if err := r.store.TouchBrowserSession(b.slug, b.session, r.clk.Now()); err != nil {
		r.logf("browser %s: record the session's use: %v", b.key(), err)
	}
}

// Command relays one web tool call for group to the browser of
// (slug, session), starting the browser when none runs, and returns
// the pod's result. An error is core's refusal or a lost browser; a
// result with OK false is the pod's refusal, which the model reads.
func (r *browserRegistry) Command(ctx context.Context, slug, session, group string, msg browserpod.Message) (browserpod.Message, error) {
	if r.isReleased(slug, session, group) {
		return browserpod.Message{}, errTaskEnded
	}
	b, err := r.acquire(ctx, slug, session)
	if err != nil {
		return browserpod.Message{}, err
	}
	r.mu.Lock()
	touch, err := r.takeGroupLocked(b, group)
	if err != nil {
		r.mu.Unlock()
		return browserpod.Message{}, err
	}
	b.seq++
	id := "c" + strconv.Itoa(b.seq)
	ch := make(chan browserpod.Message, 1)
	b.pending[id] = ch
	link := b.link
	r.mu.Unlock()
	if touch {
		r.touch(b)
	}

	msg.Type = browserpod.TypeCommand
	msg.ID = id
	msg.Group = group
	if err := link.send(browserSendTimeout, msg, nil); err != nil {
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
	if r.isReleased(slug, session, group) {
		return errTaskEnded
	}
	b, err := r.acquire(ctx, slug, session)
	if err != nil {
		return err
	}
	r.mu.Lock()
	link := b.link
	released := r.releasedLocked(slug, session, group)
	r.mu.Unlock()
	if link == nil {
		return errors.New("the browser is not running")
	}
	if released {
		return errTaskEnded
	}
	return link.send(browserFileSendTimeout, browserpod.Message{Type: browserpod.TypeFile, Group: group, FileName: name, Bytes: size}, body)
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
		coreToken, err := newBrowserToken()
		if err != nil {
			r.mu.Unlock()
			return nil, err
		}
		now := r.clk.Now()
		b = &browser{
			slug: slug, session: session, token: token, coreToken: coreToken,
			state: browserStarting, ready: make(chan struct{}),
			startedAt: now, lastUsed: now, deadline: now.Add(browserStartTimeout),
			groups: map[string]*browserGroup{},
			tabs:   map[string]string{}, pending: map[string]chan browserpod.Message{},
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
		if err := r.prov.Start(ctx, slug, session, token, coreToken); err != nil {
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

// SandboxOffFor reports whether (slug, session)'s browser runs
// Chromium without its sandbox; false when none is registered.
func (r *browserRegistry) SandboxOffFor(slug, session string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.browsers[browserKey(slug, session)]
	return ok && b.sandboxOff
}

// GroupURL is the URL group's tabs last navigated to in the browser of
// (slug, session), empty when it has none.
func (r *browserRegistry) GroupURL(slug, session, group string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.browsers[browserKey(slug, session)]
	if !ok {
		return ""
	}
	if g, found := b.groups[group]; found {
		return g.lastURL
	}
	return ""
}

// GroupTabURLs are the URLs group's tabs are at in the browser of
// (slug, session), each as the pod last reported it ("" for a tab that
// has loaded nothing), sorted; none when the group has no tab.
func (r *browserRegistry) GroupTabURLs(slug, session, group string) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.browsers[browserKey(slug, session)]
	if !ok {
		return nil
	}
	g, found := b.groups[group]
	if !found {
		return nil
	}
	out := make([]string, 0, len(g.tabs))
	for tab := range g.tabs {
		out = append(out, g.tabInfo[tab].url)
	}
	sort.Strings(out)
	return out
}

// Watch attaches a browser window to group's tabs in the browser of
// (slug, session), starting the browser when none runs, and returns
// the watcher the window reads from. input is whether the window takes
// input now; the first state event says so. The first window on a
// group turns the group's screencast on; the browser counts the group
// as a window, which keeps it from going idle, until the last window
// has been gone for browserWindowGrace.
func (r *browserRegistry) Watch(ctx context.Context, slug, session, group string, input bool) (*browserWatcher, error) {
	if r.isReleased(slug, session, group) {
		return nil, errTaskEnded
	}
	b, err := r.acquire(ctx, slug, session)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	touch, err := r.takeGroupLocked(b, group)
	if err != nil {
		r.mu.Unlock()
		return nil, err
	}
	g := b.groupLocked(group)
	w := newBrowserWatcher(b, group)
	g.watchers[w] = struct{}{}
	g.closeAt = time.Time{}
	g.input = input
	start := !g.watching
	if start {
		g.watching = true
		b.windows++
	}
	link := b.link
	w.event(apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeState, Browser: apitypes.BrowserStateRunning, Input: input, Site: b.site, SandboxOff: b.sandboxOff})
	if g.shownTab != "" {
		w.event(tabEvent(g.shownTab, g.tabInfo[g.shownTab]))
	}
	r.mu.Unlock()
	if touch {
		r.touch(b)
	}
	if start && link != nil {
		go func() {
			_ = link.send(browserSendTimeout, browserpod.Message{Type: browserpod.TypeScreencast, Group: group, OK: true}, nil)
		}()
	}
	r.syncSiteLock(b, group)
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

// closeWindow ends a group's screencast once the sweep found its grace
// run out with no window back on it (the sweep clears closeAt as it
// decides). A window back since, or a new grace begun since, is left
// alone, as is a browser no longer running: finish ended its windows
// itself, and a task waiting for a person is not told the person is
// done by a browser that died under them.
func (r *browserRegistry) closeWindow(b *browser, group string) {
	r.mu.Lock()
	g, ok := b.groups[group]
	live := b.state == browserRunning || b.state == browserIdle
	if !ok || !live || !g.watching || len(g.watchers) != 0 || !g.closeAt.IsZero() {
		r.mu.Unlock()
		return
	}
	g.watching = false
	b.windows--
	link := b.link
	r.mu.Unlock()
	if link != nil {
		snapshot := !ephemeralSession(b.session)
		go func() {
			if err := link.send(browserSendTimeout, browserpod.Message{Type: browserpod.TypeScreencast, Group: group, OK: false}, nil); err != nil {
				return
			}
			if snapshot {
				// A person may have signed in through the window: its
				// cookies are worth keeping now, not in minutes.
				_ = link.send(browserSendTimeout, browserpod.Message{Type: browserpod.TypeSnapshot}, nil)
			}
		}()
	}
	r.syncSiteLock(b, group)
	if r.OnWindowsClosed != nil {
		r.OnWindowsClosed(b.slug, b.session, group)
	}
}

// Input sends one event from a window to the tab it shows. Only the
// window's handler calls it, and only while the window takes input.
// An error says, for the person at the window, why the input was not
// taken.
func (r *browserRegistry) Input(w *browserWatcher, in apitypes.BrowserInput) error {
	if len(in.Text) > browserInputTextMax {
		return fmt.Errorf("the pasted text is longer than %d KB; paste less at a time", browserInputTextMax>>10)
	}
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
		return errors.New("the browser has no page to show yet")
	}
	args, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if err := link.send(browserSendTimeout, browserpod.Message{Type: browserpod.TypeInput, Group: w.group, Tab: tab, Args: args}, nil); err != nil {
		return errors.New("the browser did not take the input")
	}
	return nil
}

// CurrentPage is the URL and title of the tab a group's windows show
// (the tab of its last frame, else the tab of its last navigation), as
// the pod reported them; both empty when core knows no page. Page text,
// to be shown as text.
func (r *browserRegistry) CurrentPage(slug, session, group string) (url, title string) {
	if r == nil {
		return "", ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.browsers[browserKey(slug, session)]
	if !ok {
		return "", ""
	}
	g, ok := b.groups[group]
	if !ok {
		return "", ""
	}
	if info, has := g.tabInfo[g.shownTab]; has && g.shownTab != "" {
		return info.url, info.title
	}
	for _, info := range g.tabInfo {
		if info.url == g.lastURL && g.lastURL != "" {
			return info.url, info.title
		}
	}
	return "", ""
}

// BroadcastState sends every window on a group the browser's state
// again, with whether the window takes input now, and syncs the
// group's site lock: the service calls it when the group's task starts
// or stops waiting for a person.
func (r *browserRegistry) BroadcastState(slug, session, group string, input bool) {
	r.mu.Lock()
	b, ok := r.browsers[browserKey(slug, session)]
	if !ok {
		r.mu.Unlock()
		return
	}
	g, ok := b.groups[group]
	if !ok {
		r.mu.Unlock()
		return
	}
	g.input = input
	ev := apitypes.BrowserWindowEvent{Type: apitypes.BrowserWindowEventTypeState, Browser: wireState(b.state), Input: input, Site: b.site, SandboxOff: b.sandboxOff}
	for w := range g.watchers {
		w.event(ev)
	}
	r.mu.Unlock()
	r.syncSiteLock(b, group)
}

// siteUnlockedLocked is whether a group's pages may leave the site: a
// site session's group whose task waits for a person with a window on
// it. The window counts until the sweep closes it, after the grace, so
// a reload in the middle of a sign-in keeps the group unlocked. r.mu is
// held.
func siteUnlockedLocked(b *browser, g *browserGroup) bool {
	return b.site != "" && g.input && g.watching
}

// syncSiteLock tells the pod a group's site lock (TypeSiteLock) when it
// differs from what the pod was last told. It runs off the caller's
// goroutine, one at a time per browser, and reads the state when its
// turn comes, so the last message the pod gets is the current state and
// a state the pod already has is not sent again. A released group is
// not told: the pod forgets the lock with the group's tabs.
func (r *browserRegistry) syncSiteLock(b *browser, group string) {
	r.siteLocks.Add(1)
	go func() {
		defer r.siteLocks.Done()
		b.lockMu.Lock()
		defer b.lockMu.Unlock()
		r.mu.Lock()
		g, ok := b.groups[group]
		live := b.state == browserStarting || b.state == browserRunning || b.state == browserIdle
		if !ok || !live || b.link == nil {
			r.mu.Unlock()
			return
		}
		want := siteUnlockedLocked(b, g)
		if want == g.unlocked {
			r.mu.Unlock()
			return
		}
		link := b.link
		r.mu.Unlock()
		if err := link.send(browserSendTimeout, browserpod.Message{Type: browserpod.TypeSiteLock, Group: group, Unlocked: want}, nil); err != nil {
			r.logf("browser %s: site lock for %q: %v", b.key(), group, err)
			return
		}
		r.mu.Lock()
		g.unlocked = want
		r.mu.Unlock()
	}()
}

// awaitSiteLocks waits for the site-lock syncs in flight.
func (r *browserRegistry) awaitSiteLocks() { r.siteLocks.Wait() }

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
	delete(r.released, browserKey(slug, session))
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
	if body == nil || msg.Bytes <= 0 || msg.Bytes > browserMaxFrameBytes || !tabID.MatchString(msg.Tab) {
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
func (r *browserRegistry) attach(b *browser) error {
	r.mu.Lock()
	link, site, coreToken := b.link, b.site, b.coreToken
	r.mu.Unlock()
	if link == nil {
		return errors.New("no link")
	}
	hello := browserpod.Message{Type: browserpod.TypeHello, Site: site, CoreToken: coreToken}
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
	if err := link.send(browserFileSendTimeout, hello, body); err != nil {
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
			if err := link.send(browserFileSendTimeout, browserpod.Message{Type: browserpod.TypeCookies, Bytes: size}, cookies); err != nil {
				return err
			}
		}
	}
	if err := link.send(browserSendTimeout, browserpod.Message{Type: browserpod.TypeStart}, nil); err != nil {
		return err
	}
	// The pod starts with every group locked: each group core holds
	// unlocked is told again.
	r.mu.Lock()
	var groups []string
	for name, g := range b.groups {
		g.unlocked = false
		if siteUnlockedLocked(b, g) {
			groups = append(groups, name)
		}
	}
	r.mu.Unlock()
	for _, name := range groups {
		r.syncSiteLock(b, name)
	}
	return nil
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
		// A tab the pod now tracks, or one it tracks under a new title
		// or URL. The id must be one the pod would mint, the group one
		// core opened, and a group holds no more tabs than the pod
		// allows a browser; what is kept of the URL and title is
		// bounded.
		r.mu.Lock()
		g, ok := b.groups[msg.Group]
		if ok && tabID.MatchString(msg.Tab) && (g.tabs[msg.Tab] || len(g.tabs) < browserpod.MaxTabsPerBrowser) {
			g.tabs[msg.Tab] = true
			b.tabs[msg.Tab] = msg.Group
			info := browserTabInfo{url: clipText(msg.URL, browserTabTextMax), title: clipText(msg.Title, browserTabTextMax)}
			g.tabInfo[msg.Tab] = info
			if msg.Tab == g.shownTab {
				// The shown tab's title changed without a navigation
				// (a page updating its own title): the windows hear it.
				ev := tabEvent(msg.Tab, info)
				for w := range g.watchers {
					w.event(ev)
				}
			}
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
			g.lastURL = clipText(msg.URL, browserTabTextMax)
			if g.tabs[msg.Tab] {
				info := browserTabInfo{url: g.lastURL, title: clipText(msg.Title, browserTabTextMax)}
				g.tabInfo[msg.Tab] = info
				if msg.Tab == g.shownTab {
					ev := tabEvent(msg.Tab, info)
					for w := range g.watchers {
						w.event(ev)
					}
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
	b.cookieGen++
	offboarded := b.offboarded
	r.changedLocked()
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
		b.sandboxOff = b.sandboxOff || msg.Detail == "sandbox: off"
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
	// The bytes are reserved before the write, so two downloads landing
	// at once cannot both fit under the limit; a write that fails gives
	// them back.
	g.downloadBytes += msg.Bytes
	r.mu.Unlock()
	root := r.downloadsRoot(b.slug, msg.Group)
	name, err := files.WriteUntrustedDownload(root, msg.FileName, io.LimitReader(body, msg.Bytes), browserpod.MaxDownloadBytes)
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok = b.groups[msg.Group]
	if !ok {
		return
	}
	if err != nil {
		r.logf("browser %s: download %q for %s: %v", b.key(), msg.FileName, msg.Group, err)
		g.downloadBytes -= msg.Bytes
		return
	}
	g.downloads = append(g.downloads, name)
}

// clipText is s cut to at most n bytes, at a character boundary.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
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
// browserIdleAfter. The release is remembered whether or not a browser
// runs, so a call of the task's still in flight cannot open its group
// on the browser it would start.
func (r *browserRegistry) ReleaseGroup(slug, session, group string) (structured.WebFacts, []string) {
	var facts structured.WebFacts
	r.mu.Lock()
	key := browserKey(slug, session)
	rel, ok := r.released[key]
	if !ok {
		rel = &releasedGroups{set: map[string]bool{}}
		r.released[key] = rel
	}
	rel.add(group)
	b, ok := r.browsers[key]
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
			_ = link.send(browserSendTimeout, browserpod.Message{Type: browserpod.TypeCloseGroup, Group: group}, nil)
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
	// Off the caller's goroutine: a pod that has stopped reading must
	// not hold a start at the cap.
	go func() {
		if err := link.send(browserSendTimeout, browserpod.Message{Type: browserpod.TypeSave}, nil); err != nil {
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
// the cap counts the pod until it is gone. Nothing here waits on the
// pod: the link's close is immediate, and the delete runs on its own
// goroutine.
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
	// The windows end here, and are forgotten: nothing is left for a
	// sweep or a later Unwatch to act on.
	for _, g := range b.groups {
		for w := range g.watchers {
			w.end("The browser stopped.")
		}
		g.watchers = map[*browserWatcher]struct{}{}
		g.watching = false
		g.closeAt = time.Time{}
	}
	b.windows = 0
	link := b.link
	b.link = nil
	r.changedLocked()
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
	for key := range r.released {
		if strings.HasPrefix(key, slug+"/") {
			delete(r.released, key)
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
