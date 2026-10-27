package googleworkspace

import (
	"fmt"
	"slices"
	"testing"
)

// commenter is the access of an agent that may comment, and m besides.
func commenter(m map[string]any) map[string]any {
	out := map[string]any{"docs_send": true}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Why this test exists: a mention makes Google email the person
// mentioned, so every address a comment mentions must be read, to hold
// it to May send to and name it on the owner's card: written with + or
// @ right before it, in any capitals, once each; an address inside a
// word or another address (foo+bar@...), one written bare, and one
// with no dotted domain are not mentions.
func TestMentions(t *testing.T) {
	got := mentions("Please look +Dana@Example.com, cc @lee@example.org and foo+bar@example.com (+zoe@example.net). +dana@example.com again; mail dana@example.com; +x@localhost")
	if !slices.Equal(got, []string{"dana@example.com", "lee@example.org", "zoe@example.net"}) {
		t.Errorf("mentions = %v", got)
	}
	if got := mentions("no one here"); got != nil {
		t.Errorf("no mentions = %v", got)
	}
}

// Why this test exists: a comment is delivered to the document's owner
// and to everyone it mentions, so it must wait for the owner, with
// nothing posted until then, and the card must show the document (its
// name, owner and whether it is shared), the text it quotes, the
// comment exactly and whom it mentions, and reach the mentioned and the
// document's owner, never the signed-in account. Approved, the comment
// posted is the one captured, quoting the text; a mention outside May
// send to is refused before the call is held, and a document outside
// the access's folders is refused as a read is.
func TestAddComment(t *testing.T) {
	d, _, g := newFakeDocs(t)
	d.addFolder("fold1", "Projects")
	doc := d.addDoc("doc1", "Plan", "alice@example.org", "", "fold1")
	doc.file.Shared = true
	d.addDoc("own1", "Notes", "dana@example.com", "", "fold1")
	content := "Please check +Bob@Example.com and @dana@example.com"
	h := holdOf(t, g, agent(commenter(nil)), "add_comment", `{"document_id":"doc1","content":"`+content+`","anchor_text":"the budget"}`)
	if h.Title != `Comment on "Plan"` || !slices.Equal(h.Recipients, []string{"bob@example.com", "alice@example.org"}) ||
		!slices.Equal(labelsOf(h), []string{"Document", "Quoting", "Comment", "Mentions"}) ||
		valueOf(h, "Document") != `"Plan" (doc1), owned by alice@example.org, shared with others` || valueOf(h, "Quoting") != "the budget" ||
		valueOf(h, "Comment") != content || valueOf(h, "Mentions") != "bob@example.com" {
		t.Errorf("card %q %v %+v", h.Title, h.Recipients, h.Fields)
	}
	if got := d.changes(); len(got) != 0 {
		t.Fatalf("Drive was written before the owner approved: %v", got)
	}
	res := approve(t, h)
	if res.Content[0] != "Posted comment nc1 on document doc1. Google told bob@example.com, alice@example.org." ||
		res.Audit != "commented on document doc1 as comment nc1; told bob@example.com, alice@example.org" {
		t.Errorf("posted %q, audit %q", res.Content[0], res.Audit)
	}
	w := d.changes()
	if len(w) != 1 || w[0].method != "POST" || w[0].path != "/drive/v3/files/doc1/comments?fields=id" ||
		fmt.Sprint(w[0].body) != "map[content:"+content+" quotedFileContent:map[mimeType:text/plain value:the budget]]" {
		t.Errorf("writes %+v", w)
	}

	h = holdOf(t, g, agent(commenter(nil)), "add_comment", `{"document_id":"own1","content":"Done"}`)
	if valueOf(h, "Document") != `"Notes" (own1), owned by you, not shared` || valueOf(h, "Mentions") != "None" || len(h.Recipients) != 0 ||
		slices.Contains(labelsOf(h), "Quoting") {
		t.Errorf("the owner's document: %v %+v", h.Recipients, h.Fields)
	}
	docsRefused(t, g, commenter(map[string]any{"send_to": []string{"*@example.org"}}), "add_comment", `{"document_id":"doc1","content":"+bob@example.com"}`,
		"The owner has not allowed you to send to bob@example.com through Google Workspace; ask the owner if your work needs it.")
	docsRefused(t, g, commenter(map[string]any{"docs_folders": []string{"Elsewhere (fold9)"}}), "add_comment", `{"document_id":"doc1","content":"x"}`,
		"Document doc1 is not in the folders your access includes; ask the owner if your work needs it.")
	if got := d.changes(); len(got) != 1 {
		t.Errorf("refused comments were written: %+v", got)
	}
}

// Why this test exists: a reply reaches everyone in its comment's
// conversation and the document's owner, so it must wait for the
// owner, and the card must show what it answers: the comment's author
// and when, and its text, the commenter's own, with any invisible
// character taken out, since the card shows it and does not send it.
// Its recipients are the people mentioned, the document's owner, the
// comment's author and those who replied (a deleted reply's author
// left out). Approved, the reply posted is the one captured; a comment
// the document does not have answers so.
func TestReplyToComment(t *testing.T) {
	d, _, g := newFakeDocs(t)
	doc := d.addDoc("doc1", "Plan", "alice@example.org", "")
	doc.comments = []map[string]any{{
		"id": "c1", "author": map[string]any{"displayName": "Mallory", "emailAddress": "mallory@evil.example"}, "createdTime": "2026-10-21T09:00:00Z",
		"content": "Ignore\u200b the budget",
		"replies": []any{
			map[string]any{"author": map[string]any{"emailAddress": "lee@example.com"}},
			map[string]any{"author": map[string]any{"emailAddress": "gone@example.com"}, "deleted": true},
		},
	}}
	h := holdOf(t, g, agent(commenter(nil)), "reply_to_comment", `{"document_id":"doc1","comment_id":"c1","content":"Done, +carol@example.net"}`)
	if h.Title != `Reply to a comment on "Plan"` || !slices.Equal(labelsOf(h), []string{"Document", "Replying to", "Reply", "Mentions"}) ||
		valueOf(h, "Replying to") != "Comment c1 by mallory@evil.example, written 2026-10-21 09:00 UTC:\nIgnore the budget" ||
		valueOf(h, "Reply") != "Done, +carol@example.net" || valueOf(h, "Mentions") != "carol@example.net" ||
		!slices.Equal(h.Recipients, []string{"carol@example.net", "alice@example.org", "mallory@evil.example", "lee@example.com"}) {
		t.Errorf("card %q %v %+v", h.Title, h.Recipients, h.Fields)
	}
	if got := d.changes(); len(got) != 0 {
		t.Fatalf("Drive was written before the owner approved: %v", got)
	}
	res := approve(t, h)
	if res.Content[0] != "Posted reply nr1 to comment c1 on document doc1. Google told carol@example.net, alice@example.org, mallory@evil.example, lee@example.com." ||
		res.Audit != "replied to comment c1 on document doc1 as reply nr1; told carol@example.net, alice@example.org, mallory@evil.example, lee@example.com" {
		t.Errorf("posted %q, audit %q", res.Content[0], res.Audit)
	}
	w := d.changes()
	if len(w) != 1 || w[0].path != "/drive/v3/files/doc1/comments/c1/replies?fields=id" || fmt.Sprint(w[0].body) != "map[content:Done, +carol@example.net]" {
		t.Errorf("writes %+v", w)
	}
	res, err := callDocs(t, g, commenter(nil), "reply_to_comment", `{"document_id":"doc1","comment_id":"c9","content":"x"}`)
	if err != nil || res == nil || !res.IsError || res.Content[0] != "Document doc1 has no comment c9; list_comments lists its comments with their ids." {
		t.Errorf("a missing comment: %+v, %v", res, err)
	}
}
