package googleworkspace

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
}

type fakeError struct {
	status int
	body   string
}

// newFakeGoogle starts the fake, and g reaching it.
func newFakeGoogle(t *testing.T) (*fakeGoogle, *GoogleWorkspace) {
	t.Helper()
	f := &fakeGoogle{fail: map[string]fakeError{}}
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
	f.fail[path] = fakeError{status, body}
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
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`))
			return
		}
		if failing {
			w.WriteHeader(fail.status)
			_, _ = w.Write([]byte(fail.body))
			return
		}
		answer := func(v string) { _, _ = w.Write([]byte(v)) }
		switch {
		case origin == "gmail" && r.URL.Path == "/gmail/v1/users/me/profile":
			answer(`{"emailAddress":"dana@example.com","messagesTotal":12,"threadsTotal":9,"historyId":"77"}`)
		case origin == "gmail" && r.URL.Path == "/gmail/v1/users/me/labels":
			answer(fakeLabels)
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
