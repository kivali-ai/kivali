package agent

import (
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// TestNoticeDeliveryLead pins the sentence the model reads at the
// moment it decides what to do next. It is the whole reason a notice
// doesn't produce a reply, so its wording is a contract, not a
// presentation detail.
func TestNoticeDeliveryLead(t *testing.T) {
	msg := store.Message{
		Type:  store.MsgNotice,
		Title: "Pricing page changed",
		From:  "market-analyst",
		To:    store.Recipients{"recruiter", "alice"},
		Body:  "List price moved to $49.",
	}
	got := RenderInboxBody(msg, "messages/2026-09-03/notice.md")

	want := `market-analyst sent you a notice titled "Pricing page changed". No reply is expected. If it requires action from someone, open an assignment for the owner.`
	if !strings.HasPrefix(got, want) {
		t.Errorf("lead =\n%q\nwant it to start with\n%q", got, want)
	}
	// The "(at <path>)" suffix other types carry exists so the
	// recipient can cite the message in a future in_reply_to. Nothing
	// may point at a notice, so offering the handle would only lead
	// the model into a parse error.
	if strings.Contains(got, "(at messages/") {
		t.Errorf("a notice lead must not hand the model a reply handle; got %q", got)
	}
	if !strings.Contains(got, "List price moved to $49.") {
		t.Errorf("delivery should carry the full body; got %q", got)
	}
	// A notice must never invite a reply the way the other leads do.
	if strings.Contains(got, "Their message follows") {
		t.Errorf("notice lead should not reuse the reply-inviting frame; got %q", got)
	}
}

// TestNoticeIconAndLabel: notices need their own glyph and name
// everywhere a type is rendered, so the CEO can pick them out of a
// queue at a glance.
func TestNoticeIconAndLabel(t *testing.T) {
	v := BuildInboxView(store.Message{
		Type:  store.MsgNotice,
		Title: "T",
		From:  "a",
		To:    store.Recipients{"b", "c"},
	}, "messages/2026-09-03/n.md")

	if v.Label != "Notice" {
		t.Errorf("label = %q, want %q", v.Label, "Notice")
	}
	if v.Icon == "" || v.Icon == iconForType(store.MsgAssignmentEvent, "") || v.Icon == iconForType(store.MsgCEONotification, "") {
		t.Errorf("notice icon %q must be distinct from the assignment and CEO glyphs", v.Icon)
	}
	if len(v.To) != 2 {
		t.Errorf("view should carry every addressee so the bubble can name them; got %v", v.To)
	}
}
