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
// pod. The registry is in memory: on boot every browser pod is deleted
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
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/structured"
)

// Browser lifecycle limits.
const (
	// browserIdleAfter is how long a browser with no subagent and no
	// window stays up before core saves its profile and deletes it.
	browserIdleAfter = 10 * time.Minute
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

// browserGroup is one web subagent's use of a browser: its tabs and
// what core saw of its run.
type browserGroup struct {
	tabs          map[string]bool
	pages         int
	lastURL       string
	downloads     []string
	downloadBytes int64
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
	var due []action
	var next time.Time
	for _, b := range r.browsers {
		if b.deadline.IsZero() {
			continue
		}
		if !b.deadline.After(now) {
			due = append(due, action{b, b.state})
			continue
		}
		if next.IsZero() || b.deadline.Before(next) {
			next = b.deadline
		}
	}
	r.mu.Unlock()
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
	if len(due) > 0 {
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
	b.state = browserRunning
	b.deadline = time.Time{}
	b.lastUsed = r.clk.Now()
	if _, ok := b.groups[group]; !ok {
		b.groups[group] = &browserGroup{tabs: map[string]bool{}}
	}
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
		if bs, found, err := r.store.ReadBrowserSession(slug, session); err == nil && found {
			b.site = bs.Site
		}
		r.browsers[b.key()] = b
		r.mu.Unlock()
		r.kick()
		if _, err := r.store.EnsureBrowserSession(slug, session, "", r.clk.Now()); err != nil {
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
	profile, size, err := r.store.OpenBrowserProfile(b.slug, b.session)
	if err != nil {
		return fmt.Errorf("open profile: %w", err)
	}
	hello := browserpod.Message{Type: browserpod.TypeHello, Bytes: size, Site: site}
	var body io.Reader
	if profile != nil {
		defer func() { _ = profile.Close() }()
		body = profile
	}
	sctx, cancel := context.WithTimeout(ctx, browserFileSendTimeout)
	defer cancel()
	if err := link.send(sctx, hello, body); err != nil {
		return err
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
		}
		r.mu.Unlock()
	case browserpod.TypeTabClosed:
		r.mu.Lock()
		if g, ok := b.groups[msg.Group]; ok {
			delete(g.tabs, msg.Tab)
		}
		delete(b.tabs, msg.Tab)
		r.mu.Unlock()
	case browserpod.TypeNavigated:
		r.mu.Lock()
		if g, ok := b.groups[msg.Group]; ok {
			g.pages++
			g.lastURL = msg.URL
		}
		r.mu.Unlock()
	case browserpod.TypeDownload:
		r.onDownload(b, msg, body)
	case browserpod.TypeProfile:
		r.onProfile(b, msg, body)
	case browserpod.TypeSaved:
		r.finish(b)
	case browserpod.TypeCookiesChanged:
		// Periodic snapshots read this; nothing acts on it yet.
	}
}

func (r *browserRegistry) onState(b *browser, msg browserpod.Message) {
	switch msg.State {
	case browserpod.StateRunning:
		r.mu.Lock()
		if b.state == browserStarting {
			b.state = browserRunning
			b.deadline = time.Time{}
			// Nobody may be waiting any more: the command that started
			// it was cancelled. Then the browser is idle from the start
			// and is reaped like any other.
			if len(b.groups) == 0 {
				b.state = browserIdle
				b.deadline = r.clk.Now().Add(browserIdleAfter)
			}
			close(b.ready)
		}
		r.mu.Unlock()
		if msg.Detail != "" {
			r.logf("browser %s: running (%s)", b.key(), msg.Detail)
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
		delete(b.groups, group)
	}
	b.released[group] = true
	link := b.link
	idle := len(b.groups) == 0 && b.windows == 0 && b.state == browserRunning
	if idle {
		b.state = browserIdle
		b.deadline = r.clk.Now().Add(browserIdleAfter)
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
	b.state = browserSaving
	b.deadline = r.clk.Now().Add(browserSaveTimeout)
	link := b.link
	r.mu.Unlock()
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
	b.state = browserStopped
	b.deadline = time.Time{}
	for id, ch := range b.pending {
		close(ch)
		delete(b.pending, id)
	}
	link := b.link
	b.link = nil
	r.mu.Unlock()
	if link != nil {
		link.close()
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
