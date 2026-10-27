package googleworkspace

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

// fakeDocs is Drive's files and comments and the Docs API, run
// in-process in front of fakeGoogle: its servers answer what Docs'
// actions ask, and hand every other request to fakeGoogle's handler for
// the same origin, so Gmail's profile and the folder options answer as
// they do there. Every request must carry testToken; each is recorded
// in fakeGoogle's requests, and each change also in writes, with its
// body.
type fakeDocs struct {
	f *fakeGoogle

	mu     sync.Mutex
	files  map[string]*fakeFile
	order  []string
	writes []fakeWrite
	made   int
}

// fakeFile is one Drive file: a folder, or a document, whose
// documents.get answer is raw when set, else built from paras (the
// writes' model of a body). text is what fullText finds.
type fakeFile struct {
	file     driveFile
	trashed  bool
	text     string
	raw      string
	paras    []fakePara
	comments []map[string]any
	// refuseChild, on a folder, is Drive's answer to making a file in
	// it.
	refuseChild *fakeError
}

// fakePara is a paragraph of a modelled body: its text, its named
// style ("" for normal text) and whether it is a list item.
type fakePara struct {
	text   string
	style  string
	bullet bool
}

// fakeWrite is one change the fake was sent.
type fakeWrite struct {
	method, path string
	body         map[string]any
}

// newFakeDocs starts the fake, and g reaching it.
func newFakeDocs(t *testing.T) (*fakeDocs, *fakeGoogle, *GoogleWorkspace) {
	t.Helper()
	f, g := newFakeGoogle(t)
	d := &fakeDocs{f: f, files: map[string]*fakeFile{}}
	www := httptest.NewServer(d.handler("www", f.handler("www")))
	docs := httptest.NewServer(d.handler("docs", f.handler("docs")))
	t.Cleanup(func() { www.Close(); docs.Close() })
	g.APIBase, g.DocsBase = www.URL, docs.URL
	return d, f, g
}

var fakeModified = time.Date(2026, 10, 20, 14, 3, 0, 0, time.UTC)

// addFolder adds a folder inside parents.
func (d *fakeDocs) addFolder(id, name string, parents ...string) {
	d.add(&fakeFile{file: driveFile{ID: id, Name: name, MimeType: folderType, Parents: parents, Owners: []driveUser{{"dana@example.com"}}}})
}

// addDoc adds a document owned by owner ("" for a shared drive's),
// inside parents, its body raw (documents.get's answer) when it is
// set, else one empty paragraph. Each document added is a minute older
// than the one before.
func (d *fakeDocs) addDoc(id, name, owner, raw string, parents ...string) *fakeFile {
	ff := &fakeFile{file: driveFile{ID: id, Name: name, MimeType: documentType, Parents: parents,
		WebViewLink: "https://docs.google.com/document/d/" + id + "/edit"}, raw: raw, paras: []fakePara{{}}}
	if owner != "" {
		ff.file.Owners = []driveUser{{owner}}
	}
	d.mu.Lock()
	ff.file.ModifiedTime = fakeModified.Add(-time.Duration(len(d.order)) * time.Minute).Format(time.RFC3339)
	d.mu.Unlock()
	d.add(ff)
	return ff
}

func (d *fakeDocs) add(ff *fakeFile) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.files[ff.file.ID] = ff
	d.order = append(d.order, ff.file.ID)
}

func (d *fakeDocs) get(id string) *fakeFile {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.files[id]
}

func (d *fakeDocs) changes() []fakeWrite {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.writes)
}

func fakeFail(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": status, "message": message}})
	_, _ = w.Write(b)
}

func (d *fakeDocs) handler(origin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		ours := origin == "docs" || (origin == "www" && (strings.HasPrefix(p, "/drive/v3/files/") ||
			(p == "/drive/v3/files" && r.URL.Query().Get("q") != folderQuery)))
		if !ours {
			next.ServeHTTP(w, r)
			return
		}
		d.f.mu.Lock()
		d.f.seen = append(d.f.seen, origin+" "+p+"?"+r.URL.RawQuery)
		fail, failing := d.f.fail[p]
		d.f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			fakeFail(w, http.StatusUnauthorized, "Request had invalid authentication credentials.")
			return
		}
		if failing {
			w.WriteHeader(fail.status)
			_, _ = w.Write([]byte(fail.body))
			return
		}
		var body map[string]any
		if r.Method != http.MethodGet {
			b, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(b, &body); err != nil {
				fakeFail(w, http.StatusBadRequest, "the body is not JSON")
				return
			}
			d.mu.Lock()
			d.writes = append(d.writes, fakeWrite{r.Method, p + "?" + r.URL.RawQuery, body})
			d.mu.Unlock()
		}
		switch {
		case origin == "www" && p == "/drive/v3/files" && r.Method == http.MethodGet:
			d.list(w, r)
		case origin == "www" && strings.HasSuffix(p, "/comments") && r.Method == http.MethodGet:
			d.listComments(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/drive/v3/files/"), "/comments"))
		case origin == "www" && r.Method == http.MethodGet:
			ff := d.get(strings.TrimPrefix(p, "/drive/v3/files/"))
			if ff == nil {
				fakeFail(w, http.StatusNotFound, "File not found: "+strings.TrimPrefix(p, "/drive/v3/files/")+".")
				return
			}
			d.mu.Lock()
			writeJSON(w, ff.file)
			d.mu.Unlock()
		case origin == "www" && p == "/drive/v3/files" && r.Method == http.MethodPost:
			d.create(w, body)
		case origin == "docs" && strings.HasSuffix(p, ":batchUpdate") && r.Method == http.MethodPost:
			d.batch(w, strings.TrimSuffix(strings.TrimPrefix(p, "/v1/documents/"), ":batchUpdate"), body)
		case origin == "docs" && strings.HasPrefix(p, "/v1/documents/") && r.Method == http.MethodGet:
			ff := d.get(strings.TrimPrefix(p, "/v1/documents/"))
			if ff == nil || ff.file.MimeType != documentType {
				fakeFail(w, http.StatusNotFound, "Requested entity was not found.")
				return
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if ff.raw != "" {
				_, _ = w.Write([]byte(ff.raw))
				return
			}
			writeJSON(w, ff.modelled(r.URL.Query().Get("includeTabsContent") == "true"))
		default:
			fakeFail(w, http.StatusNotFound, "no "+r.Method+" "+p+" on "+origin)
		}
	})
}

var (
	queryParents = regexp.MustCompile(`'((?:[^'\\]|\\.)*)' in parents`)
	queryWords   = regexp.MustCompile(`name contains '((?:[^'\\]|\\.)*)' or fullText contains '((?:[^'\\]|\\.)*)'`)
	unquote      = strings.NewReplacer(`\'`, `'`, `\\`, `\`)
)

// list is files.list for the two questions Docs' actions ask: the
// folders in any of some folders, and the documents (in any of some
// folders, whose name or text holds some words), each not in the
// trash. Drive refuses an order for a search of text, as Google does.
// A page is pageSize files, its token the offset it starts at;
// documents are listed newest changed first unless a search of text
// asks.
func (d *fakeDocs) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	var parents []string
	for _, m := range queryParents.FindAllStringSubmatch(q, -1) {
		parents = append(parents, unquote.Replace(m[1]))
	}
	var want, words string
	switch {
	case strings.HasPrefix(q, folderQuery+" and ("):
		want = folderType
	case strings.HasPrefix(q, "mimeType='"+documentType+"' and trashed=false"):
		want = documentType
		if m := queryWords.FindStringSubmatch(q); m != nil {
			words = unquote.Replace(m[1])
			if m[2] != m[1] {
				fakeFail(w, http.StatusBadRequest, "the name and the text are searched for different words")
				return
			}
			if r.URL.Query().Get("orderBy") != "" {
				fakeFail(w, http.StatusBadRequest, "Sorting is not supported for queries with fullText terms. Results are always in descending relevance order.")
				return
			}
		}
	default:
		fakeFail(w, http.StatusBadRequest, "the fake does not answer q "+q)
		return
	}
	d.mu.Lock()
	var hits []driveFile
	for _, id := range d.order {
		ff := d.files[id]
		if ff.file.MimeType != want || ff.trashed {
			continue
		}
		if len(parents) > 0 && !slices.ContainsFunc(ff.file.Parents, func(p string) bool { return slices.Contains(parents, p) }) {
			continue
		}
		if words != "" && !strings.Contains(strings.ToLower(ff.file.Name+" "+ff.text), strings.ToLower(words)) {
			continue
		}
		hits = append(hits, ff.file)
	}
	d.mu.Unlock()
	if r.URL.Query().Get("orderBy") == "modifiedTime desc" {
		slices.SortStableFunc(hits, func(a, b driveFile) int { return strings.Compare(b.ModifiedTime, a.ModifiedTime) })
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if size <= 0 {
		size = 100
	}
	from, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
	out := map[string]any{"files": hits[min(from, len(hits)):min(from+size, len(hits))]}
	if from+size < len(hits) {
		out["nextPageToken"] = strconv.Itoa(from + size)
	}
	writeJSON(w, out)
}

// listComments is comments.list, which Drive answers only for the
// fields named, a page pageSize comments, its token the offset.
func (d *fakeDocs) listComments(w http.ResponseWriter, r *http.Request, id string) {
	if r.URL.Query().Get("fields") == "" {
		fakeFail(w, http.StatusBadRequest, "The 'fields' parameter is required for this operation.")
		return
	}
	ff := d.get(id)
	if ff == nil {
		fakeFail(w, http.StatusNotFound, "File not found: "+id+".")
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	from, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
	d.mu.Lock()
	all := ff.comments
	d.mu.Unlock()
	out := map[string]any{"comments": all[min(from, len(all)):min(from+size, len(all))]}
	if from+size < len(all) {
		out["nextPageToken"] = strconv.Itoa(from + size)
	}
	writeJSON(w, out)
}

// create is files.create for a Google Doc: one empty paragraph, named,
// owned by the signed-in account, with the app properties given, in
// the parents given or at the top of My Drive. A parent the fake does
// not hold, or that is not a folder, is refused 404 as Drive refuses
// one; a folder whose refuseChild is set answers that instead, and
// with status 0 closes the connection unanswered.
func (d *fakeDocs) create(w http.ResponseWriter, body map[string]any) {
	if body["mimeType"] != documentType {
		fakeFail(w, http.StatusBadRequest, "the fake makes only Google Docs")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	parents := []string{"rootFolder"}
	if ps, ok := body["parents"].([]any); ok {
		parents = nil
		for _, raw := range ps {
			id, _ := raw.(string)
			folder := d.files[id]
			switch {
			case folder == nil || folder.file.MimeType != folderType:
				fakeFail(w, http.StatusNotFound, "File not found: "+id+".")
				return
			case folder.refuseChild != nil && folder.refuseChild.status == 0:
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			case folder.refuseChild != nil:
				w.WriteHeader(folder.refuseChild.status)
				_, _ = w.Write([]byte(folder.refuseChild.body))
				return
			}
			parents = append(parents, id)
		}
	}
	props := map[string]string{}
	if ps, ok := body["appProperties"].(map[string]any); ok {
		for k, v := range ps {
			props[k], _ = v.(string)
		}
	}
	name, _ := body["name"].(string)
	d.made++
	id := "madeDoc" + strconv.Itoa(d.made)
	d.files[id] = &fakeFile{file: driveFile{ID: id, Name: name, MimeType: documentType, Parents: parents, AppProperties: props,
		Owners: []driveUser{{"dana@example.com"}}, ModifiedTime: fakeModified.Format(time.RFC3339)}, paras: []fakePara{{}}}
	d.order = append(d.order, id)
	writeJSON(w, map[string]any{"id": id})
}

// starts is where each paragraph of a modelled body starts, in UTF-16
// code units from index 1, and the body's end index.
func (ff *fakeFile) starts() ([]int, int) {
	at := 1
	out := make([]int, len(ff.paras))
	for i, p := range ff.paras {
		out[i] = at
		at += len(utf16.Encode([]rune(p.text))) + 1
	}
	return out, at
}

// modelled is the modelled body as documents.get answers it, in tabs or
// as the first tab's body.
func (ff *fakeFile) modelled(tabs bool) map[string]any {
	starts, _ := ff.starts()
	content := []any{map[string]any{"startIndex": 0, "endIndex": 1, "sectionBreak": map[string]any{}}}
	for i, p := range ff.paras {
		end := starts[i] + len(utf16.Encode([]rune(p.text))) + 1
		para := map[string]any{
			"elements":       []any{map[string]any{"startIndex": starts[i], "endIndex": end, "textRun": map[string]any{"content": p.text + "\n", "textStyle": map[string]any{}}}},
			"paragraphStyle": map[string]any{"namedStyleType": cmpOr(p.style, "NORMAL_TEXT")},
		}
		if p.bullet {
			para["bullet"] = map[string]any{"listId": "l1"}
		}
		content = append(content, map[string]any{"startIndex": starts[i], "endIndex": end, "paragraph": para})
	}
	body := map[string]any{"content": content}
	doc := map[string]any{"documentId": ff.file.ID, "title": ff.file.Name}
	if tabs {
		doc["tabs"] = []any{map[string]any{"tabProperties": map[string]any{"tabId": "t.0", "title": "Tab 1"}, "documentTab": map[string]any{"body": body}}}
	} else {
		doc["body"] = body
	}
	return doc
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// batch is documents.batchUpdate on a modelled body, each request in
// order: insertText (new paragraphs take the style of the one the text
// lands in), updateParagraphStyle, createParagraphBullets and
// deleteParagraphBullets on the paragraphs a range touches,
// updateTextStyle checked and otherwise kept only in writes, and
// replaceAllText within paragraphs. An index or a range outside the
// body, or an empty range, is refused as Google refuses one.
func (d *fakeDocs) batch(w http.ResponseWriter, id string, body map[string]any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ff := d.files[id]
	if ff == nil || ff.raw != "" {
		fakeFail(w, http.StatusNotFound, "Requested entity was not found.")
		return
	}
	reqs, _ := body["requests"].([]any)
	var replies []any
	for i, raw := range reqs {
		req, _ := raw.(map[string]any)
		starts, end := ff.starts()
		bad := func(words string) {
			fakeFail(w, http.StatusBadRequest, "Invalid requests["+strconv.Itoa(i)+"]: "+words)
		}
		// touched is the paragraphs a request's range touches, or nil
		// for a range Google refuses.
		touched := func(r map[string]any) []int {
			span, _ := r["range"].(map[string]any)
			s, _ := span["startIndex"].(float64)
			e, _ := span["endIndex"].(float64)
			if s >= e || s < 1 || int(e) > end {
				return nil
			}
			var out []int
			for j, st := range starts {
				pe := st + len(utf16.Encode([]rune(ff.paras[j].text))) + 1
				if st < int(e) && pe > int(s) {
					out = append(out, j)
				}
			}
			return out
		}
		reply := map[string]any{}
		switch {
		case req["insertText"] != nil:
			x := req["insertText"].(map[string]any)
			loc, _ := x["location"].(map[string]any)
			at, _ := loc["index"].(float64)
			text, _ := x["text"].(string)
			if int(at) < 1 || int(at) >= end {
				bad("Index " + strconv.Itoa(int(at)) + " must be less than the end index of the referenced segment, " + strconv.Itoa(end) + ".")
				return
			}
			var flat []uint16
			for _, p := range ff.paras {
				flat = append(flat, utf16.Encode([]rune(p.text+"\n"))...)
			}
			pos := int(at) - 1
			j := strings.Count(string(utf16.Decode(flat[:pos])), "\n")
			flat = slices.Insert(flat, pos, utf16.Encode([]rune(text))...)
			lines := strings.Split(strings.TrimSuffix(string(utf16.Decode(flat)), "\n"), "\n")
			added := strings.Count(text, "\n")
			var next []fakePara
			for k, line := range lines {
				src := k
				switch {
				case k >= j && k <= j+added:
					src = j
				case k > j+added:
					src = k - added
				}
				p := ff.paras[src]
				p.text = line
				next = append(next, p)
			}
			ff.paras = next
		case req["updateParagraphStyle"] != nil:
			x := req["updateParagraphStyle"].(map[string]any)
			ps := touched(x)
			if ps == nil {
				bad("The range should not be empty.")
				return
			}
			style, _ := x["paragraphStyle"].(map[string]any)["namedStyleType"].(string)
			for _, j := range ps {
				ff.paras[j].style = style
			}
		case req["createParagraphBullets"] != nil || req["deleteParagraphBullets"] != nil:
			x, on := req["createParagraphBullets"].(map[string]any)
			if !on {
				x = req["deleteParagraphBullets"].(map[string]any)
			}
			ps := touched(x)
			if ps == nil {
				bad("The range should not be empty.")
				return
			}
			for _, j := range ps {
				ff.paras[j].bullet = on
			}
		case req["updateTextStyle"] != nil:
			if touched(req["updateTextStyle"].(map[string]any)) == nil {
				bad("The range should not be empty.")
				return
			}
		case req["replaceAllText"] != nil:
			x := req["replaceAllText"].(map[string]any)
			ct, _ := x["containsText"].(map[string]any)
			find, _ := ct["text"].(string)
			matchCase, _ := ct["matchCase"].(bool)
			with, _ := x["replaceText"].(string)
			n := 0
			for j, p := range ff.paras {
				re := regexp.QuoteMeta(find)
				if !matchCase {
					re = "(?i)" + re
				}
				n += len(regexp.MustCompile(re).FindAllStringIndex(p.text, -1))
				ff.paras[j].text = regexp.MustCompile(re).ReplaceAllLiteralString(p.text, with)
			}
			reply["replaceAllText"] = map[string]any{"occurrencesChanged": n}
		default:
			bad("the fake does not answer this request")
			return
		}
		replies = append(replies, reply)
	}
	writeJSON(w, map[string]any{"documentId": id, "replies": replies})
}
