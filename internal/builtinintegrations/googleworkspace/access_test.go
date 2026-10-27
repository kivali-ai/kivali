package googleworkspace

import (
	"slices"
	"strings"
	"testing"
)

// Why this test exists: what the owner picks in a live list is stored
// as the option's text, and every call reads Google's id back out of
// it. The encoding must give the id for any name a label, calendar or
// folder may have (parentheses, slashes, spaces, none at all), take an
// entry with no parentheses as an id itself, and never take a name's
// own parentheses for the id.
func TestOptionRoundTrip(t *testing.T) {
	for _, tc := range []struct{ name, id, shown string }{
		{"Inbox", "INBOX", "Inbox (INBOX)"},
		{"Clients (old)", "Label_12", "Clients (old) (Label_12)"},
		{"  Work /  Q3\n", "Label_3", "Work / Q3 (Label_3)"},
		{"", "x@group.calendar.google.com", "x@group.calendar.google.com (x@group.calendar.google.com)"},
		{"Budget (2026)", "1AbC-_d", "Budget (2026) (1AbC-_d)"},
	} {
		shown := option(tc.name, tc.id)
		if shown != tc.shown || optionID(shown) != tc.id {
			t.Errorf("option(%q, %q) = %q, reads back %q", tc.name, tc.id, shown, optionID(shown))
		}
	}
	for entry, id := range map[string]string{
		"INBOX":            "INBOX",
		" Label_9 ":        "Label_9",
		"Budget (2026)":    "2026",
		"Odd (two words)":  "Odd (two words)",
		"Empty ()":         "Empty ()",
		"(Label_1)":        "(Label_1)",
		"Trailing (a)(b)":  "Trailing (a)(b)",
		"dana@example.com": "dana@example.com",
	} {
		if got := optionID(entry); got != id {
			t.Errorf("optionID(%q) = %q, want %q", entry, got, id)
		}
	}
}

// Why this test exists: the stored access form is the owner's word on
// what an agent may do, read on every call. A switch must be on only
// when stored as true; each list must read as ids, blanks dropped; and
// an empty list must allow everything while a filled one allows only
// what it names.
func TestReadAccess(t *testing.T) {
	a := readAccess(agent(map[string]any{
		"gmail_read": true, "gmail_send": "yes", "docs_write": false, "calendar_read": true,
		"gmail_labels": []string{"Inbox (INBOX)", " ", "Label_9"},
		"calendars":    []string{"Team (team@group.calendar.google.com)"},
		"send_to":      []string{"Dana@Example.com", "*@example.org"},
		"unknown":      true,
	}).Access)
	if !a.on(keyGmailRead) || !a.on(keyCalendarRead) || a.on(keyGmailSend) || a.on(keyDocsWrite) || a.on("unknown") {
		t.Errorf("switches = %v", a.switches)
	}
	if !slices.Equal(a.gmailLabels, []string{"INBOX", "Label_9"}) || !slices.Equal(a.calendars, []string{"team@group.calendar.google.com"}) ||
		a.docsFolders != nil || !slices.Equal(a.sendTo, []string{"dana@example.com", "*@example.org"}) {
		t.Errorf("lists = %+v", a)
	}
	if !includes(a.gmailLabels, "INBOX") || includes(a.gmailLabels, "SENT") || !includes(a.docsFolders, "anything") {
		t.Error("includes does not follow the lists")
	}
}

// Why this test exists: this version has no approval of sends, so the
// access form must not tell the owner that a send waits for their
// approval: an owner who read so would turn a send switch on expecting
// a step that does not exist.
func TestFormPromisesNoApproval(t *testing.T) {
	for _, f := range accessFields {
		if s := strings.ToLower(f.title + " " + f.description); strings.Contains(s, "approv") {
			t.Errorf("%s: %q, %q", f.key, f.title, f.description)
		}
	}
	for key, title := range map[string]string{keyGmailSend: "May send mail", keyCalendarSend: "May invite and respond", keyDocsSend: "May comment"} {
		if i := slices.IndexFunc(accessFields, func(f accessField) bool { return f.key == key }); i < 0 || accessFields[i].title != title {
			t.Errorf("%s is not titled %q", key, title)
		}
	}
}

// Why this test exists: every switch must have the words its refusal
// names, or an agent refused for one would read an empty cause.
func TestEverySwitchHasRefusalWords(t *testing.T) {
	for _, k := range switchKeys {
		if bucketWords[k] == "" {
			t.Errorf("%s has no words", k)
		}
	}
	for _, a := range actions {
		if !a.ownerOnly && !slices.Contains(switchKeys, a.bucket) {
			t.Errorf("%s is under %q, which is not a switch", a.name, a.bucket)
		}
	}
}
