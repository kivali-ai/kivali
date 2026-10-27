package googleworkspace

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// calSendNotGivenText is the refusal of a create_event, update_event
// or delete_event call that isSend finds is a send, for an agent whose
// access lacks calendar_send.
const calSendNotGivenText = "This call is a send: it invites people or changes who an event invites, or changes or deletes an event that has attendees other than the owner or that someone else organized, which reaches those people. The owner has not given you inviting people and answering invitations in Google Workspace; ask the owner if your work needs it. You may create an event that invites no one, and change or delete one the owner organized that has no other attendees, leaving out attendees or giving the event's own list unchanged."

func calSendNotGiven() error {
	return refuse(calSendNotGivenText, "the access does not include "+bucketWords[keyCalendarSend])
}

// isSend reports whether a create_event, update_event or delete_event
// call (action) is a send: one that reaches a person other than the
// owner. cur is the event an update_event or delete_event call names,
// nil for create_event, and o who the owner is. A create_event call is
// a send when its attendees name anyone but the owner (calOwner.self):
// one inviting only the owner's own addresses tells no one else. An
// update_event or delete_event call is a send when cur reaches others
// (reachesOthers), or when the call's attendees, ignoring case, order
// and repeats, are a list other than cur's (delete_event takes none).
// A call that gives no attendees, or null, changes no one's invitation.
// Every address must be one: a list that is not is refused.
func isSend(action string, a args, cur *calEvent, o calOwner) (bool, error) {
	var list []string
	given := false
	if action != actionDeleteEvent {
		var err error
		if list, given, err = attendeesArg(a); err != nil {
			return false, err
		}
	}
	if cur == nil {
		return slices.ContainsFunc(list, func(x string) bool { return !o.self(x) }), nil
	}
	if reachesOthers(*cur, o) {
		return true, nil
	}
	return given && !slices.Equal(list, addressSet(invited(*cur))), nil
}

// reachesOthers reports whether a change to e reaches someone other
// than the owner: e has an attendee that is not the owner (calOwner.self,
// or the calendar this copy is on), a room included; Google lists only
// some of its attendees; or the owner did not organize it.
func reachesOthers(e calEvent, o calOwner) bool {
	ownOrganizer := e.Organizer.Self || o.self(calAddress(e.Organizer.Email))
	if e.AttendeesOmitted || !ownOrganizer {
		return true
	}
	return slices.ContainsFunc(e.Attendees, func(a calAttendee) bool {
		return !a.Self && !o.self(calAddress(a.Email))
	})
}

// attendeesArg is the attendees argument as addressSet makes it, and
// whether the call gives it.
func attendeesArg(a args) ([]string, bool, error) {
	list, given, err := a.texts("attendees", maxAttendees, 254)
	if err != nil || !given {
		return nil, given, err
	}
	for _, s := range list {
		if calAddress(s) == "" {
			return nil, false, badArgument("attendees are email addresses, such as dana@example.com.")
		}
	}
	return addressSet(list), true, nil
}

// addressSet is addresses lower-cased, sorted and each once.
func addressSet(addresses []string) []string {
	out := []string{}
	for _, s := range addresses {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// invited is the addresses e invites now.
func invited(e calEvent) []string {
	var out []string
	for _, a := range e.Attendees {
		out = append(out, a.Email)
	}
	return out
}

// ownedCalendar is the calendar the argument calendar_id names, as
// calendarArg reads it, when it is one of the owner's own
// (ownCalendar). Any other is refused before an event is read or
// changed.
func (cc *calCall) ownedCalendar(ctx context.Context, required bool) (string, error) {
	cal, err := cc.calendarArg(ctx, "calendar_id", required)
	if err != nil {
		return "", err
	}
	e, ok, err := cc.entry(ctx, cal)
	if err != nil {
		return "", err
	}
	if !ok || !ownCalendar(e) {
		return "", refuse("Calendar "+cal+" is not one of the owner's own calendars, and you may change events only on "+ownWords+"; list_calendars gives each calendar's role.",
			"calendar "+cal+" is not one of the owner's own")
	}
	return cal, nil
}

// moment is a start or an end as an agent gives one: a date, for an
// all-day event (an end's its last day), or a date and time.
type moment struct {
	t    time.Time
	date bool
}

// parseMoment reads the argument key's text s.
func parseMoment(key, s string) (moment, error) {
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return moment{t: t, date: true}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return moment{t: t}, nil
	}
	return moment{}, badArgument(key + " is a date and time in " + timeWords + ", or a date such as 2026-10-27 for an all-day event.")
}

// currentMoment is an event's own start or end (end) as an agent gives
// one: an all-day event's end is its last day, the day before Google's.
func currentMoment(t eventTime, end bool) (moment, error) {
	if t.Date != "" {
		d, err := time.Parse(time.DateOnly, t.Date)
		if err == nil {
			if end {
				d = d.AddDate(0, 0, -1)
			}
			return moment{t: d, date: true}, nil
		}
	} else if d, err := time.Parse(time.RFC3339, t.DateTime); err == nil {
		return moment{t: d}, nil
	}
	return moment{}, failure("Google gave the event a time Kivali does not read; give both start and end.")
}

// eventTimes is the start and end a create_event or update_event call
// gives the event, as Google takes them: from the arguments and, for
// update_event, the event's own (cur) where they give none. changed is
// false for an update_event call that gives no time at all. An all-day
// event's end goes to Google as the day after its last; to change an
// event from all-day or back, both start and end are given.
func eventTimes(a args, cur *calEvent) (start, end map[string]any, changed bool, err error) {
	create := cur == nil
	startText, err := a.text("start", create, 64)
	if err != nil {
		return nil, nil, false, err
	}
	endText, err := a.text("end", create, 64)
	if err != nil {
		return nil, nil, false, err
	}
	zone, err := a.text("time_zone", false, 64)
	if err != nil {
		return nil, nil, false, err
	}
	if zone != "" && !timeZonePattern.MatchString(zone) {
		return nil, nil, false, badArgument("time_zone is an IANA time zone, such as Europe/London.")
	}
	allDay, allDayGiven, err := a.flag("all_day")
	if err != nil {
		return nil, nil, false, err
	}
	if !create && startText == "" && endText == "" && zone == "" && !allDayGiven {
		return nil, nil, false, nil
	}
	var s, e moment
	if startText != "" {
		s, err = parseMoment("start", startText)
	} else {
		s, err = currentMoment(cur.Start, false)
	}
	if err != nil {
		return nil, nil, false, err
	}
	if endText != "" {
		e, err = parseMoment("end", endText)
	} else {
		e, err = currentMoment(cur.End, true)
	}
	if err != nil {
		return nil, nil, false, err
	}
	switch {
	case s.date != e.date:
		return nil, nil, false, badArgument("start and end are both dates, for an all-day event, or both dates and times; to change which, give both.")
	case allDayGiven && allDay && !s.date:
		return nil, nil, false, badArgument("An all-day event's start and end are dates, such as 2026-10-27.")
	case allDayGiven && !allDay && s.date:
		return nil, nil, false, badArgument("An event that is not all day starts and ends at a date and time in " + timeWords + ".")
	}
	if s.date {
		if e.t.Before(s.t) {
			return nil, nil, false, badArgument("end is the all-day event's last day, start's day or a later one.")
		}
		start = map[string]any{"date": s.t.Format(time.DateOnly)}
		end = map[string]any{"date": e.t.AddDate(0, 0, 1).Format(time.DateOnly)}
		if !create {
			// A patch keeps what it does not name: a timed event made
			// all-day loses its date and time.
			start["dateTime"], start["timeZone"] = nil, nil
			end["dateTime"], end["timeZone"] = nil, nil
		}
		return start, end, true, nil
	}
	if !e.t.After(s.t) {
		return nil, nil, false, badArgument("end is after start.")
	}
	start = map[string]any{"dateTime": s.t.Format(time.RFC3339)}
	end = map[string]any{"dateTime": e.t.Format(time.RFC3339)}
	if zone == "" && !create {
		zone = zoneName(cur.Start.TimeZone)
	}
	if zone != "" {
		start["timeZone"], end["timeZone"] = zone, zone
	}
	if !create {
		start["date"], end["date"] = nil, nil
	}
	return start, end, true, nil
}

// The most an event's title, location and description hold, as Kivali
// writes them.
const (
	maxTitle       = 1024
	maxLocation    = 1024
	maxDescription = 8192
)

// writeQuery is every write's query: Google emails no one about it.
// sendQuery is every send's: Google tells everyone the change reaches.
const (
	writeQuery = "?sendUpdates=none"
	sendQuery  = "?sendUpdates=all"
)

// eventTimeOf is a start or an end as eventTimes writes it for Google,
// as Calendar answers one, for whenWords.
func eventTimeOf(m map[string]any) eventTime {
	s := func(k string) string { v, _ := m[k].(string); return v }
	return eventTime{Date: s("date"), DateTime: s("dateTime"), TimeZone: s("timeZone")}
}

// peopleWords is n people in words, for a card's title.
func peopleWords(n int) string {
	switch n {
	case 0:
		return "no one"
	case 1:
		return "1 person"
	}
	return fmt.Sprintf("%d people", n)
}

// orNone is s, or "None" when it is empty, for a card's field.
func orNone(s string) string { return cmp.Or(s, "None") }

// eventTitle is an event's title as a card shows it, quoted: the
// organizer's text, shown and not sent.
func eventTitle(e calEvent) string {
	if t := shownLine(e.Summary, listedTitleClip); t != "" {
		return `"` + t + `"`
	}
	return "an event with no title"
}

// reached is whom a change to e reaches, as addresses: its attendees
// after the change and those it removes (after, lower-cased), and its
// organizer, the owner's own addresses left out, each once.
func reached(o calOwner, e calEvent, after []string) []string {
	var out []string
	for _, a := range append(slices.Clone(after), calAddress(e.Organizer.Email)) {
		if a != "" && !o.self(a) && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// unreadPeople reports whether a change to e reaches people whose
// address Kivali does not read, whom it cannot hold to send_to: Google
// lists only some of e's attendees, or gives an attendee or the
// organizer, other than the calendar's own, no address Kivali reads.
func unreadPeople(e calEvent) bool {
	if e.AttendeesOmitted || (!e.Organizer.Self && calAddress(e.Organizer.Email) == "") {
		return true
	}
	return slices.ContainsFunc(e.Attendees, func(a calAttendee) bool { return !a.Self && calAddress(a.Email) == "" })
}

// eventSendFailed is a failure of the Run of a send that changes or
// deletes event id, which Kivali read at etag when it held the call:
// the event changed since (412, If-Match) or is gone (404, 410), each a
// Refusal, since nothing was done; else unsure's, with check.
func eventSendFailed(err error, id, check string) error {
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusPreconditionFailed {
		return refuse("The event changed after the owner saw it; nothing was done.", "event "+id+" changed after the owner saw it")
	}
	if g := gone(err, "The event", "event "+id+" was gone"); g != nil {
		return g
	}
	return unsure(err, check)
}

// invitedNow is the addresses e invites, lower-cased, those Kivali
// reads.
func invitedNow(e calEvent) []string {
	var out []string
	for _, a := range e.Attendees {
		if addr := calAddress(a.Email); addr != "" {
			out = append(out, addr)
		}
	}
	return out
}

// createEvent creates an event on one of the owner's own calendars,
// marked as Kivali's (kivaliMarker, marker). One that invites no one,
// or only the owner's own addresses, on a calendar shared with no one
// else (calendarShare), is a write, telling no one. One that invites
// anyone else is a send (isSend), which needs calendar_send and every
// attendee but the owner in send_to, and so is one on a calendar shared
// with others, who see it (checkShared): it is held for the owner
// (held), with the event as it will be made and who can see the
// calendar, and Run makes it, with sendUpdates=all when it invites
// others, so Google invites them.
func createEvent(ctx context.Context, c *call) (any, error) {
	attendees, _, err := attendeesArg(c.args)
	if err != nil {
		return nil, err
	}
	cc := &calCall{call: c}
	var o calOwner
	if len(attendees) > 0 {
		if o, err = cc.owner(ctx); err != nil {
			return nil, err
		}
	}
	send, err := isSend(actionCreateEvent, c.args, nil, o)
	if err != nil {
		return nil, err
	}
	if send && !c.access.on(keyCalendarSend) {
		return nil, calSendNotGiven()
	}
	cal, err := cc.ownedCalendar(ctx, false)
	if err != nil {
		return nil, err
	}
	title, err := c.args.text("title", true, maxTitle)
	if err != nil {
		return nil, err
	}
	desc, err := c.args.text("description", false, maxDescription)
	if err != nil {
		return nil, err
	}
	loc, err := c.args.text("location", false, maxLocation)
	if err != nil {
		return nil, err
	}
	start, end, _, err := eventTimes(c.args, nil)
	if err != nil {
		return nil, err
	}
	if o, err = cc.owner(ctx); err != nil {
		return nil, err
	}
	sh, err := cc.calendarShare(ctx, cal, o)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"summary":            title,
		"start":              start,
		"end":                end,
		"extendedProperties": map[string]any{"private": map[string]string{kivaliMarker: c.marker()}},
	}
	if desc != "" {
		body["description"] = desc
	}
	if loc != "" {
		body["location"] = loc
	}
	if len(attendees) > 0 {
		list := make([]map[string]string, 0, len(attendees))
		for _, a := range attendees {
			list = append(list, map[string]string{"email": a})
		}
		body["attendees"] = list
	}
	if !send && !sh.others() {
		var made calEvent
		if err := c.g.change(ctx, c.token, googleAPI, http.MethodPost, eventsPath(cal)+writeQuery, body, &made); err != nil {
			return nil, err
		}
		if !eventIDPattern.MatchString(made.ID) {
			return nil, failure("Google created the event on calendar " + cal + " and gave it an id Kivali does not read; list_events finds it.")
		}
		when := whenWords(made.Start, made.End)
		c.audit = "created event " + made.ID + " on calendar " + cal + ", " + when
		return "Created event " + made.ID + " on calendar " + cal + ", " + when + ". Google emailed no one about it, and Kivali marked it as created by Kivali.", nil
	}
	if !send && !c.access.on(keyCalendarSend) {
		return nil, sharedNotGiven("calendar", cal, keyCalendarSend)
	}
	people := reached(o, calEvent{}, attendees)
	if err := c.access.checkRecipients(people); err != nil {
		return nil, err
	}
	if err := c.access.checkShared("calendar", cal, sh); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	when := whenWords(eventTimeOf(start), eventTimeOf(end))
	fields := []builtinintegrations.HoldField{field("Calendar", cal), field("Title", title), field("When", when)}
	if loc != "" {
		fields = append(fields, field("Where", loc))
	}
	if len(attendees) > 0 {
		fields = append(fields, field("Attendees", strings.Join(attendees, ", ")))
	}
	if desc != "" {
		fields = append(fields, field("Description", desc))
	}
	if sh.others() {
		fields = append(fields, field(calendarShareLabel, sh.text()))
	}
	heading, query := "Invite "+peopleWords(len(people))+` to "`+oneLine(title, listedTitleClip)+`"`, sendQuery
	if !send {
		heading, query = `Create "`+oneLine(title, listedTitleClip)+`" on a shared calendar`, writeQuery
	}
	return &held{
		title:      heading,
		fields:     fields,
		recipients: union(people, sh.people),
		needs:      keyCalendarSend,
		run: func(ctx context.Context, c *call) (any, error) {
			var made calEvent
			if err := c.g.send(ctx, c.token, http.MethodPost, googleAPI, eventsPath(cal)+query, payload, maxAnswerBytes, &made); err != nil {
				return nil, unsure(err, "the event was created: list_events on calendar "+cal+" shows it if it was")
			}
			if !eventIDPattern.MatchString(made.ID) {
				return nil, failure("Google created the event on calendar " + cal + " and gave it an id Kivali does not read; list_events finds it.")
			}
			if !send {
				c.audit = "created event " + made.ID + " on calendar " + cal + ", " + when + sh.auditNote()
				return "Created event " + made.ID + " on calendar " + cal + ", " + when + ". Google emailed no one about it, and Kivali marked it as created by Kivali.", nil
			}
			c.audit = "created event " + made.ID + " on calendar " + cal + ", " + when + ", inviting " + idList(people) + sh.auditNote()
			return "Created event " + made.ID + " on calendar " + cal + ", " + when + ", and Google invited " + strings.Join(people, ", ") + ". Kivali marked it as created by Kivali.", nil
		},
	}, nil
}

// newAttendees is the attendee list Google is given for an event whose
// attendees become list (addressSet's): each one the event invites
// already as Google gave it, its answer and the rest kept, in the
// event's order, then each new one by address.
func newAttendees(cur calEvent, list []string) []json.RawMessage {
	var out []json.RawMessage
	var kept []string
	for i, a := range cur.Attendees {
		addr := calAddress(a.Email)
		if addr != "" && slices.Contains(list, addr) && !slices.Contains(kept, addr) && i < len(cur.rawAttendees) {
			out = append(out, cur.rawAttendees[i])
			kept = append(kept, addr)
		}
	}
	for _, addr := range list {
		if !slices.Contains(kept, addr) {
			b, _ := json.Marshal(map[string]string{"email": addr})
			out = append(out, b)
		}
	}
	return out
}

// updateEvent changes the fields the call gives of an event on one of
// the owner's own calendars. A change to an event the owner organized
// that invites no one else, which gives no attendees or the event's
// own, on a calendar shared with no one else (calendarShare), is a
// write, telling no one. Any other is a send (isSend), which needs
// calendar_send and everyone it reaches in send_to (checkReach), and so
// is a change on a calendar shared with others, who see it
// (checkShared): it is held for the owner with the event as it is, each
// change and who can see the calendar, and Run patches it with If-Match
// of the event's etag as Kivali read it, so Google changes only the
// event the owner saw, and with sendUpdates=all when it reaches others,
// so Google tells everyone it reaches, those it removes included. A
// change of attendees to an event whose attendees Kivali cannot all
// read is refused: the new list would drop those it cannot name.
func updateEvent(ctx context.Context, c *call) (any, error) {
	cc := &calCall{call: c}
	cal, err := cc.ownedCalendar(ctx, true)
	if err != nil {
		return nil, err
	}
	id, err := c.args.id("event_id", eventIDPattern)
	if err != nil {
		return nil, err
	}
	cur, err := cc.event(ctx, cal, id)
	if err != nil {
		return nil, err
	}
	o, err := cc.owner(ctx)
	if err != nil {
		return nil, err
	}
	send, err := isSend(actionUpdateEvent, c.args, &cur, o)
	if err != nil {
		return nil, err
	}
	if send && !c.access.on(keyCalendarSend) {
		return nil, calSendNotGiven()
	}
	body := map[string]any{}
	var changed []string
	var changes []builtinintegrations.HoldField
	if c.args.given("title") {
		title, err := c.args.text("title", true, maxTitle)
		if err != nil {
			return nil, err
		}
		body["summary"] = title
		changed = append(changed, "title")
		changes = append(changes, field("Title changes to", title))
	}
	for _, f := range []struct {
		key, field, label string
		most              int
	}{{"description", "description", "Description", maxDescription}, {"location", "location", "Place", maxLocation}} {
		if !c.args.given(f.key) {
			continue
		}
		s, err := c.args.text(f.key, false, f.most)
		if err != nil {
			return nil, err
		}
		body[f.field] = s
		changed = append(changed, f.key)
		if f.key == "description" {
			changes = append(changes, field("Description now", orNone(shownText(cur.Description))))
		}
		changes = append(changes, field(f.label+" changes to", cmp.Or(s, "None: it is cleared")))
	}
	start, end, timed, err := eventTimes(c.args, &cur)
	if err != nil {
		return nil, err
	}
	if timed {
		body["start"], body["end"] = start, end
		changed = append(changed, "start and end")
		changes = append(changes, field("Time changes to", whenWords(eventTimeOf(start), eventTimeOf(end))))
	}
	after := invitedNow(cur)
	var added, removed []string
	if list, given, _ := attendeesArg(c.args); send && given && !slices.Equal(list, addressSet(invited(cur))) {
		if cur.AttendeesOmitted || slices.ContainsFunc(cur.Attendees, func(a calAttendee) bool { return calAddress(a.Email) == "" }) {
			return nil, refuse("Kivali cannot change who event "+id+" invites: Google lists only some of its attendees, or shows no address for one, so a new list would drop them. Leave attendees out to change the rest of it; the owner can change who it invites in Google Calendar.",
				"event "+id+" has attendees Kivali cannot read")
		}
		for _, a := range list {
			if !slices.Contains(after, a) {
				added = append(added, a)
			}
		}
		for _, a := range after {
			if !slices.Contains(list, a) {
				removed = append(removed, a)
			}
		}
		body["attendees"] = newAttendees(cur, list)
		after = list
		changed = append(changed, "attendees")
		if len(added) > 0 {
			changes = append(changes, field("Attendees added", strings.Join(added, ", ")))
		}
		if len(removed) > 0 {
			changes = append(changes, field("Attendees removed", strings.Join(removed, ", ")))
		}
	}
	if len(body) == 0 {
		if send {
			return nil, badArgument("Give at least one field to change: title, start, end, time_zone, description, location, all_day or attendees.")
		}
		return nil, badArgument("Give at least one field to change: title, start, end, time_zone, description, location or all_day.")
	}
	what := strings.Join(changed, ", ")
	sh, err := cc.calendarShare(ctx, cal, o)
	if err != nil {
		return nil, err
	}
	if !send && !sh.others() {
		var done calEvent
		if err := c.g.change(ctx, c.token, googleAPI, http.MethodPatch, eventPath(cal, id)+writeQuery, body, &done); err != nil {
			return nil, err
		}
		when := whenWords(done.Start, done.End)
		c.audit = "changed event " + id + " on calendar " + cal + ": " + what
		return "Changed event " + id + " on calendar " + cal + ": " + what + ". It is " + when + ". Google emailed no one about it.", nil
	}
	if !send && !c.access.on(keyCalendarSend) {
		return nil, sharedNotGiven("calendar", cal, keyCalendarSend)
	}
	var people []string
	unknown := false
	if send {
		people, unknown = reached(o, cur, append(after, removed...)), unreadPeople(cur)
		if err := c.access.checkReach(people, unknown); err != nil {
			return nil, err
		}
	}
	if err := c.access.checkShared("calendar", cal, sh); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	fields := append(eventFields(o, cal, cur), changes...)
	if unknown {
		fields = append(fields, unshownField())
	}
	if sh.others() {
		fields = append(fields, field(calendarShareLabel, sh.text()))
	}
	heading, query := "Change "+eventTitle(cur)+", telling "+peopleWords(len(people)), sendQuery
	if !send {
		heading, query = "Change "+eventTitle(cur)+" on a shared calendar", writeQuery
	}
	etag := cur.Etag
	return &held{
		title:      heading,
		fields:     fields,
		recipients: union(people, sh.people),
		needs:      keyCalendarSend,
		run: func(ctx context.Context, c *call) (any, error) {
			var done calEvent
			if err := c.g.sendIfMatch(ctx, c.token, http.MethodPatch, googleAPI, eventPath(cal, id)+query, payload, maxAnswerBytes, &done, etag); err != nil {
				return nil, eventSendFailed(err, id, "the event changed: get_event shows it as it is")
			}
			if !send {
				c.audit = "changed event " + id + " on calendar " + cal + ": " + what + sh.auditNote()
				return "Changed event " + id + " on calendar " + cal + ": " + what + ". It is " + whenWords(done.Start, done.End) + ". Google emailed no one about it.", nil
			}
			c.audit = "changed event " + id + " on calendar " + cal + ": " + what + "; told " + orNone(idList(people)) + sh.auditNote()
			return "Changed event " + id + " on calendar " + cal + ": " + what + ". It is " + whenWords(done.Start, done.End) + ". Google told " + toldWords(people, unknown) + ".", nil
		},
	}, nil
}

// toldWords is whom Google told, for an answer: people, and, when
// unknown, those whose address Google does not show.
func toldWords(people []string, unknown bool) string {
	return cmp.Or(withUnshown(people, unknown), "no one Kivali can name")
}

// eventFields are the first fields of a card for a change to e on the
// calendar cal, the event as it is: its calendar, title, time, place,
// organizer and attendees. Its title and place are the organizer's
// text, shown and not sent.
func eventFields(o calOwner, cal string, e calEvent) []builtinintegrations.HoldField {
	org := cmp.Or(calAddress(e.Organizer.Email), unreadAddress)
	if e.Organizer.Self || o.self(calAddress(e.Organizer.Email)) {
		org += ", the owner"
	}
	fields := []builtinintegrations.HoldField{
		field("Calendar", cal),
		field("Title", orNone(shownLine(e.Summary, titleClip))),
		field("When", whenWords(e.Start, e.End)),
	}
	if loc := shownLine(e.Location, titleClip); loc != "" {
		fields = append(fields, field("Where", loc))
	}
	attendees := strings.Join(invitedNow(e), ", ")
	if e.AttendeesOmitted {
		attendees = cmp.Or(attendees+"; ", "") + "Google lists only some of them"
	}
	return append(fields, field("Organizer", org), field("Attendees", orNone(attendees)))
}

// deleteEvent deletes an event Kivali created on this account
// (kivaliMarker set) from one of the owner's own calendars; any other
// event is refused before anything is changed. One that invites no one
// else, on a calendar shared with no one else (calendarShare), is a
// write, telling no one. One that reaches others is a send (isSend),
// which needs calendar_send and everyone it reaches in send_to
// (checkReach), and so is one on a calendar shared with others, who see
// it go (checkShared): it is held for the owner, and Run deletes it
// with If-Match of the event's etag as Kivali read it, so Google
// deletes only the event the owner saw, and with sendUpdates=all when
// it reaches others, so Google tells them.
func deleteEvent(ctx context.Context, c *call) (any, error) {
	cc := &calCall{call: c}
	cal, err := cc.ownedCalendar(ctx, true)
	if err != nil {
		return nil, err
	}
	id, err := c.args.id("event_id", eventIDPattern)
	if err != nil {
		return nil, err
	}
	e, err := cc.event(ctx, cal, id)
	if err != nil {
		return nil, err
	}
	if !e.byKivali() {
		return nil, refuse("You may delete only events Kivali created, and Kivali did not create event "+id+".",
			"event "+id+" was not created by Kivali")
	}
	o, err := cc.owner(ctx)
	if err != nil {
		return nil, err
	}
	send, err := isSend(actionDeleteEvent, c.args, &e, o)
	if err != nil {
		return nil, err
	}
	if send && !c.access.on(keyCalendarSend) {
		return nil, calSendNotGiven()
	}
	creator := oneLine(e.ExtendedProperties.Private[kivaliMarker], 64)
	sh, err := cc.calendarShare(ctx, cal, o)
	if err != nil {
		return nil, err
	}
	if !send && !sh.others() {
		if err := c.g.change(ctx, c.token, googleAPI, http.MethodDelete, eventPath(cal, id)+writeQuery, nil, nil); err != nil {
			return nil, err
		}
		c.audit = "deleted event " + id + " on calendar " + cal + ", which " + creator + " created"
		return "Deleted event " + id + " from calendar " + cal + ". Google emailed no one about it.", nil
	}
	if !send && !c.access.on(keyCalendarSend) {
		return nil, sharedNotGiven("calendar", cal, keyCalendarSend)
	}
	var people []string
	unknown := false
	if send {
		people, unknown = reached(o, e, invitedNow(e)), unreadPeople(e)
		if err := c.access.checkReach(people, unknown); err != nil {
			return nil, err
		}
	}
	if err := c.access.checkShared("calendar", cal, sh); err != nil {
		return nil, err
	}
	note := "This cancels the event for everyone invited."
	switch {
	case !send:
		note = "This takes the event off the calendar, for everyone who can see it."
	case !e.Organizer.Self && !o.self(calAddress(e.Organizer.Email)):
		note = "This takes the event off the owner's calendar, and Google tells its organizer the owner will not come."
	}
	fields := append(eventFields(o, cal, e), field("What happens", note))
	if unknown {
		fields = append(fields, unshownField())
	}
	if sh.others() {
		fields = append(fields, field(calendarShareLabel, sh.text()))
	}
	heading, query := "Delete "+eventTitle(e)+", telling "+peopleWords(len(people)), sendQuery
	if !send {
		heading, query = "Delete "+eventTitle(e)+" from a shared calendar", writeQuery
	}
	etag := e.Etag
	return &held{
		title:      heading,
		fields:     fields,
		recipients: union(people, sh.people),
		needs:      keyCalendarSend,
		run: func(ctx context.Context, c *call) (any, error) {
			if err := c.g.sendIfMatch(ctx, c.token, http.MethodDelete, googleAPI, eventPath(cal, id)+query, nil, maxAnswerBytes, nil, etag); err != nil {
				return nil, eventSendFailed(err, id, "the event was deleted: get_event answers whether it is still there")
			}
			if !send {
				c.audit = "deleted event " + id + " on calendar " + cal + ", which " + creator + " created" + sh.auditNote()
				return "Deleted event " + id + " from calendar " + cal + ". Google emailed no one about it.", nil
			}
			c.audit = "deleted event " + id + " on calendar " + cal + ", which " + creator + " created; told " + orNone(idList(people)) + sh.auditNote()
			return "Deleted event " + id + " from calendar " + cal + ". Google told " + toldWords(people, unknown) + ".", nil
		},
	}, nil
}

// The answers respond_to_invitation takes, each with the word a card
// says it in, as Google Calendar does.
var responseWords = map[string]string{"accepted": "yes", "declined": "no", "tentative": "maybe"}

// maxResponseComment bounds the note an answer to an invitation carries.
const maxResponseComment = 1000

// respondToInvitation answers an invitation on one of the owner's own
// calendars for the owner: the event must invite the owner (its
// attendee for this calendar, or one of the owner's own addresses) and
// someone else must have organized it, whom send_to must allow
// (checkReach). It is always a send, held for the owner, and Run
// patches the owner's own answer alone, with attendeesOmitted, as
// Calendar takes a participant's answer, and sendUpdates=all, so Google
// tells the organizer.
func respondToInvitation(ctx context.Context, c *call) (any, error) {
	cc := &calCall{call: c}
	cal, err := cc.ownedCalendar(ctx, true)
	if err != nil {
		return nil, err
	}
	id, err := c.args.id("event_id", eventIDPattern)
	if err != nil {
		return nil, err
	}
	answer, err := c.args.text("response", true, 32)
	if err != nil {
		return nil, err
	}
	word, ok := responseWords[answer]
	if !ok {
		return nil, badArgument("response is accepted, declined or tentative.")
	}
	comment, err := c.args.text("comment", false, maxResponseComment)
	if err != nil {
		return nil, err
	}
	e, err := cc.event(ctx, cal, id)
	if err != nil {
		return nil, err
	}
	o, err := cc.owner(ctx)
	if err != nil {
		return nil, err
	}
	org := calAddress(e.Organizer.Email)
	if e.Organizer.Self || o.self(org) {
		return nil, refuse("The owner organized event "+id+", so there is no invitation to answer.", "event "+id+" is the owner's own")
	}
	mine := slices.IndexFunc(e.Attendees, func(a calAttendee) bool { return a.Self })
	if mine < 0 {
		mine = slices.IndexFunc(e.Attendees, func(a calAttendee) bool { return o.self(calAddress(a.Email)) })
	}
	if mine < 0 || calAddress(e.Attendees[mine].Email) == "" {
		return nil, refuse("Event "+id+" on calendar "+cal+" does not invite the owner, so there is no invitation to answer.", "event "+id+" does not invite the owner")
	}
	entry := map[string]string{"email": e.Attendees[mine].Email, "responseStatus": answer}
	if comment != "" {
		entry["comment"] = comment
	}
	payload, err := json.Marshal(map[string]any{"attendeesOmitted": true, "attendees": []map[string]string{entry}})
	if err != nil {
		return nil, err
	}
	var people []string
	if org != "" {
		people = []string{org}
	}
	if err := c.access.checkReach(people, org == ""); err != nil {
		return nil, err
	}
	told := cmp.Or(org, "whose address Google does not show")
	fields := []builtinintegrations.HoldField{
		field("Calendar", cal),
		field("Title", orNone(shownLine(e.Summary, titleClip))),
		field("When", whenWords(e.Start, e.End)),
		field("Organizer", cmp.Or(org, unreadAddress)),
		field("Your response", strings.ToUpper(word[:1])+word[1:]+" ("+answer+")"),
	}
	if comment != "" {
		fields = append(fields, field("Comment", comment))
	}
	return &held{
		title:      "Answer " + word + " to " + eventTitle(e),
		fields:     fields,
		recipients: people,
		needs:      keyCalendarSend,
		run: func(ctx context.Context, c *call) (any, error) {
			if err := c.g.send(ctx, c.token, http.MethodPatch, googleAPI, eventPath(cal, id)+sendQuery, payload, maxAnswerBytes, nil); err != nil {
				if g := gone(err, "The event", "event "+id+" was gone"); g != nil {
					return nil, g
				}
				return nil, unsure(err, "the answer was given: get_event shows how the owner answered")
			}
			c.audit = "answered " + answer + " to event " + id + " on calendar " + cal + "; told " + orNone(idList(people))
			return "Answered " + word + " to event " + id + " on calendar " + cal + ". Google told its organizer, " + told + ".", nil
		},
	}, nil
}
