package googleworkspace

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Docs' writing: creating a document, adding to one and replacing text
// in one. A write changes only a document the signed-in account owns or
// Kivali made (writableDocument), within the folders the access
// includes, and everything is checked before anything is changed.

// maxWriteChars bounds the text one write adds.
const maxWriteChars = 100000

// markdownWords say what Markdown create_document and
// append_to_document take.
const markdownWords = "The text takes a little Markdown: a line starting with # to ###### is a heading of that level, a line starting with - or * is a list item, and **text** is bold; every other line is a paragraph as written."

// writableDocument is the Drive file of the document id, checked for a
// write: readable (readableDocument), and owned by the signed-in
// account or made by Kivali.
func (c *call) writableDocument(ctx context.Context, id string) (driveFile, error) {
	f, err := c.readableDocument(ctx, id)
	if err != nil {
		return driveFile{}, err
	}
	me, err := c.account(ctx)
	if err != nil {
		return driveFile{}, err
	}
	if !f.ownedBy(me.address) && !f.madeByKivali() {
		return driveFile{}, refuse("Document "+id+" is not the owner's and Kivali did not make it: you may change only documents the owner owns or Kivali made.",
			"document "+id+" is neither the owner's nor made by Kivali")
	}
	return f, nil
}

// docsRequest sends a request that changes something to path on a,
// body as its JSON, and decodes the JSON answer into out (nil to read
// none). It is get's counterpart: the same pinned origins, client,
// bounds and error words.
func (g *GoogleWorkspace) docsRequest(ctx context.Context, token, method string, a api, path string, body, out any) error {
	base, err := g.base(a)
	if err != nil {
		return err
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, base.String()+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Kivali")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.httpClient().Do(req)
	if err != nil {
		return transportError(ctx, err, token)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes+1))
	switch {
	case err != nil:
		return transportError(ctx, err, token)
	case len(got) > maxAnswerBytes:
		return &apiError{Message: "its answer is larger than Kivali reads"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := readError(got, resp.StatusCode)
		e.Message = scrub(e.Message, token)
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(got, out); err != nil {
		return &apiError{Message: "its answer is not JSON Kivali reads"}
	}
	return nil
}

// unanswered is a write Google did not answer, which may or may not
// have been made, in words saying so.
func unanswered(err error) error {
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 0 {
		return failure("Google did not answer (" + sentence(ae.Message) + "), so Kivali cannot say whether the document changed: read it before trying again.")
	}
	return err
}

// batchUpdate applies requests to the document id, in order, and reads
// Google's replies into out (nil for none).
func (c *call) batchUpdate(ctx context.Context, id string, requests []any, out any) error {
	return c.g.docsRequest(ctx, c.token, http.MethodPost, docsAPI, "/v1/documents/"+url.PathEscape(id)+":batchUpdate",
		map[string]any{"requests": requests}, out)
}

// mdParagraph is one line of the Markdown a write takes: its text
// without the marks, its heading level (0 for none), whether it is a
// list item, and its bold spans, in UTF-16 code units from its start,
// which is how the Docs API counts.
type mdParagraph struct {
	text    string
	heading int
	bullet  bool
	bold    [][2]int
}

var (
	mdHeading = regexp.MustCompile(`^(#{1,6})[ \t]+(.*)$`)
	mdBullet  = regexp.MustCompile(`^[ \t]*[-*+][ \t]+(.*)$`)
)

// parseMarkdown reads s, a line a paragraph, in the Markdown
// markdownWords describes. Blank lines at its end are dropped; other
// Markdown is written as it is.
func parseMarkdown(s string) []mdParagraph {
	s, _ = cleanText(s)
	s = strings.TrimRight(s, " \t\n")
	if s == "" {
		return nil
	}
	var out []mdParagraph
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, " \t")
		var p mdParagraph
		if m := mdHeading.FindStringSubmatch(line); m != nil {
			p.heading, line = len(m[1]), m[2]
		} else if m := mdBullet.FindStringSubmatch(line); m != nil {
			p.bullet, line = true, m[1]
		}
		p.text, p.bold = boldSpans(line)
		out = append(out, p)
	}
	return out
}

// boldSpans is line with its ** marks taken out, and the spans they
// made bold. A last ** with no partner is text.
func boldSpans(line string) (string, [][2]int) {
	parts := strings.Split(line, "**")
	if len(parts)%2 == 0 {
		parts[len(parts)-2] += "**" + parts[len(parts)-1]
		parts = parts[:len(parts)-1]
	}
	var b strings.Builder
	var spans [][2]int
	pos := 0
	for i, part := range parts {
		n := utf16Len(part)
		if i%2 == 1 && n > 0 {
			spans = append(spans, [2]int{pos, pos + n})
		}
		b.WriteString(part)
		pos += n
	}
	return b.String(), spans
}

// utf16Len is s's length in UTF-16 code units, as the Docs API counts
// indexes.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// insertRequests are the Docs API requests that write paras at index
// at, after prefix ("\n" to start a paragraph of their own after one
// that holds text). Text inserted takes the style of the paragraph it
// lands in, so the paragraphs written are first set to normal text, no
// list and no text style of their own, then given their headings, list
// items and bold.
func insertRequests(at int, prefix string, paras []mdParagraph) []any {
	texts := make([]string, len(paras))
	for i, p := range paras {
		texts[i] = p.text
	}
	full := prefix + strings.Join(texts, "\n")
	start, end := at+utf16Len(prefix), at+utf16Len(full)
	span := func(s, e int) map[string]any { return map[string]any{"startIndex": s, "endIndex": e} }
	reqs := []any{
		map[string]any{"insertText": map[string]any{"location": map[string]any{"index": at}, "text": full}},
		map[string]any{"updateParagraphStyle": map[string]any{"range": span(start, end), "paragraphStyle": map[string]any{"namedStyleType": "NORMAL_TEXT"}, "fields": "namedStyleType"}},
		map[string]any{"deleteParagraphBullets": map[string]any{"range": span(start, end)}},
		map[string]any{"updateTextStyle": map[string]any{"range": span(start, end), "textStyle": map[string]any{}, "fields": "*"}},
	}
	var bold []any
	listFrom, listTo := -1, -1
	endList := func() {
		if listFrom >= 0 {
			reqs = append(reqs, map[string]any{"createParagraphBullets": map[string]any{"range": span(listFrom, listTo), "bulletPreset": "BULLET_DISC_CIRCLE_SQUARE"}})
		}
		listFrom = -1
	}
	pos := start
	for _, p := range paras {
		n := utf16Len(p.text)
		if p.heading > 0 && n > 0 {
			reqs = append(reqs, map[string]any{"updateParagraphStyle": map[string]any{"range": span(pos, pos+n),
				"paragraphStyle": map[string]any{"namedStyleType": fmt.Sprintf("HEADING_%d", p.heading)}, "fields": "namedStyleType"}})
		}
		if p.bullet && n > 0 {
			if listFrom < 0 {
				listFrom = pos
			}
			listTo = pos + n
		} else {
			endList()
		}
		for _, s := range p.bold {
			bold = append(bold, map[string]any{"updateTextStyle": map[string]any{"range": span(pos+s[0], pos+s[1]),
				"textStyle": map[string]any{"bold": true}, "fields": "bold"}})
		}
		pos += n + 1
	}
	endList()
	return append(reqs, bold...)
}

// bodyEnd is where text added at the end of body goes: before the last
// paragraph's own line end. empty reports whether that paragraph holds
// no text, so what is added can be its text.
func bodyEnd(body *docsBody) (at int, empty bool) {
	if body == nil || len(body.Content) == 0 {
		return 1, true
	}
	last := body.Content[len(body.Content)-1]
	empty = true
	if last.Paragraph != nil {
		for _, el := range last.Paragraph.Elements {
			if el.TextRun == nil || strings.TrimRight(el.TextRun.Content, "\n") != "" {
				empty = false
			}
		}
	}
	return max(last.EndIndex-1, 1), empty
}

// writeText is the text argument key of a write as the agent gave it,
// its paragraphs read: required, at most maxWriteChars characters.
func writeText(a args, key string, required bool) ([]mdParagraph, int, error) {
	s, err := a.text(key, required, maxWriteChars)
	if err != nil {
		return nil, 0, err
	}
	paras := parseMarkdown(s)
	n := 0
	for i, p := range paras {
		n += utf8.RuneCountInString(p.text)
		if i > 0 {
			n++
		}
	}
	return paras, n, nil
}

// createDocument makes a document titled title (documents.create),
// writes body into it, then marks it as Kivali's and moves it into its
// folder (files.update). The folder is checked first: folder_id, which
// must be one the access includes or inside one, else the first folder
// the access names, else the Drive's own top. Once the document is
// made, what fails after is said in the answer, with the document's
// id.
func createDocument(ctx context.Context, c *call) (any, error) {
	title, err := c.args.text("title", true, nameClip)
	if err != nil {
		return nil, err
	}
	title = strings.Join(strings.Fields(title), " ")
	paras, chars, err := writeText(c.args, "body", false)
	if err != nil {
		return nil, err
	}
	folder, err := folderArg(c.args)
	if err != nil {
		return nil, err
	}
	if folder != "" {
		f, err := c.file(ctx, folder, "id,mimeType")
		if err != nil {
			return nil, err
		}
		if f.MimeType != folderType {
			return nil, badArgument("folder_id is not a folder's id; search_documents lists the folder each document is in.")
		}
		in, err := c.inFolders(ctx, []string{folder})
		if err != nil {
			return nil, err
		}
		if !in {
			return nil, folderNotIncluded(folder)
		}
	} else if len(c.access.docsFolders) > 0 {
		folder = c.access.docsFolders[0]
	}

	var made docsDocument
	if err := c.g.docsRequest(ctx, c.token, http.MethodPost, docsAPI, "/v1/documents", map[string]any{"title": title}, &made); err != nil {
		return nil, unanswered(err)
	}
	id := made.DocumentID
	if !driveIDPattern.MatchString(id) {
		return nil, failure("Google made the document and answered no id Kivali reads; tell the owner.")
	}
	// undone is what was not done once the document was made, why the
	// first failure's words.
	var undone []string
	why := ""
	failed := func(what string, err error) {
		undone = append(undone, what)
		if why == "" {
			why = writeWords(err)
		}
	}
	if len(paras) > 0 {
		at, empty := bodyEnd(made.Body)
		prefix := "\n"
		if empty {
			prefix = ""
		}
		if err := c.batchUpdate(ctx, id, insertRequests(at, prefix, paras), nil); err != nil {
			failed("writing its text", err)
			chars = 0
		}
	}
	q := url.Values{"supportsAllDrives": {"true"}, "fields": {"id,parents"}}
	what := "marking it as made by Kivali"
	if folder != "" {
		f, err := c.file(ctx, id, "parents")
		if err != nil {
			failed("moving it into folder "+folder, err)
			folder = ""
		} else {
			q.Set("addParents", folder)
			if len(f.Parents) > 0 {
				q.Set("removeParents", strings.Join(f.Parents, ","))
			}
			what += " and moving it into folder " + folder
		}
	}
	mark := map[string]any{"appProperties": map[string]string{kivaliMark: cmp.Or(c.agent, "kivali")}}
	if err := c.g.docsRequest(ctx, c.token, http.MethodPatch, googleAPI, filePath(id)+"?"+q.Encode(), mark, nil); err != nil {
		failed(what, err)
		folder = ""
	}

	where := ""
	if folder != "" {
		where = " in folder " + folder
	}
	c.audit = fmt.Sprintf("created document %s%s with %d characters of text", id, where, chars)
	if len(undone) > 0 {
		left := strings.Join(undone, ", then ")
		c.audit += "; could not finish " + left
		return fmt.Sprintf("Created document %s%s, but Kivali could not finish %s: %s", id, where, left, why), nil
	}
	return fmt.Sprintf("Created document %s%s with %d characters of text, marked as made by Kivali.", id, where, chars), nil
}

// writeWords is a write's failure in the agent's words, a sentence.
func writeWords(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		if ae.Status == 0 {
			return "Google did not answer, so Kivali cannot say whether it was done; read the document before trying again."
		}
		return agentWords(ae)
	}
	return sentence(err.Error()) + "."
}

// appendToDocument adds text at the end of a document's body (its
// first tab's), as its own paragraphs.
func appendToDocument(ctx context.Context, c *call) (any, error) {
	id, err := c.args.id("document_id", driveIDPattern)
	if err != nil {
		return nil, err
	}
	paras, chars, err := writeText(c.args, "text", true)
	if err != nil {
		return nil, err
	}
	if len(paras) == 0 {
		return nil, badArgument("text is required.")
	}
	if _, err := c.writableDocument(ctx, id); err != nil {
		return nil, err
	}
	d, err := c.document(ctx, id, false)
	if err != nil {
		return nil, err
	}
	at, empty := bodyEnd(d.Body)
	prefix := "\n"
	if empty {
		prefix = ""
	}
	if err := c.batchUpdate(ctx, id, insertRequests(at, prefix, paras), nil); err != nil {
		return nil, unanswered(err)
	}
	c.audit = fmt.Sprintf("added %d characters to the end of document %s", chars, id)
	return fmt.Sprintf("Added %d characters at the end of document %s.", chars, id), nil
}

// replaceText replaces every occurrence of find in a document
// (replaceAllText, every tab), and answers how many Google replaced.
func replaceText(ctx context.Context, c *call) (any, error) {
	id, err := c.args.id("document_id", driveIDPattern)
	if err != nil {
		return nil, err
	}
	find, err := untrimmed(c.args, "find", true, 1000)
	if err != nil {
		return nil, err
	}
	with, err := untrimmed(c.args, "replace", false, maxWriteChars)
	if err != nil {
		return nil, err
	}
	matchCase, err := docsFlag(c.args, "match_case")
	if err != nil {
		return nil, err
	}
	if _, err := c.writableDocument(ctx, id); err != nil {
		return nil, err
	}
	var answer struct {
		Replies []struct {
			ReplaceAllText *struct {
				OccurrencesChanged int `json:"occurrencesChanged"`
			} `json:"replaceAllText"`
		} `json:"replies"`
	}
	req := map[string]any{"replaceAllText": map[string]any{
		"containsText": map[string]any{"text": find, "matchCase": matchCase},
		"replaceText":  with,
	}}
	if err := c.batchUpdate(ctx, id, []any{req}, &answer); err != nil {
		return nil, unanswered(err)
	}
	n := 0
	if len(answer.Replies) > 0 && answer.Replies[0].ReplaceAllText != nil {
		n = answer.Replies[0].ReplaceAllText.OccurrencesChanged
	}
	c.audit = fmt.Sprintf("replaced %d occurrences in document %s", n, id)
	if n == 0 {
		return "Document " + id + " holds no occurrence of the text; nothing was changed.", nil
	}
	if n == 1 {
		return "Replaced 1 occurrence in document " + id + ".", nil
	}
	return fmt.Sprintf("Replaced %d occurrences in document %s.", n, id), nil
}

// untrimmed is the string argument key as the agent gave it, its
// spaces kept: "" when it is missing or null, and a refusal when it is
// another type, holds more than most characters or, for required, holds
// nothing but spaces.
func untrimmed(a args, key string, required bool, most int) (string, error) {
	v, ok := a[key]
	if !ok || v == nil {
		v = ""
	}
	s, isText := v.(string)
	switch {
	case !isText:
		return "", badArgument(key + " is text.")
	case required && strings.TrimSpace(s) == "":
		return "", badArgument(key + " is required.")
	case utf8.RuneCountInString(s) > most:
		return "", badArgument(fmt.Sprintf("%s is at most %d characters.", key, most))
	}
	return s, nil
}
