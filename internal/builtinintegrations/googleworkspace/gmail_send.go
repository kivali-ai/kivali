package googleworkspace

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/textproto"
	"net/url"
	"slices"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// Gmail's sends: send_email holds an email the agent writes, and
// send_draft a draft of the account, each for the owner's approval
// (held). What the owner approves is the message as Kivali built or read
// it when it held the call, and Run sends those bytes
// (messages.send), never building or reading the message again.

// The descriptions of Gmail's sends, which the agent reads.
const (
	sendEmailWords = "Ask the owner to send an email from the owner's Gmail. Give to, cc and bcc as lists, one address an entry, a subject, and body as plain text; to reply, give in_reply_to_message_id, or thread_id alone, which must be in the labels your access includes: the reply joins its conversation and is addressed as create_draft addresses one. The email goes to the addresses alone, with no names: a name given with an address is left out. Every address must be one the owner allows you to send to." + waitsWords
	sendDraftWords = "Ask the owner to send a draft in the owner's Gmail, by its id: one create_draft wrote, or one the owner wrote. It is sent as it is when you ask, and then deleted from Drafts. Every address it names must be one the owner allows you to send to. A draft with attachments is refused: the owner can send it from Gmail." + waitsWords
)

// sendPath is messages.send.
const sendPath = "/gmail/v1/users/me/messages/send"

// sendPayload is messages.send's body for the message raw: raw as Gmail
// takes it (rawMessage) and, when it is set, the thread it joins.
func sendPayload(raw []byte, thread string) ([]byte, error) {
	body := map[string]string{"raw": rawMessage(raw)}
	if thread != "" {
		body["threadId"] = thread
	}
	return json.Marshal(body)
}

// sentMessage is the message messages.send made.
type sentMessage struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

// sendMessage sends payload, as sendPayload made it, with messages.send.
// Google not answering may have sent it, so that failure says to look
// before asking again.
func (c *call) sendMessage(ctx context.Context, payload []byte) (sentMessage, error) {
	var out sentMessage
	if err := c.g.send(ctx, c.token, http.MethodPost, gmailAPI, sendPath, payload, maxAnswerBytes, &out); err != nil {
		return sentMessage{}, unsure(err, "the email was sent: search the owner's Sent mail (in:sent) before asking again")
	}
	return out, nil
}

// unsure is a send's failure: Google not answering, which may or may
// not have done it, says so and what to check (check, the end of a
// sentence); any other is err.
func unsure(err error, check string) error {
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 0 {
		return failure("Google did not answer (" + sentence(ae.Message) + "), so Kivali cannot say whether " + check + ".")
	}
	return err
}

// firstAnd names the first of list and how many more ("alice@example.org
// and 2 more"), for a card's title.
func firstAnd(list []string) string {
	switch len(list) {
	case 0:
		return "no one"
	case 1:
		return list[0]
	}
	return fmt.Sprintf("%s and %d more", list[0], len(list)-1)
}

// recipients is every address the composition goes to, lower-cased, in
// to, cc and bcc's order.
func (m *composition) recipients() []string {
	var out []string
	for _, list := range [][]*mail.Address{m.to, m.cc, m.bcc} {
		for _, a := range list {
			out = append(out, strings.ToLower(a.Address))
		}
	}
	return out
}

// repliedWords is the message a send replies to, for the owner's card:
// who sent it, by address, when, and its subject, from its headers'
// values (from, date and subject). They are the sender's text, shown and
// never sent, so they are shownLine's.
func repliedWords(from, date, subject string) string {
	w := "The message from " + cmp.Or(senderOf(from), unreadAddress)
	if t, err := mail.ParseDate(date); err == nil {
		w += ", sent " + t.UTC().Format("2006-01-02 15:04 UTC")
	}
	if s := shownLine(decodeHeader(subject), subjectMost); s != "" {
		w += ", subject: " + s
	}
	return w
}

// sendEmail holds the email the arguments describe (readComposition) for
// the owner's approval. Names are left out of its addresses, so the
// card's plain addresses are what goes; every address must be one
// send_to allows. The message is built now, without Kivali's mark,
// dated now; Run sends it (messages.send), in the replied message's
// thread for a reply.
func sendEmail(ctx context.Context, c *call) (any, error) {
	m, err := readComposition(ctx, c)
	if err != nil {
		return nil, err
	}
	for _, list := range [][]*mail.Address{m.to, m.cc, m.bcc} {
		for _, a := range list {
			a.Name = ""
		}
	}
	to := m.recipients()
	if err := c.access.checkRecipients(to); err != nil {
		return nil, err
	}
	payload, err := sendPayload(m.message("", c.g.now()), m.threadID)
	if err != nil {
		return nil, err
	}
	fields := []builtinintegrations.HoldField{field("From", m.from.Address), field("To", addressWords(m.to))}
	if len(m.cc) > 0 {
		fields = append(fields, field("Cc", addressWords(m.cc)))
	}
	if len(m.bcc) > 0 {
		fields = append(fields, field("Bcc", addressWords(m.bcc)))
	}
	fields = append(fields, field("Subject", m.subject))
	if m.reply != "" {
		fields = append(fields, field("In reply to", repliedWords(m.repliedFrom, m.repliedDate, m.repliedSubject)))
	}
	fields = append(fields, field("Body", m.body))
	reply := m.reply
	return &held{
		title:      "Send an email to " + firstAnd(to),
		fields:     fields,
		recipients: to,
		run: func(ctx context.Context, c *call) (any, error) {
			sent, err := c.sendMessage(ctx, payload)
			if err != nil {
				return nil, err
			}
			c.audit = fmt.Sprintf("sent message %s (thread %s)", sent.ID, sent.ThreadID)
			if reply != "" {
				c.audit += ", a reply to message " + reply
			}
			c.audit += ", to " + idList(to)
			return fmt.Sprintf("Sent message %s in thread %s to %s.", sent.ID, sent.ThreadID, strings.Join(to, ", ")), nil
		},
	}, nil
}

// gmailDraft is a draft as drafts.get answers it in format raw or
// minimal: its message's ids, and in format raw the whole message.
type gmailDraft struct {
	ID      string `json:"id"`
	Message struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
		Raw      string `json:"raw"`
	} `json:"message"`
}

// sendDraft holds a draft of the account for the owner's approval: any
// draft, Kivali's or not, as drafts.get answers it in format raw now.
// Its mark is taken out of the message, and the card shows the rest as
// it will go (draftFields); every address it names must be one send_to
// allows, and a draft with attachments is refused. Run sends those
// bytes (messages.send) in the draft's thread, then deletes the draft,
// unless it changed after the owner saw it: then the bytes the owner saw
// are what goes, and the changed draft stays. A draft gone by then is
// not sent.
func sendDraft(ctx context.Context, c *call) (any, error) {
	id, err := c.args.id("draft_id", draftIDPattern)
	if err != nil {
		return nil, err
	}
	var d gmailDraft
	if err := c.g.get(ctx, c.token, gmailAPI, draftPath(id)+"?format=raw", &d); err != nil {
		return nil, err
	}
	whole, err := decodeData(d.Message.Raw)
	if err != nil || d.Message.ID == "" {
		return nil, failure("Gmail's answer for draft " + id + " does not decode; try again later.")
	}
	writer := ""
	if msg, err := mail.ReadMessage(bytes.NewReader(whole)); err == nil {
		writer = strings.TrimSpace(msg.Header.Get(markerHeader))
	}
	raw := withoutHeader(whole, markerHeader)
	content, err := readDraft(raw)
	if err != nil {
		return nil, failure("Kivali cannot read draft " + id + " (" + err.Error() + "), so it cannot show the owner what it would send; the owner can send it from Gmail.")
	}
	if content.attachments > 0 {
		return nil, refuse("Draft "+id+" has attachments, which Kivali does not send; the owner can send it from Gmail.", "draft "+id+" has attachments")
	}
	if !content.hasPlain && !content.hasHTML {
		return nil, failure("Draft " + id + " has no text Kivali can show the owner; the owner can send it from Gmail.")
	}
	lists := map[string][]*mail.Address{}
	var to []string
	for _, name := range []string{"To", "Cc", "Bcc"} {
		list, err := headerAddresses(content.headers, name)
		if err != nil {
			return nil, failure(fmt.Sprintf("Kivali cannot read the addresses in the %s of draft %s, so it cannot show the owner whom it goes to; the owner can send it from Gmail.", name, id))
		}
		lists[name] = list
		for _, a := range list {
			if addr := strings.ToLower(a.Address); !slices.Contains(to, addr) {
				to = append(to, addr)
			}
		}
	}
	if len(to) == 0 {
		return nil, failure("Draft " + id + " names no one to send to; add recipients to it first.")
	}
	if err := c.access.checkRecipients(to); err != nil {
		return nil, err
	}
	fields, err := c.draftFields(ctx, id, writer, d.Message.ThreadID, content, lists)
	if err != nil {
		return nil, err
	}
	payload, err := sendPayload(raw, d.Message.ThreadID)
	if err != nil {
		return nil, err
	}
	message := d.Message.ID
	return &held{
		title:      "Send a draft to " + firstAnd(to),
		fields:     fields,
		recipients: to,
		run: func(ctx context.Context, c *call) (any, error) {
			var now gmailDraft
			err := c.g.get(ctx, c.token, gmailAPI, draftPath(id)+"?format=minimal", &now)
			var ae *apiError
			if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
				return nil, refuse("Draft "+id+" is no longer in the owner's Drafts: it was sent or deleted after the owner saw it, so Kivali sent nothing.",
					"draft "+id+" was gone")
			}
			if err != nil {
				return nil, err
			}
			sent, err := c.sendMessage(ctx, payload)
			if err != nil {
				return nil, err
			}
			c.audit = fmt.Sprintf("sent message %s (thread %s) to %s, from draft %s", sent.ID, sent.ThreadID, idList(to), id)
			done := fmt.Sprintf("as message %s in thread %s to %s", sent.ID, sent.ThreadID, strings.Join(to, ", "))
			if now.Message.ID != message {
				c.audit += ", which had changed since the owner saw it, so Kivali kept it"
				return "Draft " + id + " changed after the owner saw it, so Kivali sent it as it stood when the owner saw it, " + done + ", and kept the changed draft in Drafts.", nil
			}
			if err := c.g.change(ctx, c.token, gmailAPI, http.MethodDelete, draftPath(id), nil, nil); err != nil {
				c.audit += "; the draft could not be deleted"
				words := err.Error()
				if errors.As(err, &ae) {
					words = agentWords(ae)
				}
				return "Sent draft " + id + " " + done + ", but Kivali could not delete the draft from Drafts: " + words, nil
			}
			c.audit += ", which Kivali deleted"
			return "Sent draft " + id + " " + done + ", and deleted the draft.", nil
		},
	}, nil
}

// draftFields are a draft's card: which draft and whether Kivali wrote
// it (writer, its mark), then its message as it will go: From, To, Cc,
// Bcc, Reply-To, Subject, the message it replies to (found in its
// thread by In-Reply-To), its text body exactly and, when it has one,
// its formatted (HTML) version as a reader sees it, with its links and
// the hidden text it holds.
func (c *call) draftFields(ctx context.Context, id, writer, thread string, d *draftContent, lists map[string][]*mail.Address) ([]builtinintegrations.HoldField, error) {
	who := "Draft " + id + ", which Kivali did not write"
	switch {
	case agentSlugPattern.MatchString(writer):
		who = "Draft " + id + ", which Kivali wrote (agent " + writer + ")"
	case writer != "":
		who = "Draft " + id + ", which Kivali wrote"
	}
	fields := []builtinintegrations.HoldField{field("Draft", who)}
	if from := headerAll(d.headers, "From"); from != "" {
		fields = append(fields, field("From", cmp.Or(senderOf(from), oneHeader(from))))
	} else {
		fields = append(fields, field("From", "None given: Gmail sends it from the signed-in account"))
	}
	for _, name := range []string{"To", "Cc", "Bcc"} {
		if list := lists[name]; len(list) > 0 {
			fields = append(fields, field(name, namedAddresses(list)))
		}
	}
	if v := headerAll(d.headers, "Reply-To"); v != "" {
		fields = append(fields, field("Reply-To", oneHeader(v)))
	}
	fields = append(fields, field("Subject", cmp.Or(oneHeader(header(d.headers, "Subject")), "None")))
	if inReplyTo := msgIDPattern.FindString(header(d.headers, "In-Reply-To")); inReplyTo != "" {
		words, err := c.repliedInThread(ctx, thread, inReplyTo)
		if err != nil {
			return nil, err
		}
		fields = append(fields, field("In reply to", words))
	}
	switch {
	case d.hasPlain:
		fields = append(fields, field("Body", d.plain))
	default:
		fields = append(fields, field("Body", "None: the draft has only a formatted version"))
	}
	if d.hasHTML {
		links := newLinkList()
		text, hidden := htmlToText(d.html, links)
		parts := []string{text}
		if l := links.listing(maxLinks); l != "" {
			parts = append(parts, l)
		}
		if n := links.moreNote(); n != "" {
			parts = append(parts, n)
		}
		if hidden > 0 {
			parts = append(parts, fmt.Sprintf("It also holds %d characters a reader does not see, which go with it.", hidden))
		}
		fields = append(fields, field("Formatted version, as a reader sees it", shownText(strings.Join(parts, "\n\n"))))
	}
	return fields, nil
}

// oneHeader is a header's value as a reader of the message sees it: its
// encoded words decoded and its runs of white space one space. It is
// what goes, so nothing else of it is changed.
func oneHeader(v string) string {
	return strings.Join(strings.Fields(decodeHeader(v)), " ")
}

// namedAddresses is a draft's addresses for its card: each address,
// then the name the draft gives it in parentheses, address first so a
// name cannot pass for another address.
func namedAddresses(list []*mail.Address) string {
	parts := make([]string, len(list))
	for i, a := range list {
		parts[i] = a.Address
		if a.Name != "" {
			parts[i] += " (" + a.Name + ")"
		}
	}
	return strings.Join(parts, ", ")
}

// repliedInThread is the message of thread whose Message-ID is id, in
// repliedWords' words; for one the thread does not hold, its
// Message-ID alone.
func (c *call) repliedInThread(ctx context.Context, thread, id string) (string, error) {
	if thread != "" {
		q := url.Values{"format": {"metadata"}, "metadataHeaders": {"From", "Date", "Subject", "Message-ID"}}
		var t struct {
			Messages []gmailMessage `json:"messages"`
		}
		err := c.g.get(ctx, c.token, gmailAPI, "/gmail/v1/users/me/threads/"+url.PathEscape(thread)+"?"+q.Encode(), &t)
		var ae *apiError
		if err != nil && (!errors.As(err, &ae) || ae.Status != http.StatusNotFound) {
			return "", err
		}
		for _, m := range t.Messages {
			hs := m.Payload.Headers
			if msgIDPattern.FindString(header(hs, "Message-ID")) == id {
				return repliedWords(header(hs, "From"), header(hs, "Date"), header(hs, "Subject")), nil
			}
		}
	}
	return "The message " + id + ", which is not in the draft's conversation", nil
}

// withoutHeader is the message raw without the header name, every
// line of it and of its folding; the rest byte for byte.
func withoutHeader(raw []byte, name string) []byte {
	end := len(raw)
	for _, sep := range []string{"\r\n\r\n", "\n\n"} {
		if i := bytes.Index(raw, []byte(sep)); i >= 0 && i < end {
			end = i
		}
	}
	var out bytes.Buffer
	dropping := false
	for rest := raw[:end]; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line = rest[:i+1]
		}
		rest = rest[len(line):]
		if line[0] == ' ' || line[0] == '\t' {
			if !dropping {
				out.Write(line)
			}
			continue
		}
		colon := bytes.IndexByte(line, ':')
		dropping = colon > 0 && strings.EqualFold(strings.TrimSpace(string(line[:colon])), name)
		if !dropping {
			out.Write(line)
		}
	}
	out.Write(raw[end:])
	return out.Bytes()
}

// draftContent is a draft's message as its card shows it: its headers,
// its first text part and its first HTML part that are not
// attachments, decoded, and how many attachments it has.
type draftContent struct {
	headers           []gmailHeader
	plain, html       string
	hasPlain, hasHTML bool
	attachments       int
}

// The bounds of reading a draft: its parts in all and how deep they
// nest.
const (
	maxDraftParts = 100
	maxDraftDepth = 10
)

// readDraft reads raw, a message, into its headers and its body's
// parts: text/plain and text/html each decoded from its
// Content-Transfer-Encoding and its character set, line ends "\n";
// any part with a file name, said to be an attachment, or of another
// type counted as an attachment. An error says why it does not read.
func readDraft(raw []byte) (*draftContent, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("its headers do not read")
	}
	d := &draftContent{}
	for name, values := range msg.Header {
		for _, v := range values {
			d.headers = append(d.headers, gmailHeader{Name: name, Value: v})
		}
	}
	slices.SortStableFunc(d.headers, func(a, b gmailHeader) int { return strings.Compare(a.Name, b.Name) })
	parts := 0
	var walk func(h textproto.MIMEHeader, body io.Reader, depth int) error
	walk = func(h textproto.MIMEHeader, body io.Reader, depth int) error {
		if parts++; parts > maxDraftParts || depth > maxDraftDepth {
			return errors.New("it has too many parts")
		}
		media, params, err := mime.ParseMediaType(h.Get("Content-Type"))
		if err != nil {
			media, params = "text/plain", map[string]string{}
		}
		disposition, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
		if disposition == "attachment" || dparams["filename"] != "" || params["name"] != "" {
			d.attachments++
			return nil
		}
		if strings.HasPrefix(media, "multipart/") {
			mr := multipart.NewReader(body, params["boundary"])
			for {
				p, err := mr.NextRawPart()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return errors.New("its parts do not read")
				}
				if err := walk(p.Header, p, depth+1); err != nil {
					return err
				}
			}
		}
		if media != "text/plain" && media != "text/html" {
			d.attachments++
			return nil
		}
		var r io.Reader
		switch strings.ToLower(strings.TrimSpace(h.Get("Content-Transfer-Encoding"))) {
		case "quoted-printable":
			r = quotedprintable.NewReader(body)
		case "base64":
			r = base64.NewDecoder(base64.StdEncoding, body)
		case "", "7bit", "8bit", "binary":
			r = body
		default:
			return errors.New("its body's encoding is not one Kivali reads")
		}
		b, err := io.ReadAll(io.LimitReader(r, maxBodyBytes+1))
		switch {
		case err != nil:
			return errors.New("its body does not decode")
		case len(b) > maxBodyBytes:
			return errors.New("its body is larger than Kivali reads")
		}
		text := strings.ReplaceAll(decodeCharset(b, h.Get("Content-Type")), "\r\n", "\n")
		switch {
		case media == "text/plain" && !d.hasPlain:
			d.plain, d.hasPlain = text, true
		case media == "text/html" && !d.hasHTML:
			d.html, d.hasHTML = text, true
		}
		return nil
	}
	if err := walk(textproto.MIMEHeader(msg.Header), msg.Body, 0); err != nil {
		return nil, err
	}
	return d, nil
}
