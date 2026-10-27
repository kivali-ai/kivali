package googleworkspace

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
)

// Gmail's actions, unprefixed.
const (
	actionListLabels     = "list_labels"
	actionSearchMessages = "search_messages"
	actionGetMessage     = "get_message"
	actionGetThread      = "get_thread"
	actionGetAttachment  = "get_attachment"
	actionCreateDraft    = "create_draft"
	actionDeleteDraft    = "delete_draft"
	actionModifyLabels   = "modify_labels"
	actionSendEmail      = "send_email"
	actionSendDraft      = "send_draft"
	actionProfile        = "profile"
)

// gmailActions is Gmail's actions, read, write, then send, and profile,
// Test connection's.
var gmailActions = []action{
	{
		name:             actionListLabels,
		description:      "List the Gmail labels your access includes, one per line as the label's name and its id in parentheses: Gmail's own Inbox, Starred, Important, Sent, Drafts, Spam and Trash, then the account's labels by name.",
		ownerDescription: "List the labels the agent's access includes, by name. Reads only.",
		schema:           schema(nil),
		bucket:           keyGmailRead,
		run:              listLabels,
	},
	{
		name:             actionSearchMessages,
		ownerDescription: "Search mail with Gmail's search words, within the labels the access names, 25 at a time. Reads only.",
		bucket:           keyGmailRead,
	},
	{
		name:             actionGetMessage,
		ownerDescription: "Read one message as text, with its attachments listed by name and size. Reads only.",
		bucket:           keyGmailRead,
	},
	{
		name:             actionGetThread,
		ownerDescription: "Read a conversation's messages as text. Reads only.",
		bucket:           keyGmailRead,
	},
	{
		name:             actionGetAttachment,
		ownerDescription: "Save one attachment of at most 25 MB into the agent's files, in a folder kept for downloads from outside. Reads only.",
		bucket:           keyGmailRead,
	},
	{
		name:             actionCreateDraft,
		ownerDescription: "Write a draft, new or as a reply in a conversation. Sends nothing.",
		bucket:           keyGmailWrite,
	},
	{
		name:             actionDeleteDraft,
		ownerDescription: "Delete a draft the agent wrote. Sends nothing.",
		bucket:           keyGmailWrite,
	},
	{
		name:             actionModifyLabels,
		ownerDescription: "Add and remove labels on messages, which also archives them, marks them read or stars them. Moves nothing to the trash.",
		bucket:           keyGmailWrite,
	},
	{
		name:             actionSendEmail,
		ownerDescription: "Ask to send an email. It goes only once you approve it, exactly as you approved it.",
		bucket:           keyGmailSend,
	},
	{
		name:             actionSendDraft,
		ownerDescription: "Ask to send a draft. It goes only once you approve it, exactly as you approved it.",
		bucket:           keyGmailSend,
	},
	{
		name:             actionProfile,
		ownerDescription: "Name the Google account the sign-in is for, for Test connection. Reads only.",
		ownerOnly:        true,
	},
}

// schema is an object's input schema with props, required naming the
// ones it must have.
func schema(props map[string]any, required ...string) string {
	if props == nil {
		props = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	b, err := json.Marshal(map[string]any{"type": "object", "properties": props, "required": required})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// gmailProfile is Gmail's users/me/profile, as far as Kivali reads it.
type gmailProfile struct {
	EmailAddress string `json:"emailAddress"`
}

// profile is the signed-in account's Gmail profile.
func (g *GoogleWorkspace) profile(ctx context.Context, token string) (gmailProfile, error) {
	var p gmailProfile
	err := g.get(ctx, token, gmailAPI, "/gmail/v1/users/me/profile", &p)
	return p, err
}

// systemLabels are Gmail's own labels an access may name, in the order
// they are listed, each with the name Gmail shows for it. Gmail names
// them by their ids (DRAFT is the Drafts label's); its other own labels
// (UNREAD, CHAT and the inbox's categories) are not listed.
var systemLabels = []struct{ id, name string }{
	{"INBOX", "Inbox"},
	{"STARRED", "Starred"},
	{"IMPORTANT", "Important"},
	{"SENT", "Sent"},
	{"DRAFT", "Drafts"},
	{"SPAM", "Spam"},
	{"TRASH", "Trash"},
}

// label is one Gmail label as it is listed: its id, which a call is
// held to, and its name.
type label struct{ id, name string }

// gmailLabel is one label as Gmail lists it (users/me/labels): Type is
// system for Gmail's own, user for the account's.
type gmailLabel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// labels is every label the access form lists, as option writes them:
// systemLabels that Gmail has, in their order, then the account's own
// labels by name, capitals ignored.
func (g *GoogleWorkspace) labels(ctx context.Context, token string) ([]label, error) {
	var answer struct {
		Labels []gmailLabel `json:"labels"`
	}
	if err := g.get(ctx, token, gmailAPI, "/gmail/v1/users/me/labels", &answer); err != nil {
		return nil, err
	}
	var out, own []label
	for _, sys := range systemLabels {
		if slices.ContainsFunc(answer.Labels, func(l gmailLabel) bool { return l.ID == sys.id }) {
			out = append(out, label{id: sys.id, name: sys.name})
		}
	}
	for _, l := range answer.Labels {
		if l.Type == "user" && l.ID != "" {
			own = append(own, label{id: l.ID, name: cmp.Or(l.Name, l.ID)})
		}
	}
	slices.SortStableFunc(own, func(a, b label) int { return strings.Compare(strings.ToLower(a.name), strings.ToLower(b.name)) })
	return append(out, own...), nil
}

// labelOptions is the options of gmail_labels.
func (g *GoogleWorkspace) labelOptions(ctx context.Context, token string) ([]string, error) {
	ls, err := g.labels(ctx, token)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, option(l.name, l.id))
	}
	return out, nil
}

// listLabels answers the labels the access includes, one per line as
// the access form names them.
func listLabels(ctx context.Context, c *call) (any, error) {
	ls, err := c.g.labels(ctx, c.token)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range ls {
		if includes(c.access.gmailLabels, l.id) {
			lines = append(lines, option(l.name, l.id))
		}
	}
	if len(lines) == 0 {
		return "Your access includes none of the account's labels.", nil
	}
	return strings.Join(lines, "\n"), nil
}
