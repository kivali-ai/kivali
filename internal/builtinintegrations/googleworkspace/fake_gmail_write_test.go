package googleworkspace

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeGmailWrites is the fake's Gmail changes, in front of its reads:
// drafts (users/me/drafts), labels on messages (messages/{id}/modify),
// and the reads only a change makes (a message in format minimal, a
// thread in format metadata). Every other request goes on to the
// fake's own handler. Its requests are in fakeGoogle.requests, a
// change's as "gmail <method> <path>?<query>".
type fakeGmailWrites struct {
	f *fakeGoogle

	mu     sync.Mutex
	drafts []fakeDraft
	made   int
	// bodies is each change's request body, by "<method> <path>".
	bodies map[string]string
}

// fakeDraft is one draft: its ids, the thread id it was asked to join,
// and its message as it was written.
type fakeDraft struct {
	id, messageID, threadID string
	askedThread             string
	raw                     []byte
}

// newFakeGmail starts the fake with its Gmail changes, and g reaching it.
func newFakeGmail(t *testing.T) (*fakeGoogle, *fakeGmailWrites, *GoogleWorkspace) {
	t.Helper()
	f := &fakeGoogle{fail: map[string]fakeError{}, held: map[string]string{}}
	w := &fakeGmailWrites{f: f, bodies: map[string]string{}}
	f.gmail = httptest.NewServer(w.handler(f.handler("gmail")))
	f.www = httptest.NewServer(f.handler("www"))
	f.docs = httptest.NewServer(f.handler("docs"))
	t.Cleanup(func() { f.gmail.Close(); f.www.Close(); f.docs.Close() })
	return f, w, &GoogleWorkspace{GmailBase: f.gmail.URL, APIBase: f.www.URL, DocsBase: f.docs.URL}
}

// addDraft gives the account a draft written elsewhere, its message raw.
func (w *fakeGmailWrites) addDraft(id, raw string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.drafts = append(w.drafts, fakeDraft{id: id, messageID: "m" + strings.ReplaceAll(id, "-", ""), threadID: "t" + strings.ReplaceAll(id, "-", ""), raw: []byte(raw)})
}

// draft is the draft id, and whether the account has it.
func (w *fakeGmailWrites) draft(id string) (fakeDraft, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	i := slices.IndexFunc(w.drafts, func(d fakeDraft) bool { return d.id == id })
	if i < 0 {
		return fakeDraft{}, false
	}
	return w.drafts[i], true
}

// body is the request body of the last change sent as method on path.
func (w *fakeGmailWrites) body(method, path string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bodies[method+" "+path]
}

func (w *fakeGmailWrites) handler(reads http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		path, q := r.URL.Path, r.URL.Query()
		mine := r.Method != http.MethodGet || strings.HasPrefix(path, "/gmail/v1/users/me/drafts/") ||
			(strings.HasPrefix(path, "/gmail/v1/users/me/threads/") && q.Get("format") == "metadata") ||
			(strings.HasPrefix(path, "/gmail/v1/users/me/messages/") && q.Get("format") == "minimal")
		if !mine {
			reads.ServeHTTP(rw, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		seen := "gmail " + path + "?" + r.URL.RawQuery
		if r.Method != http.MethodGet {
			seen = "gmail " + r.Method + " " + path + "?" + r.URL.RawQuery
		}
		f := w.f
		f.mu.Lock()
		f.seen = append(f.seen, seen)
		fail, failing := f.fail[path]
		f.mu.Unlock()
		w.mu.Lock()
		w.bodies[r.Method+" "+path] = string(body)
		w.mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(rw, `{"error":{"code":401,"message":"Request had invalid authentication credentials.","status":"UNAUTHENTICATED"}}`, http.StatusUnauthorized)
			return
		}
		if failing {
			rw.WriteHeader(fail.status)
			_, _ = rw.Write([]byte(fail.body))
			return
		}
		notFound := func() {
			http.Error(rw, `{"error":{"code":404,"message":"Requested entity was not found."}}`, http.StatusNotFound)
		}
		draftID, drafts := strings.CutPrefix(path, "/gmail/v1/users/me/drafts/")
		switch {
		case r.Method == http.MethodPost && path == "/gmail/v1/users/me/drafts":
			w.createDraft(rw, body)
		case r.Method == http.MethodGet && drafts && draftID != "":
			d, ok := w.draft(draftID)
			if !ok {
				notFound()
				return
			}
			if q.Get("format") != "metadata" {
				http.Error(rw, `{"error":{"code":400,"message":"the fake answers a draft in format metadata"}}`, http.StatusBadRequest)
				return
			}
			msg, err := mail.ReadMessage(bytes.NewReader(d.raw))
			if err != nil {
				http.Error(rw, `{"error":{"code":500,"message":"a draft does not read"}}`, http.StatusInternalServerError)
				return
			}
			var hs []gmailHeader
			for name, values := range msg.Header {
				for _, v := range values {
					hs = append(hs, gmailHeader{name, v})
				}
			}
			writeJSON(rw, map[string]any{"id": d.id, "message": gmailMessage{ID: d.messageID, ThreadID: d.threadID, LabelIDs: []string{"DRAFT"},
				Payload: gmailPart{MimeType: "text/plain", Headers: hs}}})
		case r.Method == http.MethodDelete && drafts && draftID != "":
			w.mu.Lock()
			before := len(w.drafts)
			w.drafts = slices.DeleteFunc(w.drafts, func(d fakeDraft) bool { return d.id == draftID })
			gone := len(w.drafts) < before
			w.mu.Unlock()
			if !gone {
				notFound()
				return
			}
			rw.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasPrefix(path, "/gmail/v1/users/me/messages/") && strings.HasSuffix(path, "/modify"):
			w.modify(rw, strings.TrimSuffix(strings.TrimPrefix(path, "/gmail/v1/users/me/messages/"), "/modify"), body)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/gmail/v1/users/me/messages/"):
			f.mu.Lock()
			i := slices.IndexFunc(f.mail, func(m gmailMessage) bool { return m.ID == strings.TrimPrefix(path, "/gmail/v1/users/me/messages/") })
			var m gmailMessage
			if i >= 0 {
				m = f.mail[i]
			}
			f.mu.Unlock()
			if i < 0 {
				notFound()
				return
			}
			writeJSON(rw, map[string]any{"id": m.ID, "threadId": m.ThreadID, "labelIds": m.LabelIDs})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/gmail/v1/users/me/threads/"):
			id := strings.TrimPrefix(path, "/gmail/v1/users/me/threads/")
			want := q["metadataHeaders"]
			var msgs []gmailMessage
			f.mu.Lock()
			for _, m := range slices.Backward(f.mail) {
				if m.ThreadID != id {
					continue
				}
				var hs []gmailHeader
				for _, h := range m.Payload.Headers {
					if slices.ContainsFunc(want, func(n string) bool { return strings.EqualFold(n, h.Name) }) {
						hs = append(hs, h)
					}
				}
				m.Payload = gmailPart{MimeType: m.Payload.MimeType, Headers: hs}
				msgs = append(msgs, m)
			}
			f.mu.Unlock()
			if len(msgs) == 0 {
				notFound()
				return
			}
			writeJSON(rw, map[string]any{"id": id, "messages": msgs})
		default:
			http.Error(rw, `{"error":{"code":404,"message":"no `+r.Method+` `+path+` on gmail"}}`, http.StatusNotFound)
		}
	})
}

// createDraft is drafts.create: the message's raw must decode and read
// as a message, as Gmail checks it; the draft joins the thread asked
// for, or a thread of its own.
func (w *fakeGmailWrites) createDraft(rw http.ResponseWriter, body []byte) {
	var in struct {
		Message struct {
			Raw      string `json:"raw"`
			ThreadID string `json:"threadId"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(rw, `{"error":{"code":400,"message":"Invalid JSON payload"}}`, http.StatusBadRequest)
		return
	}
	raw, err := base64.URLEncoding.DecodeString(in.Message.Raw)
	if err != nil {
		http.Error(rw, `{"error":{"code":400,"message":"Invalid value for ByteString"}}`, http.StatusBadRequest)
		return
	}
	if _, err := mail.ReadMessage(bytes.NewReader(raw)); err != nil {
		http.Error(rw, `{"error":{"code":400,"message":"Invalid raw message"}}`, http.StatusBadRequest)
		return
	}
	w.mu.Lock()
	w.made++
	d := fakeDraft{id: fmt.Sprintf("r-%d", w.made), messageID: fmt.Sprintf("dm%d", w.made), threadID: in.Message.ThreadID, askedThread: in.Message.ThreadID, raw: raw}
	if d.threadID == "" {
		d.threadID = fmt.Sprintf("dt%d", w.made)
	}
	w.drafts = append(w.drafts, d)
	w.mu.Unlock()
	writeJSON(rw, map[string]any{"id": d.id, "message": map[string]any{"id": d.messageID, "threadId": d.threadID, "labelIds": []string{"DRAFT"}}})
}

// fakeSystemLabels are Gmail's own labels a message may be given that
// fakeLabels does not list, as Gmail takes them.
var fakeSystemLabels = []string{"STARRED", "IMPORTANT", "SPAM", "UNREAD", "TRASH", "INBOX"}

// modify is messages.modify: every label named must be the account's,
// as Gmail checks; the message's labels are changed and answered.
func (w *fakeGmailWrites) modify(rw http.ResponseWriter, id string, body []byte) {
	var in struct {
		Add    []string `json:"addLabelIds"`
		Remove []string `json:"removeLabelIds"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		http.Error(rw, `{"error":{"code":400,"message":"Invalid JSON payload"}}`, http.StatusBadRequest)
		return
	}
	for _, l := range slices.Concat(in.Add, in.Remove) {
		if !strings.Contains(fakeLabels, `"id":"`+l+`"`) && !slices.Contains(fakeSystemLabels, l) {
			http.Error(rw, `{"error":{"code":400,"message":"Invalid label: `+l+`"}}`, http.StatusBadRequest)
			return
		}
	}
	f := w.f
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.IndexFunc(f.mail, func(m gmailMessage) bool { return m.ID == id })
	if i < 0 {
		http.Error(rw, `{"error":{"code":404,"message":"Requested entity was not found."}}`, http.StatusNotFound)
		return
	}
	labels := slices.DeleteFunc(slices.Clone(f.mail[i].LabelIDs), func(l string) bool { return slices.Contains(in.Remove, l) })
	for _, l := range in.Add {
		if !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	f.mail[i].LabelIDs = labels
	writeJSON(rw, map[string]any{"id": id, "threadId": f.mail[i].ThreadID, "labelIds": labels})
}
