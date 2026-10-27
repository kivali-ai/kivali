package googleworkspace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// The access form's fields. Per service, three switches, one per
// bucket, and the list of what an agent may use there:
//
//   - read: sees what is inside the owner's account;
//   - write: changes something inside the account that no other person
//     is told about;
//   - send: anything that makes Google deliver something to another
//     person, which always waits for the owner's approval: the switch
//     decides only whether the agent may ask.
//
// send_to holds every send to the addresses it names. A list left empty
// is everything: every label, calendar or folder, anyone to send to.
const (
	keyGmailRead     = "gmail_read"
	keyGmailWrite    = "gmail_write"
	keyGmailSend     = "gmail_send"
	keyGmailLabels   = "gmail_labels"
	keyCalendarRead  = "calendar_read"
	keyCalendarWrite = "calendar_write"
	keyCalendarSend  = "calendar_send"
	keyCalendars     = "calendars"
	keyDocsRead      = "docs_read"
	keyDocsWrite     = "docs_write"
	keyDocsSend      = "docs_send"
	keyDocsFolders   = "docs_folders"
	keySendTo        = "send_to"
)

// accessField is one field of the access form: a switch (boolean) or a
// list (array of text).
type accessField struct {
	key, title, description string
	list                    bool
}

// accessFields is the access form, in the order it is drawn.
var accessFields = []accessField{
	{key: keyGmailRead, title: "May read mail", description: "Lets the agent see what is in your Gmail, within the labels below. It changes nothing."},
	{key: keyGmailWrite, title: "May draft and label", description: "Lets the agent change your Gmail where no one else is told: drafts it writes, and labels on messages. It sends nothing."},
	{key: keyGmailSend, title: "May send mail (each send waits for your approval)", description: "Lets the agent ask to send mail: an email it writes, or a draft. Nothing is sent until you approve it, exactly as you saw it."},
	{key: keyGmailLabels, title: "Labels it may read", description: "The labels the agent may read mail in. Leave it empty for every label.", list: true},
	{key: keyCalendarRead, title: "May read calendars", description: "Lets the agent see the calendars below and their events. It changes nothing."},
	{key: keyCalendarWrite, title: "May change events", description: "Lets the agent create events on your own calendars, and change and delete the events there that you organized and that invite no one but you, telling no one. It deletes only events Kivali created. Inviting people, changing who an event invites, and changing or deleting an event that invites others each wait for your approval and need May invite and respond."},
	{key: keyCalendarSend, title: "May invite and respond (each waits for your approval)", description: "Lets the agent ask to invite people to events, to change and delete events that invite others, and to answer invitations. Nothing reaches anyone until you approve it, exactly as you saw it."},
	{key: keyCalendars, title: "Calendars it may use", description: "The calendars the agent may use. Leave it empty for every calendar.", list: true},
	{key: keyDocsRead, title: "May read documents", description: "Lets the agent find and read Google Docs and their comments, within the folders below. It changes nothing."},
	{key: keyDocsWrite, title: "May write documents", description: "Lets the agent create documents, and change documents you own or it created, telling no one."},
	{key: keyDocsSend, title: "May comment (each comment waits for your approval)", description: "Lets the agent ask to comment on documents Kivali made and to reply to comments on them, within the folders below. Nothing is posted until you approve it, exactly as you saw it."},
	{key: keyDocsFolders, title: "Folders it may read", description: "The Drive folders the agent may read and change documents in, the folders inside them included. Leave it empty for every document you can open.", list: true},
	{key: keySendTo, title: "May send to", description: "Who the agent's sends may reach: everyone an email goes to, everyone Google tells of an invitation, a change to an event or an answer, and everyone Google tells of a comment. An address such as dana@example.com, or *@example.com for everyone at a domain. Leave it empty for anyone. Every send still waits for your approval.", list: true},
}

// accessSchema is the access form as the manifest's JSON Schema, the
// fields in accessFields' order.
var accessSchema = func() string {
	var b bytes.Buffer
	b.WriteString(`{"type":"object","properties":{`)
	for i, f := range accessFields {
		if i > 0 {
			b.WriteByte(',')
		}
		prop := map[string]any{"type": "boolean", "title": f.title, "description": f.description}
		if f.list {
			prop["type"], prop["items"] = "array", map[string]string{"type": "string"}
		}
		k, _ := json.Marshal(f.key)
		v, err := json.Marshal(prop)
		if err != nil {
			panic(err)
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteString(`}}`)
	return b.String()
}()

// switchKeys is every switch of the form.
var switchKeys = []string{keyGmailRead, keyGmailWrite, keyGmailSend, keyCalendarRead, keyCalendarWrite, keyCalendarSend, keyDocsRead, keyDocsWrite, keyDocsSend}

// access is an agent's access as the owner's form fills it in: the
// switches on, and the ids each list names (optionID). A missing
// switch is off; a missing or empty list is everything.
type access struct {
	switches    map[string]bool
	gmailLabels []string
	calendars   []string
	docsFolders []string
	// sendTo is the addresses and *@domain patterns sends may go to,
	// lower-cased.
	sendTo []string
}

// readAccess reads the stored form: a switch on when it is true and
// nothing else, and every non-blank string of each list.
func readAccess(m map[string]any) access {
	a := access{switches: map[string]bool{}}
	for _, k := range switchKeys {
		if on, _ := m[k].(bool); on {
			a.switches[k] = true
		}
	}
	ids := func(key string) []string {
		out := malformed(m[key])
		for _, s := range listOf(m[key]) {
			out = append(out, optionID(s))
		}
		return out
	}
	a.gmailLabels, a.calendars, a.docsFolders = ids(keyGmailLabels), ids(keyCalendars), ids(keyDocsFolders)
	a.sendTo = malformed(m[keySendTo])
	for _, s := range listOf(m[keySendTo]) {
		a.sendTo = append(a.sendTo, strings.ToLower(s))
	}
	return a
}

// malformed is the start of a stored list's entries: nil for a list
// that is missing, null or a list, and an empty, non-nil list for any
// other value, which includes reads as matching nothing: a stored form
// that does not read as a list never widens an agent's access.
func malformed(v any) []string {
	if _, isList := v.([]any); v == nil || isList {
		return nil
	}
	return []string{}
}

// listOf is the non-blank strings of a stored list, trimmed.
func listOf(v any) []string {
	raw, _ := v.([]any)
	var out []string
	for _, x := range raw {
		if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// on reports whether the access has the switch key on.
func (a access) on(key string) bool { return a.switches[key] }

// includes reports whether list, one of the access's lists of ids,
// allows id: every id when it is nil, none when it is empty and not
// nil (a stored list that did not read).
func includes(list []string, id string) bool { return list == nil || slices.Contains(list, id) }

// A live option is shown, and stored in the access, as "<name> (<id>)":
// the name the owner knows a label, a calendar or a folder by, then
// Google's id for it, which is what a call is held to. option writes
// one; optionID reads the id back: the text inside the parentheses
// that end the entry, or, for an entry that does not end so, the whole
// entry, taken as an id itself ("INBOX"). Google's ids hold no
// parentheses or spaces (a label's INBOX or Label_12, a calendar's
// address, a folder's id), so a name holding parentheses of its own
// reads right: "Clients (old) (Label_12)" is Label_12.
func option(name, id string) string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		name = id
	}
	return name + " (" + id + ")"
}

func optionID(entry string) string {
	s := strings.TrimSpace(entry)
	if strings.HasSuffix(s, ")") {
		if i := strings.LastIndex(s, " ("); i >= 0 {
			if id := s[i+2 : len(s)-1]; id != "" && !strings.ContainsAny(id, "() \t\r\n") {
				return id
			}
		}
	}
	return s
}

// bucketWords names what each switch gives, for its refusal.
var bucketWords = map[string]string{
	keyGmailRead:     "reading mail",
	keyGmailWrite:    "drafting and labelling mail",
	keyGmailSend:     "sending mail",
	keyCalendarRead:  "reading calendars",
	keyCalendarWrite: "changing events",
	keyCalendarSend:  "inviting people and answering invitations",
	keyDocsRead:      "reading documents",
	keyDocsWrite:     "writing documents",
	keyDocsSend:      "commenting on documents",
}

// notGiven is the refusal of an action whose switch the access has off.
func notGiven(bucket string) error {
	w := bucketWords[bucket]
	return refuse("The owner has not given you "+w+" in Google Workspace; ask the owner if your work needs it.", "the access does not include "+w)
}

// mayReach reports whether send_to lets a send reach addr, a
// lower-cased address: any address when the list is left empty (nil),
// else one it names, or one at a domain it names as *@domain (that
// domain alone, not the domains under it). A list that is empty and not
// nil, a stored list that did not read, lets a send reach no one.
func (a access) mayReach(addr string) bool {
	if a.sendTo == nil {
		return true
	}
	for _, p := range a.sendTo {
		if dom, ok := strings.CutPrefix(p, "*@"); ok {
			if domainOf(addr) == dom {
				return true
			}
		} else if p == addr {
			return true
		}
	}
	return false
}

// checkRecipients refuses a send that would reach an address send_to
// does not allow, naming each, before the call is held for the owner.
// The addresses are lower-cased.
func (a access) checkRecipients(addrs []string) error {
	var out []string
	for _, x := range addrs {
		if !a.mayReach(x) && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return refuse("The owner has not allowed you to send to "+idList(out)+" through Google Workspace; ask the owner if your work needs it.",
		"send_to does not include "+idList(out))
}

// checkReach refuses a send that reaches an address send_to does not
// allow (checkRecipients) or, while send_to is not left empty, people
// whose address Google does not show (unknown), whom Kivali cannot hold
// to it.
// addrs is everyone the send reaches that Kivali reads, lower-cased, the
// owner's own addresses left out.
func (a access) checkReach(addrs []string, unknown bool) error {
	if err := a.checkRecipients(addrs); err != nil {
		return err
	}
	if unknown && a.sendTo != nil {
		return refuse(unshownRefusal, "send_to cannot be checked: the send reaches people whose address Google does not show")
	}
	return nil
}

// unshownRefusal is checkReach's words for a send that reaches people
// whose address Google does not show.
const unshownRefusal = "This send also reaches people whose address Google does not show, so Kivali cannot check them against the addresses the owner allows you to send to; ask the owner if your work needs it."

// sendToEntry reports whether s, an entry of send_to lower-cased, is
// one Kivali reads: an address, or *@ and a domain with a dot in it. A
// * stands only for everyone at a domain, never for part of an address.
func sendToEntry(s string) bool {
	if dom, ok := strings.CutPrefix(s, "*@"); ok {
		return len(dom) <= 253 && strings.Contains(dom, ".") && domainName.MatchString(dom)
	}
	return len(s) <= 254 && addressChars.MatchString(s) && !strings.Contains(s, "*")
}

// CheckAccess holds an agent's access, as the owner saves it, to what
// Kivali reads: every entry of the label, calendar and folder lists one
// that reads through optionID to an id, and every entry of send_to an
// address, such as dana@example.com, or a domain, such as *@example.com
// (sendToEntry). An entry that is neither would match no one, so the
// owner is told rather than left with sends or items refused for a
// typing slip. The form has made sure the fields are lists of strings.
func (g *GoogleWorkspace) CheckAccess(m map[string]any) error {
	for _, key := range []string{keyGmailLabels, keyCalendars, keyDocsFolders} {
		raw, _ := m[key].([]any)
		for _, v := range raw {
			s, _ := v.(string)
			if optionID(s) == "" {
				return fmt.Errorf("%q is not a label, calendar or folder from the list", s)
			}
		}
	}
	raw, _ := m[keySendTo].([]any)
	for _, v := range raw {
		s, _ := v.(string)
		if !sendToEntry(strings.ToLower(strings.TrimSpace(s))) {
			return fmt.Errorf("%q under May send to is not an address, such as dana@example.com, or a domain, such as *@example.com", oneLine(s, 100))
		}
	}
	return nil
}

func refuse(text, reason string) error {
	return &builtinintegrations.Refusal{Text: text, Reason: reason}
}

// badArgument is the refusal of an argument that does not read.
func badArgument(text string) error {
	return refuse(text, "the arguments do not read")
}

// args is an action's arguments as the model gave them: a JSON object,
// numbers kept as written so a whole number can be told from another.
type args map[string]any

// readArgs reads raw as an object; empty and null are {}.
func readArgs(raw json.RawMessage) (args, error) {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return args{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, badArgument("The arguments are an object.")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, badArgument("The arguments are an object.")
	}
	return args(m), nil
}

// text is the string argument key, trimmed: "" when it is missing or
// null, and a refusal when it is another type, holds more than most
// characters or, for required, is empty.
func (a args) text(key string, required bool, most int) (string, error) {
	v, ok := a[key]
	if !ok || v == nil {
		v = ""
	}
	s, isText := v.(string)
	switch s = strings.TrimSpace(s); {
	case !isText:
		return "", badArgument(key + " is text.")
	case required && s == "":
		return "", badArgument(key + " is required.")
	case utf8.RuneCountInString(s) > most:
		return "", badArgument(fmt.Sprintf("%s is at most %d characters.", key, most))
	}
	return s, nil
}

// id is the argument key as one of Google's ids: required, and only
// the characters ok allows.
func (a args) id(key string, ok *regexp.Regexp) (string, error) {
	s, err := a.text(key, true, 256)
	if err != nil {
		return "", err
	}
	if !ok.MatchString(s) {
		return "", badArgument(key + " is not an id Google gives; use one as Kivali listed it.")
	}
	return s, nil
}

// given reports whether the call gives the argument key, null being
// none.
func (a args) given(key string) bool {
	v, ok := a[key]
	return ok && v != nil
}

// texts is the argument key as a list of text, and whether the call
// gives it: not when it is missing or null. Entries are trimmed, and
// blank ones dropped. A list that is not one of text, holds more than
// most entries, or an entry of more than each characters is refused.
func (a args) texts(key string, most, each int) (list []string, given bool, err error) {
	if !a.given(key) {
		return nil, false, nil
	}
	raw, isList := a[key].([]any)
	switch {
	case !isList:
		return nil, false, badArgument(key + " is a list of text.")
	case len(raw) > most:
		return nil, false, badArgument(fmt.Sprintf("%s holds at most %d entries.", key, most))
	}
	list = []string{}
	for _, x := range raw {
		s, isText := x.(string)
		if !isText {
			return nil, false, badArgument(key + " is a list of text.")
		}
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if utf8.RuneCountInString(s) > each {
			return nil, false, badArgument(fmt.Sprintf("Each entry of %s is at most %d characters.", key, each))
		}
		list = append(list, s)
	}
	return list, true, nil
}

// flag is the boolean argument key, and whether the call gives it: false
// and not given when it is missing or null, and a refusal when it is
// another type.
func (a args) flag(key string) (on, given bool, err error) {
	if !a.given(key) {
		return false, false, nil
	}
	b, isBool := a[key].(bool)
	if !isBool {
		return false, false, badArgument(key + " is true or false.")
	}
	return b, true, nil
}

// maybeID is the argument key as one of Google's ids, as id reads one,
// or "" when it is missing or empty.
func (a args) maybeID(key string, ok *regexp.Regexp) (string, error) {
	s, err := a.text(key, false, 256)
	if err != nil || s == "" {
		return "", err
	}
	if !ok.MatchString(s) {
		return "", badArgument(key + " is not an id Google gives; use one as Kivali listed it.")
	}
	return s, nil
}
