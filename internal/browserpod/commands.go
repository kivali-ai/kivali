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
	// a navigation to begin: Chromium reports one within tens of
	// milliseconds of the action, and every click, key and keystroke
	// that does not navigate pays this wait in full.
	navStartWait = time.Second
	// idleWait is how long a loaded page gets to go network-idle.
	idleWait = 2 * time.Second
	// downloadWait is how long an aborted navigation waits for the
	// download it may have become to announce itself.
	downloadWait = time.Second
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
				out.Error = withPage(out.Error, t.pageLine())
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
	b.holdGroup(msg.Group, msg.Site)
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
		if err := b.checkURL(msg.Group, a.URL); err != nil {
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
	if wantsDialogAccepted(args) {
		defer b.acceptDialogs(msg.Group)()
	}
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

// checkURL refuses what a group's tab may not load: anything but
// http(s), and a host off the site the session or the group is held
// to, whether or not the group is unlocked: an unlock is for a person's
// sign-in, not for commands.
func (b *browser) checkURL(groupName, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return failf(CodeBadArgs, "%q is not a full URL; include https://.", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return failf(CodeBadArgs, "Only http and https URLs can be opened, not %s:.", u.Scheme)
	}
	if site := b.heldSite(groupName); site != "" && !onSiteURL(site, raw) {
		return failf(CodeOffSite, "Blocked: %s is off %s.", raw, b.offSiteName())
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
	// A navigation that becomes a download is aborted as a page load;
	// so is one to a response with nothing to show, or one another
	// navigation replaced. The download's event, on this tab, tells
	// them apart.
	downloaded := make(chan struct{}, 1)
	off := b.conn.On("", "Browser.downloadWillBegin", func(raw json.RawMessage) {
		var ev struct {
			FrameID string `json:"frameId"`
		}
		if json.Unmarshal(raw, &ev) == nil && ev.FrameID == t.targetID {
			select {
			case downloaded <- struct{}{}:
			default:
			}
		}
	})
	defer off()
	stops := t.stopCount()
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
	case res.ErrorText == "net::ERR_ABORTED" && t.stopCount() > stops:
		// A site session stopped the request (or a redirect hop of it)
		// before it was sent; the tab's note names the URL.
		return result{text: withPage(fmt.Sprintf("The navigation to %s was stopped before a page loaded: it led off this session's site.", a.URL), t.pageLine())}, nil
	case res.ErrorText == "net::ERR_ABORTED":
		timer := b.clock.NewTimer(downloadWait)
		defer timer.Stop()
		select {
		case <-downloaded:
			return result{text: withPage(fmt.Sprintf("%s started a download instead of loading as a page.", a.URL), t.pageLine())}, nil
		case <-timer.C():
			return result{text: withPage(fmt.Sprintf("The navigation to %s was aborted before a page loaded (the server sent nothing to show, or another navigation replaced it).", a.URL), t.pageLine())}, nil
		case <-ctx.Done():
			return result{}, ctx.Err()
		}
	case res.ErrorText != "":
		return result{}, failf(CodeBrowser, "Could not load %s: %s.", a.URL, res.ErrorText)
	}
	if res.LoaderID != "" {
		if err := b.waitLoad(ctx, t, m); err != nil {
			return result{}, err
		}
	}
	return result{text: withPage("Loaded.", t.pageLine())}, nil
}

func (b *browser) cmdClick(ctx context.Context, t *tab, a ClickArgs) (result, error) {
	obj, info, err := b.resolveRef(ctx, t, a.Ref)
	if err != nil {
		return result{}, err
	}
	x, y, err := b.clickPoint(ctx, t, obj, info)
	if err != nil {
		return result{}, err
	}
	if err := b.checkHit(ctx, t, obj, x, y); err != nil {
		return result{}, err
	}
	m := t.mark()
	if err := b.mouseClick(ctx, t, x, y); err != nil {
		return result{}, err
	}
	if err := b.settle(ctx, t, m); err != nil {
		return result{}, err
	}
	return result{text: withPage(fmt.Sprintf("Clicked %s.", a.Ref), elementLine(info), t.pageLine())}, nil
}

// clickPoint scrolls an element into view and returns the point a click
// reaches it at: the centre of its box, or of its label's when it is a
// form control hidden in place (the label passes the click on). When
// another element would take the click there (a dialog's backdrop, a
// cookie banner, a sticky header) the error names it, so the model can
// dismiss it instead of reading that the click happened.
func (b *browser) clickPoint(ctx context.Context, t *tab, obj string, info refInfo) (float64, float64, error) {
	var res struct {
		X, Y, W, H float64
		Covered    string `json:"covered"`
	}
	err := b.callOn(ctx, t, obj, `function() {
		const box = el => {
			let r = el.getBoundingClientRect();
			if (r.width === 0 || r.height === 0) {
				const rs = el.getClientRects();
				if (rs.length) r = rs[0];
			}
			return r;
		};
		// A form control that is not drawn is clicked through its label.
		let target = this, r = box(this);
		const form = ['INPUT', 'SELECT', 'TEXTAREA'].includes(this.tagName);
		if (form && this.labels && this.labels.length && (r.width === 0 || r.height === 0 || getComputedStyle(this).opacity === '0' || getComputedStyle(this).display === 'none')) {
			for (const lb of this.labels) {
				const lr = box(lb);
				if (lr.width && lr.height) { target = lb; r = lr; break; }
			}
		}
		target.scrollIntoView({block: 'center', inline: 'center', behavior: 'instant'});
		r = box(target);
		const x = r.left + r.width / 2, y = r.top + r.height / 2;
		const out = {X: x, Y: y, W: r.width, H: r.height, covered: ''};
		if (r.width === 0 && r.height === 0) return out;
		let hit = document.elementFromPoint(x, y);
		while (hit && hit.shadowRoot) {
			const inner = hit.shadowRoot.elementFromPoint(x, y);
			if (!inner || inner === hit) break;
			hit = inner;
		}
		if (!hit) { out.covered = 'the edge of the viewport'; return out; }
		const within = (n, a) => {
			for (let e = n; e; e = e.parentElement || (e.parentNode && e.parentNode.host)) if (e === a) return true;
			return false;
		};
		// The click lands on the element, inside it, on a label for it,
		// or on an ancestor that takes its pointer events.
		if (within(hit, target) || within(hit, this) || hit.contains(target)) return out;
		const lb = hit.closest && hit.closest('label');
		if (lb && (lb.control === this || lb.contains(this))) return out;
		const describe = e => {
			const text = s => (s || '').replace(/\s+/g, ' ').trim();
			for (let n = e, i = 0; n && n !== document.body && i < 6; n = n.parentElement, i++) {
				const role = n.getAttribute('role') || {DIALOG: 'dialog', A: 'link', BUTTON: 'button'}[n.tagName] || '';
				const t = text(n.getAttribute('aria-label')) || text(n.innerText);
				if (t || role) return (role || n.tagName.toLowerCase()) + (t ? ' ' + JSON.stringify(t.length > 80 ? t.slice(0, 79) + '…' : t) : '');
			}
			const cls = text(e.getAttribute('class'));
			return 'a ' + e.tagName.toLowerCase() + ' with no text' + (cls ? ' (class ' + JSON.stringify(cls.length > 60 ? cls.slice(0, 59) + '…' : cls) + ')' : '');
		};
		out.covered = describe(hit);
		return out;
	}`, &res)
	if err != nil {
		return 0, 0, err
	}
	if res.W == 0 && res.H == 0 {
		return 0, 0, failf(CodeNotFound, "That element is not visible on the page; take a new snapshot.")
	}
	if res.Covered != "" {
		return 0, 0, failPage(CodeNotFound, "The element is covered by another, which would take the click; dismiss or close that first, or scroll, then take a new snapshot.", elementLine(info), "Covered by: "+res.Covered)
	}
	return res.X, res.Y, nil
}

// The hold on embedded frames. A held tab's page may embed frames from
// other sites, and they load as the page asks: the pod blocks no
// subresource. What it stops is the task's own actions landing in one:
// a click whose point an off-site frame covers, and text or keys while
// the focus is inside one, which a page can move there at any time.

// frameAtJS reports where the point (x, y) lands, looking through open
// shadow roots: "" on a page element, the frame's URL on an embedded
// frame the page can read, "cross:<src>" on one it cannot (another
// origin). A frame inside the element itself counts like any other.
const frameAtJS = `function(x, y) { /* kivali-frame */
	let h = document.elementFromPoint(x, y);
	for (let i = 0; h && h.shadowRoot && i < 32; i++) {
		const inner = h.shadowRoot.elementFromPoint(x, y);
		if (!inner || inner === h) break;
		h = inner;
	}
	if (!h || (h.tagName !== 'IFRAME' && h.tagName !== 'FRAME')) return '';
	try { const d = h.contentDocument; return d ? d.location.href : 'cross:' + (h.src || ''); } catch (e) { return 'cross:' + (h.src || ''); }
}`

// focusJS reports where keyboard input would land, looking through
// open shadow roots: "" in the page itself, the URL of the innermost
// embedded frame the focus is in when the page can read it,
// "cross:<src>" when it cannot.
const focusJS = `/* kivali-frame */ (() => {
	let d = document, href = '';
	for (let i = 0; i < 8; i++) {
		let el = d.activeElement;
		for (let j = 0; el && el.shadowRoot && el.shadowRoot.activeElement && j < 32; j++) el = el.shadowRoot.activeElement;
		if (!el || (el.tagName !== 'IFRAME' && el.tagName !== 'FRAME')) return href;
		let inner = null;
		try { inner = el.contentDocument; } catch (e) { inner = null; }
		if (!inner) return 'cross:' + (el.src || '');
		d = inner;
		href = d.location.href;
	}
	return 'cross:';
})()`

// insertTextJS puts text into the element, replacing what is selected
// in it (all of it, after cmdType focuses it), as typing would: through
// the editing command when the focus is in the element, so the page
// hears the input as typed, else by setting its value and raising the
// events a page listens for. It never reaches another document.
const insertTextJS = `function(text) { /* kivali-insert */
	const here = () => document.activeElement === this || this.contains(document.activeElement) || (this.shadowRoot && this.shadowRoot.activeElement);
	let ok = false;
	if (here()) {
		try { ok = document.execCommand(text === '' ? 'delete' : 'insertText', false, text); } catch (e) { ok = false; }
	}
	if (ok) return true;
	if (this.tagName === 'INPUT' || this.tagName === 'TEXTAREA') {
		const proto = this.tagName === 'INPUT' ? HTMLInputElement.prototype : HTMLTextAreaElement.prototype;
		Object.getOwnPropertyDescriptor(proto, 'value').set.call(this, text);
	} else if (this.isContentEditable) {
		this.textContent = text;
	} else {
		return false;
	}
	this.dispatchEvent(new InputEvent('input', {bubbles: true, inputType: 'insertText', data: text}));
	this.dispatchEvent(new Event('change', {bubbles: true}));
	return true;
}`

// frameRefusal is the refusal for an action that would land in an
// embedded frame off the held site, or nil.
func (b *browser) frameRefusal(group, where, action string) error {
	if where == "" {
		return nil
	}
	if src, cross := strings.CutPrefix(where, "cross:"); cross || !b.allowed(group, where) {
		if !cross {
			src = where
		}
		return failPage(CodeOffSite, fmt.Sprintf("Blocked: %s would land in an embedded frame from another site, off the site this task is held to; nothing was sent. Use a field on the page itself.", action), "Frame: "+src)
	}
	return nil
}

// checkHit refuses a click in a held tab whose point an embedded frame
// off the site covers.
func (b *browser) checkHit(ctx context.Context, t *tab, obj string, x, y float64) error {
	if b.heldSite(t.group) == "" {
		return nil
	}
	var where string
	if err := b.callOn(ctx, t, obj, frameAtJS, &where, x, y); err != nil {
		return err
	}
	return b.frameRefusal(t.group, where, "the click")
}

// checkFocus refuses typing or a key in a held tab while the focus is
// inside an embedded frame off the site.
func (b *browser) checkFocus(ctx context.Context, t *tab) error {
	if b.heldSite(t.group) == "" {
		return nil
	}
	var where string
	if err := b.evalValue(ctx, t, focusJS, &where); err != nil {
		return err
	}
	return b.frameRefusal(t.group, where, "the input")
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

// valueFormats is what the inputs that take a value rather than
// keystrokes expect.
var valueFormats = map[string]string{
	"date": "yyyy-mm-dd", "time": "hh:mm", "month": "yyyy-mm", "week": "yyyy-Www",
	"datetime-local": "yyyy-mm-ddThh:mm", "color": "#rrggbb", "range": "a number",
}

func (b *browser) cmdType(ctx context.Context, t *tab, a TypeArgs) (result, error) {
	obj, info, err := b.resolveRef(ctx, t, a.Ref)
	if err != nil {
		return result{}, err
	}
	var f struct {
		Refuses  string `json:"refuses"`
		Focused  bool   `json:"focused"`
		Direct   bool   `json:"direct"`
		Password bool   `json:"password"`
		Type     string `json:"type"`
	}
	if err := b.callOn(ctx, t, obj, `function(formats) {
		const tag = this.tagName, type = tag === 'INPUT' ? this.type : '';
		if (tag === 'SELECT') return {refuses: 'select'};
		if (tag === 'INPUT' && ['checkbox', 'radio', 'submit', 'button', 'reset', 'image', 'file', 'hidden'].includes(type)) return {refuses: type};
		if (this.readOnly) return {refuses: 'readonly'};
		this.scrollIntoView({block: 'center', behavior: 'instant'});
		this.focus();
		const direct = type in formats;
		if (!direct && (tag === 'INPUT' || tag === 'TEXTAREA')) {
			try { this.select(); } catch (e) {}
		} else if (this.isContentEditable) {
			const r = document.createRange();
			r.selectNodeContents(this);
			const s = getSelection();
			s.removeAllRanges();
			s.addRange(r);
		}
		return {focused: document.activeElement === this || this.contains(document.activeElement), direct, password: type === 'password', type};
	}`, &f, valueFormats); err != nil {
		return result{}, err
	}
	switch f.Refuses {
	case "":
	case "select":
		return result{}, failPage(CodeBadArgs, a.Ref+" is a select; choose an option with web_select.", elementLine(info))
	case "file":
		return result{}, failPage(CodeBadArgs, a.Ref+" is a file input; attach a file with web_upload.", elementLine(info))
	case "readonly":
		return result{}, failPage(CodeBadArgs, a.Ref+" is read-only; it takes no text.", elementLine(info))
	default:
		return result{}, failPage(CodeBadArgs, a.Ref+" takes no text; click it instead.", elementLine(info))
	}
	if !f.Focused {
		return result{}, failPage(CodeNotFound, a.Ref+" did not take focus; it may not accept text.", elementLine(info))
	}
	var held struct {
		Value     string `json:"value"`
		MaxLength int    `json:"maxLength"`
	}
	onHeldSite := b.heldSite(t.group) != ""
	switch {
	case f.Direct:
		// These inputs ignore keystrokes: the value is set as a whole,
		// and the page hears the events typing would have raised.
		if err := b.callOn(ctx, t, obj, `function(v) {
			this.value = v;
			if (this.value === v) {
				this.dispatchEvent(new Event('input', {bubbles: true}));
				this.dispatchEvent(new Event('change', {bubbles: true}));
			}
			return {value: this.value};
		}`, &held, a.Text); err != nil {
			return result{}, err
		}
		if held.Value != a.Text {
			return result{}, failPage(CodeBadArgs, fmt.Sprintf("%s did not take %q; a %s input takes %s.", a.Ref, a.Text, f.Type, valueFormats[f.Type]), elementLine(info))
		}
	case onHeldSite:
		// Into the element itself, in its own document: whatever the
		// page does with the focus, the text cannot land in a frame.
		var took bool
		if err := b.callOn(ctx, t, obj, insertTextJS, &took, a.Text); err != nil {
			return result{}, err
		}
		if !took {
			return result{}, failPage(CodeNotFound, a.Ref+" did not take the text; it may not accept text.", elementLine(info))
		}
	case a.Text == "":
		if err := b.pressKey(ctx, t, chord{key: namedKeys["Backspace"]}); err != nil {
			return result{}, err
		}
	default:
		if _, err := b.conn.Call(ctx, t.session, "Input.insertText", map[string]any{"text": a.Text}); err != nil {
			return result{}, err
		}
	}
	if !f.Direct {
		if err := b.callOn(ctx, t, obj, `function() {
			if (this.tagName === 'INPUT' || this.tagName === 'TEXTAREA') return {value: this.value, maxLength: this.maxLength > 0 ? this.maxLength : 0};
			return {value: this.innerText.replace(/\n$/, '')};
		}`, &held); err != nil {
			return result{}, err
		}
	}
	var text string
	lines := []string{elementLine(info)}
	switch {
	case f.Password:
		text = fmt.Sprintf("Typed %d characters into %s; it holds %d", utf8.RuneCountInString(a.Text), a.Ref, utf8.RuneCountInString(held.Value))
	case a.Text == "" && held.Value == "":
		text = "Cleared " + a.Ref
	case held.Value == a.Text:
		text = fmt.Sprintf("Typed %q into %s", cut(a.Text, 200), a.Ref)
	default:
		// What the field holds is the page's doing: it may have
		// changed or cut the text.
		text = fmt.Sprintf("Typed %q into %s; it holds something else", cut(a.Text, 200), a.Ref)
		if held.MaxLength > 0 && utf8.RuneCountInString(a.Text) > held.MaxLength {
			text += fmt.Sprintf(" (it takes at most %d characters)", held.MaxLength)
		}
		lines = append(lines, fmt.Sprintf("Holds: %q", cut(held.Value, 200)))
	}
	if a.Submit {
		if err := b.checkFocus(ctx, t); err != nil {
			return result{}, err
		}
		m := t.mark()
		if err := b.pressKey(ctx, t, chord{key: namedKeys["Enter"]}); err != nil {
			return result{}, err
		}
		if err := b.settle(ctx, t, m); err != nil {
			return result{}, err
		}
		text += " and pressed Enter"
		lines = append(lines, t.pageLine())
	}
	return result{text: withPage(text+".", lines...)}, nil
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
		return result{}, failPage(CodeBadArgs, a.Ref+" is not a select element; click it instead.", elementLine(info))
	case res.Missing:
		return result{}, failPage(CodeNotFound, fmt.Sprintf("%s has no option %q.", a.Ref, a.Value), elementLine(info), "Options: "+cut(strings.Join(res.Options, ", "), 1000))
	}
	return result{text: withPage(fmt.Sprintf("Selected %q in %s.", a.Value, a.Ref), elementLine(info), "Selected: "+quoteAll(res.Selected))}, nil
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
	if err := b.checkFocus(ctx, t); err != nil {
		return result{}, err
	}
	m := t.mark()
	if err := b.pressKey(ctx, t, c); err != nil {
		return result{}, err
	}
	if err := b.settle(ctx, t, m); err != nil {
		return result{}, err
	}
	return result{text: withPage(fmt.Sprintf("Pressed %s.", a.Key), t.pageLine())}, nil
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
		return result{text: withPage("There is nothing to go back to.", t.pageLine())}, nil
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
	return result{text: withPage("Went back.", t.pageLine())}, nil
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
		b.setCurrent(ctx, t)
		b.bringToFront(ctx, t)
		return result{text: withPage(fmt.Sprintf("Switched to %s.", t.id), t.pageLine())}, nil
	case TabsOpen:
		if a.URL != "" {
			if err := b.checkURL(groupName, a.URL); err != nil {
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
		return result{text: withPage(fmt.Sprintf("Closed %s; the current tab is %s.", t.id, cur.id), cur.pageLine())}, nil
	}
	return result{}, failf(CodeBadArgs, "The action %q is not list, switch, open or close.", a.Action)
}

func (b *browser) listTabs(groupName string) string {
	tabs, cur := b.groupTabs(groupName)
	if len(tabs) == 0 {
		return "This task has no tab open."
	}
	lines := make([]string, len(tabs))
	for i, t := range tabs {
		marker := " "
		if t == cur {
			marker = "*"
		}
		lines[i] = marker + " " + t.id + " " + t.where()
	}
	return withPage("Your tabs, the current one marked *:", lines...)
}

func (b *browser) cmdWait(ctx context.Context, t *tab, a WaitArgs) (result, error) {
	d := time.Duration(a.TimeoutMS) * time.Millisecond
	if d <= 0 || d > maxWait {
		d = maxWait
	}
	timer := b.clock.NewTimer(d)
	defer timer.Stop()
	timedOut := func() (result, error) {
		return result{}, failPage(CodeTimeout, fmt.Sprintf("Waited %s and it did not happen.", d), t.pageLine())
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
		return result{text: withPage("Navigated.", t.pageLine())}, nil
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
				return result{text: withPage(fmt.Sprintf("The page shows %q.", a.Value), t.pageLine())}, nil
			}
			return result{text: withPage(fmt.Sprintf("%s is on the page.", a.Value), t.pageLine())}, nil
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
	return result{text: delimitPage(cut(text, MaxEvalChars))}, nil
}

func (b *browser) cmdUpload(ctx context.Context, t *tab, groupName string, a UploadArgs) (result, error) {
	name := cleanFileName(a.File)
	path := filepath.Join(b.dir, "files", cleanFileName(groupName), name)
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return result{}, failf(CodeNotFound, "There is no task file %q to upload.", a.File)
	}
	// A file goes onto a web page, and in a held tab only one on the site.
	t.mu.Lock()
	at := t.url
	t.mu.Unlock()
	// The site is checked directly, not through allowed: a person's
	// sign-in unlock is for the person, never for a file.
	site := b.heldSite(t.group)
	if p, err := url.Parse(at); err != nil || (p.Scheme != "http" && p.Scheme != "https") || (site != "" && !onSiteURL(site, at)) {
		if at == "" {
			return result{}, failf(CodeOffSite, "Blocked: a file goes only onto a web page of the site this task is held to, and this tab is at no page.")
		}
		return result{}, failPage(CodeOffSite, "Blocked: a file goes only onto a web page of the site this task is held to, and this tab is not on one.", urlLine(at))
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
		return result{}, failPage(CodeBadArgs, a.Ref+" is not a file input.", elementLine(info))
	}
	if _, err := b.conn.Call(ctx, t.session, "DOM.setFileInputFiles", map[string]any{"files": []string{path}, "objectId": obj}); err != nil {
		return result{}, err
	}
	return result{text: withPage(fmt.Sprintf("Set %s to %s.", a.Ref, name), elementLine(info))}, nil
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
	return result{text: withPage("Screenshot of the tab. The image is content of an untrusted page.", t.pageLine()), image: &Image{MIME: "image/png", Data: data}}, nil
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
