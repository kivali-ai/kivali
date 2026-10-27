package googleworkspace

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// testToken is the access token core hands in, in every test.
const testToken = "ya29.test-access-token"

// testConn is a connection as core gives it to a call: the client id
// setup kept and the current access token.
func testConn() builtinintegrations.Connection {
	return builtinintegrations.Connection{ID: "c0ffee01", Fields: map[string]string{
		"client_id": "123-abc" + clientIDSuffix, CredentialKey: testToken,
	}}
}

// agent is a caller whose access is m, as core reads a stored form:
// lists as []any.
func agent(m map[string]any) builtinintegrations.Caller {
	out := map[string]any{}
	for k, v := range m {
		if list, ok := v.([]string); ok {
			anys := make([]any, len(list))
			for i, s := range list {
				anys[i] = s
			}
			v = anys
		}
		out[k] = v
	}
	return builtinintegrations.Caller{Agent: "dana", Access: out}
}

// fakeGoogle is Google's APIs run in-process: one server per origin the
// built-in reaches, each answering only its own API's paths, so a call
// sent to the wrong origin fails. Every request must carry testToken as
// its bearer, or it is answered 401 as Google answers.
type fakeGoogle struct {
	gmail, www, docs *httptest.Server

	mu   sync.Mutex
	seen []string
	// fail answers the path (without its query) with this status and
	// Google error body, in place of its answer.
	fail map[string]fakeError

	// mail is the account's messages, newest first, as Gmail answers
	// them in full; held is the content Gmail keeps apart from them, by
	// attachment id, in base64url. Both are set before the first call
	// and only read after.
	mail []gmailMessage
	held map[string]string
}

type fakeError struct {
	status int
	body   string
	// location, when set, is sent as the Location header.
	location string
}

// newFakeGoogle starts the fake, and g reaching it.
func newFakeGoogle(t *testing.T) (*fakeGoogle, *GoogleWorkspace) {
	t.Helper()
	f := &fakeGoogle{fail: map[string]fakeError{}, held: map[string]string{}}
	f.gmail = httptest.NewServer(f.handler("gmail"))
	f.www = httptest.NewServer(f.handler("www"))
	f.docs = httptest.NewServer(f.handler("docs"))
	t.Cleanup(func() { f.gmail.Close(); f.www.Close(); f.docs.Close() })
	return f, &GoogleWorkspace{GmailBase: f.gmail.URL, APIBase: f.www.URL, DocsBase: f.docs.URL}
}

// requests is every request the fake was sent, "<origin> <path>?<query>".
func (f *fakeGoogle) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fakeGoogle) failWith(path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[path] = fakeError{status: status, body: body}
}

// redirectTo answers the path with a 302 to location.
func (f *fakeGoogle) redirectTo(path, location string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[path] = fakeError{status: http.StatusFound, location: location}
}

// fakeLabels is the account's labels as Gmail lists them: its own,
// among them ones a listing leaves out (UNREAD, a category), and the
// account's, out of order.
const fakeLabels = `{"labels":[
	{"id":"Label_9","name":"zeta","type":"user"},
	{"id":"CATEGORY_SOCIAL","name":"CATEGORY_SOCIAL","type":"system"},
	{"id":"SENT","name":"SENT","type":"system"},
	{"id":"INBOX","name":"INBOX","type":"system"},
	{"id":"UNREAD","name":"UNREAD","type":"system"},
	{"id":"Label_12","name":"Clients (old)","type":"user"},
	{"id":"DRAFT","name":"DRAFT","type":"system"},
	{"id":"Label_3","name":"Alpha/Work","type":"user"},
	{"id":"TRASH","name":"TRASH","type":"system"}
]}`

func (f *fakeGoogle) handler(origin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, origin+" "+r.URL.Path+"?"+r.URL.RawQuery)
		fail, failing := f.fail[r.URL.Path]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":{"code":405,"message":"not a read"}}`, http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			// Google's words do not echo the credential, but a
			// service in front of it might: the header is quoted so a
			// test can see that nothing passed on carries it.
			w.WriteHeader(http.StatusUnauthorized)
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": 401, "status": "UNAUTHENTICATED",
				"message": "Request had invalid authentication credentials. Got " + r.Header.Get("Authorization")}})
			_, _ = w.Write(b)
			return
		}
		if failing {
			if fail.location != "" {
				w.Header().Set("Location", fail.location)
			}
			w.WriteHeader(fail.status)
			// {{auth}} in a body is the request's Authorization header.
			_, _ = w.Write([]byte(strings.ReplaceAll(fail.body, "{{auth}}", r.Header.Get("Authorization"))))
			return
		}
		answer := func(v string) { _, _ = w.Write([]byte(v)) }
		switch {
		case origin == "gmail" && r.URL.Path == "/gmail/v1/users/me/profile":
			answer(`{"emailAddress":"dana@example.com","messagesTotal":12,"threadsTotal":9,"historyId":"77"}`)
		case origin == "gmail" && r.URL.Path == "/gmail/v1/users/me/labels":
			answer(fakeLabels)
		case origin == "gmail" && r.URL.Path == "/gmail/v1/users/me/messages":
			f.listMessages(w, r)
		case origin == "gmail" && strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/messages/"):
			f.getMessage(w, r, strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/messages/"))
		case origin == "gmail" && strings.HasPrefix(r.URL.Path, "/gmail/v1/users/me/threads/"):
			id := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me/threads/")
			var msgs []gmailMessage
			for _, m := range slices.Backward(f.mail) {
				if m.ThreadID == id {
					msgs = append(msgs, m)
				}
			}
			if len(msgs) == 0 || r.URL.Query().Get("format") != "full" {
				http.Error(w, `{"error":{"code":404,"message":"Requested entity was not found."}}`, http.StatusNotFound)
				return
			}
			writeJSON(w, map[string]any{"id": id, "messages": msgs})
		case origin == "www" && r.URL.Path == "/calendar/v3/users/me/calendarList":
			// Two pages: Google pages a list by nextPageToken.
			if r.URL.Query().Get("pageToken") == "" {
				answer(`{"items":[{"id":"team@group.calendar.google.com","summary":"Team"},{"id":"b@group.calendar.google.com","summary":"Holidays","summaryOverride":"Bank holidays"}],"nextPageToken":"p2"}`)
				return
			}
			answer(`{"items":[{"id":"dana@example.com","summary":"dana@example.com","primary":true},{"id":"x@group.calendar.google.com"}]}`)
		case origin == "www" && r.URL.Path == "/drive/v3/files":
			if q := r.URL.Query().Get("q"); q != folderQuery {
				http.Error(w, `{"error":{"code":400,"message":"wrong q `+strings.ReplaceAll(q, `"`, "")+`"}}`, http.StatusBadRequest)
				return
			}
			if r.URL.Query().Get("pageToken") == "" {
				answer(`{"files":[{"id":"f2","name":"projects"},{"id":"f1","name":"Archive"}],"nextPageToken":"n2"}`)
				return
			}
			answer(`{"files":[{"id":"f3","name":"Budget (2026)"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": 404, "message": "no " + r.URL.Path + " on " + origin}})
			_, _ = w.Write(b)
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(b)
}

// listMessages is users/me/messages as Gmail answers it: the messages
// carrying every label labelIds names, whose subject holds q (the
// fake's stand-in for Gmail's search words), Spam and Trash left out
// unless asked for, newest first, maxResults a page, the next page's
// token the offset it starts at.
func (f *fakeGoogle) listMessages(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	labels := query["labelIds"]
	spam := query.Get("includeSpamTrash") == "true"
	var hits []gmailMessage
	for _, m := range f.mail {
		if !spam && (slices.Contains(m.LabelIDs, "SPAM") || slices.Contains(m.LabelIDs, "TRASH")) && !slices.Contains(labels, "SPAM") && !slices.Contains(labels, "TRASH") {
			continue
		}
		if !strings.Contains(strings.ToLower(header(m.Payload.Headers, "Subject")), strings.ToLower(query.Get("q"))) {
			continue
		}
		if !slices.ContainsFunc(labels, func(l string) bool { return !slices.Contains(m.LabelIDs, l) }) {
			hits = append(hits, m)
		}
	}
	size, _ := strconv.Atoi(query.Get("maxResults"))
	if size <= 0 {
		size = 100
	}
	from, _ := strconv.Atoi(query.Get("pageToken"))
	out := map[string]any{"resultSizeEstimate": len(hits)}
	var page []map[string]string
	for _, m := range hits[min(from, len(hits)):min(from+size, len(hits))] {
		page = append(page, map[string]string{"id": m.ID, "threadId": m.ThreadID})
	}
	if page != nil {
		out["messages"] = page
	}
	if from+size < len(hits) {
		out["nextPageToken"] = strconv.Itoa(from + size)
	}
	writeJSON(w, out)
}

// getMessage is users/me/messages/{id} in format full, or metadata
// (the headers metadataHeaders names, no parts and no body), and
// users/me/messages/{id}/attachments/{id}, the content Gmail keeps
// apart.
func (f *fakeGoogle) getMessage(w http.ResponseWriter, r *http.Request, rest string) {
	id, att, isAtt := strings.Cut(rest, "/attachments/")
	i := slices.IndexFunc(f.mail, func(m gmailMessage) bool { return m.ID == id })
	if i < 0 {
		http.Error(w, `{"error":{"code":404,"message":"Requested entity was not found."}}`, http.StatusNotFound)
		return
	}
	if isAtt {
		data, ok := f.held[att]
		if !ok {
			http.Error(w, `{"error":{"code":400,"message":"Invalid attachment token"}}`, http.StatusBadRequest)
			return
		}
		raw, _ := base64.RawURLEncoding.DecodeString(data)
		writeJSON(w, map[string]any{"attachmentId": att, "size": len(raw), "data": data})
		return
	}
	m := f.mail[i]
	switch r.URL.Query().Get("format") {
	case "full":
	case "metadata":
		want := r.URL.Query()["metadataHeaders"]
		var hs []gmailHeader
		for _, h := range m.Payload.Headers {
			if slices.ContainsFunc(want, func(n string) bool { return strings.EqualFold(n, h.Name) }) {
				hs = append(hs, h)
			}
		}
		m.Payload = gmailPart{MimeType: m.Payload.MimeType, Headers: hs}
	default:
		http.Error(w, `{"error":{"code":400,"message":"the fake answers format full or metadata"}}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, m)
}
