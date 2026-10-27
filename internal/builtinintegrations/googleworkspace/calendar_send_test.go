package googleworkspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// calSends is an access with Calendar's write and send switches on,
// and m besides.
func calSends(m map[string]any) map[string]any {
	out := map[string]any{"calendar_write": true, "calendar_send": true}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// calHold is callCalendar's result, which must be a held call.
func calHold(t *testing.T, g *GoogleWorkspace, access map[string]any, name, args string) *builtinintegrations.Hold {
	t.Helper()
	return holdOf(t, g, agent(access), name, args)
}

// lastWrite is the last write the fake was sent, and its JSON body.
func lastWrite(t *testing.T, f *fakeCalendar) (string, map[string]any) {
	t.Helper()
	sent := f.written()
	if len(sent) == 0 {
		t.Fatal("nothing was written")
	}
	last := sent[len(sent)-1]
	var body map[string]any
	if last.body != "" {
		if err := json.Unmarshal([]byte(last.body), &body); err != nil {
			t.Fatal(err)
		}
	}
	return last.String(), body
}

// Why this test exists: an invitation reaches other people's inboxes
// and calendars, so a create_event that gives attendees must wait for
// the owner, with nothing written to Google until then, and the card
// must show the event exactly as it will be made: its calendar, title,
// time, place, every attendee and its description, and whom Google will
// invite, the owner left out. Approved, the event must be made with
// that body, Kivali's mark included, and sendUpdates=all, so Google
// invites them; an attendee outside May send to is refused before the
// call is held.
func TestCreateEventInvites(t *testing.T) {
	f, g := newFakeCalendar(t)
	h := calHold(t, g, calSends(nil), "create_event", `{"title":"Sync","start":"2026-10-27T09:00:00+01:00","end":"2026-10-27T10:00:00+01:00",
		"time_zone":"Europe/London","location":"Room 4","description":"Agenda","attendees":["Alice@Example.org","dana@example.com","bob@example.net"]}`)
	when := "from 2026-10-27T09:00:00+01:00 to 2026-10-27T10:00:00+01:00 (Europe/London)"
	if h.Title != `Invite 2 people to "Sync"` || !slices.Equal(h.Recipients, []string{"alice@example.org", "bob@example.net"}) ||
		!slices.Equal(labelsOf(h), []string{"Calendar", "Title", "When", "Where", "Attendees", "Description"}) {
		t.Errorf("card %q %v %+v", h.Title, h.Recipients, h.Fields)
	}
	for label, want := range map[string]string{"Calendar": "dana@example.com", "Title": "Sync", "When": when, "Where": "Room 4",
		"Attendees": "alice@example.org, bob@example.net, dana@example.com", "Description": "Agenda"} {
		if got := valueOf(h, label); got != want {
			t.Errorf("%s: %q, want %q", label, got, want)
		}
	}
	if got := f.written(); len(got) != 0 {
		t.Fatalf("Google was written before the owner approved: %v", got)
	}
	res := approve(t, h)
	if res.Content[0] != "Created event ev001 on calendar dana@example.com, "+when+", and Google invited alice@example.org, bob@example.net. Kivali marked it as created by Kivali." ||
		res.Audit != "created event ev001 on calendar dana@example.com, "+when+", inviting alice@example.org, bob@example.net" {
		t.Errorf("created: %q, audit %q", res.Content[0], res.Audit)
	}
	req, body := lastWrite(t, f)
	want := map[string]any{
		"summary": "Sync", "description": "Agenda", "location": "Room 4",
		"start":              map[string]any{"dateTime": "2026-10-27T09:00:00+01:00", "timeZone": "Europe/London"},
		"end":                map[string]any{"dateTime": "2026-10-27T10:00:00+01:00", "timeZone": "Europe/London"},
		"extendedProperties": map[string]any{"private": map[string]any{"kivali": "dana"}},
		"attendees":          []any{map[string]any{"email": "alice@example.org"}, map[string]any{"email": "bob@example.net"}, map[string]any{"email": "dana@example.com"}},
	}
	if req != "POST /calendar/v3/calendars/dana@example.com/events?sendUpdates=all" || fmt.Sprint(body) != fmt.Sprint(want) {
		t.Errorf("%s with %v", req, body)
	}

	calRefused(t, g, calSends(map[string]any{"send_to": []string{"*@example.org"}}), "create_event",
		`{"title":"x","start":"2026-10-27","end":"2026-10-27","attendees":["alice@example.org","bob@example.net"]}`,
		"The owner has not allowed you to send to bob@example.net through Google Workspace; ask the owner if your work needs it.")
	if got := len(f.written()); got != 1 {
		t.Errorf("%d writes", got)
	}
}

// Why this test exists: isSend routes an update_event call; a send
// must wait for the owner and say exactly what changes. Adding someone
// to an event that invited only the owner, changing an event that
// invites someone else (even its title alone), removing someone, and
// changing an event someone else organized are each held, with the
// event as it is, each change, and everyone it reaches: those it
// invites after the change, those it removes, who are told too, and
// its organizer, never the owner. Everyone it reaches is held to May
// send to, those it already invites included, since Google tells them
// of the change. Approved, the patch is the one captured, with
// sendUpdates=all and If-Match of the event as the owner saw it, and
// keeps each remaining attendee as Google gave them, their answer
// included.
func TestUpdateEventSends(t *testing.T) {
	f, g := newFakeCalendar(t)
	updateFixture(t, f)
	sends := calSends(nil)

	h := calHold(t, g, sends, "update_event", `{"calendar_id":"dana@example.com","event_id":"ev7","title":"Design review","attendees":["dana@example.com","carol@example.net"]}`)
	if h.Title != `Change "Review", telling 1 person` || !slices.Equal(h.Recipients, []string{"carol@example.net"}) ||
		!slices.Equal(labelsOf(h), []string{"Calendar", "Title", "When", "Where", "Organizer", "Attendees", "Title changes to", "Attendees added"}) ||
		valueOf(h, "Organizer") != "dana@example.com, the owner" || valueOf(h, "Attendees") != "dana@example.com" ||
		valueOf(h, "Title changes to") != "Design review" || valueOf(h, "Attendees added") != "carol@example.net" {
		t.Errorf("an invitation added: %q %v %+v", h.Title, h.Recipients, h.Fields)
	}
	if got := f.written(); len(got) != 0 {
		t.Fatalf("Google was written before the owner approved: %v", got)
	}
	seenTag := f.eventOn("dana@example.com", "ev7")["etag"]
	res := approve(t, h)
	if sent := f.written(); sent[len(sent)-1].ifMatch == "" || sent[len(sent)-1].ifMatch != seenTag {
		t.Errorf("patched with If-Match %q, the event's etag %v", sent[len(sent)-1].ifMatch, seenTag)
	}
	req, body := lastWrite(t, f)
	attendees, _ := body["attendees"].([]any)
	if req != "PATCH /calendar/v3/calendars/dana@example.com/events/ev7?sendUpdates=all" || body["summary"] != "Design review" || len(attendees) != 2 ||
		fmt.Sprint(attendees[0]) != "map[email:dana@example.com organizer:true responseStatus:accepted self:true]" || fmt.Sprint(attendees[1]) != "map[email:carol@example.net]" {
		t.Errorf("%s with %v", req, body)
	}
	if res.Audit != "changed event ev7 on calendar dana@example.com: title, attendees; told carol@example.net" ||
		!strings.HasSuffix(res.Content[0], ". Google told carol@example.net.") {
		t.Errorf("answer %q, audit %q", res.Content[0], res.Audit)
	}

	calRefused(t, g, calSends(map[string]any{"send_to": []string{"bob@example.com"}}), "update_event", `{"calendar_id":"dana@example.com","event_id":"ev6","location":"Room 9"}`,
		"The owner has not allowed you to send to alice@example.org through Google Workspace; ask the owner if your work needs it.")
	h = calHold(t, g, calSends(map[string]any{"send_to": []string{"alice@example.org"}}), "update_event", `{"calendar_id":"dana@example.com","event_id":"ev6","location":"Room 9"}`)
	if h.Title != `Change "Sync", telling 1 person` || !slices.Equal(h.Recipients, []string{"alice@example.org"}) || valueOf(h, "Place changes to") != "Room 9" {
		t.Errorf("an event inviting alice: %q %v %+v", h.Title, h.Recipients, h.Fields)
	}
	approve(t, h)
	if _, body := lastWrite(t, f); fmt.Sprint(body) != "map[location:Room 9]" {
		t.Errorf("patched %v", body)
	}

	h = calHold(t, g, sends, "update_event", `{"calendar_id":"dana@example.com","event_id":"ev6","attendees":["dana@example.com"]}`)
	if valueOf(h, "Attendees removed") != "alice@example.org" || !slices.Equal(h.Recipients, []string{"alice@example.org"}) {
		t.Errorf("alice removed: %v %+v", h.Recipients, h.Fields)
	}
	h = calHold(t, g, sends, "update_event", `{"calendar_id":"dana@example.com","event_id":"ev5","description":"Bring notes"}`)
	if !slices.Equal(h.Recipients, []string{"alice@example.org"}) || valueOf(h, "Organizer") != "alice@example.org" ||
		valueOf(h, "Description now") != "None" || valueOf(h, "Description changes to") != "Bring notes" {
		t.Errorf("alice's event: %v %+v", h.Recipients, h.Fields)
	}

	writes := len(f.written())
	calRefused(t, g, calSends(map[string]any{"send_to": []string{"*@example.com"}}), "update_event",
		`{"calendar_id":"dana@example.com","event_id":"ev6","attendees":["dana@example.com","alice@example.org","eve@evil.example"]}`,
		"The owner has not allowed you to send to alice@example.org, eve@evil.example through Google Workspace; ask the owner if your work needs it.")
	calRefused(t, g, sends, "update_event", `{"calendar_id":"dana@example.com","event_id":"ev6","attendees":["dana@example.com","alice@example.org"]}`,
		"Give at least one field to change: title, start, end, time_zone, description, location, all_day or attendees.")
	if got := f.written()[writes:]; len(got) != 0 {
		t.Errorf("refused sends were written: %v", got)
	}
}

// Why this test exists: deleting an event that invites others cancels
// it on their calendars, so it must wait for the owner, showing the
// event and saying it is cancelled for everyone, with nothing deleted
// until the owner approves; then Google is asked to tell them. An event
// Kivali did not create is still refused first, send or not.
func TestDeleteEventSends(t *testing.T) {
	f, g := newFakeCalendar(t)
	updateFixture(t, f)
	f.add(t, "dana@example.com", `{"id":"ev10","summary":"Shared hold","start":{"date":"2026-10-27"},"end":{"date":"2026-10-28"},"organizer":{"email":"dana@example.com","self":true},
		"attendees":[{"email":"alice@example.org"}],"extendedProperties":{"private":{"kivali":"riley"}}}`)
	h := calHold(t, g, calSends(nil), "delete_event", `{"calendar_id":"dana@example.com","event_id":"ev10"}`)
	if h.Title != `Delete "Shared hold", telling 1 person` || !slices.Equal(h.Recipients, []string{"alice@example.org"}) ||
		valueOf(h, "What happens") != "This cancels the event for everyone invited." || valueOf(h, "When") != "all day on 2026-10-27" {
		t.Errorf("card %q %v %+v", h.Title, h.Recipients, h.Fields)
	}
	if got := f.written(); len(got) != 0 || f.eventOn("dana@example.com", "ev10") == nil {
		t.Fatalf("deleted before the owner approved: %v", got)
	}
	res := approve(t, h)
	if req, _ := lastWrite(t, f); req != "DELETE /calendar/v3/calendars/dana@example.com/events/ev10?sendUpdates=all" || f.eventOn("dana@example.com", "ev10") != nil {
		t.Errorf("deleted with %s", req)
	}
	if res.Audit != "deleted event ev10 on calendar dana@example.com, which riley created; told alice@example.org" {
		t.Errorf("audit %q", res.Audit)
	}
	f.add(t, "dana@example.com", `{"id":"ev12","summary":"Shared hold","start":{"date":"2026-10-27"},"end":{"date":"2026-10-28"},"organizer":{"email":"dana@example.com","self":true},
		"attendees":[{"email":"alice@example.org"}],"extendedProperties":{"private":{"kivali":"riley"}}}`)
	calRefused(t, g, calSends(map[string]any{"send_to": []string{"*@example.com"}}), "delete_event", `{"calendar_id":"dana@example.com","event_id":"ev12"}`,
		"The owner has not allowed you to send to alice@example.org through Google Workspace; ask the owner if your work needs it.")
	calRefused(t, g, calSends(nil), "delete_event", `{"calendar_id":"dana@example.com","event_id":"ev6"}`,
		"You may delete only events Kivali created, and Kivali did not create event ev6.")
}

// Why this test exists: answering an invitation tells its organizer, so
// it waits for the owner, the organizer held to May send to, and it
// must change the owner's own answer and
// nothing else of an event someone else organized: the patch carries the
// owner's attendee alone, with attendeesOmitted, as Calendar takes a
// participant's answer, so no other attendee's entry is touched. The
// card shows the event, its organizer and the answer and note as they
// will go. An event the owner organized, or one that does not invite
// the owner, has no invitation to answer and is refused, as is an answer
// Calendar does not take and a calendar that is not the owner's own,
// where it would answer for someone else.
func TestRespondToInvitation(t *testing.T) {
	f, g := newFakeCalendar(t)
	updateFixture(t, f)
	f.add(t, "dana@example.com", `{"id":"ev11","summary":"Bob's","start":{"date":"2026-10-27"},"end":{"date":"2026-10-28"},"organizer":{"email":"bob@example.net"},"attendees":[{"email":"bob@example.net"}]}`)
	answers := map[string]any{"calendar_send": true}
	h := calHold(t, g, answers, "respond_to_invitation", `{"calendar_id":"dana@example.com","event_id":"ev5","response":"declined","comment":"Away that week"}`)
	if h.Title != `Answer no to "Alice's"` || !slices.Equal(h.Recipients, []string{"alice@example.org"}) ||
		!slices.Equal(labelsOf(h), []string{"Calendar", "Title", "When", "Organizer", "Your response", "Comment"}) ||
		valueOf(h, "Organizer") != "alice@example.org" || valueOf(h, "Your response") != "No (declined)" || valueOf(h, "Comment") != "Away that week" {
		t.Errorf("card %q %v %+v", h.Title, h.Recipients, h.Fields)
	}
	if got := f.written(); len(got) != 0 {
		t.Fatalf("Google was written before the owner approved: %v", got)
	}
	res := approve(t, h)
	req, body := lastWrite(t, f)
	if req != "PATCH /calendar/v3/calendars/dana@example.com/events/ev5?sendUpdates=all" ||
		fmt.Sprint(body) != "map[attendees:[map[comment:Away that week email:dana@example.com responseStatus:declined]] attendeesOmitted:true]" {
		t.Errorf("%s with %v", req, body)
	}
	if res.Content[0] != "Answered no to event ev5 on calendar dana@example.com. Google told its organizer, alice@example.org." ||
		res.Audit != "answered declined to event ev5 on calendar dana@example.com; told alice@example.org" {
		t.Errorf("answer %q, audit %q", res.Content[0], res.Audit)
	}

	before := len(f.written())
	calRefused(t, g, answers, "respond_to_invitation", `{"calendar_id":"dana@example.com","event_id":"ev6","response":"accepted"}`,
		"The owner organized event ev6, so there is no invitation to answer.")
	calRefused(t, g, answers, "respond_to_invitation", `{"calendar_id":"dana@example.com","event_id":"ev11","response":"accepted"}`,
		"Event ev11 on calendar dana@example.com does not invite the owner, so there is no invitation to answer.")
	calRefused(t, g, answers, "respond_to_invitation", `{"calendar_id":"dana@example.com","event_id":"ev5","response":"yes"}`,
		"response is accepted, declined or tentative.")
	calRefused(t, g, answers, "respond_to_invitation", `{"calendar_id":"boss@example.org","event_id":"ev5","response":"accepted"}`, notOwn("boss@example.org"))
	calRefused(t, g, calWrites(), "respond_to_invitation", `{"calendar_id":"dana@example.com","event_id":"ev5","response":"accepted"}`,
		"The owner has not given you inviting people and answering invitations in Google Workspace; ask the owner if your work needs it.")
	calRefused(t, g, map[string]any{"calendar_send": true, "send_to": []string{"*@example.com"}}, "respond_to_invitation", `{"calendar_id":"dana@example.com","event_id":"ev5","response":"accepted"}`,
		"The owner has not allowed you to send to alice@example.org through Google Workspace; ask the owner if your work needs it.")
	if got := f.written()[before:]; len(got) != 0 {
		t.Errorf("refused answers were written: %v", got)
	}
}

// Why this test exists: a send whose Google never answers may have
// been done, so a Run that times out must not tell the agent to try
// again as a plain failure would: it says Kivali cannot tell whether
// it was done and what to look at first, since asking again would
// invite people, or answer, twice.
func TestCalendarSendUnanswered(t *testing.T) {
	err := unsure(&apiError{Message: "not in time", Timeout: true}, "the event was created: list_events on calendar c shows it if it was")
	var f failure
	if !errors.As(err, &f) || string(f) != "Google did not answer (not in time), so Kivali cannot say whether the event was created: list_events on calendar c shows it if it was." {
		t.Errorf("%v", err)
	}
	if err := unsure(&apiError{Status: 403, Message: "no"}, "x"); errors.As(err, &f) {
		t.Errorf("a refusal read as unanswered: %v", err)
	}
}

// Why this test exists: May send to must hold every person a send
// reaches, and Kivali cannot hold someone whose address Google does not
// show: an attendee or an organizer Google gives no address, or the
// attendees Google leaves out of a long list. While May send to names
// anyone, such a send must be refused before it is held; with it empty,
// anyone may be reached, so it is held, and the card and the answer say
// it reaches people whose address Google does not show, rather than
// naming fewer people than Google tells. A new attendee list for such
// an event would drop the people Kivali cannot name, so changing who it
// invites is refused whatever May send to holds.
func TestCalendarSendsToPeopleGoogleDoesNotShow(t *testing.T) {
	f, g := newFakeCalendar(t)
	f.add(t, "dana@example.com",
		`{"id":"ev12","summary":"Board","start":{"date":"2026-10-27"},"end":{"date":"2026-10-28"},"organizer":{"email":"dana@example.com","self":true},
		"attendees":[{"email":"dana@example.com","self":true},{"email":"alice@example.org"},{"displayName":"Someone"}],"extendedProperties":{"private":{"kivali":"riley"}}}`,
		`{"id":"ev13","summary":"All hands","start":{"date":"2026-10-27"},"end":{"date":"2026-10-28"},"organizer":{"email":"dana@example.com","self":true},
		"attendees":[{"email":"dana@example.com","self":true},{"email":"alice@example.org"}],"attendeesOmitted":true}`,
		`{"id":"ev14","summary":"Theirs","start":{"date":"2026-10-27"},"end":{"date":"2026-10-28"},"organizer":{"displayName":"A list"},
		"attendees":[{"email":"dana@example.com","self":true}]}`)
	limited := calSends(map[string]any{"send_to": []string{"alice@example.org"}})
	for _, call := range []struct{ name, args string }{
		{"update_event", `{"calendar_id":"dana@example.com","event_id":"ev12","title":"x"}`},
		{"delete_event", `{"calendar_id":"dana@example.com","event_id":"ev12"}`},
		{"update_event", `{"calendar_id":"dana@example.com","event_id":"ev13","title":"x"}`},
		{"respond_to_invitation", `{"calendar_id":"dana@example.com","event_id":"ev14","response":"accepted"}`},
	} {
		calRefused(t, g, limited, call.name, call.args, unshownRefusal)
	}
	for _, ev := range []string{"ev12", "ev13"} {
		calRefused(t, g, calSends(nil), "update_event", `{"calendar_id":"dana@example.com","event_id":"`+ev+`","attendees":["dana@example.com","alice@example.org","bob@example.com"]}`,
			"Kivali cannot change who event "+ev+" invites: Google lists only some of its attendees, or shows no address for one, so a new list would drop them. Leave attendees out to change the rest of it; the owner can change who it invites in Google Calendar.")
	}
	if got := f.written(); len(got) != 0 {
		t.Fatalf("refused sends were written: %v", got)
	}

	h := calHold(t, g, calSends(nil), "update_event", `{"calendar_id":"dana@example.com","event_id":"ev12","title":"Board, moved"}`)
	if !slices.Equal(h.Recipients, []string{"alice@example.org"}) || valueOf(h, "Also reaches") != "People whose address Google does not show" {
		t.Errorf("card %v %+v", h.Recipients, h.Fields)
	}
	if res := approve(t, h); !strings.HasSuffix(res.Content[0], ". Google told alice@example.org and others whose address Google does not show.") {
		t.Errorf("answer %q", res.Content[0])
	}
	h = calHold(t, g, map[string]any{"calendar_send": true}, "respond_to_invitation", `{"calendar_id":"dana@example.com","event_id":"ev14","response":"accepted"}`)
	if len(h.Recipients) != 0 || valueOf(h, "Organizer") != unreadAddress {
		t.Errorf("card %v %+v", h.Recipients, h.Fields)
	}
	if res := approve(t, h); res.Content[0] != "Answered yes to event ev14 on calendar dana@example.com. Google told its organizer, whose address Google does not show." {
		t.Errorf("answer %q", res.Content[0])
	}
}

// Why this test exists: the owner approves a change to an event as the
// card showed it. If the event changes in Google Calendar before the
// owner approves (its time moved, someone invited), Google must not
// apply the change or the deletion to an event the owner never saw:
// Run sends the etag Kivali read, and Google's 412 ends the call as a
// refusal, nothing done, in words the owner and the agent read. An
// event deleted meanwhile ends the same way, saying it is gone, rather
// than as a failure the agent would try again.
func TestCalendarSendRunsOnTheEventTheOwnerSaw(t *testing.T) {
	f, g := newFakeCalendar(t)
	updateFixture(t, f)
	f.add(t, "dana@example.com", `{"id":"ev10","summary":"Shared hold","start":{"date":"2026-10-27"},"end":{"date":"2026-10-28"},"organizer":{"email":"dana@example.com","self":true},
		"attendees":[{"email":"alice@example.org"}],"extendedProperties":{"private":{"kivali":"riley"}}}`)
	changed := "The event changed after the owner saw it; nothing was done."

	h := calHold(t, g, calSends(nil), "update_event", `{"calendar_id":"dana@example.com","event_id":"ev6","location":"Room 9"}`)
	f.touch("dana@example.com", "ev6", "Sync, moved")
	runRefused(t, h, changed)
	if e := f.eventOn("dana@example.com", "ev6"); e["location"] != nil || e["summary"] != "Sync, moved" {
		t.Errorf("a changed event was patched: %v", e)
	}
	h = calHold(t, g, calSends(nil), "delete_event", `{"calendar_id":"dana@example.com","event_id":"ev10"}`)
	f.touch("dana@example.com", "ev10", "Shared hold, moved")
	runRefused(t, h, changed)
	if f.eventOn("dana@example.com", "ev10") == nil {
		t.Error("a changed event was deleted")
	}

	gone := "The event is gone; nothing was done."
	h = calHold(t, g, calSends(nil), "update_event", `{"calendar_id":"dana@example.com","event_id":"ev6","location":"Room 9"}`)
	d := calHold(t, g, calSends(nil), "delete_event", `{"calendar_id":"dana@example.com","event_id":"ev10"}`)
	r := calHold(t, g, map[string]any{"calendar_send": true}, "respond_to_invitation", `{"calendar_id":"dana@example.com","event_id":"ev5","response":"accepted"}`)
	f.drop("dana@example.com", "ev6")
	f.drop("dana@example.com", "ev10")
	f.drop("dana@example.com", "ev5")
	runRefused(t, h, gone)
	runRefused(t, d, gone)
	runRefused(t, r, gone)
}

// Why this test exists: an event whose attendees are only the owner's
// own addresses invites no one else, so it is a write: it must be made
// at once, with those attendees and sendUpdates=none, by an agent
// without May invite and respond, and never wait for the owner as an
// invitation would. Anyone else among them makes it a send.
func TestCreateEventInvitingOnlyTheOwnerIsAWrite(t *testing.T) {
	f, g := newFakeCalendar(t)
	res := calAnswer(t, g, calWrites(), "create_event", `{"title":"Focus","start":"2026-10-27","end":"2026-10-27","attendees":["DANA@example.com","team@group.calendar.google.com"]}`)
	if !strings.HasSuffix(res.Content[0], "Google emailed no one about it, and Kivali marked it as created by Kivali.") {
		t.Errorf("answer %q", res.Content[0])
	}
	req, body := lastWrite(t, f)
	if req != "POST /calendar/v3/calendars/dana@example.com/events?sendUpdates=none" ||
		fmt.Sprint(body["attendees"]) != "[map[email:dana@example.com] map[email:team@group.calendar.google.com]]" {
		t.Errorf("%s with %v", req, body)
	}
	calRefused(t, g, calWrites(), "create_event", `{"title":"Focus","start":"2026-10-27","end":"2026-10-27","attendees":["dana@example.com","alice@example.org"]}`, calSendNotGivenText)
	if got := len(f.written()); got != 1 {
		t.Errorf("%d writes", got)
	}
}
