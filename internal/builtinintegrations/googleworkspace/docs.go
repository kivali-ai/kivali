package googleworkspace

import (
	"context"
	"net/url"
	"slices"
	"strings"
)

// Docs' actions, unprefixed.
const (
	actionSearchDocuments  = "search_documents"
	actionGetDocument      = "get_document"
	actionListComments     = "list_comments"
	actionCreateDocument   = "create_document"
	actionAppendToDocument = "append_to_document"
	actionReplaceText      = "replace_text"
	actionAddComment       = "add_comment"
	actionReplyToComment   = "reply_to_comment"
)

// docsActions is Docs' actions, read, write, then send.
var docsActions = []action{
	{
		name:             actionSearchDocuments,
		ownerDescription: "Search Google Docs, those shared with you included, within the folders the access names, a page at a time. Reads only.",
		bucket:           keyDocsRead,
	},
	{
		name:             actionGetDocument,
		ownerDescription: "Read a document as text, 60,000 characters at a time. Reads only.",
		bucket:           keyDocsRead,
	},
	{
		name:             actionListComments,
		ownerDescription: "List a document's comments and their replies. Reads only.",
		bucket:           keyDocsRead,
	},
	{
		name:             actionCreateDocument,
		ownerDescription: "Create a document.",
		bucket:           keyDocsWrite,
	},
	{
		name:             actionAppendToDocument,
		ownerDescription: "Add text at the end of a document you own or the agent created.",
		bucket:           keyDocsWrite,
	},
	{
		name:             actionReplaceText,
		ownerDescription: "Replace text in a document you own or the agent created.",
		bucket:           keyDocsWrite,
	},
	{
		name:             actionAddComment,
		ownerDescription: "Ask to comment on a document. The comment is posted only once you approve it.",
		bucket:           keyDocsSend,
	},
	{
		name:             actionReplyToComment,
		ownerDescription: "Ask to reply to a comment on a document. The reply is posted only once you approve it.",
		bucket:           keyDocsSend,
	},
}

// folderQuery is the Drive search for folders: every folder the
// sign-in can open that is not in the trash.
const folderQuery = "mimeType='application/vnd.google-apps.folder' and trashed=false"

// folderOptions is the options of docs_folders: the Drive folders the
// sign-in can open (files.list), shared drives' among them, by name,
// capitals ignored. At most maxPages pages of 1000 are read.
func (g *GoogleWorkspace) folderOptions(ctx context.Context, token string) ([]string, error) {
	type folder struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var all []folder
	q := url.Values{
		"q":                         {folderQuery},
		"fields":                    {"nextPageToken,files(id,name)"},
		"pageSize":                  {"1000"},
		"orderBy":                   {"name"},
		"supportsAllDrives":         {"true"},
		"includeItemsFromAllDrives": {"true"},
	}
	for range maxPages {
		var page struct {
			Files         []folder `json:"files"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := g.get(ctx, token, googleAPI, "/drive/v3/files?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		all = append(all, page.Files...)
		if page.NextPageToken == "" {
			break
		}
		q.Set("pageToken", page.NextPageToken)
	}
	slices.SortStableFunc(all, func(a, b folder) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	out := make([]string, 0, len(all))
	for _, f := range all {
		if f.ID != "" {
			out = append(out, option(f.Name, f.ID))
		}
	}
	return out, nil
}
