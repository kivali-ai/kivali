package googleworkspace

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// shareCalendar sets the calendar cal's access control list to the
// owner's own rules and rules besides.
func (f *fakeCalendar) shareCalendar(cal string, rules ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acl[cal] = append(ownerOnlyACL(), rules...)
}

// aclReads is the acl.list requests the fake was sent, as sent.
func aclReads(f *fakeCalendar) []string {
	var out []string
	for _, r := range f.requests() {
		if r.method == http.MethodGet && strings.HasSuffix(r.path, "/acl") {
			out = append(out, r.String())
		}
	}
	return out
}

// sharedCalendarRefusal is the refusal of a write to the primary
// calendar, shared with others, for an agent without calendar_send.
const sharedCalendarRefusal = "This call is a send: calendar dana@example.com is shared with others, who would see the change. The owner has not given you inviting people and answering invitations in Google Workspace, which a change others see needs; ask the owner if your work needs it."

// Why this test exists: a write is what no one else sees, and Kivali
// tells that from the calendar's access control list, read before
// every create_event, update_event and delete_event. A calendar whose
// list gives no one but the owner anything (the public's rule of role
// none, as Google lists one) or shows others only busy times
// (freeBusyReader, which carries none of an event's text) must stay a
// write, done at once and telling no one, or every owner would be asked
// to approve each change to a calendar no one reads.
func TestWriteToUnsharedCalendarStaysAWrite(t *testing.T) {
	f, g := newFakeCalendar(t)
	updateFixture(t, f)
	f.shareCalendar("dana@example.com", aclEntry("freeBusyReader", "domain", "example.com"), aclEntry("freeBusyReader", "user", "boss@example.org"))
	calAnswer(t, g, calWrites(), "create_event", `{"title":"Focus","start":"2026-10-27","end":"2026-10-27"}`)
	calAnswer(t, g, calWrites(), "update_event", `{"calendar_id":"dana@example.com","event_id":"ev7","title":"Design review"}`)
	want := []string{
		"POST /calendar/v3/calendars/dana@example.com/events?sendUpdates=none",
		"PATCH /calendar/v3/calendars/dana@example.com/events/ev7?sendUpdates=none",
	}
	var got []string
	for _, r := range f.written() {
		got = append(got, r.String())
	}
	if !slices.Equal(got, want) {
		t.Errorf("writes = %v", got)
	}
	if reads := aclReads(f); len(reads) != 2 || reads[0] != "GET /calendar/v3/calendars/dana@example.com/acl?maxResults=250" {
		t.Errorf("the calendar's sharing was read as %v", reads)
	}
}

// Why this test exists: whoever a calendar is shared with sees what an
// agent writes on it, so a create, change or delete there reaches them
// as much as an invitation does and must wait for the owner. For each
// kind of rule that shows the calendar to others (a person, a group, a
// whole domain, the public) each write must be held with nothing
// written, needing calendar_send (the Hold's Switch), its card naming
// who can see the calendar in words and its recipients the addresses
// among them. Approved, each must do what the write would have, still
// emailing no one, with If-Match of the event the owner saw, and its
// audit must say who it was shared with.
func TestWriteToSharedCalendarIsHeld(t *testing.T) {
	cases := []struct {
		name       string
		rule       map[string]any
		who        string
		recipients []string
	}{
		{"a person", aclEntry("reader", "user", "Alice@Example.org"), "alice@example.org", []string{"alice@example.org"}},
		{"a group", aclEntry("writer", "group", "team@example.com"), "team@example.com (a group)", []string{"team@example.com"}},
		{"a domain", aclEntry("reader", "domain", "example.com"), "everyone at example.com", nil},
		{"the public", aclEntry("reader", "default", ""), "anyone, as the calendar is public", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, g := newFakeCalendar(t)
			updateFixture(t, f)
			f.add(t, "dana@example.com", `{"id":"ev8","summary":"Hold","start":{"date":"2026-10-27"},"end":{"date":"2026-10-28"},"organizer":{"email":"dana@example.com","self":true},"extendedProperties":{"private":{"kivali":"riley"}}}`)
			f.shareCalendar("dana@example.com", tc.rule)

			h := calHold(t, g, calSends(nil), "create_event", `{"title":"Focus","start":"2026-10-27","end":"2026-10-27","description":"Prep"}`)
			if h.Title != `Create "Focus" on a shared calendar` || !slices.Equal(h.Recipients, tc.recipients) ||
				!slices.Equal(labelsOf(h), []string{"Calendar", "Title", "When", "Description", calendarShareLabel}) ||
				valueOf(h, calendarShareLabel) != tc.who || valueOf(h, "When") != "all day on 2026-10-27" {
				t.Errorf("create: %q %v %+v", h.Title, h.Recipients, h.Fields)
			}
			update := calHold(t, g, calSends(nil), "update_event", `{"calendar_id":"dana@example.com","event_id":"ev7","title":"Design review"}`)
			if update.Title != `Change "Review" on a shared calendar` || !slices.Equal(update.Recipients, tc.recipients) ||
				!slices.Equal(labelsOf(update), []string{"Calendar", "Title", "When", "Where", "Organizer", "Attendees", "Title changes to", calendarShareLabel}) {
				t.Errorf("update: %q %v %+v", update.Title, update.Recipients, update.Fields)
			}
			del := calHold(t, g, calSends(nil), "delete_event", `{"calendar_id":"dana@example.com","event_id":"ev8"}`)
			if del.Title != `Delete "Hold" from a shared calendar` || valueOf(del, "What happens") != "This takes the event off the calendar, for everyone who can see it." ||
				valueOf(del, calendarShareLabel) != tc.who {
				t.Errorf("delete: %q %+v", del.Title, del.Fields)
			}
			if got := f.written(); len(got) != 0 {
				t.Fatalf("Google was written before the owner approved: %v", got)
			}

			res := approve(t, h)
			if req, body := lastWrite(t, f); req != "POST /calendar/v3/calendars/dana@example.com/events?sendUpdates=none" || body["description"] != "Prep" {
				t.Errorf("created with %s %v", req, body)
			}
			if res.Audit != "created event ev001 on calendar dana@example.com, all day on 2026-10-27; shared with "+tc.who ||
				!strings.HasSuffix(res.Content[0], "Google emailed no one about it, and Kivali marked it as created by Kivali.") {
				t.Errorf("created: %q, audit %q", res.Content[0], res.Audit)
			}
			seen := f.eventOn("dana@example.com", "ev7")["etag"]
			res = approve(t, update)
			if sent := f.written(); sent[len(sent)-1].String() != "PATCH /calendar/v3/calendars/dana@example.com/events/ev7?sendUpdates=none" || sent[len(sent)-1].ifMatch != seen {
				t.Errorf("patched with %s, If-Match %q", sent[len(sent)-1], sent[len(sent)-1].ifMatch)
			}
			if res.Audit != "changed event ev7 on calendar dana@example.com: title; shared with "+tc.who {
				t.Errorf("update audit %q", res.Audit)
			}
			res = approve(t, del)
			if req, _ := lastWrite(t, f); req != "DELETE /calendar/v3/calendars/dana@example.com/events/ev8?sendUpdates=none" || f.eventOn("dana@example.com", "ev8") != nil {
				t.Errorf("deleted with %s", req)
			}
			if res.Audit != "deleted event ev8 on calendar dana@example.com, which riley created; shared with "+tc.who {
				t.Errorf("delete audit %q", res.Audit)
			}

			writes := len(f.written())
			calRefused(t, g, calWrites(), "create_event", `{"title":"Focus","start":"2026-10-27","end":"2026-10-27"}`, sharedCalendarRefusal)
			calRefused(t, g, calWrites(), "update_event", `{"calendar_id":"dana@example.com","event_id":"ev7","title":"x"}`, sharedCalendarRefusal)
			if got := f.written()[writes:]; len(got) != 0 {
				t.Errorf("a write to a shared calendar ran without calendar_send: %v", got)
			}
		})
	}
}

// Why this test exists: an invitation on a shared calendar reaches its
// attendees and shows to everyone the calendar is shared with, so its
// card must name both and its recipients hold both, while Run still
// invites with sendUpdates=all.
func TestInvitationOnSharedCalendar(t *testing.T) {
	f, g := newFakeCalendar(t)
	f.shareCalendar("dana@example.com", aclEntry("reader", "group", "team@example.com"))
	h := calHold(t, g, calSends(nil), "create_event", `{"title":"Sync","start":"2026-10-27","end":"2026-10-27","attendees":["alice@example.org"]}`)
	if h.Title != `Invite 1 person to "Sync"` || !slices.Equal(h.Recipients, []string{"alice@example.org", "team@example.com"}) ||
		valueOf(h, "Attendees") != "alice@example.org" || valueOf(h, calendarShareLabel) != "team@example.com (a group)" {
		t.Errorf("card %q %v %+v", h.Title, h.Recipients, h.Fields)
	}
	res := approve(t, h)
	if req, _ := lastWrite(t, f); req != "POST /calendar/v3/calendars/dana@example.com/events?sendUpdates=all" ||
		res.Audit != "created event ev001 on calendar dana@example.com, all day on 2026-10-27, inviting alice@example.org; shared with team@example.com (a group)" {
		t.Errorf("%s, audit %q", req, res.Audit)
	}
}

// Why this test exists: May send to must hold everyone a shared
// calendar shows a change to. An address it does not allow is refused
// by name before the call is held; a rule Kivali cannot hold to it by
// address (a domain, which may reach the organization's other domains
// too, even one May send to names with *@; the public; a person Google
// gives no address for) is refused while May send to names anyone, and
// held for the owner when it is empty.
func TestSharedCalendarHeldToSendTo(t *testing.T) {
	f, g := newFakeCalendar(t)
	limited := calSends(map[string]any{"send_to": []string{"*@example.org"}})
	create := `{"title":"Focus","start":"2026-10-27","end":"2026-10-27"}`

	f.shareCalendar("dana@example.com", aclEntry("reader", "user", "alice@example.org"))
	calHold(t, g, limited, "create_event", create)
	f.shareCalendar("dana@example.com", aclEntry("reader", "user", "bob@example.net"))
	calRefused(t, g, limited, "create_event", create, "The owner has not allowed you to send to bob@example.net through Google Workspace; ask the owner if your work needs it.")
	for _, tc := range []struct {
		rule map[string]any
		who  string
	}{
		{aclEntry("reader", "domain", "example.org"), "everyone at example.org"},
		{aclEntry("reader", "default", ""), "anyone, as the calendar is public"},
		{aclEntry("reader", "user", "not an address"), unshownSharer},
	} {
		f.shareCalendar("dana@example.com", tc.rule)
		calRefused(t, g, limited, "create_event", create,
			"Calendar dana@example.com is shared with "+tc.who+", whom Kivali cannot check against the addresses the owner allows you to send to, and they would see this change; ask the owner if your work needs it.")
		if h := calHold(t, g, calSends(nil), "create_event", create); valueOf(h, calendarShareLabel) != tc.who || len(h.Recipients) != 0 {
			t.Errorf("with May send to empty: %v %+v", h.Recipients, h.Fields)
		}
	}
	if got := f.written(); len(got) != 0 {
		t.Errorf("held and refused writes reached Google: %v", got)
	}
}

// Why this test exists: Calendar lists a calendar's rules a page at a
// time, and a rule past the first page shows the calendar to its
// people as much as one on it, so every page must be read before a
// write is taken as one no one else sees.
func TestCalendarShareReadsEveryPage(t *testing.T) {
	f, g := newFakeCalendar(t)
	var rules []map[string]any
	for i := range 250 {
		rules = append(rules, aclEntry("freeBusyReader", "user", fmt.Sprintf("p%d@example.org", i)))
	}
	f.shareCalendar("dana@example.com", append(rules, aclEntry("reader", "user", "carol@example.net"))...)
	h := calHold(t, g, calSends(nil), "create_event", `{"title":"Focus","start":"2026-10-27","end":"2026-10-27"}`)
	if !slices.Equal(h.Recipients, []string{"carol@example.net"}) {
		t.Errorf("recipients %v", h.Recipients)
	}
	if reads := aclReads(f); len(reads) != 2 || !strings.Contains(reads[1], "pageToken=250") {
		t.Errorf("acl reads %v", reads)
	}
}

// docsSender is the access of an agent that may read, write and
// comment on documents, and m besides.
func docsSender(m map[string]any) map[string]any {
	out := map[string]any{"docs_read": true, "docs_write": true, "docs_send": true}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sharedDocFixture is a document the owner owns, "Plan" (shared1), in
// the folder "Team" (fold1), its permissions the owner's and perms.
func sharedDocFixture(d *fakeDocs, perms ...map[string]any) *fakeFile {
	d.addFolder("fold1", "Team")
	doc := d.addDoc("shared1", "Plan", "dana@example.com", "", "fold1")
	doc.paras = []fakePara{{text: "Old plan"}}
	doc.perms = append(doc.ownerPermissions(), perms...)
	return doc
}

// Why this test exists: Kivali tells a write from a send by the
// document's permissions, read before append_to_document and
// replace_text. A document only its owner holds a permission on (a
// deleted account's reaching no one) must stay a write, done at once,
// its audit saying nothing of sharing; and the permissions must be
// asked for with supportsAllDrives, or Drive answers nothing for a
// shared drive's document.
func TestWriteToUnsharedDocumentStaysAWrite(t *testing.T) {
	d, f, g := newFakeDocs(t)
	gone := permission("user", "lee@example.com", false)
	gone["deleted"] = true
	sharedDocFixture(d, gone)
	res := docsAnswer(t, g, nil, "append_to_document", `{"document_id":"shared1","text":"More"}`)
	if res.Audit != "added 4 characters to the end of document shared1" {
		t.Errorf("append: %q", res.Audit)
	}
	res = docsAnswer(t, g, nil, "replace_text", `{"document_id":"shared1","find":"Old","replace":"New"}`)
	if res.Audit != "replaced 1 occurrences in document shared1" {
		t.Errorf("replace: %q", res.Audit)
	}
	if q := lastQuery(t, f, "www /drive/v3/files/shared1/permissions"); q.Get("supportsAllDrives") != "true" || q.Get("fields") != permissionFields {
		t.Errorf("the permissions were asked for with %v", q)
	}
}

// sharedDocRefusal is the refusal of a write to the kind ("document" or
// "folder") id, shared with others, for an agent without docs_send.
func sharedDocRefusal(kind, id string) string {
	return "This call is a send: " + kind + " " + id + " is shared with others, who would see the change. The owner has not given you commenting on documents in Google Workspace, which a change others see needs; ask the owner if your work needs it."
}

// Why this test exists: everyone a document is shared with sees what an
// agent adds to it or replaces in it, so each such write is a send. For
// each kind of permission that shows the document to others (a person,
// a group, a domain, anyone with the link, the public) append_to_document
// and replace_text must be held with nothing written, needing docs_send,
// the card naming the document, who can see it in words and the change
// exactly as it goes (the text to add as Kivali writes it, invisible
// characters dropped, since the owner could not see them; what is found,
// what replaces it, whether capitals count), its recipients the
// addresses among them. Approved, each must make the change, and its
// audit say who the document was shared with.
func TestWriteToSharedDocumentIsHeld(t *testing.T) {
	cases := []struct {
		name       string
		perm       map[string]any
		who        string
		recipients []string
	}{
		{"a person", permission("user", "Alice@Example.org", false), "alice@example.org", []string{"alice@example.org"}},
		{"a group", permission("group", "team@example.com", false), "team@example.com (a group)", []string{"team@example.com"}},
		{"a domain by link", permission("domain", "example.com", false), "everyone at example.com with the link", nil},
		{"a domain", permission("domain", "example.com", true), "everyone at example.com", nil},
		{"anyone with the link", permission("anyone", "", false), "anyone with the link", nil},
		{"the public", permission("anyone", "", true), "anyone, as it is public on the web", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, g := newFakeDocs(t)
			doc := sharedDocFixture(d, tc.perm)
			add := holdOf(t, g, agent(docsSender(nil)), "append_to_document", `{"document_id":"shared1","text":"# Next\nMore\u200b  "}`)
			if add.Title != `Add to "Plan", a shared document` || !slices.Equal(add.Recipients, tc.recipients) ||
				!slices.Equal(labelsOf(add), []string{"Document", fileShareLabel, "Text to add at the end"}) ||
				valueOf(add, "Document") != `"Plan" (shared1), owned by you` || valueOf(add, fileShareLabel) != tc.who ||
				valueOf(add, "Text to add at the end") != "# Next\nMore" {
				t.Errorf("append: %q %v %+v", add.Title, add.Recipients, add.Fields)
			}
			replace := holdOf(t, g, agent(docsSender(nil)), "replace_text", `{"document_id":"shared1","find":"Old","replace":"","match_case":true}`)
			if replace.Title != `Replace text in "Plan", a shared document` || !slices.Equal(replace.Recipients, tc.recipients) ||
				!slices.Equal(labelsOf(replace), []string{"Document", fileShareLabel, "Find", "Replace with", "Capitals"}) ||
				valueOf(replace, "Find") != "Old" || valueOf(replace, "Replace with") != "Nothing: each occurrence is deleted" || valueOf(replace, "Capitals") != "Must match" {
				t.Errorf("replace: %q %v %+v", replace.Title, replace.Recipients, replace.Fields)
			}
			if got := d.changes(); len(got) != 0 {
				t.Fatalf("the document was changed before the owner approved: %+v", got)
			}

			res := approve(t, add)
			if res.Content[0] != "Added 9 characters at the end of document shared1." || res.Audit != "added 9 characters to the end of document shared1; shared with "+tc.who {
				t.Errorf("appended: %q, audit %q", res.Content[0], res.Audit)
			}
			res = approve(t, replace)
			if res.Content[0] != "Replaced 1 occurrence in document shared1." || res.Audit != "replaced 1 occurrences in document shared1; shared with "+tc.who {
				t.Errorf("replaced: %q, audit %q", res.Content[0], res.Audit)
			}
			want := []fakePara{{" plan", "", false}, {"Next", "HEADING_1", false}, {"More", "NORMAL_TEXT", false}}
			if !slices.Equal(doc.paras, want) {
				t.Errorf("the document holds %+v", doc.paras)
			}

			before := len(d.changes())
			docsRefused(t, g, nil, "append_to_document", `{"document_id":"shared1","text":"x"}`, sharedDocRefusal("document", "shared1"))
			docsRefused(t, g, nil, "replace_text", `{"document_id":"shared1","find":"a","replace":"b"}`, sharedDocRefusal("document", "shared1"))
			if got := d.changes(); len(got) != before {
				t.Errorf("a write to a shared document ran without docs_send: %+v", got[before:])
			}
		})
	}
}

// Why this test exists: a document made in a folder takes the folder's
// sharing, so everyone the folder is shared with sees it. Making one in
// a shared folder, named or the first the access names, must be held,
// with nothing made, its card naming the folder, who can see it, the
// title and the text as it goes; approved, the document is made there
// with its text, the audit saying who sees it. A folder no one else can
// see, or no folder (the top of My Drive), is a write.
func TestCreateDocumentInSharedFolderIsHeld(t *testing.T) {
	d, _, g := newFakeDocs(t)
	d.addFolder("fold1", "Team")
	d.get("fold1").perms = append(d.get("fold1").ownerPermissions(), permission("group", "team@example.com", false))
	d.addFolder("fold2", "Mine")

	h := holdOf(t, g, agent(docsSender(nil)), "create_document", `{"title":"Launch   plan","body":"# Plan\nIntro","folder_id":"fold1"}`)
	if h.Title != `Create "Launch plan" in a shared folder` || !slices.Equal(h.Recipients, []string{"team@example.com"}) ||
		!slices.Equal(labelsOf(h), []string{"Folder", fileShareLabel, "Title", "Text"}) ||
		valueOf(h, "Folder") != `"Team" (fold1)` || valueOf(h, fileShareLabel) != "team@example.com (a group)" ||
		valueOf(h, "Title") != "Launch plan" || valueOf(h, "Text") != "# Plan\nIntro" {
		t.Errorf("card %q %v %+v", h.Title, h.Recipients, h.Fields)
	}
	holdOf(t, g, agent(docsSender(map[string]any{"docs_folders": []string{"Team (fold1)"}})), "create_document", `{"title":"T"}`)
	if got := d.changes(); len(got) != 0 {
		t.Fatalf("a document was made before the owner approved: %+v", got)
	}
	res := approve(t, h)
	if res.Content[0] != "Created document madeDoc1 in folder fold1 with 10 characters of text, marked as made by Kivali." ||
		res.Audit != "created document madeDoc1 in folder fold1 with 10 characters of text; shared with team@example.com (a group)" {
		t.Errorf("answer %q, audit %q", res.Content[0], res.Audit)
	}
	if doc := d.get("madeDoc1"); !slices.Equal(doc.file.Parents, []string{"fold1"}) || doc.file.AppProperties[kivaliMark] != "dana" || len(doc.paras) != 2 {
		t.Errorf("made %+v", doc)
	}

	docsRefused(t, g, nil, "create_document", `{"title":"T","folder_id":"fold1"}`, sharedDocRefusal("folder", "fold1"))
	res = docsAnswer(t, g, nil, "create_document", `{"title":"T","folder_id":"fold2"}`)
	if res.Audit != "created document madeDoc2 in folder fold2 with 0 characters of text" {
		t.Errorf("a folder no one else sees: %q", res.Audit)
	}
	res = docsAnswer(t, g, nil, "create_document", `{"title":"T"}`)
	if res.Audit != "created document madeDoc3 with 0 characters of text" {
		t.Errorf("no folder: %q", res.Audit)
	}
}

// Why this test exists: May send to must hold everyone a shared
// document or folder shows a change to: an address it does not allow
// is refused by name, and a permission Kivali cannot hold to it by
// address (a domain's, anyone's) is refused while it names anyone,
// before anything is held or written. A shared drive's document Kivali
// made shows to the drive's members, so it is held too, named as in the
// drive.
func TestSharedDocumentHeldToSendTo(t *testing.T) {
	d, _, g := newFakeDocs(t)
	doc := sharedDocFixture(d)
	limited := agent(docsSender(map[string]any{"send_to": []string{"*@example.org"}}))
	// refusal is the words of the call's refusal, "" for none.
	refusal := func(name, args string) string {
		_, err := g.Call(t.Context(), limited, testConn(), name, []byte(args))
		var ref *builtinintegrations.Refusal
		if !errors.As(err, &ref) || ref.Reason == "" {
			return ""
		}
		return ref.Text
	}
	appendArgs := `{"document_id":"shared1","text":"x"}`

	doc.perms = append(doc.ownerPermissions(), permission("user", "alice@example.org", false))
	holdOf(t, g, limited, "append_to_document", appendArgs)
	for _, tc := range []struct {
		perm  map[string]any
		words string
	}{
		{permission("user", "bob@example.net", false), "The owner has not allowed you to send to bob@example.net through Google Workspace; ask the owner if your work needs it."},
		{permission("domain", "example.org", true), "Document shared1 is shared with everyone at example.org, whom Kivali cannot check against the addresses the owner allows you to send to, and they would see this change; ask the owner if your work needs it."},
		{permission("anyone", "", false), "Document shared1 is shared with anyone with the link, whom Kivali cannot check against the addresses the owner allows you to send to, and they would see this change; ask the owner if your work needs it."},
	} {
		doc.perms = append(doc.ownerPermissions(), tc.perm)
		if got := refusal("append_to_document", appendArgs); got != tc.words {
			t.Errorf("%v: %q, want %q", tc.perm, got, tc.words)
		}
	}
	d.get("fold1").perms = append(d.get("fold1").ownerPermissions(), permission("domain", "example.org", false))
	if got := refusal("create_document", `{"title":"T","folder_id":"fold1"}`); got != "Folder fold1 is shared with everyone at example.org with the link, whom Kivali cannot check against the addresses the owner allows you to send to, and they would see this change; ask the owner if your work needs it." {
		t.Errorf("a folder shared with a domain: %q", got)
	}
	if got := d.changes(); len(got) != 0 {
		t.Errorf("held and refused writes changed Google: %+v", got)
	}

	made := d.addDoc("made1", "Made", "", "", "fold1")
	made.file.AppProperties = map[string]string{kivaliMark: "dana"}
	made.perms = append(made.ownerPermissions(), permission("user", "lee@example.org", false))
	h := holdOf(t, g, limited, "append_to_document", `{"document_id":"made1","text":"x"}`)
	if valueOf(h, "Document") != `"Made" (made1), in a shared drive` || !slices.Equal(h.Recipients, []string{"lee@example.org"}) {
		t.Errorf("a shared drive's document: %v %+v", h.Recipients, h.Fields)
	}
}

// Why this test exists: a change to a shared document approved after
// the document was deleted changes nothing, and the agent must not
// read that as a failure to try again: Run's refusal says it is gone
// and nothing was done, whether Google says so as Kivali reads the end
// of the document or as it writes.
func TestSharedDocumentGoneAtApproval(t *testing.T) {
	d, f, g := newFakeDocs(t)
	sharedDocFixture(d, permission("user", "alice@example.org", false))
	add := holdOf(t, g, agent(docsSender(nil)), "append_to_document", `{"document_id":"shared1","text":"x"}`)
	replace := holdOf(t, g, agent(docsSender(nil)), "replace_text", `{"document_id":"shared1","find":"Old","replace":"New"}`)
	missing := `{"error":{"code":404,"message":"Requested entity was not found."}}`
	f.failWith("/v1/documents/shared1", 404, missing)
	f.failWith("/v1/documents/shared1:batchUpdate", 404, missing)
	runRefused(t, add, "The document is gone; nothing was done.")
	runRefused(t, replace, "The document is gone; nothing was done.")
}
