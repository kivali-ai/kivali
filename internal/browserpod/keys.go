package browserpod

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// keyDef is one key as Input.dispatchKeyEvent describes it.
type keyDef struct {
	key  string
	code string
	vk   int
	text string
}

// namedKeys are the keys web_press accepts by name.
var namedKeys = map[string]keyDef{
	"Enter":      {"Enter", "Enter", 13, "\r"},
	"Tab":        {"Tab", "Tab", 9, ""},
	"Escape":     {"Escape", "Escape", 27, ""},
	"Backspace":  {"Backspace", "Backspace", 8, ""},
	"Delete":     {"Delete", "Delete", 46, ""},
	"ArrowUp":    {"ArrowUp", "ArrowUp", 38, ""},
	"ArrowDown":  {"ArrowDown", "ArrowDown", 40, ""},
	"ArrowLeft":  {"ArrowLeft", "ArrowLeft", 37, ""},
	"ArrowRight": {"ArrowRight", "ArrowRight", 39, ""},
	"Home":       {"Home", "Home", 36, ""},
	"End":        {"End", "End", 35, ""},
	"PageUp":     {"PageUp", "PageUp", 33, ""},
	"PageDown":   {"PageDown", "PageDown", 34, ""},
	"Space":      {" ", "Space", 32, " "},
}

// Modifier bits, as CDP numbers them.
const (
	modAlt   = 1
	modCtrl  = 2
	modMeta  = 4
	modShift = 8
)

var modifierNames = map[string]int{
	"alt": modAlt, "option": modAlt,
	"control": modCtrl, "ctrl": modCtrl,
	"meta": modMeta, "cmd": modMeta, "command": modMeta,
	"shift": modShift,
}

// chord is a key with the modifiers held while it is pressed.
type chord struct {
	mods int
	key  keyDef
}

// parseChord reads "Enter", "a", "Control+a", "Shift+Tab". A chord
// that Chromium itself acts on (browserChord) is refused.
func parseChord(s string) (chord, error) {
	var parts []string
	switch {
	case s == "+":
		parts = []string{"+"}
	case strings.HasSuffix(s, "++"):
		parts = append(strings.Split(strings.TrimSuffix(s, "++"), "+"), "+")
	default:
		parts = strings.Split(s, "+")
	}
	var c chord
	for _, p := range parts[:len(parts)-1] {
		bit, ok := modifierNames[strings.ToLower(strings.TrimSpace(p))]
		if !ok {
			return chord{}, failf(CodeBadArgs, "%q is not a modifier; use Control, Shift, Alt or Meta.", p)
		}
		c.mods |= bit
	}
	name := parts[len(parts)-1]
	k, ok := lookupKey(name)
	if !ok {
		return chord{}, failf(CodeBadArgs, "%q is not a key web_press knows: name one (Enter, Tab, Escape, Backspace, Delete, ArrowUp, ArrowDown, ArrowLeft, ArrowRight, Home, End, PageUp, PageDown, Space) or give one character.", name)
	}
	if browserChord(c.mods, k.key) {
		return chord{}, failf(CodeBadArgs, "%q is a shortcut the browser acts on itself (its tabs, windows and pages), so the page would never get it; it is refused.", s)
	}
	if c.mods&modShift != 0 && utf8.RuneCountInString(k.key) == 1 {
		up := strings.ToUpper(k.key)
		k.key, k.text = up, up
	}
	if c.mods&(modCtrl|modAlt|modMeta) != 0 {
		k.text = ""
	}
	c.key = k
	return c, nil
}

// browserChord reports a key or chord Chromium acts on itself instead
// of handing it to the page: closing, opening and switching tabs and
// windows, its address bar, history, downloads, bookmarks, find bar,
// print, save and open dialogs, view-source and developer tools, going
// back and forward, and the function keys. web_press refuses one; the
// browser window drops one.
func browserChord(mods int, key string) bool {
	k := strings.ToLower(key)
	if len(k) >= 2 && len(k) <= 3 && k[0] == 'f' {
		if n, err := strconv.Atoi(k[1:]); err == nil && n >= 1 && n <= 12 {
			return true
		}
	}
	switch {
	case mods&(modCtrl|modMeta) != 0:
		switch k {
		case "w", "t", "n", "q", "tab", "pageup", "pagedown",
			"l", "h", "j", "d", "o", "s", "p", "u", "f", "g", "e", "k", "b", "r", "i", "delete",
			"1", "2", "3", "4", "5", "6", "7", "8", "9", "0":
			return true
		}
	case mods&modAlt != 0:
		switch k {
		case "arrowleft", "arrowright", "home", "d", "e", "f":
			return true
		}
	}
	return false
}

// lookupKey finds a named key (in any case) or describes a single
// character.
func lookupKey(name string) (keyDef, bool) {
	for n, k := range namedKeys {
		if strings.EqualFold(n, name) {
			return k, true
		}
	}
	if utf8.RuneCountInString(name) != 1 {
		return keyDef{}, false
	}
	r, _ := utf8.DecodeRuneInString(name)
	k := keyDef{key: name, text: name}
	switch {
	case r < 128 && unicode.IsLetter(r):
		u := unicode.ToUpper(r)
		k.code = "Key" + string(u)
		k.vk = int(u)
	case r >= '0' && r <= '9':
		k.code = "Digit" + name
		k.vk = int(r)
	}
	return k, true
}

// pressKey sends a key down and up with its modifiers.
func (b *browser) pressKey(ctx context.Context, t *tab, c chord) error {
	down := "rawKeyDown"
	if c.key.text != "" {
		down = "keyDown"
	}
	ev := map[string]any{
		"type":                  down,
		"modifiers":             c.mods,
		"key":                   c.key.key,
		"code":                  c.key.code,
		"windowsVirtualKeyCode": c.key.vk,
		"nativeVirtualKeyCode":  c.key.vk,
	}
	if c.key.text != "" {
		ev["text"] = c.key.text
		ev["unmodifiedText"] = c.key.text
	}
	if _, err := b.conn.Call(ctx, t.session, "Input.dispatchKeyEvent", ev); err != nil {
		return err
	}
	up := map[string]any{
		"type":                  "keyUp",
		"modifiers":             c.mods,
		"key":                   c.key.key,
		"code":                  c.key.code,
		"windowsVirtualKeyCode": c.key.vk,
		"nativeVirtualKeyCode":  c.key.vk,
	}
	_, err := b.conn.Call(ctx, t.session, "Input.dispatchKeyEvent", up)
	return err
}

// buttonBits is the buttons field of Input.dispatchMouseEvent for each
// button held.
var buttonBits = map[string]int{"left": 1, "right": 2, "middle": 4}

// clickTrack is the last press on a tab from the browser window. A
// press of the same button soon after it, at the same place, counts as
// its double or triple click: the window's events carry no count of
// their own, and the page sees one only through clickCount.
type clickTrack struct {
	button string
	x, y   float64
	at     time.Time
	count  int
}

// clicks holds the last press per tab, pruned of closed tabs as it is
// used.
var clicks = struct {
	sync.Mutex
	last map[*tab]*clickTrack
}{last: map[*tab]*clickTrack{}}

const (
	// doubleClickWithin and doubleClickSlop bound the time and the
	// distance between two presses that make one double click.
	doubleClickWithin = 500 * time.Millisecond
	doubleClickSlop   = 5.0
)

// clickCount is the clickCount of a press from the window, or of the
// release that ends it.
func (b *browser) clickCount(t *tab, action, button string, x, y float64) int {
	clicks.Lock()
	defer clicks.Unlock()
	for k := range clicks.last {
		k.mu.Lock()
		closed := k.closed
		k.mu.Unlock()
		if closed {
			delete(clicks.last, k)
		}
	}
	c := clicks.last[t]
	if action == "released" {
		if c != nil && c.button == button {
			return c.count
		}
		return 1
	}
	now := b.clock.Now()
	if c != nil && c.button == button && c.count < 3 && now.Sub(c.at) <= doubleClickWithin &&
		math.Abs(x-c.x) <= doubleClickSlop && math.Abs(y-c.y) <= doubleClickSlop {
		c.count++
	} else {
		c = &clickTrack{button: button, count: 1}
	}
	c.x, c.y, c.at = x, y, now
	clicks.last[t] = c
	return c.count
}

// input relays one event from the browser window to a tab.
func (b *browser) input(ctx context.Context, t *tab, raw json.RawMessage) error {
	var in InputArgs
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	switch in.Kind {
	case "mouse":
		types := map[string]string{"pressed": "mousePressed", "released": "mouseReleased", "moved": "mouseMoved", "wheel": "mouseWheel"}
		typ, ok := types[in.Action]
		if !ok {
			return fmt.Errorf("unknown mouse action %q", in.Action)
		}
		button := in.Button
		if button == "" {
			button = "none"
			if in.Action == "pressed" || in.Action == "released" {
				button = "left"
			}
		}
		ev := map[string]any{"type": typ, "x": in.X, "y": in.Y, "button": button, "modifiers": in.Modifiers}
		switch typ {
		case "mousePressed":
			ev["clickCount"] = b.clickCount(t, in.Action, button, in.X, in.Y)
			ev["buttons"] = buttonBits[button]
		case "mouseReleased":
			ev["clickCount"] = b.clickCount(t, in.Action, button, in.X, in.Y)
		case "mouseMoved":
			// The window names the button a move drags with; the page
			// sees it held, as a drag needs.
			if in.Button != "" {
				ev["buttons"] = buttonBits[in.Button]
			}
		case "mouseWheel":
			ev["deltaX"], ev["deltaY"] = in.DeltaX, in.DeltaY
		}
		_, err := b.conn.Call(ctx, t.session, "Input.dispatchMouseEvent", ev)
		return err
	case "key":
		if browserChord(in.Modifiers, in.Key) {
			// A person's habit (Ctrl+W, F5) would act on the task's
			// browser, not the page: dropped, down and up alike.
			return nil
		}
		var typ string
		switch in.Action {
		case "down":
			if in.Text == "" && in.Key == "Enter" {
				// Enter acts (a form submits, a button fires) only as a
				// character: Chromium wants the carriage return as the
				// key's text.
				in.Text = "\r"
			}
			typ = "rawKeyDown"
			if in.Text != "" {
				typ = "keyDown"
			}
		case "up":
			typ = "keyUp"
		default:
			return fmt.Errorf("unknown key action %q", in.Action)
		}
		ev := map[string]any{"type": typ, "key": in.Key, "code": in.Code, "modifiers": in.Modifiers}
		if k, ok := namedKeys[in.Key]; ok {
			ev["windowsVirtualKeyCode"], ev["nativeVirtualKeyCode"] = k.vk, k.vk
		} else if k, ok := lookupKey(in.Key); ok && k.vk != 0 {
			ev["windowsVirtualKeyCode"], ev["nativeVirtualKeyCode"] = k.vk, k.vk
		}
		if in.Text != "" && typ == "keyDown" {
			ev["text"], ev["unmodifiedText"] = in.Text, in.Text
		}
		_, err := b.conn.Call(ctx, t.session, "Input.dispatchKeyEvent", ev)
		return err
	case "paste":
		_, err := b.conn.Call(ctx, t.session, "Input.insertText", map[string]any{"text": in.Text})
		return err
	}
	return fmt.Errorf("unknown input kind %q", in.Kind)
}
