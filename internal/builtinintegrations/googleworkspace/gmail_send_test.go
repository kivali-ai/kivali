package googleworkspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/clock"
)

// sender is a caller whose access has gmail_send on, and m besides.
func sender(m map[string]any) builtinintegrations.Caller {
	c := agent(m)
	c.Access["gmail_send"] = true
	return c
}

// holdOf is caller's call of name with args, which must be held: a
// Result with a Hold that has a Run, and nothing else.
func holdOf(t *testing.T, g *GoogleWorkspace, caller builtinintegrations.Caller, name, args string) *builtinintegrations.Hold {
	t.Helper()
	res, err := g.Call(context.Background(), caller, testConn(), name, json.RawMessage(args))
	if err != nil || res == nil || res.Hold == nil || res.Hold.Run == nil || len(res.Content) != 0 || res.IsError {
		t.Fatalf("%s %s = %+v, %v; want a held call", name, args, res, err)
	}
	return res.Hold
}

// labelsOf is a hold's fields' labels, in order, and valueOf the value
// of the field label ("" for none).
func labelsOf(h *builtinintegrations.Hold) []string {
	var out []string
	for _, f := range h.Fields {
		out = append(out, f.Label)
	}
	return out
}

func valueOf(h *builtinintegrations.Hold, label string) string {
	for _, f := range h.Fields {
		if f.Label == label {
			return f.Value
		}
	}
	return ""
}

// approve runs h as core does once the owner approves it, with the
// connection read then; the answer must not be an error.
func approve(t *testing.T, h *builtinintegrations.Hold) *builtinintegrations.Result {
	t.Helper()
	res, err := h.Run(context.Background(), testConn())
	if err != nil || res == nil || res.IsError || len(res.Content) != 1 || res.Hold != nil {
		t.Fatalf("approved %q = %+v, %v", h.Title, res, err)
	}
	return res
}

// sentMessage is the i-th message the fake was sent, read, after
// checking it is RFC 2822's (draftMessage's checks).
func sentAt(t *testing.T, w *fakeGmailWrites, i int) (*mail.Message, fakeSent) {
	t.Helper()
	sent := w.sentMessages()
	if len(sent) <= i {
		t.Fatalf("%d messages sent, want message %d", len(sent), i+1)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(sent[i].raw))
	if err != nil {
		t.Fatalf("the sent message does not read: %v\n%s", err, sent[i].raw)
	}
	return msg, sent[i]
}

// Why this test exists: every email an agent sends waits for the
// owner, and what the owner approves must be exactly what goes. A
// send_email call must hold, reaching nothing in Gmail, with a card
// naming the recipients and showing From, To, Cc, Bcc, the subject and
// the whole body as they will go, plain addresses alone (a name given
// is left out of the message too, so the card is the whole truth), and
// the recipients the hold reaches. Approved, the message sent must be
// the one built when the call was held (its date from then, however
// late the owner answers), without Kivali's mark, and say whom it went
// to; the audit names the message and the addresses, never the text.
func TestSendEmailHolds(t *testing.T) {
	f, w, g := newFakeGmail(t)
	clk := clock.NewFakeAt(monday)
	g.Clock = clk
	h := holdOf(t, g, sender(map[string]any{}), "send_email", `{
		"to": ["Zoë Ångström <zoe@example.org>", "bob@example.com"],
		"cc": ["carol@example.net"],
		"bcc": ["dave@example.com", "BOB@example.com"],
		"subject": "Q3 figures",
		"body": "Hello Zoë,\n\nThe figures are in."
	}`)
	if h.Title != "Send an email to zoe@example.org and 3 more" {
		t.Errorf("title %q", h.Title)
	}
	if got := labelsOf(h); !slices.Equal(got, []string{"From", "To", "Cc", "Bcc", "Subject", "Body"}) {
		t.Errorf("labels %v", got)
	}
	for label, want := range map[string]string{
		"From": "dana@example.com", "To": "zoe@example.org, bob@example.com", "Cc": "carol@example.net", "Bcc": "dave@example.com",
		"Subject": "Q3 figures", "Body": "Hello Zoë,\n\nThe figures are in.",
	} {
		if got := valueOf(h, label); got != want {
			t.Errorf("%s: %q, want %q", label, got, want)
		}
	}
	if !slices.Equal(h.Recipients, []string{"zoe@example.org", "bob@example.com", "carol@example.net", "dave@example.com"}) {
		t.Errorf("recipients %v", h.Recipients)
	}
	if got := requestsFor(f, "gmail POST"); len(got) != 0 || len(w.sentMessages()) != 0 {
		t.Fatalf("Gmail was changed before the owner approved: %v", got)
	}

	clk.Advance(20 * time.Hour)
	res := approve(t, h)
	if res.Content[0] != "Sent message sm1 in thread st1 to zoe@example.org, bob@example.com, carol@example.net, dave@example.com." ||
		res.Audit != "sent message sm1 (thread st1), to zoe@example.org, bob@example.com, carol@example.net, dave@example.com" || len(res.Outside) != 0 {
		t.Errorf("sent: %q, audit %q, outside %v", res.Content[0], res.Audit, res.Outside)
	}
	msg, sent := sentAt(t, w, 0)
	if got := addresses(t, msg, "To"); !slices.Equal(got, []string{" <zoe@example.org>", " <bob@example.com>"}) {
		t.Errorf("To: %q", got)
	}
	if strings.Join(addressesOnly(t, msg, "To"), ", ") != valueOf(h, "To") || strings.Join(addressesOnly(t, msg, "Cc"), ", ") != valueOf(h, "Cc") ||
		strings.Join(addressesOnly(t, msg, "Bcc"), ", ") != valueOf(h, "Bcc") || msg.Header.Get("Subject") != valueOf(h, "Subject") {
		t.Errorf("the message is not the card:\n%s", sent.raw)
	}
	if got := strings.TrimSuffix(strings.ReplaceAll(bodyOf(t, msg), "\r\n", "\n"), "\n"); got != valueOf(h, "Body") {
		t.Errorf("body %q, the card %q", got, valueOf(h, "Body"))
	}
	if msg.Header.Get("Date") != "Mon, 26 Oct 2026 09:00:00 +0000" || msg.Header.Get(markerHeader) != "" || sent.askedThread != "" {
		t.Errorf("Date %q, mark %q, thread %q", msg.Header.Get("Date"), msg.Header.Get(markerHeader), sent.askedThread)
	}
}

// addressesOnly is the header name's addresses alone.
func addressesOnly(t *testing.T, msg *mail.Message, name string) []string {
	t.Helper()
	list, err := msg.Header.AddressList(name)
	if err != nil && !errors.Is(err, mail.ErrHeaderNotPresent) {
		t.Fatalf("%s: %v", name, err)
	}
	var out []string
	for _, a := range list {
		out = append(out, a.Address)
	}
	return out
}

// Why this test exists: a reply the owner approves must show what it
// answers, so the owner can tell which conversation it joins: the
// replied message's sender by address, when it was sent and its
// subject. Approved, it must go in that message's thread, threaded
// under its Message-ID, as a draft reply would; the audit names the
// message it replies to.
func TestSendEmailReply(t *testing.T) {
	f, w, g := newFakeGmail(t)
	replyMail(f)
	h := holdOf(t, g, sender(map[string]any{}), "send_email", `{"in_reply_to_message_id":"m1","body":"Paid today."}`)
	if h.Title != "Send an email to alice@example.org" || valueOf(h, "Subject") != "Re: Invoice 1" ||
		valueOf(h, "In reply to") != "The message from alice@example.org, sent 2026-10-26 09:00 UTC, subject: Invoice 1" ||
		!slices.Equal(labelsOf(h), []string{"From", "To", "Subject", "In reply to", "Body"}) {
		t.Errorf("a reply's card: %q %+v", h.Title, h.Fields)
	}
	res := approve(t, h)
	if res.Audit != "sent message sm1 (thread t1), a reply to message m1, to alice@example.org" {
		t.Errorf("audit %q", res.Audit)
	}
	msg, sent := sentAt(t, w, 0)
	if sent.askedThread != "t1" || msg.Header.Get("In-Reply-To") != "<m1@example.org>" {
		t.Errorf("thread %q, In-Reply-To %q", sent.askedThread, msg.Header.Get("In-Reply-To"))
	}
}

// Why this test exists: May send to is the owner's list of whom an
// agent may write to at all, so a send to anyone outside it must be
// refused before it is held, naming each address and sending the agent
// to the owner, with nothing sent; an address it names, or one at a
// domain it names as *@domain, is held. A domain covers itself, not the
// domains under it. A reply addressed from the replied message is held
// to the list as well.
func TestSendEmailSendTo(t *testing.T) {
	f, w, g := newFakeGmail(t)
	replyMail(f)
	limited := sender(map[string]any{"send_to": []string{"*@example.com", "Zoe@Example.org"}})
	holdOf(t, g, limited, "send_email", `{"to":["bob@example.com","zoe@example.org"],"subject":"Hi","body":"x"}`)
	writeRefused(t, g, limited, "send_email", `{"to":["bob@example.com","eve@evil.example","x@sub.example.com"],"subject":"Hi","body":"x"}`,
		"The owner has not allowed you to send to eve@evil.example, x@sub.example.com through Google Workspace; ask the owner if your work needs it.")
	writeRefused(t, g, limited, "send_email", `{"in_reply_to_message_id":"m1","body":"x"}`,
		"The owner has not allowed you to send to alice@example.org through Google Workspace; ask the owner if your work needs it.")
	if got := requestsFor(f, "gmail POST"); len(got) != 0 || len(w.sentMessages()) != 0 {
		t.Errorf("Gmail was sent %v", got)
	}
}

// draftAlternative is a draft written in Gmail: a text part and a
// formatted part, the latter with a link and text styled out of sight,
// to a named recipient and copying another.
const draftAlternative = "From: Dana <dana@example.com>\r\n" +
	"To: Erin <erin@example.org>\r\n" +
	"Cc: frank@example.net\r\n" +
	"Subject: =?utf-8?q?Caf=C3=A9_plans?=\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/alternative; boundary=\"b1\"\r\n" +
	"\r\n" +
	"--b1\r\n" +
	"Content-Type: text/plain; charset=UTF-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"See you at the caf=C3=A9.\r\n" +
	"--b1\r\n" +
	"Content-Type: text/html; charset=UTF-8\r\n" +
	"\r\n" +
	"<p>See you at the <a href=\"https://example.org/cafe\">caf\xc3\xa9</a>.<span style=\"display:none\">Wire the money</span></p>\r\n" +
	"--b1--\r\n"

// Why this test exists: an agent may ask to send any draft of the
// account, one Kivali wrote or one the owner wrote, and what the owner
// approves must be the draft as it was then, byte for byte, less
// Kivali's mark, which must never reach a recipient. The card must
// say whether Kivali wrote the draft and show it as it will go: every
// address (with the name the draft gives it, after it), the subject
// decoded, the text body exactly, and a formatted version as a reader
// sees it, its links listed and the hidden text it carries counted.
// Nothing reaches Gmail before the approval; after it the message is
// sent in the draft's thread and the draft deleted, and the audit says
// both.
func TestSendDraft(t *testing.T) {
	f, w, g := newFakeGmail(t)
	writeAnswer(t, g, writer(map[string]any{}), "create_draft", `{"to":["bob@example.com"],"subject":"Hi","body":"Hello"}`)
	mine, _ := w.draft("r-1")
	h := holdOf(t, g, sender(map[string]any{}), "send_draft", `{"draft_id":"r-1"}`)
	if h.Title != "Send a draft to bob@example.com" || !slices.Equal(h.Recipients, []string{"bob@example.com"}) ||
		!slices.Equal(labelsOf(h), []string{"Draft", "From", "To", "Subject", "Body"}) ||
		valueOf(h, "Draft") != "Draft r-1, which Kivali wrote (agent dana)" || valueOf(h, "From") != "dana@example.com" ||
		valueOf(h, "To") != "bob@example.com" || valueOf(h, "Subject") != "Hi" || valueOf(h, "Body") != "Hello\n" {
		t.Errorf("Kivali's draft: %q %v %+v", h.Title, h.Recipients, h.Fields)
	}
	if got := requestsFor(f, "gmail POST"); len(got) != 1 || len(w.sentMessages()) != 0 {
		t.Fatalf("Gmail was sent %v before the owner approved", got)
	}
	res := approve(t, h)
	if res.Content[0] != "Sent draft r-1 as message sm1 in thread dt1 to bob@example.com, and deleted the draft." ||
		res.Audit != "sent message sm1 (thread dt1) to bob@example.com, from draft r-1, which Kivali deleted" {
		t.Errorf("sent: %q, audit %q", res.Content[0], res.Audit)
	}
	_, sent := sentAt(t, w, 0)
	if want := strings.Replace(string(mine.raw), "X-Kivali-Agent: dana\r\n", "", 1); string(sent.raw) != want || want == string(mine.raw) || sent.askedThread != "dt1" {
		t.Errorf("sent, in thread %q:\n%s\nwant:\n%s", sent.askedThread, sent.raw, want)
	}
	if _, ok := w.draft("r-1"); ok {
		t.Error("the draft is still in Drafts")
	}

	w.addDraft("r-77", draftAlternative)
	h = holdOf(t, g, sender(map[string]any{}), "send_draft", `{"draft_id":"r-77"}`)
	formatted := valueOf(h, "Formatted version, as a reader sees it")
	if valueOf(h, "Draft") != "Draft r-77, which Kivali did not write" || valueOf(h, "To") != "erin@example.org (Erin)" ||
		valueOf(h, "Cc") != "frank@example.net" || valueOf(h, "Subject") != "Café plans" || valueOf(h, "Body") != "See you at the café." ||
		!strings.Contains(formatted, "See you at the café [link 1].") || !strings.Contains(formatted, "Links:\n1. example.org https://example.org/cafe") ||
		!strings.Contains(formatted, "It also holds 14 characters a reader does not see, which go with it.") || strings.Contains(formatted, "Wire") {
		t.Errorf("the owner's draft: %+v", h.Fields)
	}
	if !slices.Equal(h.Recipients, []string{"erin@example.org", "frank@example.net"}) || h.Title != "Send a draft to erin@example.org and 1 more" {
		t.Errorf("recipients %v, title %q", h.Recipients, h.Title)
	}
	approve(t, h)
	if _, sent := sentAt(t, w, 1); string(sent.raw) != draftAlternative {
		t.Errorf("the owner's draft was sent as\n%s", sent.raw)
	}
}

// Why this test exists: the owner approves the draft as the card showed
// it. If the draft changes in Gmail after that, the bytes the owner saw
// must still be what goes, never the changed draft, and the changed
// draft must stay in Drafts rather than be deleted with work in it; the
// agent and the audit are told so. A draft gone by the time the owner
// approves (sent or deleted in Gmail) must not be sent again: the Run
// refuses, sending nothing. A draft with attachments, which a card
// cannot show, is refused before it is held, as is one addressed
// outside May send to.
func TestSendDraftEdges(t *testing.T) {
	f, w, g := newFakeGmail(t)
	before := "From: dana@example.com\r\nTo: erin@example.org\r\nSubject: v1\r\n\r\nfirst\r\n"
	w.addDraft("r-80", before)
	h := holdOf(t, g, sender(map[string]any{}), "send_draft", `{"draft_id":"r-80"}`)
	w.changeDraft("r-80", "From: dana@example.com\r\nTo: mallory@evil.example\r\nSubject: v2\r\n\r\nsecond\r\n")
	res := approve(t, h)
	if !strings.HasPrefix(res.Content[0], "Draft r-80 changed after the owner saw it, so Kivali sent it as it stood when the owner saw it, as message sm1") ||
		!strings.HasSuffix(res.Content[0], ", and kept the changed draft in Drafts.") ||
		res.Audit != "sent message sm1 (thread tr80) to erin@example.org, from draft r-80, which had changed since the owner saw it, so Kivali kept it" {
		t.Errorf("a changed draft: %q, audit %q", res.Content[0], res.Audit)
	}
	if _, sent := sentAt(t, w, 0); string(sent.raw) != before {
		t.Errorf("sent\n%s", sent.raw)
	}
	if d, ok := w.draft("r-80"); !ok || !strings.Contains(string(d.raw), "second") {
		t.Error("the changed draft was not kept")
	}

	w.addDraft("r-81", before)
	h = holdOf(t, g, sender(map[string]any{}), "send_draft", `{"draft_id":"r-81"}`)
	w.mu.Lock()
	w.drafts = slices.DeleteFunc(w.drafts, func(d fakeDraft) bool { return d.id == "r-81" })
	w.mu.Unlock()
	_, err := h.Run(context.Background(), testConn())
	var ref *builtinintegrations.Refusal
	if !errors.As(err, &ref) || ref.Text != "Draft r-81 is no longer in the owner's Drafts: it was sent or deleted after the owner saw it, so Kivali sent nothing." {
		t.Errorf("a draft gone: %v", err)
	}
	if got := len(w.sentMessages()); got != 1 {
		t.Errorf("%d messages sent", got)
	}

	w.addDraft("r-82", "From: dana@example.com\r\nTo: erin@example.org\r\nSubject: s\r\nContent-Type: multipart/mixed; boundary=\"m\"\r\n\r\n"+
		"--m\r\nContent-Type: text/plain\r\n\r\nSee attached.\r\n--m\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\n\r\n%PDF\r\n--m--\r\n")
	writeRefused(t, g, sender(map[string]any{}), "send_draft", `{"draft_id":"r-82"}`, "Draft r-82 has attachments, which Kivali does not send; the owner can send it from Gmail.")
	w.addDraft("r-83", before)
	writeRefused(t, g, sender(map[string]any{"send_to": []string{"*@example.com"}}), "send_draft", `{"draft_id":"r-83"}`,
		"The owner has not allowed you to send to erin@example.org through Google Workspace; ask the owner if your work needs it.")
	if got := len(w.sentMessages()); got != 1 || len(requestsFor(f, "gmail POST")) != 1 {
		t.Errorf("refused drafts were sent: %v", requestsFor(f, "gmail POST"))
	}
}

// Why this test exists: Kivali's mark is a header of the draft's
// message, and sending must take out exactly it, folded lines and any
// capitals included, leaving every other byte, the body's lines that
// look like the header among them, as the owner saw them.
func TestWithoutHeader(t *testing.T) {
	raw := "From: a@example.org\r\nx-kivali-agent: dana\r\n more\r\nSubject: s\r\n\tfolded\r\n\r\nX-Kivali-Agent: in the body\r\n"
	want := "From: a@example.org\r\nSubject: s\r\n\tfolded\r\n\r\nX-Kivali-Agent: in the body\r\n"
	if got := string(withoutHeader([]byte(raw), markerHeader)); got != want {
		t.Errorf("got %q", got)
	}
	if got := string(withoutHeader([]byte("To: b@example.org\n\nbody\n"), markerHeader)); got != "To: b@example.org\n\nbody\n" {
		t.Errorf("LF lines: %q", got)
	}
}

// Why this test exists: an agent with May send mail on must be given
// send_email and send_draft, and each must tell the agent the call
// waits for the owner and that the outcome comes to its chat, so the
// agent neither waits on the call nor takes "waiting" for sent. They
// take the arguments their schemas declare, send_email the ones
// create_draft takes.
func TestGmailSendTools(t *testing.T) {
	tools, err := New().Tools(context.Background(), agent(map[string]any{"gmail_send": true}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
		if !strings.HasSuffix(tl.Description, waitsWords+ownerDecides) {
			t.Errorf("%s: %q", tl.Name, tl.Description)
		}
	}
	if !slices.Equal(names, []string{"send_email", "send_draft"}) {
		t.Errorf("tools = %v", names)
	}
	if a, b := actionNamed("send_email"), actionNamed("send_draft"); a.schema != composeSchema || b.schema != draftSchema {
		t.Errorf("send_email %s, send_draft %s", a.schema, b.schema)
	}
	var declared struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal([]byte(composeSchema), &declared); err != nil || len(declared.Properties) != 8 || !slices.Equal(declared.Required, []string{"body"}) {
		t.Errorf("compose schema %s: %v", composeSchema, err)
	}
}
