package googleworkspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/clock"
)

// writer is a caller whose access has gmail_write on, and m besides.
func writer(m map[string]any) builtinintegrations.Caller {
	c := agent(m)
	c.Access["gmail_write"] = true
	return c
}

func callWrite(g *GoogleWorkspace, caller builtinintegrations.Caller, name, args string) (*builtinintegrations.Result, error) {
	return g.Call(context.Background(), caller, testConn(), name, json.RawMessage(args))
}

// writeAnswer is callWrite's result, which must be an answer.
func writeAnswer(t *testing.T, g *GoogleWorkspace, caller builtinintegrations.Caller, name, args string) *builtinintegrations.Result {
	t.Helper()
	res, err := callWrite(g, caller, name, args)
	if err != nil || res == nil || res.IsError || len(res.Content) != 1 {
		t.Fatalf("%s %s = %+v, %v", name, args, res, err)
	}
	return res
}

// writeRefused checks callWrite is refused with text.
func writeRefused(t *testing.T, g *GoogleWorkspace, caller builtinintegrations.Caller, name, args, text string) {
	t.Helper()
	res, err := callWrite(g, caller, name, args)
	var ref *builtinintegrations.Refusal
	if res != nil || !errors.As(err, &ref) || ref.Text != text || ref.Reason == "" {
		t.Errorf("%s %s = %+v, %v; want the refusal %q", name, args, res, err, text)
	}
}

// writeFailed checks callWrite answers the error result text.
func writeFailed(t *testing.T, g *GoogleWorkspace, caller builtinintegrations.Caller, name, args, text string) {
	t.Helper()
	res, err := callWrite(g, caller, name, args)
	if err != nil || res == nil || !res.IsError || len(res.Content) != 1 || res.Content[0] != text {
		t.Errorf("%s %s = %+v, %v; want the error %q", name, args, res, err, text)
	}
}

// draftMessage is draft id's message as the fake keeps it, after
// checking each of its lines is ASCII, at most 998 characters and ended
// by CRLF, as RFC 2822 has them.
func draftMessage(t *testing.T, w *fakeGmailWrites, id string) (*mail.Message, fakeDraft) {
	t.Helper()
	d, ok := w.draft(id)
	if !ok {
		t.Fatalf("no draft %s", id)
	}
	if !bytes.HasSuffix(d.raw, []byte("\r\n")) {
		t.Errorf("the message does not end its last line")
	}
	for i, line := range strings.Split(string(d.raw), "\r\n") {
		if len(line) > 998 || strings.ContainsAny(line, "\r\n") || strings.ContainsFunc(line, func(r rune) bool { return r > 126 || (r < 32 && r != '\t') }) {
			t.Errorf("line %d of the message is not RFC 2822's: %q", i+1, line)
		}
	}
	msg, err := mail.ReadMessage(bytes.NewReader(d.raw))
	if err != nil {
		t.Fatalf("the draft's message does not read: %v\n%s", err, d.raw)
	}
	return msg, d
}

// bodyOf is msg's body decoded by its Content-Transfer-Encoding.
func bodyOf(t *testing.T, msg *mail.Message) string {
	t.Helper()
	r := msg.Body
	switch cte := msg.Header.Get("Content-Transfer-Encoding"); cte {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, r)
	case "7bit":
	default:
		t.Fatalf("Content-Transfer-Encoding %q", cte)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// addresses is the header name's addresses, "name <address>" each.
func addresses(t *testing.T, msg *mail.Message, name string) []string {
	t.Helper()
	list, err := msg.Header.AddressList(name)
	if err != nil && !errors.Is(err, mail.ErrHeaderNotPresent) {
		t.Fatalf("%s: %v", name, err)
	}
	var out []string
	for _, a := range list {
		out = append(out, a.Name+" <"+a.Address+">")
	}
	return out
}

// Why this test exists: a draft is a message other people may read
// once the owner sends it, so it must be one every mail program reads
// as the agent wrote it: RFC 2822 in ASCII lines, a subject and names
// outside ASCII in encoded words that decode back exactly, a long
// subject folded rather than overlong, recipients in To, Cc and Bcc as
// given, each address once, the body decoding back to its text, and
// the header that marks it as Kivali's naming the agent. A new email
// joins no thread. The answer gives the draft's, message's and
// thread's ids and what the draft holds; the audit names the ids, not
// the text. Google refusing it reaches the agent in words.
func TestCreateDraft(t *testing.T) {
	f, w, g := newFakeGmail(t)
	subject := "Résumé für Q3 — 東京 office, a subject long enough that its encoded words fold over more than one line"
	res := writeAnswer(t, g, writer(map[string]any{}), "create_draft", `{
		"to": ["Zoë Ångström <zoe@example.org>", "bob@example.com"],
		"cc": ["\"Smith, Carol\" <carol@example.net>"],
		"bcc": ["dave@example.com", "BOB@example.com"],
		"subject": "`+subject+`",
		"body": "Hello Zoë,\n\nThe figures are attached.\n"
	}`)
	want := "Created draft r-1: message id dm1, thread id dt1. Nothing was sent: the draft waits in the owner's Drafts.\n" +
		`A new email, from dana@example.com to zoe@example.org, bob@example.com; cc carol@example.net; bcc dave@example.com; subject "` + subject + `"; 37 characters of plain text.`
	if res.Content[0] != want {
		t.Errorf("create_draft:\n%s", res.Content[0])
	}
	if res.Audit != "created draft r-1 (message dm1, thread dt1), for 4 recipients" || len(res.Outside) != 0 {
		t.Errorf("audit %q, outside %v", res.Audit, res.Outside)
	}

	msg, d := draftMessage(t, w, "r-1")
	if got := msg.Header.Get("From"); got != "<dana@example.com>" {
		t.Errorf("From: %q", got)
	}
	if got := addresses(t, msg, "To"); !slices.Equal(got, []string{"Zoë Ångström <zoe@example.org>", " <bob@example.com>"}) {
		t.Errorf("To: %q", got)
	}
	if got := addresses(t, msg, "Cc"); !slices.Equal(got, []string{"Smith, Carol <carol@example.net>"}) {
		t.Errorf("Cc: %q", got)
	}
	if got := addresses(t, msg, "Bcc"); !slices.Equal(got, []string{" <dave@example.com>"}) {
		t.Errorf("Bcc: %q", got)
	}
	raw := msg.Header.Get("Subject")
	decoded, err := new(mime.WordDecoder).DecodeHeader(raw)
	if !strings.HasPrefix(raw, "=?utf-8?q?") || err != nil || decoded != subject {
		t.Errorf("Subject %q decodes to %q, %v", raw, decoded, err)
	}
	if !strings.Contains(string(d.raw), "?=\r\n =?utf-8?q?") {
		t.Errorf("the long subject is not folded:\n%s", d.raw)
	}
	for name, value := range map[string]string{"X-Kivali-Agent": "dana", "MIME-Version": "1.0", "Content-Type": "text/plain; charset=UTF-8",
		"Content-Transfer-Encoding": "quoted-printable", "In-Reply-To": "", "References": ""} {
		if got := msg.Header.Get(name); got != value {
			t.Errorf("%s: %q, want %q", name, got, value)
		}
	}
	if got := bodyOf(t, msg); got != "Hello Zoë,\r\n\r\nThe figures are attached.\r\n" {
		t.Errorf("body %q", got)
	}
	if d.askedThread != "" {
		t.Errorf("a new email asked to join thread %s", d.askedThread)
	}
	if got := f.requests(); !slices.Equal(got, []string{"gmail /gmail/v1/users/me/profile?", "gmail POST /gmail/v1/users/me/drafts?"}) {
		t.Errorf("requests = %v", got)
	}

	f.failWith("/gmail/v1/users/me/drafts", 403, `{"error":{"code":403,"message":"Request had insufficient authentication scopes.","errors":[{"reason":"insufficientPermissions"}]}}`)
	writeFailed(t, g, writer(map[string]any{}), "create_draft", `{"to":["bob@example.com"],"subject":"Hi","body":"x"}`,
		"Google refused this (HTTP 403): the owner's sign-in does not allow it. Ask the owner.")
}

// replyMail is the account's mail for the reply tests: a conversation
// t1 of two messages, the latest m1 from Alice to the owner and Bob,
// copying Carol and the owner, under earlier References; m2, which the
// owner sent to Erin; m3, whose Reply-To is Billing's and whose subject
// starts RE:; and m4, under zeta alone.
func replyMail(f *fakeGoogle) {
	withMail(f,
		mailMsg{id: "m1", thread: "t1", labels: []string{"INBOX"}, at: monday, from: "Alice <alice@example.org>", to: "dana@example.com, Bob <bob@example.com>", subject: "Invoice 1", plain: "one"},
		mailMsg{id: "m0", thread: "t1", labels: []string{"INBOX"}, at: monday.Add(-time.Hour), from: "Bob <bob@example.com>", to: "alice@example.org", subject: "Invoice 1", plain: "zero"},
		mailMsg{id: "m2", thread: "t2", labels: []string{"SENT"}, at: monday.Add(-2 * time.Hour), from: "Dana <dana@example.com>", to: "Erin <erin@example.org>", subject: "Plans", plain: "two"},
		mailMsg{id: "m3", thread: "t3", labels: []string{"INBOX"}, at: monday.Add(-3 * time.Hour), from: "shop@example.org", to: "dana@example.com", subject: "RE: Your order", plain: "three"},
		mailMsg{id: "m4", thread: "t4", labels: []string{"Label_9"}, at: monday.Add(-4 * time.Hour), from: "zed@example.org", to: "dana@example.com", subject: "Zeta", plain: "four"},
	)
	f.mail[0].Payload.Headers = append(f.mail[0].Payload.Headers,
		gmailHeader{"Cc", "carol@example.net, Dana <DANA@example.com>"}, gmailHeader{"References", "<a@x.example> <b@x.example>"})
	f.mail[3].Payload.Headers = append(f.mail[3].Payload.Headers, gmailHeader{"Reply-To", "Billing <billing@example.org>"})
}

// Why this test exists: a reply must land in its conversation for
// whoever reads it, which takes Gmail's thread id and the replied
// message's Message-ID in In-Reply-To and References, and a subject
// with Re: before it. With no to, it must go where Gmail's own Reply
// sends it (Reply-To, else From; for a message the owner sent, its
// recipients), and only with reply_all to the others too, the owner
// never among them. A subject Kivali took from the replied message is
// the sender's text, so the answer names it without quoting it. The
// message replied to must be in the access's labels, and a thread id
// that does not match the message is refused; thread_id alone replies
// to the conversation's latest message.
func TestCreateDraftReply(t *testing.T) {
	f, w, g := newFakeGmail(t)
	replyMail(f)
	dana := writer(map[string]any{})

	res := writeAnswer(t, g, dana, "create_draft", `{"in_reply_to_message_id":"m1","body":"Paid today."}`)
	want := "Created draft r-1: message id dm1, thread id t1. Nothing was sent: the draft waits in the owner's Drafts.\n" +
		"A reply to message m1 in thread t1, from dana@example.com to alice@example.org; subject Re: and the replied message's subject; 11 characters of plain text."
	if res.Content[0] != want {
		t.Errorf("a reply:\n%s", res.Content[0])
	}
	if res.Audit != "created draft r-1 (message dm1, thread t1), a reply to message m1, for 1 recipient" {
		t.Errorf("audit %q", res.Audit)
	}
	msg, d := draftMessage(t, w, "r-1")
	for name, value := range map[string]string{"Subject": "Re: Invoice 1", "In-Reply-To": "<m1@example.org>", "References": "<a@x.example> <b@x.example> <m1@example.org>"} {
		if got := msg.Header.Get(name); got != value {
			t.Errorf("%s: %q, want %q", name, got, value)
		}
	}
	if got := addresses(t, msg, "To"); !slices.Equal(got, []string{"Alice <alice@example.org>"}) || msg.Header.Get("Cc") != "" {
		t.Errorf("To %q, Cc %q", got, msg.Header.Get("Cc"))
	}
	if d.askedThread != "t1" {
		t.Errorf("thread asked for: %q", d.askedThread)
	}
	if got := requestsFor(f, "gmail /gmail/v1/users/me/messages/m1"); len(got) != 1 || !strings.Contains(got[0], "format=metadata") {
		t.Errorf("the replied message was read as %v", got)
	}

	writeAnswer(t, g, dana, "create_draft", `{"in_reply_to_message_id":"m1","reply_all":true,"body":"Paid."}`)
	msg, _ = draftMessage(t, w, "r-2")
	if to, cc := addresses(t, msg, "To"), addresses(t, msg, "Cc"); !slices.Equal(to, []string{"Alice <alice@example.org>", "Bob <bob@example.com>"}) ||
		!slices.Equal(cc, []string{" <carol@example.net>"}) {
		t.Errorf("reply all: To %q, Cc %q", to, cc)
	}

	res = writeAnswer(t, g, dana, "create_draft", `{"thread_id":"t1","subject":"Thanks","body":"Thanks."}`)
	msg, d = draftMessage(t, w, "r-3")
	if msg.Header.Get("In-Reply-To") != "<m1@example.org>" || msg.Header.Get("Subject") != "Re: Thanks" || d.askedThread != "t1" ||
		!strings.Contains(res.Content[0], `A reply to message m1 in thread t1, from dana@example.com to alice@example.org; subject "Re: Thanks";`) {
		t.Errorf("thread_id alone:\n%s\n%s", res.Content[0], d.raw)
	}

	writeAnswer(t, g, dana, "create_draft", `{"in_reply_to_message_id":"m3","body":"Where is it?"}`)
	msg, _ = draftMessage(t, w, "r-4")
	if got := addresses(t, msg, "To"); !slices.Equal(got, []string{"Billing <billing@example.org>"}) || msg.Header.Get("Subject") != "RE: Your order" {
		t.Errorf("Reply-To: To %q, Subject %q", got, msg.Header.Get("Subject"))
	}
	writeAnswer(t, g, dana, "create_draft", `{"in_reply_to_message_id":"m2","body":"Any news?"}`)
	msg, _ = draftMessage(t, w, "r-5")
	if got := addresses(t, msg, "To"); !slices.Equal(got, []string{"Erin <erin@example.org>"}) {
		t.Errorf("the owner's own message: To %q", got)
	}
	writeAnswer(t, g, dana, "create_draft", `{"in_reply_to_message_id":"m1","to":["zoe@example.org"],"reply_all":true,"body":"FYI"}`)
	msg, _ = draftMessage(t, w, "r-6")
	if got := addresses(t, msg, "To"); !slices.Equal(got, []string{" <zoe@example.org>"}) || msg.Header.Get("Cc") != "" || msg.Header.Get("In-Reply-To") != "<m1@example.org>" {
		t.Errorf("to given: To %q, Cc %q", got, msg.Header.Get("Cc"))
	}

	writeRefused(t, g, dana, "create_draft", `{"in_reply_to_message_id":"m1","thread_id":"t2","body":"x"}`, "Message m1 is not in thread t2; give one of them.")
	clients := writer(map[string]any{"gmail_labels": []string{"Clients (old) (Label_12)"}})
	writeRefused(t, g, clients, "create_draft", `{"in_reply_to_message_id":"m1","body":"x"}`, "Message m1 is not in the labels your access includes; ask the owner if your work needs it.")
	writeRefused(t, g, clients, "create_draft", `{"thread_id":"t4","body":"x"}`, "Thread t4 is not in the labels your access includes; ask the owner if your work needs it.")
	if got := requestsFor(f, "gmail POST"); len(got) != 6 {
		t.Errorf("drafts made: %v", got)
	}
}

// Why this test exists: an agent's addresses become headers of a
// message, so one that does not read as one address, that is two in
// one entry, that is outside ASCII, that has no domain Kivali can name,
// or that carries a line end to smuggle in a header, must be refused
// in words that say what to give instead, before anything reaches
// Google. So must a new email with no one to send to, no subject or no
// body, arguments of the wrong type, and reply_all with nothing to
// reply to.
func TestCreateDraftRefusesWhatDoesNotRead(t *testing.T) {
	f, _, g := newFakeGmail(t)
	one := ", which is not one email address: give one address an entry, such as dana@example.com or Dana Smith <dana@example.com>."
	for args, text := range map[string]string{
		`{"to":["not an address"],"subject":"s","body":"b"}`:                         `to holds "not an address"` + one,
		`{"to":["a@example.com, b@example.com"],"subject":"s","body":"b"}`:           `to holds "a@example.com, b@example.com"` + one,
		`{"to":["a@example.com"],"cc":["dana@例え.jp"],"subject":"s","body":"b"}`:      `cc holds "dana@例え.jp"` + one,
		`{"to":["a@example.com"],"bcc":["x@localhost"],"subject":"s","body":"b"}`:    `bcc holds "x@localhost"` + one,
		`{"to":["a@example.com\r\nBcc: evil@example.net"],"subject":"s","body":"b"}`: `to holds "a@example.com Bcc: evil@example.net"` + one,
		`{"subject":"s","body":"b"}`:                                                 "to is required for a new email: name at least one recipient.",
		`{"to":["a@example.com"],"body":"b"}`:                                        "subject is required for a new email.",
		`{"to":["a@example.com"],"subject":"s"}`:                                     "body is required.",
		`{"to":"a@example.com","subject":"s","body":"b"}`:                            "to is a list of text.",
		`{"to":["a@example.com"],"subject":"s","body":"b","reply_all":1}`:            "reply_all is true or false.",
		`{"to":["a@example.com"],"subject":"s","body":"b","reply_all":true}`:         "reply_all is for a reply: give in_reply_to_message_id or thread_id too.",
		`{"body":"b","in_reply_to_message_id":"../labels"}`:                          "in_reply_to_message_id is not an id Google gives; use one as Kivali listed it.",
	} {
		writeRefused(t, g, writer(map[string]any{}), "create_draft", args, text)
	}
	if got := f.requests(); len(got) != 0 {
		t.Errorf("Google was sent %v", got)
	}
}

// Why this test exists: the body is the email's whole text, so it must
// decode back exactly, line ends as CRLF, however it is written: as it
// is when it is plain ASCII, quoted-printable for text mostly in ASCII
// or with a line too long to send as it is, base64 for text mostly
// outside it. And a name holding line ends must never become a header
// of its own.
func TestMessageEncoding(t *testing.T) {
	for body, cte := range map[string]string{
		"Plain text.\nTwo lines.":    "7bit",
		"Grüße aus Köln.\nBis bald.": "quoted-printable",
		strings.Repeat("x", 1200):    "quoted-printable",
		"東京の会議は来週です。\nよろしくお願いします。": "base64",
	} {
		m := &composition{from: mail.Address{Address: "dana@example.com"}, to: []*mail.Address{{Name: "Eve\r\nBcc: evil@example.net", Address: "eve@example.org"}},
			subject: "Hi", ownSubject: true, body: body}
		msg, err := mail.ReadMessage(bytes.NewReader(m.message("dana", monday)))
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if got := msg.Header.Get("Content-Transfer-Encoding"); got != cte {
			t.Errorf("%q is written %s, want %s", body, got, cte)
		}
		if got, want := bodyOf(t, msg), strings.ReplaceAll(body, "\n", "\r\n")+"\r\n"; got != want {
			t.Errorf("%q decodes to %q", body, got)
		}
		if _, ok := msg.Header["Bcc"]; ok {
			t.Errorf("a name's line end made a header: %v", msg.Header)
		}
	}
}

// Why this test exists: an agent may delete only drafts Kivali made,
// never one the owner, or anything else, wrote. The mark is the
// X-Kivali-Agent header, written by create_draft, so a draft without it
// must be refused in words that say so and stay in the account, and a
// draft another agent of the team wrote may go. The audit names the
// draft, its message and who wrote it.
func TestDeleteDraft(t *testing.T) {
	f, w, g := newFakeGmail(t)
	dana := writer(map[string]any{})
	writeAnswer(t, g, dana, "create_draft", `{"to":["bob@example.com"],"subject":"Hi","body":"Hello"}`)
	res := writeAnswer(t, g, dana, "delete_draft", `{"draft_id":"r-1"}`)
	if res.Content[0] != "Deleted draft r-1." || res.Audit != "deleted draft r-1 (message dm1), written by agent dana" {
		t.Errorf("delete_draft = %q, audit %q", res.Content[0], res.Audit)
	}
	if _, ok := w.draft("r-1"); ok {
		t.Error("the draft is still there")
	}

	w.addDraft("r-77", "From: dana@example.com\r\nTo: a@example.org\r\nSubject: mine\r\n\r\nhello\r\n")
	writeRefused(t, g, dana, "delete_draft", `{"draft_id":"r-77"}`, "Draft r-77 was not written by Kivali: you may delete only drafts Kivali made.")
	if _, ok := w.draft("r-77"); !ok || len(requestsFor(f, "gmail DELETE /gmail/v1/users/me/drafts/r-77")) != 0 {
		t.Error("a draft Kivali did not write was deleted")
	}

	w.addDraft("r-78", "From: dana@example.com\r\nTo: a@example.org\r\nSubject: sam's\r\nX-Kivali-Agent: sam\r\n\r\nhello\r\n")
	res = writeAnswer(t, g, dana, "delete_draft", `{"draft_id":"r-78"}`)
	if res.Audit != "deleted draft r-78 (message mr78), written by agent sam" {
		t.Errorf("another agent's draft: audit %q", res.Audit)
	}

	writeFailed(t, g, dana, "delete_draft", `{"draft_id":"r-404"}`, "Google answered 404: Requested entity was not found.")
	writeRefused(t, g, dana, "delete_draft", `{"draft_id":"../messages"}`, "draft_id is not an id Google gives; use one as Kivali listed it.")
}

// Why this test exists: labels are how an agent files, archives, stars
// and marks mail read, so a label must reach Gmail as its id however the
// agent names it: by name with capitals ignored, by id, as an option the
// access form writes, by a name holding parentheses, Gmail's own by
// theirs. Archiving is removing INBOX. Moving mail to Trash or Spam is
// not in this version and must be refused in words saying so; a label
// Gmail does not have, nothing to change, or one label both added and
// removed fail before Gmail changes anything. The answer names the
// message's labels afterwards, and the audit the ids changed.
func TestModifyLabels(t *testing.T) {
	f, w, g := newFakeGmail(t)
	searchMail(f)
	dana := writer(map[string]any{})

	res := writeAnswer(t, g, dana, "modify_labels", `{"message_id":"m1","add":["ZETA","starred"],"remove":["Inbox (INBOX)","unread"]}`)
	want := "Changed the labels of message m1: added zeta (Label_9), Starred (STARRED); removed Inbox (INBOX), Unread (UNREAD).\nIt now carries: zeta (Label_9), Starred (STARRED)."
	if res.Content[0] != want || res.Audit != "changed labels of message m1: added Label_9, STARRED; removed INBOX, UNREAD" {
		t.Errorf("modify_labels:\n%s\naudit %q", res.Content[0], res.Audit)
	}
	if got := w.body("POST", "/gmail/v1/users/me/messages/m1/modify"); got != `{"addLabelIds":["Label_9","STARRED"],"removeLabelIds":["INBOX","UNREAD"]}` {
		t.Errorf("modify body %s", got)
	}
	res = writeAnswer(t, g, dana, "modify_labels", `{"message_id":"m3","add":["Clients (old)","Label_3"]}`)
	if !strings.HasSuffix(res.Content[0], "It now carries: zeta (Label_9), Clients (old) (Label_12), Alpha/Work (Label_3).") {
		t.Errorf("by a name with parentheses and an id:\n%s", res.Content[0])
	}
	res = writeAnswer(t, g, dana, "modify_labels", `{"message_id":"m5","remove":["INBOX"]}`)
	if res.Content[0] != "Changed the labels of message m5: removed Inbox (INBOX).\nIt now carries: Clients (old) (Label_12)." {
		t.Errorf("archive:\n%s", res.Content[0])
	}
	modified := len(requestsFor(f, "gmail POST"))

	noTrash := "Kivali does not move mail to Trash or Spam in this version. To take a message out of the inbox, remove INBOX, which archives it."
	writeRefused(t, g, dana, "modify_labels", `{"message_id":"m1","add":["Trash"]}`, noTrash)
	writeRefused(t, g, dana, "modify_labels", `{"message_id":"m1","add":["SPAM"],"remove":["INBOX"]}`, noTrash)
	writeFailed(t, g, dana, "modify_labels", `{"message_id":"m1","add":["Nope"]}`, `Gmail has no label "Nope"; name a label by its name or its id, as list_labels lists them.`)
	writeRefused(t, g, dana, "modify_labels", `{"message_id":"m1"}`, "Name at least one label in add or remove.")
	writeRefused(t, g, dana, "modify_labels", `{"message_id":"m1","add":["STARRED"],"remove":["starred"]}`, "STARRED is both added and removed; name it in one of add or remove.")
	if got := len(requestsFor(f, "gmail POST")); got != modified {
		t.Errorf("Gmail was asked to change labels %d more times", got-modified)
	}

	f.failWith("/gmail/v1/users/me/messages/m1/modify", 403, `{"error":{"code":403,"message":"Request had insufficient authentication scopes.","details":[{"reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}]}}`)
	writeFailed(t, g, dana, "modify_labels", `{"message_id":"m1","add":["STARRED"]}`, "Google refused this (HTTP 403): the owner's sign-in does not allow it. Ask the owner.")
}

// Why this test exists: the labels the owner names hold what an agent
// may change as much as what it may read. A message carrying none of
// them must be refused as not included, and a label outside them may be
// neither added nor removed, but for INBOX, UNREAD and STARRED, so the
// agent can archive, mark read and star what it may read; each refusal
// comes before Gmail changes anything. The answer names only the labels
// the agent may use, not others the message carries.
func TestModifyLabelsRestricted(t *testing.T) {
	f, _, g := newFakeGmail(t)
	searchMail(f)
	withMail(f, mailMsg{id: "m7", thread: "t7", labels: []string{"Label_12", "Label_3"}, at: monday, from: "x@example.org", subject: "Seven", plain: "seven"})
	clients := writer(map[string]any{"gmail_labels": []string{"Clients (old) (Label_12)"}})

	writeRefused(t, g, clients, "modify_labels", `{"message_id":"m3","add":["STARRED"]}`, "Message m3 is not in the labels your access includes; ask the owner if your work needs it.")
	outside := "; you may add or remove only the labels it includes, and INBOX, UNREAD and STARRED. Ask the owner if your work needs it."
	writeRefused(t, g, clients, "modify_labels", `{"message_id":"m5","add":["zeta"]}`, "Your access does not include the label Label_9"+outside)
	writeRefused(t, g, clients, "modify_labels", `{"message_id":"m2","remove":["IMPORTANT"]}`, "Your access does not include the label IMPORTANT"+outside)
	if got := requestsFor(f, "gmail POST"); len(got) != 0 {
		t.Errorf("Gmail was asked to change %v", got)
	}

	res := writeAnswer(t, g, clients, "modify_labels", `{"message_id":"m5","add":["STARRED"],"remove":["INBOX"]}`)
	if want := "Changed the labels of message m5: added Starred (STARRED); removed Inbox (INBOX).\nOf the labels your access includes and INBOX, UNREAD and STARRED, it now carries: Clients (old) (Label_12), Starred (STARRED)."; res.Content[0] != want {
		t.Errorf("archive and star:\n%s", res.Content[0])
	}
	res = writeAnswer(t, g, clients, "modify_labels", `{"message_id":"m7","add":["UNREAD"]}`)
	if strings.Contains(res.Content[0], "Label_3") || !strings.HasSuffix(res.Content[0], "it now carries: Clients (old) (Label_12), Unread (UNREAD).") {
		t.Errorf("a label outside the access is named:\n%s", res.Content[0])
	}
	if got := requestsFor(f, "gmail /gmail/v1/users/me/messages/m5"); len(got) != 1 || got[0] != "gmail /gmail/v1/users/me/messages/m5?format=minimal" {
		t.Errorf("the message was read as %v", got)
	}
}

// Why this test exists: an access that names no labels still does not
// read Spam or Trash. Changing labels without reading the message first
// would let an empty access add INBOX to a message in Spam or Trash and
// pull it out, so modify_labels must refuse those messages as not
// included, before Gmail changes anything.
func TestModifyLabelsSpamTrashEmptyAccess(t *testing.T) {
	f, _, g := newFakeGmail(t)
	searchMail(f)
	dana := writer(map[string]any{})
	for _, id := range []string{"m4", "m6"} {
		writeRefused(t, g, dana, "modify_labels", `{"message_id":"`+id+`","add":["INBOX"]}`,
			"Message "+id+" is not in the labels your access includes; ask the owner if your work needs it.")
	}
	if got := requestsFor(f, "gmail POST"); len(got) != 0 {
		t.Errorf("Gmail was asked to change %v", got)
	}
}

// Why this test exists: create_draft says a reply's message is not a
// draft and not in Spam or Trash. That held for the thread path only; a
// reply by in_reply_to_message_id must refuse the same messages.
func TestCreateDraftReplyByIDRefusesDraftSpamTrash(t *testing.T) {
	f, _, g := newFakeGmail(t)
	replyMail(f)
	withMail(f,
		mailMsg{id: "m6b", thread: "t6", labels: []string{"SPAM"}, at: monday, from: "spam@example.org", to: "dana@example.com", subject: "Six", plain: "b"},
		mailMsg{id: "m6c", thread: "t6", labels: []string{"TRASH"}, at: monday, from: "bin@example.org", to: "dana@example.com", subject: "Six", plain: "c"},
		mailMsg{id: "m6d", thread: "t6", labels: []string{"DRAFT"}, at: monday, from: "dana@example.com", to: "zed@example.org", subject: "Six", plain: "d"},
	)
	dana := writer(map[string]any{})
	for _, id := range []string{"m6b", "m6c"} {
		writeRefused(t, g, dana, "create_draft", `{"in_reply_to_message_id":"`+id+`","body":"Hi"}`,
			"Message "+id+" is not in the labels your access includes; ask the owner if your work needs it.")
	}
	writeRefused(t, g, dana, "create_draft", `{"in_reply_to_message_id":"m6d","body":"Hi"}`,
		"Message m6d is a draft; reply to a sent or received message.")
}

// Why this test exists: sending waits for the owner's approval, which
// this version does not have, so send_email and send_draft are declared
// with the arguments they take and never given to an agent: a call with
// the send switch on answers that the action is not available, one
// without it is refused, and nothing reaches Google either way. The
// write switch gives the write actions and no send.
func TestGmailSendNotAvailable(t *testing.T) {
	f, _, g := newFakeGmail(t)
	sends := agent(map[string]any{"gmail_send": true})
	for _, name := range []string{"send_email", "send_draft"} {
		writeFailed(t, g, sends, name, `{"to":["bob@example.com"],"subject":"Hi","body":"x","draft_id":"r-1"}`, notAvailable)
		writeRefused(t, g, writer(map[string]any{}), name, `{}`, "The owner has not given you sending mail in Google Workspace; ask the owner if your work needs it.")
	}
	if got := f.requests(); len(got) != 0 {
		t.Errorf("Google was sent %v", got)
	}
	if a, b := actionNamed("send_email"), actionNamed("send_draft"); a.schema != composeSchema || b.schema != draftSchema || a.run != nil || b.run != nil {
		t.Errorf("send_email %+v, send_draft %+v", a, b)
	}
	var declared struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal([]byte(composeSchema), &declared); err != nil || len(declared.Properties) != 8 || !slices.Equal(declared.Required, []string{"body"}) {
		t.Errorf("compose schema %s: %v", composeSchema, err)
	}

	tools, err := g.Tools(context.Background(), agent(map[string]any{"gmail_write": true, "gmail_send": true}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	if !slices.Equal(names, []string{"create_draft", "delete_draft", "modify_labels"}) {
		t.Errorf("tools = %v", names)
	}
}

// Why this test exists: a display name is shown in the owner's mail
// program in place of the address, so a name that holds an address (an
// @), angle brackets, or an invisible or reordering character could make
// a recipient read as someone else. Such a name is refused in words that
// say to give a plain name or none, before anything reaches Google.
func TestCreateDraftRefusesMisleadingNames(t *testing.T) {
	f, _, g := newFakeGmail(t)
	for _, name := range []string{`ceo@bank.example`, `Dana <ceo@bank.example>`, "Dana\u202e", "Da\u200bna"} {
		entry, _ := json.Marshal(`"` + strings.ReplaceAll(name, `"`, ``) + `" <eve@example.org>`)
		args := `{"to":[` + string(entry) + `],"subject":"s","body":"b"}`
		res, err := callWrite(g, writer(map[string]any{}), "create_draft", args)
		var ref *builtinintegrations.Refusal
		if res != nil || !errors.As(err, &ref) || !strings.Contains(ref.Text, "give a plain name, or none.") {
			t.Errorf("the name %q = %+v, %v", name, res, err)
		}
	}
	if got := f.requests(); len(got) != 0 {
		t.Errorf("Google was sent %v", got)
	}
}

// Why this test exists: a draft with no Date header leaves the owner's
// mail program to date it by when it was opened, so the message writes
// its own, from the injected clock, in RFC 5322's form.
func TestCreateDraftDatesTheMessage(t *testing.T) {
	_, w, g := newFakeGmail(t)
	g.Clock = clock.NewFakeAt(monday)
	writeAnswer(t, g, writer(map[string]any{}), "create_draft", `{"to":["bob@example.com"],"subject":"Hi","body":"x"}`)
	msg, _ := draftMessage(t, w, "r-1")
	if got := msg.Header.Get("Date"); got != "Mon, 26 Oct 2026 09:00:00 +0000" {
		t.Errorf("Date: %q", got)
	}
}

// Why this test exists: a reply must be addressed by who wrote the
// message it answers: one the owner sent goes to its To even when it
// carries a Reply-To, which is for replies to its sender; the latest
// message of a conversation for thread_id alone must be mail the owner
// received or sent, never a draft, a message in Spam or one in Trash; a
// reply to a message with a long References keeps the first and the
// last 19 of them, 20 in all; and a reply to all that names more than
// 100 addresses is refused in words that tell the agent to name the
// recipients itself.
func TestCreateDraftReplyEdges(t *testing.T) {
	f, w, g := newFakeGmail(t)
	replyMail(f)
	withMail(f,
		mailMsg{id: "m5", thread: "t5", labels: []string{"SENT"}, at: monday, from: "Dana <dana@example.com>", to: "Erin <erin@example.org>", subject: "Plans", plain: "five"},
		mailMsg{id: "m6a", thread: "t6", labels: []string{"INBOX"}, at: monday, from: "zed@example.org", to: "dana@example.com", subject: "Six", plain: "a"},
		mailMsg{id: "m6b", thread: "t6", labels: []string{"SPAM"}, at: monday.Add(time.Hour), from: "spam@example.org", to: "dana@example.com", subject: "Six", plain: "b"},
		mailMsg{id: "m6c", thread: "t6", labels: []string{"TRASH"}, at: monday.Add(2 * time.Hour), from: "bin@example.org", to: "dana@example.com", subject: "Six", plain: "c"},
		mailMsg{id: "m6d", thread: "t6", labels: []string{"DRAFT"}, at: monday.Add(3 * time.Hour), from: "dana@example.com", to: "zed@example.org", subject: "Six", plain: "d"},
		mailMsg{id: "m7", thread: "t7", labels: []string{"INBOX"}, at: monday, from: "zed@example.org", to: "dana@example.com", subject: "Seven", plain: "g"},
		mailMsg{id: "m8", thread: "t8", labels: []string{"INBOX"}, at: monday, from: "zed@example.org", to: "dana@example.com", subject: "Eight", plain: "h"},
	)
	var refs, many []string
	for i := 1; i <= 25; i++ {
		refs = append(refs, fmt.Sprintf("<r%d@x.example>", i))
	}
	for i := 1; i <= 101; i++ {
		many = append(many, fmt.Sprintf("p%d@example.net", i))
	}
	for i := range f.mail {
		switch f.mail[i].ID {
		case "m5":
			f.mail[i].Payload.Headers = append(f.mail[i].Payload.Headers, gmailHeader{"Reply-To", "Billing <billing@example.org>"})
		case "m7":
			f.mail[i].Payload.Headers = append(f.mail[i].Payload.Headers, gmailHeader{"References", strings.Join(refs, " ")})
		case "m8":
			f.mail[i].Payload.Headers = append(f.mail[i].Payload.Headers, gmailHeader{"Cc", strings.Join(many, ", ")})
		}
	}
	dana := writer(map[string]any{})

	writeAnswer(t, g, dana, "create_draft", `{"in_reply_to_message_id":"m5","body":"Any news?"}`)
	msg, _ := draftMessage(t, w, "r-1")
	if got := addresses(t, msg, "To"); !slices.Equal(got, []string{"Erin <erin@example.org>"}) {
		t.Errorf("the owner's message with a Reply-To: To %q", got)
	}

	writeAnswer(t, g, dana, "create_draft", `{"thread_id":"t6","body":"Hi"}`)
	msg, _ = draftMessage(t, w, "r-2")
	if got := msg.Header.Get("In-Reply-To"); got != "<m6a@example.org>" {
		t.Errorf("thread t6 replied to %q, not its only received message", got)
	}

	writeAnswer(t, g, dana, "create_draft", `{"in_reply_to_message_id":"m7","body":"Hi"}`)
	msg, _ = draftMessage(t, w, "r-3")
	want := []string{"<r1@x.example>"}
	for i := 8; i <= 25; i++ {
		want = append(want, fmt.Sprintf("<r%d@x.example>", i))
	}
	want = append(want, "<m7@example.org>")
	if got := strings.Fields(msg.Header.Get("References")); !slices.Equal(got, want) {
		t.Errorf("References: %v, want %v", got, want)
	}

	writeRefused(t, g, dana, "create_draft", `{"in_reply_to_message_id":"m8","reply_all":true,"body":"Hi"}`,
		"Replying to all of message m8 names 102 addresses, and an email goes to at most 100; name the recipients in to (and cc) yourself.")
}
