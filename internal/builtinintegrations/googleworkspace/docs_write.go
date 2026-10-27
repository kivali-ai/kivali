package googleworkspace

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// Docs' writing: creating a document, adding to one and replacing text
// in one. A write changes only a document the signed-in account owns or
// that counts as Kivali's (writableDocument), within the folders the
// access includes, and everything is checked before anything is
// changed. A write to a document, or into a folder, that anyone besides
// the owner can see (fileShare) is a send, since they see the change:
// it needs docs_send, everyone it is shared with in send_to
// (checkShared), and is held for the owner (held), its Run doing what
// the write would have. Each write's audit is set before its request,
// so a write Google does not answer still names what it changed.

// maxWriteChars bounds the text one write adds.
const maxWriteChars = 100000

// markdownWords say what Markdown create_document and
// append_to_document take.
const markdownWords = "The text takes a little Markdown: a line starting with # to ###### is a heading of that level, a line starting with - or * is a list item, and **text** is bold; every other line is a paragraph as written."

// writableDocument is the Drive file of the document id, checked for a
// write: readable (readableDocument), and owned by the signed-in
// account or Kivali's, marked as made by Kivali and owned by no one
// else (driveFile.writableBy).
func (c *call) writableDocument(ctx context.Context, id string) (driveFile, error) {
	f, err := c.readableDocument(ctx, id)
	if err != nil {
		return driveFile{}, err
	}
	me, err := c.account(ctx)
	if err != nil {
		return driveFile{}, err
	}
	if !f.writableBy(me.address) {
		return driveFile{}, refuse("Document "+id+" is not the owner's and Kivali did not make it: you may change only documents the owner owns or Kivali made.",
			"document "+id+" is neither the owner's nor made by Kivali")
	}
	return f, nil
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
// Google's replies into out (nil for none). revision, when it is not
// "", is the revision the requests' indexes were read from: Google
// applies them to that revision, moved past what others changed since.
func (c *call) batchUpdate(ctx context.Context, id, revision string, requests []any, out any) error {
	body := map[string]any{"requests": requests}
	if revision != "" {
		body["writeControl"] = map[string]any{"targetRevisionId": revision}
	}
	return c.g.change(ctx, c.token, docsAPI, http.MethodPost, "/v1/documents/"+url.PathEscape(id)+":batchUpdate", body, out)
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

// parseMarkdown reads s, a line a paragraph (markdownLines), in the
// Markdown markdownWords describes; other Markdown is written as it is.
func parseMarkdown(s string) []mdParagraph {
	var out []mdParagraph
	for _, line := range markdownLines(s) {
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

// markdownLines is s as parseMarkdown reads it, a line a paragraph,
// which a card shows as what is written: cleanText's, each line's
// spaces at its end and blank lines at the end dropped. The Docs API
// strips the Private Use Area (U+E000 to U+F8FF) from text it inserts,
// so it is taken out first, and the indexes counted are those of the
// text Google keeps.
func markdownLines(s string) []string {
	s, _ = cleanText(s)
	s = strings.Map(func(r rune) rune {
		if r >= 0xE000 && r <= 0xF8FF {
			return -1
		}
		return r
	}, s)
	s = strings.TrimRight(s, " \t\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return lines
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
// lands in, so the paragraphs written are first set to normal text with
// no paragraph style of their own (fields "*": no shading, indent or
// spacing kept), no list and no text style of their own, then given
// their headings, list items and bold.
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
		map[string]any{"updateParagraphStyle": map[string]any{"range": span(start, end), "paragraphStyle": map[string]any{"namedStyleType": "NORMAL_TEXT"}, "fields": "*"}},
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
// its paragraphs read: required, at most maxWriteChars characters, and,
// when given, holding text once its marks are taken out, so a write
// never makes a document or adds paragraphs with nothing in them.
func writeText(a args, key string, required bool) ([]mdParagraph, int, error) {
	s, err := a.text(key, required, maxWriteChars)
	if err != nil {
		return nil, 0, err
	}
	paras := parseMarkdown(s)
	if s != "" && !slices.ContainsFunc(paras, func(p mdParagraph) bool { return strings.TrimSpace(p.text) != "" }) {
		return nil, 0, badArgument(key + " holds no text once its Markdown marks are taken out.")
	}
	n := 0
	for i, p := range paras {
		n += utf8.RuneCountInString(p.text)
		if i > 0 {
			n++
		}
	}
	return paras, n, nil
}

// createDocument makes a Google Doc titled title through Drive
// (newDocument), marked as Kivali's and in its folder from the start,
// then writes body into it (batchUpdate). The folder is checked first:
// folder_id, which must be one the access includes or inside one, else
// the first folder the access names, else none, the top of the owner's
// My Drive. A folder anyone besides the owner can see (fileShare) shows
// them the document, so making one there is held for the owner, with
// its title, its text and who can see the folder. Once the document is
// made, a failure to write its text is said in the answer, with the
// document's id.
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
	named := folder != ""
	if !named && len(c.access.docsFolders) > 0 {
		folder = c.access.docsFolders[0]
	}
	var dir driveFile
	var sh share
	if folder != "" {
		if dir, err = c.file(ctx, folder, "id,name,mimeType"); err != nil {
			return nil, err
		}
		if named {
			if dir.MimeType != folderType {
				return nil, badArgument("folder_id is not a folder's id; search_documents lists the folder each document is in.")
			}
			in, err := c.inFolders(ctx, []string{folder})
			if err != nil {
				return nil, err
			}
			if !in {
				return nil, folderNotIncluded(folder)
			}
		}
		if sh, err = c.fileShare(ctx, folder); err != nil {
			return nil, err
		}
	}
	// limited is whether the access names folders, read now, since a
	// held call's Run has no access of its own.
	limited := len(c.access.docsFolders) > 0
	create := func(ctx context.Context, c *call) (any, error) {
		folder, note := folder, sh.auditNote()
		asked := folder
		if folder != "" {
			c.audit = "creating a document in folder " + folder
		} else {
			c.audit = "creating a document"
		}
		id, refused, err := c.newDocument(ctx, title, folder)
		if err != nil {
			return nil, unanswered(err)
		}
		if refused != "" {
			folder, note = "", ""
		}
		where := ""
		if folder != "" {
			where = " in folder " + folder
		}
		// A Google Doc Drive makes is one empty paragraph, so the body is
		// its text from index 1.
		why := ""
		if len(paras) > 0 {
			if err := c.batchUpdate(ctx, id, "", insertRequests(1, "", paras), nil); err != nil {
				why = writeWords(err)
			}
		}
		answer := fmt.Sprintf("Created document %s%s with %d characters of text, marked as made by Kivali.", id, where, chars)
		c.audit = fmt.Sprintf("created document %s%s with %d characters of text%s", id, where, chars, note)
		if why != "" {
			answer = fmt.Sprintf("Created document %s%s, marked as made by Kivali, but Kivali could not write its text: %s", id, where, why)
			c.audit = fmt.Sprintf("created document %s%s; could not write its text%s", id, where, note)
		}
		if refused != "" {
			answer += " Google would not put it in folder " + asked + " (" + refused + "), so Kivali made it at the top of the owner's My Drive."
			if limited {
				answer += " It is outside the folders your access includes, so you cannot read or change it; tell the owner."
			}
			c.audit += "; Google refused folder " + asked + ", so it is at the top of My Drive"
		}
		return answer, nil
	}
	if !sh.others() {
		return create(ctx, c)
	}
	if !c.access.on(keyDocsSend) {
		return nil, sharedNotGiven("folder", folder, keyDocsSend)
	}
	if err := c.access.checkShared("folder", folder, sh); err != nil {
		return nil, err
	}
	text, _ := c.args.text("body", false, maxWriteChars)
	return &held{
		title: `Create "` + oneLine(title, listedTitleClip) + `" in a shared folder`,
		fields: []builtinintegrations.HoldField{
			field("Folder", folderCard(dir)),
			field(fileShareLabel, sh.text()),
			field("Title", title),
			field("Text", orNone(strings.Join(markdownLines(text), "\n"))),
		},
		recipients: sh.people,
		needs:      keyDocsSend,
		run:        create,
	}, nil
}

// folderCard is a folder as a card names it: its name, the owner's
// text shown and not sent, and its id.
func folderCard(f driveFile) string {
	if name := shownLine(f.Name, nameClip); name != "" {
		return `"` + name + `" (` + f.ID + `)`
	}
	return "Folder " + f.ID
}

// sharedDocumentHold is a write to the document f that anyone besides
// the owner can see (s), held for the owner: titled "<what> <name>, a
// shared document", its card the document, who can see it, then
// fields, and run what the write does, refusing once the document is
// gone (404, 410), since nothing was done. It needs docs_send and
// everyone s names in send_to (checkShared).
func (c *call) sharedDocumentHold(ctx context.Context, f driveFile, s share, what string, fields []builtinintegrations.HoldField, run func(ctx context.Context, c *call) (any, error)) (any, error) {
	if !c.access.on(keyDocsSend) {
		return nil, sharedNotGiven("document", f.ID, keyDocsSend)
	}
	if err := c.access.checkShared("document", f.ID, s); err != nil {
		return nil, err
	}
	o, err := c.ownerOf(ctx, f)
	if err != nil {
		return nil, err
	}
	me, err := c.account(ctx)
	if err != nil {
		return nil, err
	}
	return &held{
		title:      what + " " + docTitle(f) + ", a shared document",
		fields:     append([]builtinintegrations.HoldField{field("Document", docNamed(f, o, me)), field(fileShareLabel, s.text())}, fields...),
		recipients: s.people,
		needs:      keyDocsSend,
		run: func(ctx context.Context, c *call) (any, error) {
			answer, err := run(ctx, c)
			if g := gone(err, "The document", "document "+f.ID+" was gone"); g != nil {
				return nil, g
			}
			return answer, err
		},
	}, nil
}

// newDocument makes an empty Google Doc titled title through Drive
// (files.create), carrying Kivali's mark and, when folder is set, in
// that folder, and answers its id. When Drive refuses the folder
// itself, with a 403 or a 404 whose words name it, the document is
// made again with no folder, at the top of the owner's My Drive, and
// refused is Google's answer for the folder. Any other failure is the
// error and nothing is made again: Google not answering or a 5xx may
// have made the document.
func (c *call) newDocument(ctx context.Context, title, folder string) (id, refused string, err error) {
	file := map[string]any{
		"name":          title,
		"mimeType":      documentType,
		"appProperties": map[string]string{kivaliMark: c.marker()},
	}
	path := "/drive/v3/files?" + url.Values{"fields": {"id"}, "supportsAllDrives": {"true"}}.Encode()
	var made driveFile
	if folder != "" {
		file["parents"] = []string{folder}
		err = c.g.change(ctx, c.token, googleAPI, http.MethodPost, path, file, &made)
		var ae *apiError
		if !errors.As(err, &ae) || !ae.refuses(folder) {
			id, err = madeID(made, err)
			return id, "", err
		}
		refused = fmt.Sprintf("HTTP %d: %s", ae.Status, sentence(ae.Message))
		delete(file, "parents")
	}
	err = c.g.change(ctx, c.token, googleAPI, http.MethodPost, path, file, &made)
	id, err = madeID(made, err)
	return id, refused, err
}

// refuses reports whether Google's answer refuses the Drive file id
// itself: a 403 or a 404, not a rate limit, whose words name it.
func (e *apiError) refuses(id string) bool {
	return (e.Status == http.StatusForbidden || e.Status == http.StatusNotFound) && !e.RateLimited && strings.Contains(e.Message, id)
}

// madeID is the id of the file Drive answered it made, or err.
func madeID(made driveFile, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if !driveIDPattern.MatchString(made.ID) {
		return "", failure("Google made the document and answered no id Kivali reads; tell the owner.")
	}
	return made.ID, nil
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
// first tab's), as its own paragraphs, against the revision it read the
// end from. On a document anyone besides the owner can see (fileShare)
// it is held for the owner with the text it adds, and Run reads the end
// when the owner approves, since a revision read a day before is one
// Google no longer writes against.
func appendToDocument(ctx context.Context, c *call) (any, error) {
	id, err := c.args.id("document_id", driveIDPattern)
	if err != nil {
		return nil, err
	}
	paras, chars, err := writeText(c.args, "text", true)
	if err != nil {
		return nil, err
	}
	f, err := c.writableDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	sh, err := c.fileShare(ctx, id)
	if err != nil {
		return nil, err
	}
	add := func(ctx context.Context, c *call) (any, error) {
		d, err := c.documentEnd(ctx, id)
		if err != nil {
			return nil, err
		}
		at, empty := bodyEnd(d.Body)
		prefix := "\n"
		if empty {
			prefix = ""
		}
		c.audit = fmt.Sprintf("adding %d characters to the end of document %s", chars, id)
		if err := c.batchUpdate(ctx, id, d.RevisionID, insertRequests(at, prefix, paras), nil); err != nil {
			return nil, unanswered(err)
		}
		c.audit = fmt.Sprintf("added %d characters to the end of document %s%s", chars, id, sh.auditNote())
		return fmt.Sprintf("Added %d characters at the end of document %s.", chars, id), nil
	}
	if !sh.others() {
		return add(ctx, c)
	}
	text, _ := c.args.text("text", true, maxWriteChars)
	return c.sharedDocumentHold(ctx, f, sh, "Add to", []builtinintegrations.HoldField{
		field("Text to add at the end", strings.Join(markdownLines(text), "\n")),
	}, add)
}

// replaceText replaces every occurrence of find in a document
// (replaceAllText, every tab), and answers how many Google replaced. On
// a document anyone besides the owner can see (fileShare) it is held
// for the owner with what it finds and what replaces it.
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
	matchCase, _, err := c.args.flag("match_case")
	if err != nil {
		return nil, err
	}
	f, err := c.writableDocument(ctx, id)
	if err != nil {
		return nil, err
	}
	sh, err := c.fileShare(ctx, id)
	if err != nil {
		return nil, err
	}
	req := map[string]any{"replaceAllText": map[string]any{
		"containsText": map[string]any{"text": find, "matchCase": matchCase},
		"replaceText":  with,
	}}
	replace := func(ctx context.Context, c *call) (any, error) {
		var answer struct {
			Replies []struct {
				ReplaceAllText *struct {
					OccurrencesChanged int `json:"occurrencesChanged"`
				} `json:"replaceAllText"`
			} `json:"replies"`
		}
		c.audit = "replacing text in document " + id
		if err := c.batchUpdate(ctx, id, "", []any{req}, &answer); err != nil {
			return nil, unanswered(err)
		}
		n := 0
		if len(answer.Replies) > 0 && answer.Replies[0].ReplaceAllText != nil {
			n = answer.Replies[0].ReplaceAllText.OccurrencesChanged
		}
		c.audit = fmt.Sprintf("replaced %d occurrences in document %s%s", n, id, sh.auditNote())
		if n == 0 {
			return "Document " + id + " holds no occurrence of the text; nothing was changed.", nil
		}
		if n == 1 {
			return "Replaced 1 occurrence in document " + id + ".", nil
		}
		return fmt.Sprintf("Replaced %d occurrences in document %s.", n, id), nil
	}
	if !sh.others() {
		return replace(ctx, c)
	}
	capitals := "Ignored"
	if matchCase {
		capitals = "Must match"
	}
	return c.sharedDocumentHold(ctx, f, sh, "Replace text in", []builtinintegrations.HoldField{
		field("Find", find),
		field("Replace with", cmp.Or(with, "Nothing: each occurrence is deleted")),
		field("Capitals", capitals),
	}, replace)
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
