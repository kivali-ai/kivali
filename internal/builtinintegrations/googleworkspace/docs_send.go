package googleworkspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// Docs' sends: a comment on a document and a reply to a comment, each
// held for the owner's approval (held). Drive tells the document's
// owner of a comment, everyone in a comment's conversation of a reply,
// and each person a comment mentions; Run posts the text the card
// showed.

// The bounds of a comment an agent writes: its text, the document's
// text it quotes, and the people it mentions.
const (
	maxCommentChars = 4000
	maxAnchorChars  = 500
	maxMentions     = 50
)

// The descriptions of Docs' sends, which the agent reads.
const (
	mentionWords    = " To mention someone, write their address with + or @ before it, such as +dana@example.com; Google emails each person mentioned, and each must be one the owner allows you to send to."
	addCommentWords = "Ask the owner to post a comment on a document within the folders your access includes, by its id: content is the comment's text, and anchor_text, when given, the document's text it is about, which the comment quotes. Google tells the document's owner." + mentionWords + waitsWords
	replyWords      = "Ask the owner to post a reply to a comment on a document within the folders your access includes, by the document's id and the comment's, as list_comments lists them: content is the reply's text. Google tells the document's owner and everyone in the comment's conversation." + mentionWords + waitsWords
)

// mentionPattern is a mention in a comment's text: + or @ right before
// an address, not inside a word or an address of its own.
var mentionPattern = regexp.MustCompile(`(?i)(?:^|[^a-z0-9.!#$%&'*+/=?^_{|}~-])[+@]([a-z0-9.!#$%&'*+/=?^_{|}~-]+@[a-z0-9-]+(?:\.[a-z0-9-]+)+)`)

// mentions is every address text mentions (mentionPattern), lower-cased,
// once each, in the order it first names them.
func mentions(text string) []string {
	var out []string
	for _, m := range mentionPattern.FindAllStringSubmatch(text, -1) {
		if a := strings.ToLower(m[1]); len(a) <= 254 && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// commentText is the argument key as a comment's or a reply's text:
// required, at most maxCommentChars characters, cleanText's, mentioning
// at most maxMentions people.
func commentText(a args, key string) (string, error) {
	s, err := a.text(key, true, maxCommentChars)
	if err != nil {
		return "", err
	}
	s, _ = cleanText(s)
	if len(mentions(s)) > maxMentions {
		return "", badArgument(fmt.Sprintf("%s mentions more than %d people; mention fewer.", key, maxMentions))
	}
	return s, nil
}

// docCard is a document as a send's card names it: its name, its id,
// who owns it and whether it is shared. The name is its owner's text,
// shown and not sent.
func docCard(f driveFile, o docOwner, me account) string {
	name := shownLine(f.Name, nameClip)
	w := `"` + name + `" (` + f.ID + `)`
	if name == "" {
		w = "Document " + f.ID
	}
	switch {
	case o.sharedDrive:
		return w + ", in a shared drive"
	case o.address == me.address:
		w += ", owned by you"
	case o.address != "":
		w += ", owned by " + o.address
	default:
		w += ", owned by someone whose address Google does not show"
	}
	if f.Shared {
		return w + ", shared with others"
	}
	return w + ", not shared"
}

// commentPeople is whom a comment reaches besides those it mentions:
// the document's owner, and others (a comment's author and its
// repliers), each when Kivali reads their address and it is not the
// signed-in account's.
func commentPeople(me account, mentioned []string, o docOwner, others ...string) []string {
	out := slices.Clone(mentioned)
	for _, a := range append([]string{o.address}, others...) {
		if a != "" && a != me.address && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// sendComment is a comment's or a reply's run: content posted to path
// on Drive (comments.create, replies.create), answering the new one's
// id. Google not answering may have posted it, so that failure says to
// look first.
func (c *call) sendComment(ctx context.Context, path string, payload []byte) (string, error) {
	var made struct {
		ID string `json:"id"`
	}
	if err := c.g.send(ctx, c.token, http.MethodPost, googleAPI, path+"?fields=id", payload, maxAnswerBytes, &made); err != nil {
		return "", unsure(err, "it was posted: list_comments shows it if it was")
	}
	if !driveIDPattern.MatchString(made.ID) {
		return "", failure("Google posted it and answered an id Kivali does not read; list_comments shows it.")
	}
	return made.ID, nil
}

// addComment holds a comment on a document the access includes
// (readableDocument) for the owner's approval: its text, the document's
// text it quotes when anchor_text is given, and whom it mentions, each
// of whom must be in send_to. Run posts it (comments.create).
func addComment(ctx context.Context, c *call) (any, error) {
	id, err := c.args.id("document_id", driveIDPattern)
	if err != nil {
		return nil, err
	}
	content, err := commentText(c.args, "content")
	if err != nil {
		return nil, err
	}
	anchor, err := c.args.text("anchor_text", false, maxAnchorChars)
	if err != nil {
		return nil, err
	}
	anchor, _ = cleanText(anchor)
	f, err := c.readableDocument(ctx, id)
	if err != nil {
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
	mentioned := slices.DeleteFunc(mentions(content), func(a string) bool { return a == me.address })
	if err := c.access.checkRecipients(mentioned); err != nil {
		return nil, err
	}
	body := map[string]any{"content": content}
	fields := []builtinintegrations.HoldField{field("Document", docCard(f, o, me))}
	if anchor != "" {
		body["quotedFileContent"] = map[string]string{"mimeType": "text/plain", "value": anchor}
		fields = append(fields, field("Quoting", anchor))
	}
	fields = append(fields, field("Comment", content), field("Mentions", orNone(strings.Join(mentioned, ", "))))
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	people := commentPeople(me, mentioned, o)
	return &held{
		title:      "Comment on " + docTitle(f),
		fields:     fields,
		recipients: people,
		run: func(ctx context.Context, c *call) (any, error) {
			made, err := c.sendComment(ctx, filePath(id)+"/comments", payload)
			if err != nil {
				return nil, err
			}
			c.audit = "commented on document " + id + " as comment " + made + "; told " + orNone(idList(people))
			return "Posted comment " + made + " on document " + id + ". Google told " + toldWords(people) + ".", nil
		},
	}, nil
}

// docTitle is a document's name as a card's title quotes it.
func docTitle(f driveFile) string {
	if name := shownLine(f.Name, listedTitleClip); name != "" {
		return `"` + name + `"`
	}
	return "document " + f.ID
}

// commentGetFields are the fields reply_to_comment reads of the
// comment it answers: who wrote it, when, its text, and who replied.
const commentGetFields = "id,author(displayName,emailAddress,me),createdTime,content,deleted,replies(author(emailAddress,me),deleted)"

// replyToComment holds a reply to a comment on a document the access
// includes for the owner's approval, its card showing the comment it
// answers: who wrote it, when, and its text, the commenter's, shown and
// not sent. Everyone it mentions must be in send_to. Run posts it
// (replies.create).
func replyToComment(ctx context.Context, c *call) (any, error) {
	id, err := c.args.id("document_id", driveIDPattern)
	if err != nil {
		return nil, err
	}
	cid, err := c.args.id("comment_id", driveIDPattern)
	if err != nil {
		return nil, err
	}
	content, err := commentText(c.args, "content")
	if err != nil {
		return nil, err
	}
	f, err := c.readableDocument(ctx, id)
	if err != nil {
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
	var cm struct {
		Author struct {
			EmailAddress string `json:"emailAddress"`
			Me           bool   `json:"me"`
		} `json:"author"`
		CreatedTime string `json:"createdTime"`
		Content     string `json:"content"`
		Deleted     bool   `json:"deleted"`
		Replies     []struct {
			Author struct {
				EmailAddress string `json:"emailAddress"`
				Me           bool   `json:"me"`
			} `json:"author"`
			Deleted bool `json:"deleted"`
		} `json:"replies"`
	}
	err = c.g.get(ctx, c.token, googleAPI, filePath(id)+"/comments/"+url.PathEscape(cid)+"?"+url.Values{"fields": {commentGetFields}}.Encode(), &cm)
	var ae *apiError
	if (errors.As(err, &ae) && ae.Status == http.StatusNotFound) || (err == nil && cm.Deleted) {
		return nil, failure("Document " + id + " has no comment " + cid + "; list_comments lists its comments with their ids.")
	}
	if err != nil {
		return nil, err
	}
	mentioned := slices.DeleteFunc(mentions(content), func(a string) bool { return a == me.address })
	if err := c.access.checkRecipients(mentioned); err != nil {
		return nil, err
	}
	author := func(addr string, mine bool) string {
		if mine {
			return me.address
		}
		if a := strings.ToLower(strings.TrimSpace(addr)); len(a) <= 254 && addressChars.MatchString(a) {
			return a
		}
		return ""
	}
	wrote := author(cm.Author.EmailAddress, cm.Author.Me)
	others := []string{wrote}
	for _, r := range cm.Replies {
		if !r.Deleted {
			others = append(others, author(r.Author.EmailAddress, r.Author.Me))
		}
	}
	by := "by " + wrote
	switch wrote {
	case me.address:
		by = "by you"
	case "":
		by = "by someone whose address Google does not show"
	}
	if t := driveTime(cm.CreatedTime); t != "" {
		by += ", written " + t
	}
	payload, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return nil, err
	}
	people := commentPeople(me, mentioned, o, others...)
	return &held{
		title: "Reply to a comment on " + docTitle(f),
		fields: []builtinintegrations.HoldField{
			field("Document", docCard(f, o, me)),
			field("Replying to", "Comment "+cid+" "+by+":\n"+orNone(shownText(cm.Content))),
			field("Reply", content),
			field("Mentions", orNone(strings.Join(mentioned, ", "))),
		},
		recipients: people,
		run: func(ctx context.Context, c *call) (any, error) {
			made, err := c.sendComment(ctx, filePath(id)+"/comments/"+url.PathEscape(cid)+"/replies", payload)
			if err != nil {
				return nil, err
			}
			c.audit = "replied to comment " + cid + " on document " + id + " as reply " + made + "; told " + orNone(idList(people))
			return "Posted reply " + made + " to comment " + cid + " on document " + id + ". Google told " + toldWords(people) + ".", nil
		},
	}, nil
}
