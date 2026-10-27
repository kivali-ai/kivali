package googleworkspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// markerHeader marks a draft's message as Kivali's, naming the agent
// that wrote it. Gmail keeps no properties of an integration's own on a
// draft, so the mark is a header of the message itself. delete_draft
// deletes only a draft whose message carries it.
const markerHeader = "X-Kivali-Agent"

// draftIDPattern is a draft's id as Gmail gives it ("r-8171384447376520312").
var draftIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// msgIDPattern is one Message-ID as a header writes it: printable ASCII
// but angle brackets, between angle brackets.
var msgIDPattern = regexp.MustCompile(`<[!-;=?-~]{1,250}>`)

// The bounds of an email an agent writes: its addresses in all, an
// address entry's characters, a name's, its subject's and its body's,
// and the Message-IDs its References keep.
const (
	maxRecipients   = 100
	addressMost     = 320
	nameMost        = 200
	subjectMost     = 500
	composeBodyMost = 200000
	maxReferences   = 20
)

// composeProps are the arguments of an email an agent writes, which
// create_draft and send_email take, read by readComposition.
var composeProps = map[string]any{
	"to":                     addressListProp("who the email is to"),
	"cc":                     addressListProp("who is copied"),
	"bcc":                    addressListProp("who is copied without the other recipients seeing"),
	"subject":                map[string]string{"type": "string", "description": "the subject, one line; for a reply, leave it empty to keep the replied message's"},
	"body":                   map[string]string{"type": "string", "description": "the email's text, as plain text"},
	"in_reply_to_message_id": map[string]string{"type": "string", "description": "the id of the message this replies to"},
	"thread_id":              map[string]string{"type": "string", "description": "the id of the conversation this replies in, replying to its latest message"},
	"reply_all":              map[string]string{"type": "boolean", "description": "for a reply with no to: true addresses it to every recipient of the replied message, not only its sender"},
}

func addressListProp(what string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]string{"type": "string"},
		"description": what + ": one address an entry, such as dana@example.com or Dana Smith <dana@example.com>"}
}

// The descriptions of Gmail's write actions, which the agent reads.
const (
	createDraftWords  = "Write an email as a draft in the owner's Gmail. A draft goes to no one: it waits in the owner's Drafts, where the owner can change it and send it. Give to, cc and bcc as lists, one address an entry (dana@example.com, or Dana Smith <dana@example.com>), a subject, and body as plain text. To reply, give in_reply_to_message_id, the message's id, or thread_id alone to reply to the conversation's latest message, which must be in the labels your access includes: the draft joins its conversation, its subject gets Re: before it and is the message's own when you give none (Gmail keeps a reply in its conversation only while the subject matches), and with no to it is addressed to the message's sender (to its To when the owner sent it), or with reply_all to its sender and its other recipients, the owner left out. Answers the draft's id, its message's id, its thread's id and what the draft holds."
	deleteDraftWords  = "Delete a draft Kivali wrote, by the draft id create_draft answered. You may delete only drafts Kivali made: a draft written in Gmail, or by anything else, is refused."
	modifyLabelsWords = "Add labels to and remove labels from one message, by its id. Name each label by its name, capitals ignored, or its id, as list_labels lists them; Gmail's own go by INBOX, UNREAD, STARRED and IMPORTANT. Remove INBOX to archive the message, remove UNREAD to mark it read or add it to mark it unread, and add STARRED to star it. Kivali does not move mail to Trash or Spam. When your access names labels, the message must carry one of them, and you may add or remove only those, INBOX, UNREAD and STARRED. Answers the labels the message carries afterwards."
)

// The arguments' schemas: create_draft's and send_email's, delete_draft's
// and send_draft's, and modify_labels'.
var (
	composeSchema = schema(composeProps, "body")
	draftSchema   = schema(map[string]any{"draft_id": map[string]string{"type": "string", "description": "the draft's id, as create_draft answered it"}}, "draft_id")
	labelsSchema  = schema(map[string]any{
		"message_id": map[string]string{"type": "string", "description": "the message's id"},
		"add":        map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "the labels to add, by name or id"},
		"remove":     map[string]any{"type": "array", "items": map[string]string{"type": "string"}, "description": "the labels to remove, by name or id"},
	}, "message_id")
)

// composition is an email an agent writes, from create_draft's or
// send_email's arguments (composeProps), read and checked by
// readComposition: from the signed-in account, to its recipients, with
// its subject and plain-text body, and for a reply the message it
// answers (reply), that message's thread, and the Message-ID and
// References it threads under. message writes it as RFC 2822, and
// summary in words for the agent.
type composition struct {
	from        mail.Address
	to, cc, bcc []*mail.Address
	subject     string
	// ownSubject is false for a subject taken from the replied message,
	// which the agent did not write: summary does not quote it.
	ownSubject bool
	body       string
	reply      string
	threadID   string
	inReplyTo  string
	references []string
	// repliedFrom, repliedDate and repliedSubject are the From, Date
	// and Subject headers of the message a reply answers, as its sender
	// wrote them, for a send's card (repliedWords).
	repliedFrom, repliedDate, repliedSubject string
}

// readComposition reads the email the call's arguments write. Every
// address is one of net/mail's, in ASCII, and an entry that is not one
// is refused in words, before anything reaches Google. A new email
// needs to and a subject. A reply (in_reply_to_message_id, or thread_id
// alone) is made by replyTo. Each address is kept once, in to, cc and
// bcc's order, and an email goes to at most maxRecipients of them. The
// subject and the body are cleanText's, the subject one line.
func readComposition(ctx context.Context, c *call) (*composition, error) {
	var lists [3][]*mail.Address
	for i, key := range []string{"to", "cc", "bcc"} {
		entries, _, err := c.args.texts(key, maxRecipients, addressMost)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			a, err := readAddress(e)
			if errors.Is(err, errDisplayName) {
				return nil, badArgument(fmt.Sprintf("%s holds %q, whose name has an @, a < or > or an invisible character, which could make the recipient look like someone else: give a plain name, or none.", key, oneLine(e, 100)))
			}
			if err != nil {
				return nil, badArgument(fmt.Sprintf("%s holds %q, which is not one email address: give one address an entry, such as dana@example.com or Dana Smith <dana@example.com>.", key, oneLine(e, 100)))
			}
			lists[i] = append(lists[i], a)
		}
	}
	subject, err := c.args.text("subject", false, subjectMost)
	if err != nil {
		return nil, err
	}
	body, err := c.args.text("body", true, composeBodyMost)
	if err != nil {
		return nil, err
	}
	reply, err := c.args.maybeID("in_reply_to_message_id", messageIDPattern)
	if err != nil {
		return nil, err
	}
	thread, err := c.args.maybeID("thread_id", messageIDPattern)
	if err != nil {
		return nil, err
	}
	all, _, err := c.args.flag("reply_all")
	if err != nil {
		return nil, err
	}
	subject = oneLine(subject, subjectMost)
	replying := reply != "" || thread != ""
	switch {
	case all && !replying:
		return nil, badArgument("reply_all is for a reply: give in_reply_to_message_id or thread_id too.")
	case !replying && len(lists[0]) == 0:
		return nil, badArgument("to is required for a new email: name at least one recipient.")
	case !replying && subject == "":
		return nil, badArgument("subject is required for a new email.")
	}
	me, err := c.account(ctx)
	if err != nil {
		return nil, err
	}
	body, _ = cleanText(body)
	m := &composition{from: mail.Address{Address: me.address}, to: lists[0], cc: lists[1], bcc: lists[2],
		subject: subject, ownSubject: subject != "", body: body}
	if replying {
		if err := c.replyTo(ctx, m, reply, thread, all, me); err != nil {
			return nil, err
		}
	}
	m.distinct()
	switch n := len(m.to) + len(m.cc) + len(m.bcc); {
	case replying && all && n > maxRecipients:
		return nil, badArgument(fmt.Sprintf("Replying to all of message %s names %d addresses, and an email goes to at most %d; name the recipients in to (and cc) yourself.", m.reply, n, maxRecipients))
	case len(m.to) == 0:
		return nil, failure(fmt.Sprintf("Message %s names no one to reply to but the owner; name the recipients in to.", m.reply))
	case n > maxRecipients:
		return nil, badArgument(fmt.Sprintf("An email goes to at most %d addresses; this one names %d.", maxRecipients, n))
	}
	return m, nil
}

// replyParser reads addresses, the encoded words of their names in any
// character set the HTML package knows.
var replyParser = &mail.AddressParser{WordDecoder: headerDecoder}

// errNotAddress is an entry that is not one address; errDisplayName is
// one whose name could make the recipient look like someone else.
var (
	errNotAddress  = errors.New("not one email address")
	errDisplayName = errors.New("a name that could pass for another address")
)

// readAddress is an entry of to, cc or bcc as one address: one of
// net/mail's whose address is plain ASCII (plainAddress), its name made
// one line. A name with an @, a < or a > in it, or a Unicode format
// character (invisible, or one that reorders text), is refused: it could
// make the recipient read as another address.
func readAddress(entry string) (*mail.Address, error) {
	if strings.ContainsAny(entry, "\r\n") {
		return nil, errNotAddress
	}
	a, err := replyParser.Parse(entry)
	if err != nil || plainAddress(a) == "" {
		return nil, errNotAddress
	}
	if strings.ContainsAny(a.Name, "@<>") || strings.ContainsFunc(a.Name, func(r rune) bool { return unicode.Is(unicode.Cf, r) }) {
		return nil, errDisplayName
	}
	a.Name = oneLine(a.Name, nameMost)
	return a, nil
}

// headerAddresses is every address of the header name, each as
// readAddress reads one; an error when one does not read.
func headerAddresses(hs []gmailHeader, name string) ([]*mail.Address, error) {
	v := strings.TrimSpace(headerAll(hs, name))
	if v == "" {
		return nil, nil
	}
	list, err := replyParser.ParseList(v)
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if plainAddress(a) == "" {
			return nil, errors.New("an address is not plain ASCII")
		}
		a.Name = oneLine(a.Name, nameMost)
	}
	return list, nil
}

// replyQuery asks for the headers a reply is made from: who sent the
// message and to whom, its subject, the ids it threads under, and when
// it was sent, which a send's card names.
var replyQuery = url.Values{
	"format":          {"metadata"},
	"metadataHeaders": {"From", "Reply-To", "To", "Cc", "Subject", "Date", "Message-ID", "References", "In-Reply-To"},
}.Encode()

// replyTo makes m a reply: to the message reply, or, for thread alone,
// to the thread's latest message (latestInThread). The message must be
// one the access includes, and in thread when both are given. m joins
// its thread under its Message-ID (In-Reply-To, and References: the
// message's own References, or its In-Reply-To, then its Message-ID,
// the first and the last of them kept past maxReferences); takes its
// subject when the agent gave none, with "Re: " before either unless it
// starts so; and, when the agent named no one in to, is addressed as
// replyRecipients says.
func (c *call) replyTo(ctx context.Context, m *composition, reply, thread string, all bool, me account) error {
	var r gmailMessage
	if reply != "" {
		if err := c.g.get(ctx, c.token, gmailAPI, messagePath(reply)+"?"+replyQuery, &r); err != nil {
			return err
		}
		if !c.readable(r) {
			return notIncluded("Message", reply)
		}
		if slices.Contains(r.LabelIDs, "DRAFT") {
			return badArgument("Message " + reply + " is a draft; reply to a sent or received message.")
		}
		if thread != "" && r.ThreadID != thread {
			return badArgument(fmt.Sprintf("Message %s is not in thread %s; give one of them.", reply, thread))
		}
	} else {
		var err error
		if r, err = c.latestInThread(ctx, thread); err != nil {
			return err
		}
	}
	hs := r.Payload.Headers
	m.reply, m.threadID = r.ID, r.ThreadID
	m.repliedFrom, m.repliedDate, m.repliedSubject = header(hs, "From"), header(hs, "Date"), header(hs, "Subject")
	if id := msgIDPattern.FindString(header(hs, "Message-ID")); id != "" {
		refs := msgIDPattern.FindAllString(header(hs, "References"), -1)
		if len(refs) == 0 {
			refs = msgIDPattern.FindAllString(header(hs, "In-Reply-To"), -1)
		}
		if len(refs) == 0 || refs[len(refs)-1] != id {
			refs = append(refs, id)
		}
		if len(refs) > maxReferences {
			refs = append(refs[:1], refs[len(refs)-maxReferences+1:]...)
		}
		m.inReplyTo, m.references = id, refs
	}
	if !m.ownSubject {
		m.subject = oneLine(decodeHeader(header(hs, "Subject")), subjectMost)
	}
	if !strings.HasPrefix(strings.ToLower(m.subject), "re:") {
		m.subject = strings.TrimSpace("Re: " + m.subject)
	}
	if len(m.to) > 0 {
		return nil
	}
	to, cc, err := replyRecipients(r, me, all)
	if err != nil {
		return err
	}
	m.to = to
	if all && len(m.cc) == 0 {
		m.cc = cc
	}
	return nil
}

// latestInThread is the latest message of thread id, by when Gmail
// received it, that the access includes and is not a draft, in Spam or in Trash; refused as
// not included when the thread has such messages only outside the
// access's labels.
func (c *call) latestInThread(ctx context.Context, id string) (gmailMessage, error) {
	var t struct {
		Messages []gmailMessage `json:"messages"`
	}
	if err := c.g.get(ctx, c.token, gmailAPI, "/gmail/v1/users/me/threads/"+url.PathEscape(id)+"?"+replyQuery, &t); err != nil {
		return gmailMessage{}, err
	}
	var latest *gmailMessage
	var when int64
	outside := false
	for i, m := range t.Messages {
		if slices.ContainsFunc(m.LabelIDs, func(l string) bool { return l == "DRAFT" || l == "SPAM" || l == "TRASH" }) {
			continue
		}
		if !c.readable(m) {
			outside = true
			continue
		}
		at, _ := strconv.ParseInt(m.InternalDate, 10, 64)
		if latest == nil || at >= when {
			latest, when = &t.Messages[i], at
		}
	}
	switch {
	case latest != nil:
		return *latest, nil
	case outside:
		return gmailMessage{}, notIncluded("Thread", id)
	}
	return gmailMessage{}, failure(fmt.Sprintf("Thread %s has no message to reply to.", id))
}

// replyRecipients is whom a reply to r goes to, as Gmail's Reply and
// Reply all address one: its Reply-To, else its From, or for a message
// the owner sent (its From is the owner's), whom it went to (its To);
// with all, its sender and its To, and its Cc in cc. The owner's own
// address is left out.
func replyRecipients(r gmailMessage, me account, all bool) (to, cc []*mail.Address, err error) {
	read := func(name string) ([]*mail.Address, error) {
		list, err := headerAddresses(r.Payload.Headers, name)
		if err != nil {
			return nil, failure(fmt.Sprintf("Kivali cannot read the addresses in the %s of message %s; name the recipients in to.", name, r.ID))
		}
		return list, nil
	}
	mine := func(a *mail.Address) bool { return strings.EqualFold(a.Address, me.address) }
	sender, err := read("From")
	if err != nil {
		return nil, nil, err
	}
	own := len(sender) > 0 && !slices.ContainsFunc(sender, func(a *mail.Address) bool { return !mine(a) })
	if !own {
		var replyTo []*mail.Address
		if replyTo, err = read("Reply-To"); err != nil {
			return nil, nil, err
		}
		if len(replyTo) > 0 {
			sender = replyTo
		}
	}
	var others []*mail.Address
	if all || own {
		if others, err = read("To"); err != nil {
			return nil, nil, err
		}
	}
	switch {
	case all:
		to = append(sender, others...)
		if cc, err = read("Cc"); err != nil {
			return nil, nil, err
		}
	case own:
		to = others
	default:
		to = sender
	}
	return slices.DeleteFunc(to, mine), slices.DeleteFunc(cc, mine), nil
}

// distinct keeps each address once, where to, cc and bcc, in that
// order, first name it, capitals ignored.
func (m *composition) distinct() {
	seen := map[string]bool{}
	keep := func(list []*mail.Address) []*mail.Address {
		var out []*mail.Address
		for _, a := range list {
			if k := strings.ToLower(a.Address); !seen[k] {
				seen[k] = true
				out = append(out, a)
			}
		}
		return out
	}
	m.to, m.cc, m.bcc = keep(m.to), keep(m.cc), keep(m.bcc)
}

// foldAt is the length past which a header is folded, at a space.
const foldAt = 78

// message is the composition as an RFC 2822 message, ASCII alone in CRLF
// lines: From, Date (date, in RFC 5322's form), To, Cc, Bcc, Subject, In-Reply-To and References for a
// reply, markerHeader with marker when it is not "", then text/plain in
// UTF-8. A name or a subject outside ASCII is written in encoded words
// (net/mail and mime.QEncoding), and a header longer than foldAt is
// folded at its spaces. The body is as it is when it is ASCII in lines
// of at most 998 characters, else quoted-printable, or base64 when more
// than a third of its bytes are outside ASCII. Bcc is a header of the
// message, which is how a draft keeps it.
func (m *composition) message(marker string, date time.Time) []byte {
	var b bytes.Buffer
	writeHeader(&b, "From", m.from.String())
	writeHeader(&b, "Date", date.Format(time.RFC1123Z))
	writeHeader(&b, "To", addressLine(m.to))
	if len(m.cc) > 0 {
		writeHeader(&b, "Cc", addressLine(m.cc))
	}
	if len(m.bcc) > 0 {
		writeHeader(&b, "Bcc", addressLine(m.bcc))
	}
	writeHeader(&b, "Subject", mime.QEncoding.Encode("utf-8", m.subject))
	if m.inReplyTo != "" {
		writeHeader(&b, "In-Reply-To", m.inReplyTo)
		writeHeader(&b, "References", strings.Join(m.references, " "))
	}
	if marker != "" {
		writeHeader(&b, markerHeader, marker)
	}
	encoding, body := encodeBody(m.body)
	writeHeader(&b, "MIME-Version", "1.0")
	writeHeader(&b, "Content-Type", "text/plain; charset=UTF-8")
	writeHeader(&b, "Content-Transfer-Encoding", encoding)
	b.WriteString("\r\n")
	b.Write(body)
	return b.Bytes()
}

// rawMessage is a message as Gmail's raw field takes it: base64url.
func rawMessage(msg []byte) string { return base64.URLEncoding.EncodeToString(msg) }

func addressLine(list []*mail.Address) string {
	parts := make([]string, len(list))
	for i, a := range list {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", ")
}

// writeHeader writes one header, its line ends made spaces, folded
// before a word that would take its line past foldAt.
func writeHeader(b *bytes.Buffer, name, value string) {
	value = strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
	b.WriteString(name + ":")
	n := len(name) + 1
	for i, w := range strings.Split(value, " ") {
		if i > 0 && w != "" && n+1+len(w) > foldAt {
			b.WriteString("\r\n")
			n = 0
		}
		b.WriteString(" " + w)
		n += 1 + len(w)
	}
	b.WriteString("\r\n")
}

// encodeBody is text, whose line ends are "\n", as a body: its
// Content-Transfer-Encoding and its bytes, in CRLF lines, the last
// ended too.
func encodeBody(text string) (string, []byte) {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	crlf := strings.ReplaceAll(text, "\n", "\r\n")
	outside, longest := 0, 0
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			outside++
		}
	}
	for _, line := range strings.Split(text, "\n") {
		longest = max(longest, len(line))
	}
	switch {
	case outside == 0 && longest <= 998:
		return "7bit", []byte(crlf)
	case outside*3 > len(text):
		enc := base64.StdEncoding.EncodeToString([]byte(crlf))
		var b strings.Builder
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
		return "base64", []byte(b.String())
	}
	var b bytes.Buffer
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(crlf))
	_ = w.Close()
	return "quoted-printable", b.Bytes()
}

// summary is what the email holds, in one paragraph for the agent: what
// it replies to, who it is from and to, its subject, quoted only when
// the agent wrote it, and its length. Addresses are named without their
// names.
func (m *composition) summary() string {
	var b strings.Builder
	if m.reply != "" {
		fmt.Fprintf(&b, "A reply to message %s in thread %s", m.reply, m.threadID)
	} else {
		b.WriteString("A new email")
	}
	fmt.Fprintf(&b, ", from %s to %s", m.from.Address, addressWords(m.to))
	if len(m.cc) > 0 {
		b.WriteString("; cc " + addressWords(m.cc))
	}
	if len(m.bcc) > 0 {
		b.WriteString("; bcc " + addressWords(m.bcc))
	}
	if m.ownSubject {
		fmt.Fprintf(&b, "; subject %q", m.subject)
	} else {
		b.WriteString("; subject Re: and the replied message's subject")
	}
	fmt.Fprintf(&b, "; %d characters of plain text.", utf8.RuneCountInString(m.body))
	return b.String()
}

func addressWords(list []*mail.Address) string {
	parts := make([]string, len(list))
	for i, a := range list {
		parts[i] = a.Address
	}
	return strings.Join(parts, ", ")
}

// recipientWords is n recipients in words.
func recipientWords(n int) string {
	if n == 1 {
		return "1 recipient"
	}
	return fmt.Sprintf("%d recipients", n)
}

func draftPath(id string) string { return "/gmail/v1/users/me/drafts/" + url.PathEscape(id) }

// createDraft writes the email the arguments describe (readComposition)
// as a draft (drafts.create), marked as the calling agent's, in the
// replied message's thread for a reply. The access's send_to does not
// hold a draft: nothing leaves the account.
func createDraft(ctx context.Context, c *call) (any, error) {
	m, err := readComposition(ctx, c)
	if err != nil {
		return nil, err
	}
	msg := map[string]string{"raw": rawMessage(m.message(c.marker(), c.g.now()))}
	if m.threadID != "" {
		msg["threadId"] = m.threadID
	}
	var made struct {
		ID      string `json:"id"`
		Message struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		} `json:"message"`
	}
	if err := c.g.change(ctx, c.token, gmailAPI, http.MethodPost, "/gmail/v1/users/me/drafts", map[string]any{"message": msg}, &made); err != nil {
		return nil, err
	}
	if made.ID == "" {
		return nil, failure("Gmail answered without the draft's id; tell the owner.")
	}
	if made.Message.ThreadID != "" {
		m.threadID = made.Message.ThreadID
	}
	c.audit = fmt.Sprintf("created draft %s (message %s, thread %s)", made.ID, made.Message.ID, made.Message.ThreadID)
	if m.reply != "" {
		c.audit += ", a reply to message " + m.reply
	}
	c.audit += ", for " + recipientWords(len(m.to)+len(m.cc)+len(m.bcc))
	return fmt.Sprintf("Created draft %s: message id %s, thread id %s. Nothing was sent: the draft waits in the owner's Drafts.\n%s",
		made.ID, made.Message.ID, made.Message.ThreadID, m.summary()), nil
}

// deleteDraft deletes a draft whose message carries markerHeader, which
// any agent of the team may have written (drafts.get in format
// metadata, then drafts.delete); any other draft is refused.
func deleteDraft(ctx context.Context, c *call) (any, error) {
	id, err := c.args.id("draft_id", draftIDPattern)
	if err != nil {
		return nil, err
	}
	var d struct {
		Message gmailMessage `json:"message"`
	}
	if err := c.g.get(ctx, c.token, gmailAPI, draftPath(id)+"?format=metadata", &d); err != nil {
		return nil, err
	}
	writer := strings.TrimSpace(header(d.Message.Payload.Headers, markerHeader))
	if writer == "" {
		return nil, refuse("Draft "+id+" was not written by Kivali: you may delete only drafts Kivali made.", "draft "+id+" carries no "+markerHeader+" header")
	}
	if err := c.g.change(ctx, c.token, gmailAPI, http.MethodDelete, draftPath(id), nil, nil); err != nil {
		return nil, err
	}
	c.audit = fmt.Sprintf("deleted draft %s (message %s)", id, d.Message.ID)
	if agentSlugPattern.MatchString(writer) {
		c.audit += ", written by agent " + writer
	}
	return "Deleted draft " + id + ".", nil
}

// namedSystemLabels are Gmail's own labels modify_labels takes by name
// whether or not Gmail lists them.
var namedSystemLabels = []string{"INBOX", "UNREAD", "STARRED", "IMPORTANT", "SPAM", "TRASH"}

// alwaysChangeable are the labels an agent whose access names labels
// may add and remove besides those: the inbox (archiving), unread and
// starred.
var alwaysChangeable = []string{"INBOX", "UNREAD", "STARRED"}

// maxLabelChanges bounds the labels one call adds, and those it removes.
const maxLabelChanges = 20

// mayChange reports whether the access lets a call add or remove the
// label id.
func (c *call) mayChange(id string) bool {
	return includes(c.access.gmailLabels, id) || slices.Contains(alwaysChangeable, id)
}

// labelTable is every label of the account as Gmail lists it, Gmail's
// own included.
func (g *GoogleWorkspace) labelTable(ctx context.Context, token string) ([]gmailLabel, error) {
	var answer struct {
		Labels []gmailLabel `json:"labels"`
	}
	err := g.get(ctx, token, gmailAPI, "/gmail/v1/users/me/labels", &answer)
	return answer.Labels, err
}

// resolveLabels is the id of each entry, once each, in order. An entry
// is a label's id as Gmail lists it, or an option as the access form
// writes one; else one of namedSystemLabels, capitals ignored; else the
// name of exactly one label, capitals ignored. Any other fails in words.
func resolveLabels(entries []string, table []gmailLabel) ([]string, error) {
	var out []string
	for _, e := range entries {
		id, err := resolveLabel(e, table)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

func resolveLabel(entry string, table []gmailLabel) (string, error) {
	tries := []string{entry}
	if id := optionID(entry); id != entry {
		tries = append(tries, id)
	}
	for _, s := range tries {
		if slices.ContainsFunc(table, func(l gmailLabel) bool { return l.ID == s }) {
			return s, nil
		}
		if up := strings.ToUpper(s); slices.Contains(namedSystemLabels, up) {
			return up, nil
		}
	}
	var found []string
	for _, l := range table {
		if l.ID != "" && strings.EqualFold(strings.TrimSpace(l.Name), entry) && !slices.Contains(found, l.ID) {
			found = append(found, l.ID)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", failure(fmt.Sprintf("Gmail has no label %q; name a label by its name or its id, as list_labels lists them.", oneLine(entry, 100)))
	}
	return "", failure(fmt.Sprintf("Gmail has %d labels named %q; name the one you mean by its id: %s.", len(found), oneLine(entry, 100), strings.Join(found, ", ")))
}

// labelName is the name a label is shown by: Gmail's own by the names
// list_labels gives them (UNREAD as Unread), the rest as Gmail lists
// them, else its id.
func labelName(id string, table []gmailLabel) string {
	for _, s := range systemLabels {
		if s.id == id {
			return s.name
		}
	}
	if id == "UNREAD" {
		return "Unread"
	}
	for _, l := range table {
		if l.ID == id && strings.TrimSpace(l.Name) != "" {
			return l.Name
		}
	}
	return id
}

// modifyLabels adds labels to a message and removes others
// (messages.modify), each named as resolveLabels reads them. Adding
// TRASH or SPAM is refused: this version moves no mail there. When the
// access names labels, every label added or removed must be one of them
// or alwaysChangeable, and the message must carry one of them (read in
// format minimal, as get_message decides). The answer names the labels
// the message carries afterwards, of those the agent may change.
func modifyLabels(ctx context.Context, c *call) (any, error) {
	id, err := c.args.id("message_id", messageIDPattern)
	if err != nil {
		return nil, err
	}
	add, _, err := c.args.texts("add", maxLabelChanges, 300)
	if err != nil {
		return nil, err
	}
	remove, _, err := c.args.texts("remove", maxLabelChanges, 300)
	if err != nil {
		return nil, err
	}
	if len(add)+len(remove) == 0 {
		return nil, badArgument("Name at least one label in add or remove.")
	}
	table, err := c.g.labelTable(ctx, c.token)
	if err != nil {
		return nil, err
	}
	addIDs, err := resolveLabels(add, table)
	if err != nil {
		return nil, err
	}
	removeIDs, err := resolveLabels(remove, table)
	if err != nil {
		return nil, err
	}
	if slices.Contains(addIDs, "TRASH") || slices.Contains(addIDs, "SPAM") {
		return nil, refuse("Kivali does not move mail to Trash or Spam in this version. To take a message out of the inbox, remove INBOX, which archives it.",
			"moving mail to Trash or Spam is not supported")
	}
	for _, l := range addIDs {
		if slices.Contains(removeIDs, l) {
			return nil, badArgument(l + " is both added and removed; name it in one of add or remove.")
		}
	}
	restricted := len(c.access.gmailLabels) > 0
	if restricted {
		for _, l := range slices.Concat(addIDs, removeIDs) {
			if !c.mayChange(l) {
				return nil, refuse("Your access does not include the label "+l+"; you may add or remove only the labels it includes, and INBOX, UNREAD and STARRED. Ask the owner if your work needs it.",
					"the access does not include the label "+l)
			}
		}
	}
	// The message is read whatever the access names, so that mail in
	// Spam or Trash, which the access reads only when it names them, is
	// not changed (an INBOX add would pull it out).
	var m gmailMessage
	if err := c.g.get(ctx, c.token, gmailAPI, messagePath(id)+"?format=minimal", &m); err != nil {
		return nil, err
	}
	if !c.readable(m) {
		return nil, notIncluded("Message", id)
	}
	change := struct {
		Add    []string `json:"addLabelIds,omitempty"`
		Remove []string `json:"removeLabelIds,omitempty"`
	}{addIDs, removeIDs}
	var after gmailMessage
	if err := c.g.change(ctx, c.token, gmailAPI, http.MethodPost, messagePath(id)+"/modify", change, &after); err != nil {
		return nil, err
	}
	named := func(ids []string) string {
		parts := make([]string, len(ids))
		for i, l := range ids {
			parts[i] = option(labelName(l, table), l)
		}
		return strings.Join(parts, ", ")
	}
	var done, audit []string
	if len(addIDs) > 0 {
		done, audit = append(done, "added "+named(addIDs)), append(audit, "added "+strings.Join(addIDs, ", "))
	}
	if len(removeIDs) > 0 {
		done, audit = append(done, "removed "+named(removeIDs)), append(audit, "removed "+strings.Join(removeIDs, ", "))
	}
	c.audit = "changed labels of message " + id + ": " + strings.Join(audit, "; ")
	var carries []string
	for _, l := range after.LabelIDs {
		if !restricted || c.mayChange(l) {
			carries = append(carries, l)
		}
	}
	now := "It now carries: " + named(carries) + "."
	switch {
	case restricted && len(carries) == 0:
		now = "Of the labels your access includes and INBOX, UNREAD and STARRED, it now carries none."
	case restricted:
		now = "Of the labels your access includes and INBOX, UNREAD and STARRED, it now carries: " + named(carries) + "."
	case len(carries) == 0:
		now = "It now carries no labels."
	}
	return "Changed the labels of message " + id + ": " + strings.Join(done, "; ") + ".\n" + now, nil
}
