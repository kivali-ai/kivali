package googleworkspace

import (
	"cmp"
	"context"
	"encoding/json"
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
// nil for create_event, and o who the owner is. A call is a send when
// cur reaches others (reachesOthers), or when the call's attendees,
// ignoring case, order and repeats, are a list other than cur's (none
// for create_event; delete_event takes none). A call that gives no
// attendees, or null, changes no one's invitation. Every address must
// be one: a list that is not is refused.
func isSend(action string, a args, cur *calEvent, o calOwner) (bool, error) {
	var list []string
	given := false
	if action != actionDeleteEvent {
		var err error
		if list, given, err = attendeesArg(a); err != nil {
			return false, err
		}
	}
	var now []string
	if cur != nil {
		if reachesOthers(*cur, o) {
			return true, nil
		}
		now = invited(*cur)
	}
	return given && !slices.Equal(list, addressSet(now)), nil
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
// marked as Kivali's (kivaliMarker, marker). One that invites no one
// is a write, telling no one. One that invites people is a send
// (isSend), which needs calendar_send and every attendee but the owner
// in send_to: it is held for the owner (held), with the event as it
// will be made, and Run makes it with sendUpdates=all, so Google
// invites them.
func createEvent(ctx context.Context, c *call) (any, error) {
	send, err := isSend(actionCreateEvent, c.args, nil, calOwner{})
	if err != nil {
		return nil, err
	}
	if send && !c.access.on(keyCalendarSend) {
		return nil, calSendNotGiven()
	}
	cc := &calCall{call: c}
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
	if !send {
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
	o, err := cc.owner(ctx)
	if err != nil {
		return nil, err
	}
	attendees, _, _ := attendeesArg(c.args)
	list := make([]map[string]string, 0, len(attendees))
	for _, a := range attendees {
		list = append(list, map[string]string{"email": a})
	}
	body["attendees"] = list
	people := reached(o, calEvent{}, attendees)
	if err := c.access.checkRecipients(people); err != nil {
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
	fields = append(fields, field("Attendees", strings.Join(attendees, ", ")))
	if desc != "" {
		fields = append(fields, field("Description", desc))
	}
	return &held{
		title:      "Invite " + peopleWords(len(people)) + ` to "` + oneLine(title, listedTitleClip) + `"`,
		fields:     fields,
		recipients: people,
		run: func(ctx context.Context, c *call) (any, error) {
			var made calEvent
			if err := c.g.send(ctx, c.token, http.MethodPost, googleAPI, eventsPath(cal)+sendQuery, payload, maxAnswerBytes, &made); err != nil {
				return nil, unsure(err, "the event was created: list_events on calendar "+cal+" shows it if it was")
			}
			if !eventIDPattern.MatchString(made.ID) {
				return nil, failure("Google created the event on calendar " + cal + " and gave it an id Kivali does not read; list_events finds it.")
			}
			c.audit = "created event " + made.ID + " on calendar " + cal + ", " + when + ", inviting " + idList(people)
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
// own, is a write, telling no one. Any other is a send (isSend), which
// needs calendar_send and everyone it adds in send_to: it is held for
// the owner with the event as it is and each change, and Run patches
// it with sendUpdates=all, so Google tells everyone it reaches, those
// it removes included.
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
	if !send {
		var done calEvent
		if err := c.g.change(ctx, c.token, googleAPI, http.MethodPatch, eventPath(cal, id)+writeQuery, body, &done); err != nil {
			return nil, err
		}
		when := whenWords(done.Start, done.End)
		c.audit = "changed event " + id + " on calendar " + cal + ": " + what
		return "Changed event " + id + " on calendar " + cal + ": " + what + ". It is " + when + ". Google emailed no one about it.", nil
	}
	var newcomers []string
	for _, a := range added {
		if !o.self(a) {
			newcomers = append(newcomers, a)
		}
	}
	if err := c.access.checkRecipients(newcomers); err != nil {
		return nil, err
	}
	people := reached(o, cur, append(after, removed...))
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	fields := append(eventFields(o, cal, cur), changes...)
	return &held{
		title:      "Change " + eventTitle(cur) + ", telling " + peopleWords(len(people)),
		fields:     fields,
		recipients: people,
		run: func(ctx context.Context, c *call) (any, error) {
			var done calEvent
			if err := c.g.send(ctx, c.token, http.MethodPatch, googleAPI, eventPath(cal, id)+sendQuery, payload, maxAnswerBytes, &done); err != nil {
				return nil, unsure(err, "the event changed: get_event shows it as it is")
			}
			c.audit = "changed event " + id + " on calendar " + cal + ": " + what + "; told " + orNone(idList(people))
			return "Changed event " + id + " on calendar " + cal + ": " + what + ". It is " + whenWords(done.Start, done.End) + ". Google told " + toldWords(people) + ".", nil
		},
	}, nil
}

// toldWords is whom Google told, for an answer.
func toldWords(people []string) string {
	if len(people) == 0 {
		return "no one Kivali can name"
	}
	return strings.Join(people, ", ")
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
// else is a write, telling no one. One that reaches others is a send
// (isSend), which needs calendar_send: it is held for the owner, and
// Run deletes it with sendUpdates=all, so Google tells them.
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
	if !send {
		if err := c.g.change(ctx, c.token, googleAPI, http.MethodDelete, eventPath(cal, id)+writeQuery, nil, nil); err != nil {
			return nil, err
		}
		c.audit = "deleted event " + id + " on calendar " + cal + ", which " + creator + " created"
		return "Deleted event " + id + " from calendar " + cal + ". Google emailed no one about it.", nil
	}
	people := reached(o, e, invitedNow(e))
	note := "This cancels the event for everyone invited."
	if !e.Organizer.Self && !o.self(calAddress(e.Organizer.Email)) {
		note = "This takes the event off the owner's calendar, and Google tells its organizer the owner will not come."
	}
	return &held{
		title:      "Delete " + eventTitle(e) + ", telling " + peopleWords(len(people)),
		fields:     append(eventFields(o, cal, e), field("What happens", note)),
		recipients: people,
		run: func(ctx context.Context, c *call) (any, error) {
			if err := c.g.send(ctx, c.token, http.MethodDelete, googleAPI, eventPath(cal, id)+sendQuery, nil, maxAnswerBytes, nil); err != nil {
				return nil, unsure(err, "the event was deleted: get_event answers whether it is still there")
			}
			c.audit = "deleted event " + id + " on calendar " + cal + ", which " + creator + " created; told " + orNone(idList(people))
			return "Deleted event " + id + " from calendar " + cal + ". Google told " + toldWords(people) + ".", nil
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
// someone else must have organized it. It is always a send, held for
// the owner, and Run patches the owner's own answer alone, with
// attendeesOmitted, as Calendar takes a participant's answer, and
// sendUpdates=all, so Google tells the organizer.
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
		run: func(ctx context.Context, c *call) (any, error) {
			if err := c.g.send(ctx, c.token, http.MethodPatch, googleAPI, eventPath(cal, id)+sendQuery, payload, maxAnswerBytes, nil); err != nil {
				return nil, unsure(err, "the answer was given: get_event shows how the owner answered")
			}
			c.audit = "answered " + answer + " to event " + id + " on calendar " + cal + "; told " + orNone(idList(people))
			return "Answered " + word + " to event " + id + " on calendar " + cal + ". Google told its organizer, " + toldWords(people) + ".", nil
		},
	}, nil
}
