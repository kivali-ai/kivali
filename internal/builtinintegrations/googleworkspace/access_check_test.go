package googleworkspace

import "testing"

// Why this test exists: a stored form is read on every call, and a
// list that is present but not a list (hand-edited or written by an
// older build) must never widen an agent to everything; only a list
// that is missing, null or empty means everything.
func TestMalformedListMatchesNothing(t *testing.T) {
	a := readAccess(map[string]any{"gmail_labels": "INBOX", "calendars": nil, "docs_folders": []any{}, "send_to": map[string]any{}})
	if includes(a.gmailLabels, "INBOX") || a.gmailLabels == nil {
		t.Errorf("a string where a list belongs allows %v", a.gmailLabels)
	}
	if !includes(a.calendars, "x") || !includes(a.docsFolders, "x") {
		t.Error("a null or empty list no longer means everything")
	}
	if a.sendTo == nil {
		t.Error("a send_to that is not a list reads as anyone")
	}
}

// Why this test exists: the owner's save is the one place a bad entry
// can be turned back with words. May send to is read on every send, and
// an entry that is neither an address nor *@ and a domain would match
// no one, so a send the owner meant to allow would be refused for a
// typing slip; a label, calendar or folder entry that reads to no id
// would refuse every item. Saving the access must refuse such an entry
// in words that say what to write, and take addresses and domains in
// any capitals and with spaces around them.
func TestCheckAccess(t *testing.T) {
	g := New()
	ok := map[string]any{
		"send_to":      []any{"dana@example.com", "*@Example.COM", " Sam@Mail.Example.com "},
		"gmail_labels": []any{"Inbox (INBOX)", "Label_9"},
		"calendars":    []any{"Team (team@group.calendar.google.com)"},
	}
	if err := g.CheckAccess(ok); err != nil {
		t.Errorf("a good access: %v", err)
	}
	if err := g.CheckAccess(map[string]any{}); err != nil {
		t.Errorf("an empty access: %v", err)
	}
	for _, bad := range []string{"dana", "dana@", "@example.com", "dana@example", "a b@example.com", "*dana@example.com", "dana@@example.com",
		"dana@example..com", "*", "*@localhost", "*example.com", "Dana <dana@example.com>", "*@*.example.com", ""} {
		err := g.CheckAccess(map[string]any{"send_to": []any{bad}})
		if err == nil || err.Error() != `"`+bad+`" under May send to is not an address, such as dana@example.com, or a domain, such as *@example.com` {
			t.Errorf("send_to %q: %v", bad, err)
		}
	}
	for _, key := range []string{"gmail_labels", "calendars", "docs_folders"} {
		if err := g.CheckAccess(map[string]any{key: []any{" "}}); err == nil || err.Error() != `" " is not a label, calendar or folder from the list` {
			t.Errorf("a blank %s entry: %v", key, err)
		}
	}
}
