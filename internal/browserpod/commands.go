package browserpod

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// navTimeout bounds navigate and wait; cmdTimeout every other
	// command.
	navTimeout = 45 * time.Second
	cmdTimeout = 20 * time.Second
	// navStartWait is how long an action that may navigate waits for
	// a navigation to begin.
	navStartWait = 5 * time.Second
	// idleWait is how long a loaded page gets to go network-idle.
	idleWait = 2 * time.Second
	// maxWait bounds web_wait.
	maxWait = 30 * time.Second
	// waitPoll is how often web_wait checks the page.
	waitPoll = 250 * time.Millisecond
	// maxScreenshotWidth is the widest screenshot sent to core.
	maxScreenshotWidth = 1280
)

// errCommandTimeout is the cause of a command context that ran out.
var errCommandTimeout = errors.New("command timed out")

// cmdError is a refused or failed command: Code for core, msg for the
// model.
type cmdError struct {
	code string
	msg  string
}

func (e *cmdError) Error() string { return e.msg }

func failf(code, format string, args ...any) error {
	return &cmdError{code: code, msg: fmt.Sprintf(format, args...)}
}

// result is a successful command's answer.
type result struct {
	text  string
	image *Image
}

func timeoutFor(name string) time.Duration {
	if name == CommandNavigate || name == CommandWait {
		return navTimeout
	}
	return cmdTimeout
}

// withTimeout is context.WithTimeout on the browser's clock; the
// context's cause is errCommandTimeout when it runs out.
func (b *browser) withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	timer := b.clock.NewTimer(d)
	go func() {
		select {
		case <-timer.C():
			cancel(errCommandTimeout)
		case <-ctx.Done():
		}
	}()
	// The timer stops before the command's result goes out, so a test
	// driving the clock never sees it armed after the answer.
	return ctx, func() {
		timer.Stop()
		cancel(context.Canceled)
	}
}

// runCommand executes one command message and returns its result
// message.
func (b *browser) runCommand(parent context.Context, msg Message) Message {
	d := timeoutFor(msg.Name)
	ctx, cancel := b.withTimeout(parent, d)
	defer cancel()

	var t *tab
	res, err := b.dispatch(ctx, msg, &t)
	out := Message{Type: TypeResult, ID: msg.ID}
	var notes []string
	if t != nil {
		notes = t.takeNotes()
	}
	if err != nil {
		var ce *cmdError
		switch {
		case errors.As(err, &ce):
			out.Code, out.Error = ce.code, ce.msg
		case errors.Is(context.Cause(ctx), errCommandTimeout):
			out.Code = CodeTimeout
			out.Error = fmt.Sprintf("The %s command ran out of time after %d s.", msg.Name, int(d/time.Second))
			if t != nil {
				out.Error += " The tab is at " + t.where() + "."
			}
		case parent.Err() != nil:
			out.Code, out.Error = CodeBrowser, "The browser is shutting down."
		default:
			out.Code, out.Error = CodeBrowser, "The browser refused: "+err.Error()
		}
		if len(notes) > 0 {
			out.Error += "\n" + strings.Join(notes, "\n")
		}
		return out
	}
	out.OK = true
	out.Text = res.text
	if len(notes) > 0 {
		out.Text += "\n" + strings.Join(notes, "\n")
	}
	out.Image = res.image
	return out
}

func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return failf(CodeBadArgs, "The arguments did not decode: %v.", err)
	}
	return nil
}

func need(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return failf(CodeBadArgs, "The %s argument is missing.", field)
	}
	return nil
}

// dispatch decodes a command and runs it on its tab, which it reports
// through tp so the caller can attach the tab's notes.
func (b *browser) dispatch(ctx context.Context, msg Message, tp **tab) (result, error) {
	if msg.Group == "" {
		return result{}, failf(CodeBadArgs, "The command names no group.")
	}
	if msg.Name == CommandTabs {
		var a TabsArgs
		if err := decodeArgs(msg.Args, &a); err != nil {
			return result{}, err
		}
		return b.cmdTabs(ctx, msg.Group, msg.Tab, a, tp)
	}
	// Validate before opening a tab for the command.
	var args any
	switch msg.Name {
	case CommandNavigate:
		var a NavigateArgs
		if err := decodeArgs(msg.Args, &a); err != nil {
			return result{}, err
		}
		if err := need("url", a.URL); err != nil {
			return result{}, err
		}
		if err := b.checkURL(a.URL); err != nil {
			return result{}, err
		}
		args = a
	case CommandSnapshot, CommandScreenshot, CommandBack:
	case CommandClick:
		var a ClickArgs
		if err := decodeArgs(msg.Args, &a); err != nil {
			return result{}, err
		}
		if err := need("ref", a.Ref); err != nil {
			return result{}, err
		}
		args = a
	case CommandType:
		var a TypeArgs
		if err := decodeArgs(msg.Args, &a); err != nil {
			return result{}, err
		}
		if err := need("ref", a.Ref); err != nil {
			return result{}, err
		}
		args = a
	case CommandSelect:
		var a SelectArgs
		if err := decodeArgs(msg.Args, &a); err != nil {
			return result{}, err
		}
		if err := need("ref", a.Ref); err != nil {
			return result{}, err
		}
		args = a
	case CommandPress:
		var a PressArgs
		if err := decodeArgs(msg.Args, &a); err != nil {
			return result{}, err
		}
		if err := need("key", a.Key); err != nil {
			return result{}, err
		}
		if _, err := parseChord(a.Key); err != nil {
			return result{}, err
		}
		args = a
	case CommandScroll:
		var a ScrollArgs
		if err := decodeArgs(msg.Args, &a); err != nil {
			return result{}, err
		}
		switch a.Direction {
		case "up", "down", "left", "right":
		case "":
			return result{}, failf(CodeBadArgs, "The direction argument is missing: up, down, left or right.")
		default:
			return result{}, failf(CodeBadArgs, "The direction %q is not up, down, left or right.", a.Direction)
		}
		if a.Amount < 0 || a.Amount > 20 {
			return result{}, failf(CodeBadArgs, "The amount must be between 0 and 20 viewports.")
		}
		args = a
	case CommandWait:
		var a WaitArgs
		if err := decodeArgs(msg.Args, &a); err != nil {
			return result{}, err
		}
		switch a.For {
		case WaitText, WaitRef:
			if err := need("value", a.Value); err != nil {
				return result{}, err
			}
		case WaitNavigation:
		case "":
			return result{}, failf(CodeBadArgs, "The for argument is missing: text, ref or navigation.")
		default:
			return result{}, failf(CodeBadArgs, "The for argument %q is not text, ref or navigation.", a.For)
		}
		if a.TimeoutMS < 0 {
			return result{}, failf(CodeBadArgs, "The timeout_ms argument cannot be negative.")
		}
		args = a
	case CommandEval:
		var a EvalArgs
		if err := decodeArgs(msg.Args, &a); err != nil {
			return result{}, err
		}
		if err := need("script", a.Script); err != nil {
			return result{}, err
		}
		args = a
	case CommandUpload:
		var a UploadArgs
		if err := decodeArgs(msg.Args, &a); err != nil {
			return result{}, err
		}
		if err := need("ref", a.Ref); err != nil {
			return result{}, err
		}
		if err := need("file", a.File); err != nil {
			return result{}, err
		}
		args = a
	default:
		return result{}, failf(CodeBadArgs, "There is no command %q.", msg.Name)
	}

	t, err := b.lookupTab(ctx, msg.Group, msg.Tab, true)
	if err != nil {
		return result{}, err
	}
	*tp = t
	b.bringToFront(ctx, t)
	switch a := args.(type) {
	case NavigateArgs:
		return b.cmdNavigate(ctx, t, a)
	case ClickArgs:
		return b.cmdClick(ctx, t, a)
	case TypeArgs:
		return b.cmdType(ctx, t, a)
	case SelectArgs:
		return b.cmdSelect(ctx, t, a)
	case PressArgs:
		return b.cmdPress(ctx, t, a)
	case ScrollArgs:
		return b.cmdScroll(ctx, t, a)
	case WaitArgs:
		return b.cmdWait(ctx, t, a)
	case EvalArgs:
		return b.cmdEval(ctx, t, a)
	case UploadArgs:
		return b.cmdUpload(ctx, t, msg.Group, a)
	}
	switch msg.Name {
	case CommandSnapshot:
		return b.cmdSnapshot(ctx, t)
	case CommandScreenshot:
		return b.cmdScreenshot(ctx, t)
	default:
		return b.cmdBack(ctx, t)
	}
}

// checkURL refuses what a tab may not load: anything but http(s), and
// in a site session a host off the site.
func (b *browser) checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return failf(CodeBadArgs, "%q is not a full URL; include https://.", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return failf(CodeBadArgs, "Only http and https URLs can be opened, not %s:.", u.Scheme)
	}
	if !b.allowed(raw) {
		return failf(CodeOffSite, "Blocked: %s is off this session's site.", raw)
	}
	return nil
}

// settle is the wait after an action that may navigate: up to
// navStartWait for a navigation to begin and, when one did, until it
// loads. A tab the action opened ends the wait too.
func (b *browser) settle(ctx context.Context, t *tab, m mark) error {
	timer := b.clock.NewTimer(navStartWait)
	defer timer.Stop()
	started, err := t.waitUntil(ctx, timer.C(), func() bool {
		return t.starts > m.starts || t.navs > m.navs || t.navigated > m.navigated || t.popups > m.popups
	})
	if err != nil || !started {
		return err
	}
	t.mu.Lock()
	navigating := t.starts > m.starts || t.navs > m.navs
	t.mu.Unlock()
	if !navigating {
		return nil
	}
	return b.waitLoad(ctx, t, m)
}

// waitLoad waits for the navigation begun since m to load, then
// briefly for the network to go idle.
func (b *browser) waitLoad(ctx context.Context, t *tab, m mark) error {
	if _, err := t.waitUntil(ctx, nil, func() bool {
		return t.navigated > m.navigated || (t.starts > m.starts && !t.loading && !t.pendingNav)
	}); err != nil {
		return err
	}
	timer := b.clock.NewTimer(idleWait)
	defer timer.Stop()
	_, err := t.waitUntil(ctx, timer.C(), func() bool {
		return t.loaderID == "" || t.idleLoader == t.loaderID
	})
	return err
}

func (b *browser) cmdNavigate(ctx context.Context, t *tab, a NavigateArgs) (result, error) {
	m := t.mark()
	raw, err := b.conn.Call(ctx, t.session, "Page.navigate", map[string]any{"url": a.URL})
	if err != nil {
		return result{}, err
	}
	var res struct {
		LoaderID  string `json:"loaderId"`
		ErrorText string `json:"errorText"`
	}
	_ = json.Unmarshal(raw, &res)
	switch {
	case res.ErrorText == "net::ERR_ABORTED":
		return result{text: fmt.Sprintf("%s did not load as a page (it may have started a download); the tab is at %s", a.URL, t.where())}, nil
	case res.ErrorText != "":
		return result{}, failf(CodeBrowser, "Could not load %s: %s.", a.URL, res.ErrorText)
	}
	if res.LoaderID != "" {
		if err := b.waitLoad(ctx, t, m); err != nil {
			return result{}, err
		}
	}
	t.mu.Lock()
	u, title := t.url, t.title
	t.mu.Unlock()
	if title == "" {
		title = "(untitled)"
	}
	return result{text: fmt.Sprintf("Loaded %s — %s", title, u)}, nil
}

func (b *browser) cmdClick(ctx context.Context, t *tab, a ClickArgs) (result, error) {
	obj, info, err := b.resolveRef(ctx, t, a.Ref)
	if err != nil {
		return result{}, err
	}
	x, y, err := b.centre(ctx, t, obj)
	if err != nil {
		return result{}, err
	}
	m := t.mark()
	if err := b.mouseClick(ctx, t, x, y); err != nil {
		return result{}, err
	}
	if err := b.settle(ctx, t, m); err != nil {
		return result{}, err
	}
	return result{text: fmt.Sprintf("Clicked %s %q; now at %s", info.Role, info.Name, t.where())}, nil
}

// centre scrolls an element into view and returns its centre in
// viewport coordinates.
func (b *browser) centre(ctx context.Context, t *tab, obj string) (float64, float64, error) {
	var box struct {
		X, Y, W, H float64
	}
	err := b.callOn(ctx, t, obj, `function() {
		this.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
		let r = this.getBoundingClientRect();
		if (r.width === 0 || r.height === 0) {
			const rs = this.getClientRects();
			if (rs.length) r = rs[0];
		}
		return {X: r.left + r.width / 2, Y: r.top + r.height / 2, W: r.width, H: r.height};
	}`, &box)
	if err != nil {
		return 0, 0, err
	}
	if box.W == 0 && box.H == 0 {
		return 0, 0, failf(CodeNotFound, "That element is not visible on the page; take a new snapshot.")
	}
	return box.X, box.Y, nil
}

func (b *browser) mouseClick(ctx context.Context, t *tab, x, y float64) error {
	for _, ev := range []map[string]any{
		{"type": "mouseMoved", "x": x, "y": y},
		{"type": "mousePressed", "x": x, "y": y, "button": "left", "buttons": 1, "clickCount": 1},
		{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1},
	} {
		if _, err := b.conn.Call(ctx, t.session, "Input.dispatchMouseEvent", ev); err != nil {
			return err
		}
	}
	return nil
}

func (b *browser) cmdType(ctx context.Context, t *tab, a TypeArgs) (result, error) {
	obj, info, err := b.resolveRef(ctx, t, a.Ref)
	if err != nil {
		return result{}, err
	}
	var focused bool
	if err := b.callOn(ctx, t, obj, `function() {
		this.scrollIntoView({block: 'center', behavior: 'instant'});
		this.focus();
		if (this.tagName === 'INPUT' || this.tagName === 'TEXTAREA') {
			try { this.select(); } catch (e) {}
		} else if (this.isContentEditable) {
			const r = document.createRange();
			r.selectNodeContents(this);
			const s = getSelection();
			s.removeAllRanges();
			s.addRange(r);
		}
		return document.activeElement === this || this.contains(document.activeElement);
	}`, &focused); err != nil {
		return result{}, err
	}
	if !focused {
		return result{}, failf(CodeNotFound, "%s %q did not take focus; it may not accept text.", info.Role, info.Name)
	}
	if a.Text == "" {
		if err := b.pressKey(ctx, t, chord{key: namedKeys["Backspace"]}); err != nil {
			return result{}, err
		}
	} else if _, err := b.conn.Call(ctx, t.session, "Input.insertText", map[string]any{"text": a.Text}); err != nil {
		return result{}, err
	}
	text := fmt.Sprintf("Typed %d characters into %s %q", utf8.RuneCountInString(a.Text), info.Role, info.Name)
	if a.Submit {
		m := t.mark()
		if err := b.pressKey(ctx, t, chord{key: namedKeys["Enter"]}); err != nil {
			return result{}, err
		}
		if err := b.settle(ctx, t, m); err != nil {
			return result{}, err
		}
		text += " and pressed Enter; now at " + t.where()
	}
	return result{text: text}, nil
}

func (b *browser) cmdSelect(ctx context.Context, t *tab, a SelectArgs) (result, error) {
	obj, info, err := b.resolveRef(ctx, t, a.Ref)
	if err != nil {
		return result{}, err
	}
	var res struct {
		NotSelect bool     `json:"notSelect"`
		Missing   bool     `json:"missing"`
		Options   []string `json:"options"`
		Selected  []string `json:"selected"`
	}
	if err := b.callOn(ctx, t, obj, `function(v) {
		if (this.tagName !== 'SELECT') return {notSelect: true};
		const opts = Array.from(this.options);
		const label = o => (o.label || o.text || '').trim();
		const o = opts.find(o => o.value === v) || opts.find(o => label(o) === v);
		if (!o) return {missing: true, options: opts.map(o => label(o) === o.value ? o.value : label(o) + ' (' + o.value + ')')};
		if (this.multiple) o.selected = true; else this.value = o.value;
		this.dispatchEvent(new Event('input', {bubbles: true}));
		this.dispatchEvent(new Event('change', {bubbles: true}));
		return {selected: Array.from(this.selectedOptions).map(label)};
	}`, &res, a.Value); err != nil {
		return result{}, err
	}
	switch {
	case res.NotSelect:
		return result{}, failf(CodeBadArgs, "%s %q is not a select element; click it instead.", info.Role, info.Name)
	case res.Missing:
		return result{}, failf(CodeNotFound, "%s %q has no option %q. Its options: %s.", info.Role, info.Name, a.Value, cut(strings.Join(res.Options, ", "), 1000))
	}
	return result{text: fmt.Sprintf("Selected %s in %s %q", quoteAll(res.Selected), info.Role, info.Name)}, nil
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = strconv.Quote(s)
	}
	if len(q) == 0 {
		return "nothing"
	}
	return strings.Join(q, ", ")
}

func (b *browser) cmdPress(ctx context.Context, t *tab, a PressArgs) (result, error) {
	c, err := parseChord(a.Key)
	if err != nil {
		return result{}, err
	}
	m := t.mark()
	if err := b.pressKey(ctx, t, c); err != nil {
		return result{}, err
	}
	if err := b.settle(ctx, t, m); err != nil {
		return result{}, err
	}
	return result{text: fmt.Sprintf("Pressed %s; now at %s", a.Key, t.where())}, nil
}

func (b *browser) cmdScroll(ctx context.Context, t *tab, a ScrollArgs) (result, error) {
	amount := a.Amount
	if amount == 0 {
		amount = 1
	}
	var vp struct{ W, H float64 }
	if err := b.evalValue(ctx, t, `({W: innerWidth, H: innerHeight})`, &vp); err != nil {
		return result{}, err
	}
	x, y := vp.W/2, vp.H/2
	var obj string
	if a.Ref != "" {
		o, _, err := b.resolveRef(ctx, t, a.Ref)
		if err != nil {
			return result{}, err
		}
		obj = o
		if x, y, err = b.centre(ctx, t, obj); err != nil {
			return result{}, err
		}
	}
	dx, dy := 0.0, 0.0
	switch a.Direction {
	case "down":
		dy = amount * vp.H
	case "up":
		dy = -amount * vp.H
	case "right":
		dx = amount * vp.W
	case "left":
		dx = -amount * vp.W
	}
	if _, err := b.conn.Call(ctx, t.session, "Input.dispatchMouseEvent", map[string]any{
		"type": "mouseWheel", "x": x, "y": y, "deltaX": dx, "deltaY": dy,
	}); err != nil {
		return result{}, err
	}
	var pos struct{ X, Y, W, H float64 }
	const after = `new Promise(r => requestAnimationFrame(() => requestAnimationFrame(() => r(%s))))`
	if obj != "" {
		err := b.callOn(ctx, t, obj, `function() { const e = this; return `+fmt.Sprintf(after, `({X: e.scrollLeft, Y: e.scrollTop, W: e.scrollWidth, H: e.scrollHeight})`)+`; }`, &pos)
		if err != nil {
			return result{}, err
		}
		return result{text: fmt.Sprintf("Scrolled %s; the element is at %.0f of %.0f px", a.Direction, pos.Y, pos.H)}, nil
	}
	if err := b.evalValue(ctx, t, fmt.Sprintf(after, `({X: scrollX, Y: scrollY, W: document.documentElement.scrollWidth, H: document.documentElement.scrollHeight})`), &pos); err != nil {
		return result{}, err
	}
	if a.Direction == "left" || a.Direction == "right" {
		return result{text: fmt.Sprintf("Scrolled %s; the page is at %.0f of %.0f px across", a.Direction, pos.X, pos.W)}, nil
	}
	return result{text: fmt.Sprintf("Scrolled %s; the page is at %.0f of %.0f px", a.Direction, pos.Y, pos.H)}, nil
}

func (b *browser) cmdBack(ctx context.Context, t *tab) (result, error) {
	raw, err := b.conn.Call(ctx, t.session, "Page.getNavigationHistory", nil)
	if err != nil {
		return result{}, err
	}
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID  int    `json:"id"`
			URL string `json:"url"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return result{}, err
	}
	if h.CurrentIndex <= 0 || h.CurrentIndex > len(h.Entries) {
		return result{text: "There is nothing to go back to; the tab is at " + t.where()}, nil
	}
	m := t.mark()
	if _, err := b.conn.Call(ctx, t.session, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[h.CurrentIndex-1].ID}); err != nil {
		return result{}, err
	}
	// A page restored from the back-forward cache commits after it
	// stops loading, so only the navigation itself ends the wait.
	if _, err := t.waitUntil(ctx, nil, func() bool { return t.navigated > m.navigated }); err != nil {
		return result{}, err
	}
	return result{text: "Went back; now at " + t.where()}, nil
}

func (b *browser) cmdTabs(ctx context.Context, groupName, tabID string, a TabsArgs, tp **tab) (result, error) {
	switch a.Action {
	case TabsList, "":
		return result{text: b.listTabs(groupName)}, nil
	case TabsSwitch:
		id := a.Tab
		if id == "" {
			id = tabID
		}
		if err := need("tab", id); err != nil {
			return result{}, err
		}
		t, err := b.lookupTab(ctx, groupName, id, false)
		if err != nil {
			return result{}, err
		}
		*tp = t
		b.setCurrent(t)
		b.bringToFront(ctx, t)
		return result{text: fmt.Sprintf("Switched to %s: %s", t.id, t.where())}, nil
	case TabsOpen:
		if a.URL != "" {
			if err := b.checkURL(a.URL); err != nil {
				return result{}, err
			}
		}
		t, err := b.createTab(ctx, groupName)
		if err != nil {
			return result{}, err
		}
		*tp = t
		if a.URL == "" {
			return result{text: fmt.Sprintf("Opened %s at about:blank", t.id)}, nil
		}
		res, err := b.cmdNavigate(ctx, t, NavigateArgs{URL: a.URL})
		if err != nil {
			return result{}, err
		}
		return result{text: fmt.Sprintf("Opened %s. %s", t.id, res.text)}, nil
	case TabsClose:
		id := a.Tab
		if id == "" {
			id = tabID
		}
		t, err := b.lookupTab(ctx, groupName, id, false)
		if err != nil {
			return result{}, err
		}
		b.closeTab(ctx, t)
		_, cur := b.groupTabs(groupName)
		if cur == nil {
			return result{text: fmt.Sprintf("Closed %s; this task has no tab open now", t.id)}, nil
		}
		*tp = cur
		return result{text: fmt.Sprintf("Closed %s; current tab is %s: %s", t.id, cur.id, cur.where())}, nil
	}
	return result{}, failf(CodeBadArgs, "The action %q is not list, switch, open or close.", a.Action)
}

func (b *browser) listTabs(groupName string) string {
	tabs, cur := b.groupTabs(groupName)
	if len(tabs) == 0 {
		return "This task has no tab open."
	}
	var sb strings.Builder
	for i, t := range tabs {
		if i > 0 {
			sb.WriteByte('\n')
		}
		marker := " "
		if t == cur {
			marker = "*"
		}
		fmt.Fprintf(&sb, "%s %s %s", marker, t.id, t.where())
	}
	return sb.String()
}

func (b *browser) cmdWait(ctx context.Context, t *tab, a WaitArgs) (result, error) {
	d := time.Duration(a.TimeoutMS) * time.Millisecond
	if d <= 0 || d > maxWait {
		d = maxWait
	}
	timer := b.clock.NewTimer(d)
	defer timer.Stop()
	timedOut := func() (result, error) {
		return result{}, failf(CodeTimeout, "Waited %s and it did not happen; the page shows %s.", d, t.where())
	}
	switch a.For {
	case WaitNavigation:
		m := t.mark()
		ok, err := t.waitUntil(ctx, timer.C(), func() bool { return t.navigated > m.navigated })
		if err != nil {
			return result{}, err
		}
		if !ok {
			return timedOut()
		}
		return result{text: "Navigated; now at " + t.where()}, nil
	}
	ticker := b.clock.NewTicker(waitPoll)
	defer ticker.Stop()
	for {
		var found bool
		var err error
		if a.For == WaitText {
			err = b.evalValue(ctx, t, `document.body ? document.body.innerText.includes(`+jsString(a.Value)+`) : false`, &found)
		} else {
			_, _, err = b.resolveRef(ctx, t, a.Value)
			found = err == nil
			var ce *cmdError
			if errors.As(err, &ce) && ce.code == CodeNotFound {
				err = nil
			}
		}
		if err != nil && ctx.Err() != nil {
			return result{}, ctx.Err()
		}
		if found {
			if a.For == WaitText {
				return result{text: fmt.Sprintf("The page shows %q; it is at %s", a.Value, t.where())}, nil
			}
			return result{text: fmt.Sprintf("%s is on the page; it is at %s", a.Value, t.where())}, nil
		}
		select {
		case <-ticker.C():
		case <-timer.C():
			return timedOut()
		case <-ctx.Done():
			return result{}, ctx.Err()
		}
	}
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (b *browser) cmdEval(ctx context.Context, t *tab, a EvalArgs) (result, error) {
	obj, err := b.evaluate(ctx, t, a.Script, true)
	if err != nil {
		return result{}, err
	}
	var text string
	switch {
	case obj.Type == "undefined":
		text = "undefined"
	case obj.UnserializableValue != "":
		text = obj.UnserializableValue
	case len(obj.Value) > 0:
		text = string(obj.Value)
	default:
		text = obj.Description
	}
	return result{text: cut(text, MaxEvalChars)}, nil
}

func (b *browser) cmdUpload(ctx context.Context, t *tab, groupName string, a UploadArgs) (result, error) {
	name := cleanFileName(a.File)
	path := filepath.Join(b.dir, "files", cleanFileName(groupName), name)
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return result{}, failf(CodeNotFound, "There is no task file %q to upload.", a.File)
	}
	obj, info, err := b.resolveRef(ctx, t, a.Ref)
	if err != nil {
		return result{}, err
	}
	var isFile bool
	if err := b.callOn(ctx, t, obj, `function() { return this.tagName === 'INPUT' && this.type === 'file'; }`, &isFile); err != nil {
		return result{}, err
	}
	if !isFile {
		return result{}, failf(CodeBadArgs, "%s %q is not a file input.", info.Role, info.Name)
	}
	if _, err := b.conn.Call(ctx, t.session, "DOM.setFileInputFiles", map[string]any{"files": []string{path}, "objectId": obj}); err != nil {
		return result{}, err
	}
	return result{text: fmt.Sprintf("Set %s %q to %s", info.Role, info.Name, name)}, nil
}

func (b *browser) cmdScreenshot(ctx context.Context, t *tab) (result, error) {
	data, err := b.capture(ctx, t, nil)
	if err != nil {
		return result{}, err
	}
	if cfg, err := png.DecodeConfig(bytes.NewReader(data)); err == nil && cfg.Width > maxScreenshotWidth {
		var vp struct{ X, Y, W, H float64 }
		if err := b.evalValue(ctx, t, `({X: scrollX, Y: scrollY, W: innerWidth, H: innerHeight})`, &vp); err == nil && vp.W > 0 {
			scale := float64(maxScreenshotWidth) / float64(cfg.Width)
			if smaller, err := b.capture(ctx, t, map[string]any{"x": vp.X, "y": vp.Y, "width": vp.W, "height": vp.H, "scale": scale}); err == nil {
				data = smaller
			}
		}
	}
	t.mu.Lock()
	u := t.url
	t.mu.Unlock()
	return result{text: "Screenshot of " + u, image: &Image{MIME: "image/png", Data: data}}, nil
}

func (b *browser) capture(ctx context.Context, t *tab, clip map[string]any) ([]byte, error) {
	params := map[string]any{"format": "png"}
	if clip != nil {
		params["clip"] = clip
	}
	raw, err := b.conn.Call(ctx, t.session, "Page.captureScreenshot", params)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.Data)
}

// cut shortens s to n runes, ending it with an ellipsis.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
