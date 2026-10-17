package browserpod

import (
	"context"
	"encoding/json"
	"fmt"
)

// maxDialogChars bounds the text of a JavaScript dialog the pod reports.
const maxDialogChars = 500

// JavaScript dialog types, as Page.javascriptDialogOpening names them.
const (
	dialogAlert        = "alert"
	dialogConfirm      = "confirm"
	dialogPrompt       = "prompt"
	dialogBeforeUnload = "beforeunload"
)

// dialogPolicy decides whether a JavaScript dialog is accepted. The
// open session accepts every dialog. A site session accepts an alert,
// which asks nothing; a confirm, a prompt or a beforeunload is
// accepted only when the action that opened it asked for that
// (acceptRequested) or a person is attached to the group's waiting
// task (unlocked), and dismissed otherwise.
func dialogPolicy(siteSession, unlocked, acceptRequested bool, dialogType string) bool {
	if !siteSession || unlocked || acceptRequested {
		return true
	}
	return dialogType == dialogAlert
}

// onDialog answers a JavaScript dialog on t by dialogPolicy and notes
// it for the tab's next result.
func (b *browser) onDialog(t *tab, session string, raw json.RawMessage) {
	var ev struct {
		Type          string `json:"type"`
		Message       string `json:"message"`
		DefaultPrompt string `json:"defaultPrompt"`
	}
	_ = json.Unmarshal(raw, &ev)
	b.mu.Lock()
	var unlocked, requested bool
	if g := b.groups[t.group]; g != nil {
		unlocked, requested = g.unlocked, g.acceptDialogs > 0
	}
	b.mu.Unlock()
	site := b.site != ""
	accept := dialogPolicy(site, unlocked, requested, ev.Type)
	t.note(dialogNote(ev.Type, ev.Message, accept))
	params := map[string]any{"accept": accept}
	if accept && ev.Type == dialogPrompt {
		params["promptText"] = ev.DefaultPrompt
	}
	go func() {
		_, _ = b.conn.Call(context.Background(), session, "Page.handleJavaScriptDialog", params)
	}()
}

// dialogNote is what the model reads about a dialog: its type, what
// became of it, and its text as page content.
func dialogNote(typ, message string, accepted bool) string {
	var s string
	switch {
	case accepted && typ == dialogPrompt:
		s = "The page showed a prompt dialog; it was accepted with its default answer."
	case accepted:
		s = fmt.Sprintf("The page showed %s dialog; it was accepted.", withArticle(typ))
	case typ == dialogBeforeUnload:
		s = "The page asked to confirm leaving it; that was dismissed, so the tab stayed on the page. To leave anyway, repeat the action with accept_dialog set to true."
	default:
		s = fmt.Sprintf("The page showed %s dialog; it was dismissed. To accept it, repeat the action with accept_dialog set to true.", withArticle(typ))
	}
	if message != "" {
		s += " Its text:\n" + delimitPage(cut(message, maxDialogChars))
	}
	return s
}

func withArticle(typ string) string {
	if typ == dialogAlert {
		return "an " + typ
	}
	return "a " + typ
}

// acceptDialogs makes the group's dialogs accepted until the returned
// func is called: an action whose arguments set accept_dialog runs
// between the two.
func (b *browser) acceptDialogs(name string) func() {
	b.mu.Lock()
	g := b.groups[name]
	if g != nil {
		g.acceptDialogs++
	}
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		if g != nil {
			g.acceptDialogs--
		}
		b.mu.Unlock()
	}
}

// wantsDialogAccepted reports whether a command's arguments ask for a
// dialog it opens to be accepted.
func wantsDialogAccepted(args any) bool {
	switch a := args.(type) {
	case ClickArgs:
		return a.AcceptDialog
	case TypeArgs:
		return a.AcceptDialog
	case PressArgs:
		return a.AcceptDialog
	}
	return false
}
