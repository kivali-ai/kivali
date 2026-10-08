package message

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

func noticeBlock(raw string) provider.ContentBlock {
	return provider.ContentBlock{
		Type:      provider.ContentToolUse,
		ToolName:  ToolNotice,
		ToolInput: json.RawMessage(raw),
		ToolUseID: "t1",
	}
}

// TestParseNoticeMultipleRecipients is the shape the whole feature
// rests on: one message, several addressees, normalized and
// de-duplicated in the order the sender wrote them.
func TestParseNoticeMultipleRecipients(t *testing.T) {
	got, err := Parse(noticeBlock(`{
		"to": ["Market-Analyst", "  recruiter  ", "market-analyst"],
		"title": "Pricing page changed",
		"body": "List price moved to $49. Details in the attached diff."
	}`), ParseContext{From: "chief-of-staff"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	msg := got.Message
	if msg.Type != store.MsgNotice {
		t.Errorf("type = %q, want notice", msg.Type)
	}
	want := store.Recipients{"market-analyst", "recruiter"}
	if strings.Join(msg.To, ",") != strings.Join(want, ",") {
		t.Errorf("to = %v, want %v (normalized, de-duplicated, order preserved)", msg.To, want)
	}
	if msg.InReplyTo != "" {
		t.Errorf("a notice must never carry in_reply_to; got %q", msg.InReplyTo)
	}
}

// TestParseNoticeRejectsInReplyTo: the tool has no in_reply_to field,
// and strictUnmarshal turns an attempt to set one into a parse error
// rather than a silently-dropped value. This is the "none is possible"
// half of the contract at the sending end.
func TestParseNoticeRejectsInReplyTo(t *testing.T) {
	_, err := Parse(noticeBlock(`{"to":["alice"],"title":"T","in_reply_to":"messages/2026-09-03/x.md"}`),
		ParseContext{From: "bob"})
	if err == nil {
		t.Fatal("expected a parse error for in_reply_to on a notice")
	}
	if !strings.Contains(err.Error(), "in_reply_to") {
		t.Errorf("error should name the offending field; got %v", err)
	}
}

func TestParseNoticeRecipientRules(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"no recipients", `{"to":[],"title":"T"}`, "at least one recipient"},
		{"missing to", `{"title":"T"}`, "at least one recipient"},
		{"ceo is reserved", `{"to":["ceo"],"title":"T"}`, "publish_ceo_notification"},
		{"bad slug", `{"to":["Not A Slug!"],"title":"T"}`, "only lowercase"},
		{"missing title", `{"to":["alice"]}`, "title"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(noticeBlock(tc.raw), ParseContext{From: "bob"})
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestParseNoticeBodyCap pins the tighter 2048-byte cap: a notice body
// is a pointer to substance, not the substance.
func TestParseNoticeBodyCap(t *testing.T) {
	atCap := strings.Repeat("x", MaxNoticeBodyBytes)
	if _, err := Parse(noticeBlock(fmt.Sprintf(`{"to":["alice"],"title":"T","body":%q}`, atCap)),
		ParseContext{From: "bob"}); err != nil {
		t.Fatalf("a body exactly at the cap should parse; got %v", err)
	}
	over := strings.Repeat("x", MaxNoticeBodyBytes+1)
	_, err := Parse(noticeBlock(fmt.Sprintf(`{"to":["alice"],"title":"T","body":%q}`, over)),
		ParseContext{From: "bob"})
	if err == nil {
		t.Fatal("a body one byte over the cap should be refused")
	}
	if !strings.Contains(err.Error(), "2048") {
		t.Errorf("error should quote the notice cap, not the 4096 one; got %v", err)
	}
}

// TestNoticeAskLint: a notice that reads like an ask gets a nudge that
// names the tracker, because an ask inside a notice goes unanswered.
func TestNoticeAskLint(t *testing.T) {
	cases := []struct {
		name string
		body string
		warn bool
	}{
		{"plain tell", "List price moved to $49 as of today.", false},
		{"question mark", "Pricing changed. Does that affect your model?", true},
		{"please", "Pricing changed. Please update your model.", true},
		{"can you", "Pricing changed — can you refresh the deck.", true},
		{"need you to", "I need you to know the price moved.", true},
		{"cased phrase", "Pricing changed. PLEASE note.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NoticeAskLint(store.Message{Type: store.MsgNotice, Body: tc.body})
			if tc.warn && got == "" {
				t.Errorf("body %q should have produced an ask warning", tc.body)
			}
			if !tc.warn && got != "" {
				t.Errorf("body %q should not warn; got %q", tc.body, got)
			}
			if tc.warn && !strings.Contains(got, "assignment_create") {
				t.Errorf("warning should name the tracker; got %q", got)
			}
		})
	}
	// Only notices are linted — a CEO notification full of questions
	// is exactly what the CEO's inbox is for.
	if got := NoticeAskLint(store.Message{Type: store.MsgCEONotification, Body: "Can you do this?"}); got != "" {
		t.Errorf("non-notice types must not be linted; got %q", got)
	}
}

// TestNoticeToolContract guards the model-facing half of the feature:
// the description says a reply is impossible and names where an ask
// goes instead, and the schema exposes no in_reply_to.
func TestNoticeToolContract(t *testing.T) {
	var def ToolDef
	for _, d := range AllTools() {
		if d.Name == ToolNotice {
			def = d
		}
	}
	if def.Name == "" {
		t.Fatal("publish_notice is not in AllTools")
	}
	// Pinned verbatim, not by substring. The description IS the
	// feature — it is what runs at the moment the model chooses a
	// tool — so a reworded "no reply is possible" is a contract
	// change and should fail here, loudly, rather than quietly
	// weaken the affordance.
	const want = "Tell one or more agents something they should know. " +
		"No reply is expected and none is possible. " +
		"Use this for findings, decisions, heads-ups, and anything the recipient should know but you are not asking them to do. " +
		"If you need someone to act, open an assignment for them with assignment_create instead; if you are reporting on work you were given, close the assignment with assignment_close. " +
		"Address only the agents who need this; the owner moderates every notice like any other message."
	if def.Description != want {
		t.Errorf("publish_notice description drifted.\n got: %q\nwant: %q", def.Description, want)
	}
	if strings.Contains(string(def.Schema), "in_reply_to") {
		t.Error("publish_notice must not expose an in_reply_to field")
	}
}
