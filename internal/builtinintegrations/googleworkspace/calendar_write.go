package googleworkspace

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

// invitesNotAvailable is the answer to a create_event or update_event
// call that isSend finds is a send.
const invitesNotAvailable = "Inviting people to an event, or changing who it invites, waits for the owner's approval, which this version of Kivali does not have, so Kivali did not run this call. Leave out attendees, or give the event's own list unchanged, to create or change the rest of the event."

// isSend reports whether a create_event or update_event call is a send:
// its attendees, ignoring case, order and repeats, are a list other
// than current, the addresses the event invites now (none for
// create_event). A call that gives no attendees, or null, changes no
// one's invitation. Every address must be one: a list that is not is
// refused.
func isSend(a args, current []string) (bool, error) {
	list, given, err := attendeesArg(a)
	if err != nil || !given {
		return false, err
	}
	return !slices.Equal(list, addressSet(current)), nil
}

// attendeesArg is the attendees argument as addressSet makes it, and
// whether the call gives it.
func attendeesArg(a args) ([]string, bool, error) {
	list, given, err := textListArg(a, "attendees", maxAttendees, 254)
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
// calendarArg reads it, when the owner owns it: its role in the
// account's list is owner. Any other is refused before an event is
// read or changed.
func (cc *calCall) ownedCalendar(ctx context.Context, required bool) (string, error) {
	cal, err := cc.calendarArg(ctx, "calendar_id", required)
	if err != nil {
		return "", err
	}
	e, ok, err := cc.entry(ctx, cal)
	if err != nil {
		return "", err
	}
	if !ok || e.AccessRole != "owner" {
		return "", refuse("Calendar "+cal+" is not one the owner owns, and you may change events only on calendars the owner owns; list_calendars gives each calendar's role.",
			"calendar "+cal+" is not one the owner owns")
	}
	return cal, nil
}

// optionalBool is the boolean argument key, and whether the call gives it.
func optionalBool(a args, key string) (on, given bool, err error) {
	v, ok := a[key]
	if !ok || v == nil {
		return false, false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, false, badArgument(key + " is true or false.")
	}
	return b, true, nil
}

// argGiven reports whether the call gives the argument key, null being
// none.
func argGiven(a args, key string) bool {
	v, ok := a[key]
	return ok && v != nil
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
	allDay, allDayGiven, err := optionalBool(a, "all_day")
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
const writeQuery = "?sendUpdates=none"

// createEvent creates an event on a calendar the owner owns, marked as
// Kivali's (kivaliMarker) with the agent's slug, telling no one. A
// call that invites people is a send, which this version does not run.
func createEvent(ctx context.Context, c *call) (any, error) {
	send, err := isSend(c.args, nil)
	if err != nil {
		return nil, err
	}
	if send {
		return nil, failure(invitesNotAvailable)
	}
	if c.agent == "" {
		return nil, errors.New("the call names no agent to mark the event with")
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
		"extendedProperties": map[string]any{"private": map[string]string{kivaliMarker: c.agent}},
	}
	if desc != "" {
		body["description"] = desc
	}
	if loc != "" {
		body["location"] = loc
	}
	var made calEvent
	if err := c.g.calendarRequest(ctx, c.token, http.MethodPost, eventsPath(cal)+writeQuery, body, &made); err != nil {
		return nil, err
	}
	if !eventIDPattern.MatchString(made.ID) {
		return nil, failure("Google created the event on calendar " + cal + " and gave it an id Kivali does not read; list_events finds it.")
	}
	when := whenWords(made.Start, made.End)
	c.audit = "created event " + made.ID + " on calendar " + cal + ", " + when
	return "Created event " + made.ID + " on calendar " + cal + ", " + when + ". Google emailed no one about it, and Kivali marked it as created by Kivali.", nil
}

// updateEvent changes the fields the call gives of an event on a
// calendar the owner owns, telling no one. A call whose attendees are
// other than the event's is a send, which this version does not run;
// one that gives the event's own list leaves it as it is.
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
	send, err := isSend(c.args, invited(cur))
	if err != nil {
		return nil, err
	}
	if send {
		return nil, failure(invitesNotAvailable)
	}
	body := map[string]any{}
	var changed []string
	if argGiven(c.args, "title") {
		title, err := c.args.text("title", true, maxTitle)
		if err != nil {
			return nil, err
		}
		body["summary"] = title
		changed = append(changed, "title")
	}
	for _, f := range []struct {
		key, field string
		most       int
	}{{"description", "description", maxDescription}, {"location", "location", maxLocation}} {
		if !argGiven(c.args, f.key) {
			continue
		}
		s, err := c.args.text(f.key, false, f.most)
		if err != nil {
			return nil, err
		}
		body[f.field] = s
		changed = append(changed, f.key)
	}
	start, end, timed, err := eventTimes(c.args, &cur)
	if err != nil {
		return nil, err
	}
	if timed {
		body["start"], body["end"] = start, end
		changed = append(changed, "start and end")
	}
	if len(body) == 0 {
		return nil, badArgument("Give at least one field to change: title, start, end, time_zone, description, location or all_day.")
	}
	var after calEvent
	if err := c.g.calendarRequest(ctx, c.token, http.MethodPatch, eventPath(cal, id)+writeQuery, body, &after); err != nil {
		return nil, err
	}
	what := strings.Join(changed, ", ")
	when := whenWords(after.Start, after.End)
	c.audit = "changed event " + id + " on calendar " + cal + ": " + what
	return "Changed event " + id + " on calendar " + cal + ": " + what + ". It is " + when + ". Google emailed no one about it.", nil
}

// deleteEvent deletes an event Kivali created (kivaliMarker set, by any
// agent) from a calendar the owner owns, telling no one; any other
// event is refused before anything is changed.
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
	if err := c.g.calendarRequest(ctx, c.token, http.MethodDelete, eventPath(cal, id)+writeQuery, nil, nil); err != nil {
		return nil, err
	}
	c.audit = "deleted event " + id + " on calendar " + cal + ", which " + oneLine(e.ExtendedProperties.Private[kivaliMarker], 64) + " created"
	return "Deleted event " + id + " from calendar " + cal + ". Google emailed no one about it.", nil
}
