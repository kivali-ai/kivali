package browserpod

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// browser is the pod's model of Chromium: its page targets as tabs,
// the groups they belong to, and the downloads in flight.
type browser struct {
	conn  cdpConn
	clock clock.Clock
	log   *log.Logger
	site  string
	dir   string
	send  func(msg Message, body io.Reader) error
	// ephemeral: every group's tabs live in a browser context of the
	// group's own, with its own cookies, storage and cache, disposed
	// with the group; the start page stays the spare and is never
	// handed to a group. Nothing of the session outlives its tasks.
	ephemeral bool

	mu      sync.Mutex
	nextTab int
	// tabs by target id, and by tab id.
	tabs map[string]*tab
	byID map[string]*tab
	// groups by name.
	groups map[string]*group
	// attached holds page targets that are attached but not yet a
	// tab: one a command is creating, waiting to be claimed.
	attached map[string]attachment
	// claims are commands waiting for their new target to attach.
	claims map[string]chan struct{}
	// creating counts commands between Target.createTarget and their
	// claim; while it is above zero a page with no opener may be
	// theirs.
	creating int
	// spare is a page in no group, kept so the browser always has a
	// window: Chromium's start page, or a fresh page made as the last
	// tab closed.
	spare *attachment
	// frames maps every frame below a tab's main frame to its tab, so
	// a download an iframe starts is the tab's.
	frames map[string]*tab

	downloads map[string]*download
	// sent holds, per group, the names of the downloads sent for it, so
	// no two of a group's downloads share a name.
	sent map[string]map[string]bool
	// front is the tab last brought to the front, nil when Chromium has
	// said that tab is hidden.
	front *tab
	// running counts commands in flight. While one runs, a watched tab
	// that loses the front is brought back when the command ends, not
	// at once: the command's own tab needs the front meanwhile.
	running int
}

// attachment is an attached page target.
type attachment struct {
	targetID  string
	sessionID string
	openerID  string
	url       string
}

// group is one subagent's tabs.
type group struct {
	name    string
	tabs    []*tab
	current *tab
	// contextID is the group's own browser context in an ephemeral
	// session; empty means the default context.
	contextID string
	// site is the registrable domain the group's tabs are held to in a
	// browser whose session is not locked (holdGroup); empty for none.
	site string
	// watched: a browser window is on this group, so its current tab
	// is screencast (followWatch).
	watched bool
	// closing: closeGroup is closing the group's tabs; a popup one of
	// them opens meanwhile is closed, not adopted.
	closing bool
	// unlocked: in a site session, a person is attached to the group's
	// waiting task (TypeSiteLock), so its pages may go off the site to
	// sign in through an identity provider and its dialogs are accepted
	// (dialogPolicy). Commands stay on the site.
	unlocked bool
	// acceptDialogs counts the group's commands running with
	// accept_dialog set; while above zero its dialogs are accepted.
	acceptDialogs int
}

// tab is a page target in a group.
type tab struct {
	id       string
	group    string
	targetID string
	session  string
	opener   string

	mu    sync.Mutex
	url   string
	title string
	// notes are things the model should hear with the next result:
	// a blocked redirect, a dropped download, a dialog.
	notes []string
	// Counters the waits compare against a mark taken before an
	// action.
	starts    int  // navigations requested or started in the main frame
	stops     int  // main-frame requests off the site failed before they were sent
	loading   bool // the main frame is loading
	popups    int  // tabs this tab's pages opened
	navs      int  // cross-document commits in the main frame
	navigated int  // navigated messages sent
	// pendingNav is a commit whose navigated message waits for load.
	pendingNav bool
	loaderID   string
	idleLoader string
	changed    chan struct{}
	world      int
	hasRefs    bool
	refsNav    int
	refs       []refInfo
	screencast bool
	lastSent   string
	closed     bool
	offs       []func()
}

// refInfo is what the last snapshot said about a ref.
type refInfo struct {
	Role string `json:"role"`
	Name string `json:"name"`
}

// mark is a tab's counters at one moment.
type mark struct{ starts, navs, navigated, popups int }

func newBrowser(conn cdpConn, clk clock.Clock, logger *log.Logger, site, dir string, send func(Message, io.Reader) error) *browser {
	return &browser{
		conn:      conn,
		clock:     clk,
		log:       logger,
		site:      strings.ToLower(strings.TrimPrefix(site, ".")),
		dir:       dir,
		send:      send,
		tabs:      map[string]*tab{},
		byID:      map[string]*tab{},
		groups:    map[string]*group{},
		attached:  map[string]attachment{},
		claims:    map[string]chan struct{}{},
		frames:    map[string]*tab{},
		downloads: map[string]*download{},
		sent:      map[string]map[string]bool{},
	}
}

type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	OpenerID string `json:"openerId"`
}

// start subscribes to the browser's target and download events and
// attaches to every page. New pages start paused (waitForDebuggerOnStart)
// and run once the pod is watching them: a popup that is not paused
// this way stalls its opener's input in Chromium until it is resumed.
func (b *browser) start(ctx context.Context) error {
	b.conn.On("", "Target.attachedToTarget", b.onAttached)
	b.conn.On("", "Target.detachedFromTarget", b.onDetached)
	b.conn.On("", "Target.targetDestroyed", b.onDestroyed)
	b.conn.On("", "Browser.downloadWillBegin", b.onDownloadWillBegin)
	b.conn.On("", "Browser.downloadProgress", b.onDownloadProgress)
	if err := b.startDownloads(ctx); err != nil {
		return err
	}
	if _, err := b.conn.Call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}); err != nil {
		return fmt.Errorf("Target.setDiscoverTargets: %w", err)
	}
	if _, err := b.conn.Call(ctx, "", "Target.setAutoAttach", map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
	}); err != nil {
		return fmt.Errorf("Target.setAutoAttach: %w", err)
	}
	return nil
}

func (b *browser) onAttached(raw json.RawMessage) {
	var ev struct {
		SessionID          string     `json:"sessionId"`
		TargetInfo         targetInfo `json:"targetInfo"`
		WaitingForDebugger bool       `json:"waitingForDebugger"`
	}
	if json.Unmarshal(raw, &ev) != nil {
		return
	}
	if ev.TargetInfo.Type != "page" {
		b.resume(ev.SessionID)
		return
	}
	a := attachment{targetID: ev.TargetInfo.TargetID, sessionID: ev.SessionID, openerID: ev.TargetInfo.OpenerID, url: ev.TargetInfo.URL}

	b.mu.Lock()
	if _, known := b.tabs[a.targetID]; known {
		b.mu.Unlock()
		return
	}
	if opener := b.tabs[a.openerID]; opener != nil {
		b.mu.Unlock()
		b.adoptPopup(a, opener)
		return
	}
	if ch, ok := b.claims[a.targetID]; ok {
		b.attached[a.targetID] = a
		delete(b.claims, a.targetID)
		close(ch)
		b.mu.Unlock()
		return
	}
	if b.creating > 0 {
		b.attached[a.targetID] = a
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	b.orphan(a)
}

// orphan handles a page that belongs to no group: it becomes the
// spare when the browser has no other page, and is closed otherwise.
func (b *browser) orphan(a attachment) {
	b.mu.Lock()
	if b.spare == nil && len(b.tabs) == 0 {
		b.spare = &a
		b.mu.Unlock()
		b.resume(a.sessionID)
		return
	}
	b.mu.Unlock()
	b.log.Printf("closing a page that belongs to no task")
	_, _ = b.conn.Call(context.Background(), "", "Target.closeTarget", map[string]any{"targetId": a.targetID})
}

// adoptPopup makes a page another tab opened into a tab of the
// opener's group. A popup of a group that is closing is closed with it,
// and a popup off the site the group is held to is closed and never
// becomes a tab, so nothing the task does can reach it.
func (b *browser) adoptPopup(a attachment, opener *tab) {
	at := a.url
	if info, err := b.targetInfo(context.Background(), a.targetID); err == nil && info.URL != "" {
		at = info.URL
	}
	if !b.allowed(opener.group, a.url) || !b.allowed(opener.group, at) {
		opener.note(withPage("The page opened a new tab off the site this task is held to; it was closed.", urlLine(at)))
		_, _ = b.conn.Call(context.Background(), "", "Target.closeTarget", map[string]any{"targetId": a.targetID})
		return
	}
	b.mu.Lock()
	if g := b.groups[opener.group]; g == nil || g.closing {
		b.mu.Unlock()
		_, _ = b.conn.Call(context.Background(), "", "Target.closeTarget", map[string]any{"targetId": a.targetID})
		return
	}
	if len(b.tabs) >= MaxTabsPerBrowser {
		b.mu.Unlock()
		opener.note(withPage(fmt.Sprintf("The page opened a new tab and it was closed: the browser holds at most %d tabs.", MaxTabsPerBrowser), urlLine(a.url)))
		_, _ = b.conn.Call(context.Background(), "", "Target.closeTarget", map[string]any{"targetId": a.targetID})
		return
	}
	t := b.registerLocked(a, opener.group, opener.id)
	b.front = t
	b.mu.Unlock()
	opener.note(fmt.Sprintf("The page opened a new tab, %s; web_tabs switches to it.", t.id))
	opener.update(func() { opener.popups++ })
	b.watch(t)
	b.enableSession(context.Background(), t.session, t.group)
	b.resume(t.session)
	// The page may have committed before the pod saw it.
	if info, err := b.targetInfo(context.Background(), t.targetID); err == nil {
		t.mu.Lock()
		if info.URL != "" {
			t.url = info.URL
		}
		t.title = info.Title
		t.mu.Unlock()
	}
	t.mu.Lock()
	u := t.url
	t.mu.Unlock()
	if !b.allowed(t.group, u) {
		// It went off the site before its navigations were watched.
		opener.note(withPage("The page opened a new tab off the site this task is held to; it was closed.", urlLine(u)))
		b.closeTab(context.Background(), t)
		return
	}
	b.emitTab(t)
	if u != "" && u != "about:blank" {
		b.emitNavigated(t)
	}
}

// closeOffSitePopup closes a popup whose page heads off the site its
// group is held to: a page opened with window.open attaches blank and
// navigates only once resumed, so this is where most are caught. The
// note goes to the tab that opened it. Reports whether t was a popup.
func (b *browser) closeOffSitePopup(t *tab, u string) bool {
	if t.opener == "" {
		return false
	}
	b.mu.Lock()
	opener := b.byID[t.opener]
	b.mu.Unlock()
	note := withPage("The page opened a new tab off the site this task is held to; it was closed.", urlLine(u))
	if opener != nil {
		opener.note(note)
	} else {
		t.note(note)
	}
	b.closeTab(context.Background(), t)
	return true
}

// registerLocked makes a tab of an attached page. b.mu is held.
func (b *browser) registerLocked(a attachment, groupName, opener string) *tab {
	b.nextTab++
	t := &tab{
		id:       fmt.Sprintf("t%d", b.nextTab),
		group:    groupName,
		targetID: a.targetID,
		session:  a.sessionID,
		opener:   opener,
		url:      a.url,
		changed:  make(chan struct{}),
	}
	b.tabs[a.targetID] = t
	b.byID[t.id] = t
	g := b.groupLocked(groupName)
	g.tabs = append(g.tabs, t)
	if g.current == nil {
		g.current = t
	}
	return t
}

func (b *browser) groupLocked(name string) *group {
	g := b.groups[name]
	if g == nil {
		g = &group{name: name}
		b.groups[name] = g
	}
	return g
}

func (b *browser) emitTab(t *tab) {
	t.mu.Lock()
	u := t.url
	t.mu.Unlock()
	b.sendMsg(Message{Type: TypeTab, Group: t.group, Tab: t.id, Opener: t.opener, URL: u})
}

func (b *browser) sendMsg(m Message) {
	if err := b.send(m, nil); err != nil {
		b.log.Printf("send %s: %v", m.Type, err)
	}
}

// enableSession turns on the domains the pod reads on a page of group.
// Where a site holds the page (a site session's, or the one a task that
// uploads files is held to) it also pauses every document request
// before it is sent (Fetch.requestPaused, handled in watch), so a
// main-frame request off the site, a redirect hop included, fails
// before anything loads.
func (b *browser) enableSession(ctx context.Context, session, group string) {
	for _, m := range []string{"Page.enable", "Runtime.enable", "DOM.enable", "Network.enable"} {
		if _, err := b.conn.Call(ctx, session, m, nil); err != nil {
			b.log.Printf("%s: %v", m, err)
		}
	}
	if _, err := b.conn.Call(ctx, session, "Page.setLifecycleEventsEnabled", map[string]any{"enabled": true}); err != nil {
		b.log.Printf("Page.setLifecycleEventsEnabled: %v", err)
	}
	if b.heldSite(group) != "" {
		if _, err := b.conn.Call(ctx, session, "Fetch.enable", map[string]any{
			"patterns": []map[string]any{{"urlPattern": "*", "resourceType": "Document", "requestStage": "Request"}},
		}); err != nil {
			b.log.Printf("Fetch.enable: %v", err)
		}
	}
}

// resume lets a page Chromium paused for the pod run: new targets
// wait until the pod has subscribed to their events.
func (b *browser) resume(session string) {
	if _, err := b.conn.Call(context.Background(), session, "Runtime.runIfWaitingForDebugger", nil); err != nil {
		b.log.Printf("Runtime.runIfWaitingForDebugger: %v", err)
	}
}

func (b *browser) targetInfo(ctx context.Context, targetID string) (targetInfo, error) {
	raw, err := b.conn.Call(ctx, "", "Target.getTargetInfo", map[string]any{"targetId": targetID})
	if err != nil {
		return targetInfo{}, err
	}
	var res struct {
		TargetInfo targetInfo `json:"targetInfo"`
	}
	err = json.Unmarshal(raw, &res)
	return res.TargetInfo, err
}

// watch subscribes to a tab's page events.
func (b *browser) watch(t *tab) {
	s := t.session
	on := func(method string, fn func(json.RawMessage)) {
		off := b.conn.On(s, method, fn)
		t.mu.Lock()
		t.offs = append(t.offs, off)
		t.mu.Unlock()
	}
	on("Page.frameRequestedNavigation", func(raw json.RawMessage) {
		var ev struct {
			FrameID     string `json:"frameId"`
			URL         string `json:"url"`
			Disposition string `json:"disposition"`
		}
		if json.Unmarshal(raw, &ev) != nil || ev.FrameID != t.targetID {
			return
		}
		// A navigation into a new tab or a download leaves this tab
		// where it is.
		if ev.Disposition != "" && ev.Disposition != "currentTab" {
			return
		}
		if !b.allowed(t.group, ev.URL) {
			_, _ = b.conn.Call(context.Background(), s, "Page.stopLoading", nil)
			// A group held to a site for its uploads loses an off-site
			// popup; a site session's stays, stopped, like any page.
			if b.site == "" && b.closeOffSitePopup(t, ev.URL) {
				return
			}
			t.note(withPage(fmt.Sprintf("The page tried to go off %s and was stopped.", b.offSiteName()), urlLine(ev.URL)))
		}
		t.update(func() {
			t.starts++
			t.loading = true
		})
	})
	on("Fetch.requestPaused", func(raw json.RawMessage) {
		// Held tabs only (enableSession). Every document request
		// pauses here before it is sent; a main-frame one off the site
		// fails, so an on-site URL that redirects off the site never
		// loads there, and the model hears why with its next result.
		// It fails as aborted: Chromium commits no error page for an
		// aborted navigation, so the tab stays on the page it was on.
		// A subframe's request is let through, and so is every request
		// of a group that is unlocked.
		var ev struct {
			RequestID    string `json:"requestId"`
			FrameID      string `json:"frameId"`
			ResourceType string `json:"resourceType"`
			Request      struct {
				URL string `json:"url"`
			} `json:"request"`
		}
		if json.Unmarshal(raw, &ev) != nil || ev.RequestID == "" {
			return
		}
		if ev.FrameID == t.targetID && ev.ResourceType == "Document" && !b.allowed(t.group, ev.Request.URL) {
			t.note(withPage(fmt.Sprintf("The page tried to go off %s and was stopped.", b.offSiteName()), urlLine(ev.Request.URL)))
			t.update(func() { t.stops++ })
			if _, err := b.conn.Call(context.Background(), s, "Fetch.failRequest", map[string]any{"requestId": ev.RequestID, "errorReason": "Aborted"}); err != nil {
				b.log.Printf("Fetch.failRequest: %v", err)
			}
			return
		}
		if _, err := b.conn.Call(context.Background(), s, "Fetch.continueRequest", map[string]any{"requestId": ev.RequestID}); err != nil {
			b.log.Printf("Fetch.continueRequest: %v", err)
		}
	})
	on("Page.frameAttached", func(raw json.RawMessage) {
		var ev struct {
			FrameID string `json:"frameId"`
		}
		if json.Unmarshal(raw, &ev) == nil && ev.FrameID != "" {
			b.mu.Lock()
			b.frames[ev.FrameID] = t
			b.mu.Unlock()
		}
	})
	on("Page.frameDetached", func(raw json.RawMessage) {
		var ev struct {
			FrameID string `json:"frameId"`
			Reason  string `json:"reason"`
		}
		// A frame swapped to another process is still the tab's.
		if json.Unmarshal(raw, &ev) == nil && ev.Reason != "swap" {
			b.mu.Lock()
			if b.frames[ev.FrameID] == t {
				delete(b.frames, ev.FrameID)
			}
			b.mu.Unlock()
		}
	})
	on("Page.frameStartedLoading", func(raw json.RawMessage) {
		var ev struct {
			FrameID string `json:"frameId"`
		}
		if json.Unmarshal(raw, &ev) == nil && ev.FrameID == t.targetID {
			t.update(func() {
				t.starts++
				t.loading = true
			})
		}
	})
	on("Page.frameStoppedLoading", func(raw json.RawMessage) {
		var ev struct {
			FrameID string `json:"frameId"`
		}
		if json.Unmarshal(raw, &ev) == nil && ev.FrameID == t.targetID {
			t.update(func() { t.loading = false })
		}
	})
	on("Page.frameNavigated", func(raw json.RawMessage) {
		var ev struct {
			Frame struct {
				ID             string `json:"id"`
				ParentID       string `json:"parentId"`
				LoaderID       string `json:"loaderId"`
				URL            string `json:"url"`
				URLFragment    string `json:"urlFragment"`
				UnreachableURL string `json:"unreachableUrl"`
			} `json:"frame"`
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &ev) != nil || ev.Frame.ParentID != "" || ev.Frame.ID != t.targetID {
			return
		}
		u := ev.Frame.URL + ev.Frame.URLFragment
		switch {
		case ev.Frame.UnreachableURL != "" && !b.allowed(t.group, ev.Frame.UnreachableURL):
			// Chromium's error page for an off-site URL that failed to
			// load: the tab goes to about:blank rather than showing it.
			t.note(withPage(fmt.Sprintf("The page could not load a URL off %s; the tab was sent to about:blank.", b.offSiteName()), urlLine(ev.Frame.UnreachableURL)))
			go func() {
				_, _ = b.conn.Call(context.Background(), s, "Page.navigate", map[string]any{"url": "about:blank"})
			}()
		case !b.allowed(t.group, u):
			// The backstop for a commit the request pause did not catch.
			if b.site == "" && b.closeOffSitePopup(t, u) {
				return
			}
			t.note(withPage(fmt.Sprintf("The page went off %s; the tab was sent to about:blank.", b.offSiteName()), urlLine(u)))
			go func() {
				_, _ = b.conn.Call(context.Background(), s, "Page.navigate", map[string]any{"url": "about:blank"})
			}()
		}
		t.update(func() {
			t.url = u
			t.navs++
			t.pendingNav = true
			t.loaderID = ev.Frame.LoaderID
			t.world = 0
		})
		if ev.Type == "BackForwardCacheRestore" {
			// A page restored from the back-forward cache fires no load.
			b.emitNavigated(t)
		}
	})
	on("Page.navigatedWithinDocument", func(raw json.RawMessage) {
		var ev struct {
			FrameID string `json:"frameId"`
			URL     string `json:"url"`
		}
		if json.Unmarshal(raw, &ev) != nil || ev.FrameID != t.targetID {
			return
		}
		t.update(func() { t.url = ev.URL })
		b.emitNavigated(t)
	})
	on("Page.loadEventFired", func(json.RawMessage) {
		t.mu.Lock()
		pending := t.pendingNav
		t.mu.Unlock()
		if pending {
			b.emitNavigated(t)
		}
	})
	on("Page.lifecycleEvent", func(raw json.RawMessage) {
		var ev struct {
			FrameID  string `json:"frameId"`
			LoaderID string `json:"loaderId"`
			Name     string `json:"name"`
		}
		if json.Unmarshal(raw, &ev) == nil && ev.FrameID == t.targetID && ev.Name == "networkIdle" {
			t.update(func() { t.idleLoader = ev.LoaderID })
		}
	})
	on("Runtime.executionContextsCleared", func(json.RawMessage) {
		t.update(func() { t.world = 0 })
	})
	on("Runtime.executionContextDestroyed", func(raw json.RawMessage) {
		var ev struct {
			ID int `json:"executionContextId"`
		}
		if json.Unmarshal(raw, &ev) == nil {
			t.update(func() {
				if t.world == ev.ID {
					t.world = 0
				}
			})
		}
	})
	on("Page.javascriptDialogOpening", func(raw json.RawMessage) { b.onDialog(t, s, raw) })
	on("Page.screencastFrame", func(raw json.RawMessage) { b.onScreencastFrame(t, raw) })
	on("Page.screencastVisibilityChanged", func(raw json.RawMessage) { b.onScreencastVisibility(t, raw) })
}

// emitNavigated sends navigated for the tab's current URL with the
// page's title, once per settled navigation.
func (b *browser) emitNavigated(t *tab) {
	info, err := b.targetInfo(context.Background(), t.targetID)
	t.mu.Lock()
	if err == nil {
		t.title = info.Title
	}
	u, title := t.url, t.title
	t.mu.Unlock()
	b.sendMsg(Message{Type: TypeNavigated, Group: t.group, Tab: t.id, URL: u, Title: title})
	t.update(func() {
		t.pendingNav = false
		t.navigated++
		t.lastSent = u
	})
}

func (b *browser) onDetached(raw json.RawMessage) {
	var ev struct {
		TargetID string `json:"targetId"`
	}
	if json.Unmarshal(raw, &ev) == nil {
		b.dropTarget(ev.TargetID)
	}
}

func (b *browser) onDestroyed(raw json.RawMessage) {
	var ev struct {
		TargetID string `json:"targetId"`
	}
	if json.Unmarshal(raw, &ev) == nil {
		b.dropTarget(ev.TargetID)
	}
}

// dropTarget forgets a page that went away on its own. The group's
// current tab may have moved on, and a watched group's screencast
// follows it.
func (b *browser) dropTarget(targetID string) {
	b.mu.Lock()
	if b.spare != nil && b.spare.targetID == targetID {
		b.spare = nil
	}
	t := b.tabs[targetID]
	if t != nil {
		b.unregisterLocked(t)
	}
	b.mu.Unlock()
	if t != nil {
		b.sendMsg(Message{Type: TypeTabClosed, Group: t.group, Tab: t.id})
		b.followWatch(context.Background(), t.group)
	}
}

// unregisterLocked removes a tab from the maps and its group, and
// moves the group's current tab on. b.mu is held.
func (b *browser) unregisterLocked(t *tab) {
	delete(b.tabs, t.targetID)
	delete(b.byID, t.id)
	for id, x := range b.frames {
		if x == t {
			delete(b.frames, id)
		}
	}
	var offs []func()
	t.update(func() {
		t.closed = true
		offs, t.offs = t.offs, nil
	})
	for _, off := range offs {
		off()
	}
	g := b.groups[t.group]
	if g == nil {
		return
	}
	for i, x := range g.tabs {
		if x != t {
			continue
		}
		g.tabs = append(g.tabs[:i:i], g.tabs[i+1:]...)
		if g.current == t {
			g.current = nil
			switch {
			case i < len(g.tabs):
				g.current = g.tabs[i]
			case len(g.tabs) > 0:
				g.current = g.tabs[len(g.tabs)-1]
			}
		}
		break
	}
}

// lookupTab finds the tab a command names in its group; empty names
// the group's current tab, opened on first use when create is set.
func (b *browser) lookupTab(ctx context.Context, groupName, id string, create bool) (*tab, error) {
	b.mu.Lock()
	if id != "" {
		t := b.byID[id]
		b.mu.Unlock()
		if t == nil || t.group != groupName {
			return nil, failf(CodeNotFound, "There is no tab %s in this task; web_tabs lists yours.", id)
		}
		return t, nil
	}
	g := b.groups[groupName]
	if g != nil && g.current != nil {
		t := g.current
		b.mu.Unlock()
		return t, nil
	}
	b.mu.Unlock()
	if !create {
		return nil, failf(CodeNotFound, "This task has no tab open.")
	}
	return b.createTab(ctx, groupName)
}

// createTab opens a blank tab in the group and makes it current.
func (b *browser) createTab(ctx context.Context, groupName string) (*tab, error) {
	b.mu.Lock()
	if len(b.tabs)+b.creating >= MaxTabsPerBrowser {
		b.mu.Unlock()
		return nil, failf(CodeTabLimit, "The browser already has %d tabs open, its limit; close one with web_tabs first.", MaxTabsPerBrowser)
	}
	if b.spare != nil && !b.ephemeral {
		a := *b.spare
		b.spare = nil
		t := b.registerLocked(a, groupName, "")
		b.groupLocked(groupName).current = t
		b.mu.Unlock()
		b.watch(t)
		b.enableSession(ctx, t.session, groupName)
		b.resume(t.session)
		t.update(func() { t.url = "about:blank" })
		b.emitTab(t)
		b.followWatch(ctx, groupName)
		return t, nil
	}
	b.creating++
	b.mu.Unlock()

	contextID, err := b.contextFor(ctx, groupName)
	var a attachment
	if err == nil {
		a, err = b.newTarget(ctx, contextID)
	}

	b.mu.Lock()
	b.creating--
	var leftover []attachment
	if b.creating == 0 {
		for id, x := range b.attached {
			leftover = append(leftover, x)
			delete(b.attached, id)
		}
	}
	var t *tab
	if err == nil {
		t = b.registerLocked(a, groupName, "")
		b.groupLocked(groupName).current = t
	}
	b.mu.Unlock()
	for _, x := range leftover {
		b.orphan(x)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failf(CodeBrowser, "Could not open a tab: %v", err)
	}
	b.watch(t)
	b.enableSession(ctx, t.session, groupName)
	b.resume(t.session)
	b.emitTab(t)
	b.followWatch(ctx, groupName)
	return t, nil
}

// newTarget creates a blank page and waits for it to attach.
func (b *browser) newTarget(ctx context.Context, contextID string) (attachment, error) {
	params := map[string]any{"url": "about:blank"}
	if contextID != "" {
		params["browserContextId"] = contextID
	}
	raw, err := b.conn.Call(ctx, "", "Target.createTarget", params)
	if err != nil {
		return attachment{}, err
	}
	var res struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return attachment{}, err
	}
	b.mu.Lock()
	if a, ok := b.attached[res.TargetID]; ok {
		delete(b.attached, res.TargetID)
		b.mu.Unlock()
		return a, nil
	}
	ch := make(chan struct{})
	b.claims[res.TargetID] = ch
	b.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.claims, res.TargetID)
		b.mu.Unlock()
		_, _ = b.conn.Call(context.Background(), "", "Target.closeTarget", map[string]any{"targetId": res.TargetID})
		return attachment{}, ctx.Err()
	}
	b.mu.Lock()
	a := b.attached[res.TargetID]
	delete(b.attached, res.TargetID)
	b.mu.Unlock()
	return a, nil
}

// closeTab closes a tab. Before the browser's last page closes a fresh
// page is made and kept as the spare, so the browser never runs out of
// windows and the next group's first tab carries no history of this
// one: web_back there has nowhere to go. In an ephemeral session no
// spare is made: a group's context goes with it, and the start page is
// the spare.
func (b *browser) closeTab(ctx context.Context, t *tab) {
	b.mu.Lock()
	if _, ok := b.tabs[t.targetID]; !ok {
		b.mu.Unlock()
		return
	}
	// The group's current tab may change here; a watched group's
	// screencast follows it once the lock is released.
	defer b.followWatch(ctx, t.group)
	last := !b.ephemeral && len(b.tabs) == 1 && b.spare == nil
	if last {
		b.creating++
	}
	b.mu.Unlock()
	var spare *attachment
	if last {
		a, err := b.newTarget(ctx, "")
		b.mu.Lock()
		b.creating--
		var leftover []attachment
		if b.creating == 0 {
			for id, x := range b.attached {
				leftover = append(leftover, x)
				delete(b.attached, id)
			}
		}
		b.mu.Unlock()
		for _, x := range leftover {
			b.orphan(x)
		}
		if err != nil {
			b.log.Printf("spare page: %v", err)
		} else {
			spare = &a
		}
	}
	b.mu.Lock()
	b.unregisterLocked(t)
	if spare != nil {
		b.spare = spare
	}
	b.mu.Unlock()
	b.sendMsg(Message{Type: TypeTabClosed, Group: t.group, Tab: t.id})
	if spare != nil {
		b.resume(spare.sessionID)
	}
	if _, err := b.conn.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": t.targetID}); err != nil {
		b.log.Printf("close %s: %v", t.id, err)
	}
}

// closeGroup closes every tab of a group and, in an ephemeral session,
// disposes of the group's browser context: its cookies, storage and
// cache go with it. The group is marked closing first, so a popup a
// closing tab opens is closed rather than adopted, and forgotten, with
// its site unlock, once its tabs are gone.
func (b *browser) closeGroup(ctx context.Context, name string) {
	b.mu.Lock()
	g := b.groups[name]
	var tabs []*tab
	var contextID string
	if g != nil {
		g.closing = true
		tabs = append(tabs, g.tabs...)
		contextID = g.contextID
	}
	b.mu.Unlock()
	for _, t := range tabs {
		b.closeTab(ctx, t)
	}
	b.mu.Lock()
	if b.groups[name] == g {
		delete(b.groups, name)
	}
	delete(b.sent, name)
	b.mu.Unlock()
	if contextID != "" {
		if _, err := b.conn.Call(ctx, "", "Target.disposeBrowserContext", map[string]any{"browserContextId": contextID}); err != nil {
			b.log.Printf("dispose the context of %s: %v", name, err)
		}
	}
}

// contextFor is the browser context a group's tabs are created in: in
// an ephemeral session a context of the group's own, made on first use
// with downloads reported like the default context's; otherwise the
// default context, "".
func (b *browser) contextFor(ctx context.Context, groupName string) (string, error) {
	if !b.ephemeral {
		return "", nil
	}
	b.mu.Lock()
	id := b.groupLocked(groupName).contextID
	b.mu.Unlock()
	if id != "" {
		return id, nil
	}
	raw, err := b.conn.Call(ctx, "", "Target.createBrowserContext", map[string]any{})
	if err != nil {
		return "", fmt.Errorf("Target.createBrowserContext: %w", err)
	}
	var res struct {
		ID string `json:"browserContextId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.ID == "" {
		return "", fmt.Errorf("Target.createBrowserContext: no context id in %s", raw)
	}
	dispose := func() {
		_, _ = b.conn.Call(ctx, "", "Target.disposeBrowserContext", map[string]any{"browserContextId": res.ID})
	}
	if err := b.setDownloadBehavior(ctx, res.ID); err != nil {
		dispose()
		return "", err
	}
	b.mu.Lock()
	g := b.groupLocked(groupName)
	if g.contextID == "" {
		g.contextID = res.ID
		b.mu.Unlock()
		return res.ID, nil
	}
	// Another command of the group got there first.
	id = g.contextID
	b.mu.Unlock()
	dispose()
	return id, nil
}

// groupTabs is a snapshot of a group's tabs and its current one.
func (b *browser) groupTabs(name string) ([]*tab, *tab) {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.groups[name]
	if g == nil {
		return nil, nil
	}
	return append([]*tab(nil), g.tabs...), g.current
}

func (b *browser) setCurrent(ctx context.Context, t *tab) {
	b.mu.Lock()
	if g := b.groups[t.group]; g != nil {
		g.current = t
	}
	b.mu.Unlock()
	b.followWatch(ctx, t.group)
}

// bringToFront makes t the browser's active tab. Chromium defers
// work in background tabs (a download a click starts, for one), and a
// popup takes the front when it opens.
func (b *browser) bringToFront(ctx context.Context, t *tab) {
	b.mu.Lock()
	if b.front == t {
		b.mu.Unlock()
		return
	}
	b.front = t
	b.mu.Unlock()
	if _, err := b.conn.Call(ctx, t.session, "Page.bringToFront", nil); err != nil {
		b.log.Printf("Page.bringToFront %s: %v", t.id, err)
	}
}

// beginCommand and endCommand bracket a command. Commands bring their
// tab to the front, and a popup one opens takes it; after a command the
// watched group's current tab is brought back, so a person's window on
// it keeps its frames and its input. A command whose own group is
// watched keeps that group in front.
func (b *browser) beginCommand() {
	b.mu.Lock()
	b.running++
	b.mu.Unlock()
}

func (b *browser) endCommand(ctx context.Context, group string) {
	b.mu.Lock()
	b.running--
	b.mu.Unlock()
	b.frontWatched(ctx, group)
}

// frontWatched brings a watched group's current tab to the front:
// prefer's when that group is watched, else the first watched group by
// name. With no group watched it does nothing.
func (b *browser) frontWatched(ctx context.Context, prefer string) {
	b.mu.Lock()
	g := b.groups[prefer]
	if g == nil || !g.watched {
		g = nil
		var names []string
		for name, x := range b.groups {
			if x.watched {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			sort.Strings(names)
			g = b.groups[names[0]]
		}
	}
	var t *tab
	if g != nil {
		t = g.current
	}
	b.mu.Unlock()
	if t != nil {
		b.bringToFront(ctx, t)
	}
}

func (b *browser) tabByID(id string) *tab {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.byID[id]
}

// holdGroup holds a group's tabs to site, a registrable domain, from
// now until the group closes: core names it on every command of a task
// that uploads files. A group already held keeps its site, and an
// empty site changes nothing.
func (b *browser) holdGroup(name, site string) {
	site = strings.ToLower(strings.TrimPrefix(site, "."))
	if site == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if g := b.groupLocked(name); g.site == "" {
		g.site = site
	}
}

// heldSite is the site a group's tabs are held to: the session's, else
// the group's own; empty for none.
func (b *browser) heldSite(groupName string) string {
	if b.site != "" {
		return b.site
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if g := b.groups[groupName]; g != nil {
		return g.site
	}
	return ""
}

// offSiteName names the site a page went off, for a note: the
// session's, or the one a task is held to.
func (b *browser) offSiteName() string {
	if b.site != "" {
		return "this session's site"
	}
	return "the site this task is held to"
}

// allowed reports whether a page of the group may be at u: non-web
// URLs always, and web URLs on the site the session is locked to or
// the group is held to, or a subdomain of it. With neither, or in a
// site session's group a person has unlocked, anywhere.
func (b *browser) allowed(groupName, u string) bool {
	site := b.heldSite(groupName)
	if site == "" || b.isUnlocked(groupName) {
		return true
	}
	return onSiteURL(site, u)
}

// onSiteURL reports whether u is a non-web URL or one on site or a
// subdomain of it.
func onSiteURL(site, u string) bool {
	p, err := url.Parse(u)
	if err != nil {
		return false
	}
	if p.Scheme != "http" && p.Scheme != "https" {
		return true
	}
	return onSite(site, p.Hostname())
}

func (b *browser) isUnlocked(group string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.groups[group]
	return g != nil && g.unlocked
}

// setSiteLock unlocks or relocks a group in a site session
// (TypeSiteLock). A relock returns the group's tabs whose page is off
// the site, for returnToSite; nothing else does.
func (b *browser) setSiteLock(group string, unlocked bool) []*tab {
	if b.site == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.groupLocked(group)
	was := g.unlocked
	g.unlocked = unlocked
	if unlocked || !was {
		return nil
	}
	var off []*tab
	for _, t := range g.tabs {
		t.mu.Lock()
		u := t.url
		t.mu.Unlock()
		if !onSiteURL(b.site, u) {
			off = append(off, t)
		}
	}
	return off
}

// returnToSite sends each tab to the site's root page, https://<site>/,
// with a note for the model's next result.
func (b *browser) returnToSite(ctx context.Context, tabs []*tab) {
	home := "https://" + b.site + "/"
	for _, t := range tabs {
		t.mu.Lock()
		u := t.url
		t.mu.Unlock()
		t.note(withPage(fmt.Sprintf("The tab was off this session's site when the person finished; it was sent to %s.", home), urlLine(u)))
		if _, err := b.conn.Call(ctx, t.session, "Page.navigate", map[string]any{"url": home}); err != nil {
			b.log.Printf("return %s to the site: %v", t.id, err)
		}
	}
}

func onSite(site, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == site || strings.HasSuffix(host, "."+site)
}

// update changes a tab's state under its lock and wakes its waiters.
func (t *tab) update(fn func()) {
	t.mu.Lock()
	fn()
	close(t.changed)
	t.changed = make(chan struct{})
	t.mu.Unlock()
}

func (t *tab) note(s string) {
	t.mu.Lock()
	t.notes = append(t.notes, s)
	t.mu.Unlock()
}

func (t *tab) takeNotes() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.notes
	t.notes = nil
	return n
}

func (t *tab) mark() mark {
	t.mu.Lock()
	defer t.mu.Unlock()
	return mark{starts: t.starts, navs: t.navs, navigated: t.navigated, popups: t.popups}
}

// stopCount is how many main-frame requests off the site this tab has
// had stopped before they were sent.
func (t *tab) stopCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stops
}

// pageLine is the page content line saying where the tab is.
func (t *tab) pageLine() string { return "Page: " + t.where() }

// where is the tab's URL and title, both the page's: page content.
func (t *tab) where() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return where(t.url, t.title)
}

func where(u, title string) string {
	if title == "" {
		return u
	}
	return u + " — " + title
}

// waitUntil blocks until pred holds for the tab (checked under its
// lock), reporting true; or until timeout fires, reporting false. A
// closed tab or an ended ctx is an error. A nil timeout never fires.
func (t *tab) waitUntil(ctx context.Context, timeout <-chan time.Time, pred func() bool) (bool, error) {
	for {
		t.mu.Lock()
		if pred() {
			t.mu.Unlock()
			return true, nil
		}
		if t.closed {
			t.mu.Unlock()
			return false, failf(CodeNotFound, "The tab closed.")
		}
		ch := t.changed
		t.mu.Unlock()
		select {
		case <-ch:
		case <-timeout:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}
