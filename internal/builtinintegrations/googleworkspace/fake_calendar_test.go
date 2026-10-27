package googleworkspace

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// fakeCalendar is Google Calendar's API run in-process on its own
// www.googleapis.com origin, and Gmail's profile on its own: the
// account's calendar list, each calendar's events (listed, read,
// inserted, patched and deleted as Calendar does, each with an etag
// that every change moves on, a patch or a delete with an If-Match of
// another refused 412) and their busy times. Every request must carry
// testToken as its bearer, or it is answered 401 as Google answers.
type fakeCalendar struct {
	gmail, www *httptest.Server

	mu   sync.Mutex
	seen []fakeCalRequest
	next int
	// version numbers the etags the fake gives.
	version int

	// calendars is the account's list; events each calendar's, as
	// Calendar answers them; busy each calendar's busy times as
	// freeBusy answers them, and busyErrors the reason it gives in
	// place of them. A calendar with events or busy times that the list
	// does not name is one Google serves all the same, as a colleague's
	// calendar the account does not list. Set before the first call.
	calendars  []calendarEntry
	events     map[string][]map[string]any
	busy       map[string][]map[string]string
	busyErrors map[string]string
}

// fakeCalRequest is one request the fake was sent: its method, its
// path as sent (escaped), its query, its body and its If-Match header.
type fakeCalRequest struct {
	method, path, query, body, ifMatch string
}

func (r fakeCalRequest) String() string { return r.method + " " + r.path + "?" + r.query }

// fakeCalendars is the account's calendar list: the primary, which the
// owner owns; a calendar made in Google Calendar the owner owns; a
// colleague's the owner may change events on; and a holiday calendar the owner may read,
// whose id holds a "#".
var fakeCalendars = []calendarEntry{
	{ID: "team@group.calendar.google.com", Summary: "Team", AccessRole: "owner", TimeZone: "Europe/London"},
	{ID: "boss@example.org", Summary: "Boss", AccessRole: "writer", TimeZone: "America/New_York"},
	{ID: "dana@example.com", Summary: "dana@example.com", Primary: true, AccessRole: "owner", TimeZone: "Europe/London"},
	{ID: "en.uk#holiday@group.v.calendar.google.com", Summary: "Holidays in the United Kingdom", SummaryOverride: "Bank holidays", AccessRole: "reader"},
}

// newFakeCalendar starts the fake, and g reaching it, its clock at
// monday.
func newFakeCalendar(t *testing.T) (*fakeCalendar, *GoogleWorkspace) {
	t.Helper()
	f := &fakeCalendar{calendars: slices.Clone(fakeCalendars), events: map[string][]map[string]any{},
		busy: map[string][]map[string]string{}, busyErrors: map[string]string{}}
	f.gmail = httptest.NewServer(http.HandlerFunc(f.serveGmail))
	f.www = httptest.NewServer(http.HandlerFunc(f.serveCalendar))
	t.Cleanup(func() { f.gmail.Close(); f.www.Close() })
	return f, &GoogleWorkspace{GmailBase: f.gmail.URL, APIBase: f.www.URL, Clock: clock.NewFakeAt(monday)}
}

// add puts the events, each as Calendar's JSON, on the calendar cal.
func (f *fakeCalendar) add(t *testing.T, cal string, events ...string) {
	t.Helper()
	for _, s := range events {
		var e map[string]any
		if err := json.Unmarshal([]byte(s), &e); err != nil {
			t.Fatalf("event %s: %v", s, err)
		}
		if _, ok := e["status"]; !ok {
			e["status"] = "confirmed"
		}
		if _, ok := e["etag"]; !ok {
			e["etag"] = f.etag()
		}
		f.events[cal] = append(f.events[cal], e)
	}
}

// etag is a new etag, as Calendar writes one: a number in quotes.
func (f *fakeCalendar) etag() string {
	f.version++
	return fmt.Sprintf(`"%d"`, 3181161784712000+f.version)
}

// touch changes the event id on the calendar cal as someone else's edit
// does in Google Calendar: its title, and so its etag.
func (f *fakeCalendar) touch(cal, id, title string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events[cal] {
		if e["id"] == id {
			e["summary"], e["etag"] = title, f.etag()
		}
	}
}

// drop deletes the event id on the calendar cal as someone else does
// in Google Calendar.
func (f *fakeCalendar) drop(cal, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[cal] = slices.DeleteFunc(f.events[cal], func(e map[string]any) bool { return e["id"] == id })
}

// requests is every request the fake was sent to the Calendar origin.
func (f *fakeCalendar) requests() []fakeCalRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen)
}

// written is the requests the fake was sent that are not GETs.
func (f *fakeCalendar) written() []fakeCalRequest {
	var out []fakeCalRequest
	for _, r := range f.requests() {
		if r.method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

// eventOn is the event id on the calendar cal as the fake holds it,
// nil for none.
func (f *fakeCalendar) eventOn(cal, id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events[cal] {
		if e["id"] == id {
			return e
		}
	}
	return nil
}

func (f *fakeCalendar) serveGmail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		http.Error(w, `{"error":{"code":401,"message":"Request had invalid authentication credentials."}}`, http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/gmail/v1/users/me/profile" {
		_, _ = w.Write([]byte(`{"emailAddress":"dana@example.com"}`))
		return
	}
	http.Error(w, `{"error":{"code":404,"message":"not found"}}`, http.StatusNotFound)
}

func calNotFound(w http.ResponseWriter) {
	http.Error(w, `{"error":{"code":404,"message":"Not Found","errors":[{"reason":"notFound"}]}}`, http.StatusNotFound)
}

func (f *fakeCalendar) serveCalendar(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, fakeCalRequest{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, string(body), r.Header.Get("If-Match")})
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		http.Error(w, `{"error":{"code":401,"message":"Request had invalid authentication credentials."}}`, http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/calendar/v3/")
	if !ok {
		calNotFound(w)
		return
	}
	parts := strings.Split(rest, "/")
	for i, p := range parts {
		parts[i], _ = url.PathUnescape(p)
	}
	switch {
	case r.Method == http.MethodGet && rest == "users/me/calendarList":
		writeJSON(w, map[string]any{"items": f.calendars})
	case r.Method == http.MethodPost && rest == "freeBusy":
		f.freeBusy(w, body)
	case len(parts) >= 3 && parts[0] == "calendars" && parts[2] == "events":
		cal := parts[1]
		if !f.served(cal) {
			calNotFound(w)
			return
		}
		switch {
		case len(parts) == 3 && r.Method == http.MethodGet:
			f.list(w, r, cal)
		case len(parts) == 3 && r.Method == http.MethodPost:
			f.insert(w, cal, body)
		case len(parts) == 4:
			f.one(w, r, cal, parts[3], body)
		default:
			calNotFound(w)
		}
	default:
		calNotFound(w)
	}
}

// served reports whether Google answers for the calendar cal: one of
// the list, or one given events or busy times.
func (f *fakeCalendar) served(cal string) bool {
	return slices.ContainsFunc(f.calendars, func(c calendarEntry) bool { return c.ID == cal }) || f.events[cal] != nil || f.busy[cal] != nil
}

// startOf is when an event starts or ends (key "start" or "end"), a
// date as its midnight in UTC.
func startOf(e map[string]any, key string) time.Time {
	m, _ := e[key].(map[string]any)
	if s, ok := m["dateTime"].(string); ok {
		t, _ := time.Parse(time.RFC3339, s)
		return t
	}
	s, _ := m["date"].(string)
	t, _ := time.Parse(time.DateOnly, s)
	return t
}

// list is events.list as Calendar answers it with singleEvents and
// orderBy startTime: the events ending after timeMin and starting
// before timeMax whose title holds q (the fake's stand-in for
// Calendar's search), soonest first, maxResults a page, the next
// page's token the offset it starts at.
func (f *fakeCalendar) list(w http.ResponseWriter, r *http.Request, cal string) {
	q := r.URL.Query()
	if q.Get("singleEvents") != "true" || q.Get("orderBy") != "startTime" {
		http.Error(w, `{"error":{"code":400,"message":"the fake lists single events by start time"}}`, http.StatusBadRequest)
		return
	}
	from, err1 := time.Parse(time.RFC3339, q.Get("timeMin"))
	to, err2 := time.Parse(time.RFC3339, q.Get("timeMax"))
	if err1 != nil || err2 != nil {
		http.Error(w, `{"error":{"code":400,"message":"Bad Request"}}`, http.StatusBadRequest)
		return
	}
	var hits []map[string]any
	for _, e := range f.events[cal] {
		title, _ := e["summary"].(string)
		if startOf(e, "end").After(from) && startOf(e, "start").Before(to) && strings.Contains(strings.ToLower(title), strings.ToLower(q.Get("q"))) {
			hits = append(hits, e)
		}
	}
	slices.SortStableFunc(hits, func(a, b map[string]any) int { return startOf(a, "start").Compare(startOf(b, "start")) })
	size, _ := strconv.Atoi(q.Get("maxResults"))
	if size <= 0 {
		size = 250
	}
	at, _ := strconv.Atoi(q.Get("pageToken"))
	out := map[string]any{"items": hits[min(at, len(hits)):min(at+size, len(hits))], "timeZone": "Europe/London"}
	if at+size < len(hits) {
		out["nextPageToken"] = strconv.Itoa(at + size)
	}
	writeJSON(w, out)
}

// insert is events.insert: the event as given, with an id, a status,
// and the calendar as its organizer and the account as its creator.
func (f *fakeCalendar) insert(w http.ResponseWriter, cal string, body []byte) {
	var e map[string]any
	if err := json.Unmarshal(body, &e); err != nil {
		http.Error(w, `{"error":{"code":400,"message":"Parse Error"}}`, http.StatusBadRequest)
		return
	}
	f.next++
	e["id"] = fmt.Sprintf("ev%03d", f.next)
	e["status"] = "confirmed"
	e["etag"] = f.etag()
	e["organizer"] = map[string]any{"email": cal, "self": true}
	e["creator"] = map[string]any{"email": "dana@example.com", "self": true}
	f.events[cal] = append(f.events[cal], e)
	writeJSON(w, e)
}

// one is events.get, events.patch (its fields merged in, an object's
// fields merged and null clearing one, and a new etag) and
// events.delete, which answers no content. A patch or a delete whose
// If-Match is not the event's etag is refused 412, as Calendar refuses
// one.
func (f *fakeCalendar) one(w http.ResponseWriter, r *http.Request, cal, id string, body []byte) {
	i := slices.IndexFunc(f.events[cal], func(e map[string]any) bool { return e["id"] == id })
	if i < 0 {
		calNotFound(w)
		return
	}
	if m := r.Header.Get("If-Match"); m != "" && r.Method != http.MethodGet && m != f.events[cal][i]["etag"] {
		http.Error(w, `{"error":{"code":412,"message":"Precondition Failed","errors":[{"reason":"conditionNotMet"}]}}`, http.StatusPreconditionFailed)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, f.events[cal][i])
	case http.MethodPatch:
		var patch map[string]any
		if err := json.Unmarshal(body, &patch); err != nil {
			http.Error(w, `{"error":{"code":400,"message":"Parse Error"}}`, http.StatusBadRequest)
			return
		}
		mergePatch(f.events[cal][i], patch)
		f.events[cal][i]["etag"] = f.etag()
		writeJSON(w, f.events[cal][i])
	case http.MethodDelete:
		f.events[cal] = slices.Delete(f.events[cal], i, i+1)
		w.WriteHeader(http.StatusNoContent)
	default:
		calNotFound(w)
	}
}

func mergePatch(dst, patch map[string]any) {
	for k, v := range patch {
		switch v := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			cur, ok := dst[k].(map[string]any)
			if !ok {
				cur = map[string]any{}
				dst[k] = cur
			}
			mergePatch(cur, v)
		default:
			dst[k] = v
		}
	}
}

// freeBusy is freeBusy.query: each calendar asked for, its busy times
// or the reason it gives none; one not in the list is notFound.
func (f *fakeCalendar) freeBusy(w http.ResponseWriter, body []byte) {
	var q struct {
		TimeMin string `json:"timeMin"`
		TimeMax string `json:"timeMax"`
		Items   []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &q); err != nil || q.TimeMin == "" || q.TimeMax == "" {
		http.Error(w, `{"error":{"code":400,"message":"Bad Request"}}`, http.StatusBadRequest)
		return
	}
	cals := map[string]any{}
	for _, it := range q.Items {
		reason := f.busyErrors[it.ID]
		if reason == "" && !f.served(it.ID) {
			reason = "notFound"
		}
		if reason != "" {
			cals[it.ID] = map[string]any{"errors": []map[string]string{{"domain": "global", "reason": reason}}, "busy": []any{}}
			continue
		}
		busy := f.busy[it.ID]
		if busy == nil {
			busy = []map[string]string{}
		}
		cals[it.ID] = map[string]any{"busy": busy}
	}
	writeJSON(w, map[string]any{"kind": "calendar#freeBusy", "timeMin": q.TimeMin, "timeMax": q.TimeMax, "calendars": cals})
}
