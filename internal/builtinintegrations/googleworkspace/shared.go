package googleworkspace

import (
	"context"
	"net/url"
	"slices"
	"strings"
)

// Who besides the owner can see a calendar, a document or a folder. A
// write there is a send: the people it is shared with see what it
// changes, and the Write bucket changes only what no other person is
// told about. A calendar's are read from its access control list
// (acl.list, calendar.acls.readonly), a document's or a folder's from
// its permissions (permissions.list, drive.readonly), when the call is
// held; the card shows them, and Run does not read them again.

// calSharedWords and docSharedTail say, in the descriptions the agent
// reads of the writes, that one others can see is a send.
const (
	calSharedWords = " A create, change or delete on a calendar the owner shares with anyone else is a send too, since they see it: everyone it is shared with must be one the owner allows you to send to, and while the owner names whom you may send to, one on a calendar shared with everyone at a domain or publicly is refused."
	docSharedTail  = " is a send, since they see it: it needs the owner to have given you commenting on documents, everyone it is shared with must be one the owner allows you to send to, and while the owner names whom you may send to, one shared with everyone at a domain or with anyone who has the link is refused." + waitsWords
)

// share is who besides the owner can see a calendar, a document or a
// folder: people, the addresses of the users and groups Kivali reads,
// lower-cased, each once; words, every entry as a card names it, in
// Google's order; and open, the words of the entries Kivali cannot
// hold to send_to by address: everyone at a domain, anyone, one whose
// address Google does not show, and those past the pages Kivali reads.
type share struct {
	people []string
	words  []string
	open   []string
}

// others reports whether anyone besides the owner can see it.
func (s share) others() bool { return len(s.words) > 0 }

// person adds addr, a user's or, for group, a group's lower-cased
// address; "" is one Google gives no address Kivali reads for.
func (s *share) person(addr string, group bool) {
	if addr == "" {
		s.openTo(unshownSharer)
		return
	}
	w := addr
	if group {
		w += " (a group)"
	}
	if !slices.Contains(s.words, w) {
		s.words = append(s.words, w)
	}
	if !slices.Contains(s.people, addr) {
		s.people = append(s.people, addr)
	}
}

// openTo adds an entry Kivali cannot hold to send_to, in words w.
func (s *share) openTo(w string) {
	if !slices.Contains(s.words, w) {
		s.words = append(s.words, w)
		s.open = append(s.open, w)
	}
}

// domain adds everyone at the domain d, as Google names it; tail
// follows in words ("" or " with the link").
func (s *share) domain(d, tail string) {
	d = strings.ToLower(strings.TrimSpace(d))
	if len(d) > 253 || !domainName.MatchString(d) || !strings.Contains(d, ".") {
		s.openTo("everyone at a domain Google does not name" + tail)
		return
	}
	s.openTo("everyone at " + d + tail)
}

// The words of entries a card names in place of an address.
const (
	unshownSharer  = "someone whose address Google does not show"
	unlistedSharer = "more than Kivali lists"
	unknownSharer  = "others Google names in a way Kivali does not read"
)

// text is everyone in words, for the card's field.
func (s share) text() string { return strings.Join(s.words, ", ") }

// auditNote ends the audit of a write held because of s.
func (s share) auditNote() string {
	if !s.others() {
		return ""
	}
	return "; shared with " + idList(s.words)
}

// sharePages bounds the pages of an access control list or of a file's
// permissions Kivali reads; entries past them are unlistedSharer.
const sharePages = 10

// The labels of a card's field naming whom a change is shown to.
const (
	calendarShareLabel = "Who can see this calendar"
	fileShareLabel     = "Who can see it"
)

// sharedNotGiven is the refusal of a write to the kind ("calendar",
// "document" or "folder") id, which others can see, for an agent whose
// access lacks the send switch bucket.
func sharedNotGiven(kind, id, bucket string) error {
	w := bucketWords[bucket]
	return refuse("This call is a send: "+kind+" "+id+" is shared with others, who would see the change. The owner has not given you "+w+" in Google Workspace, which a change others see needs; ask the owner if your work needs it.",
		"the access does not include "+w)
}

// checkShared refuses a write to the kind id that s reaches beyond
// send_to: an address it does not allow (checkRecipients) or, while
// send_to names anyone, an entry Kivali cannot hold to it by address
// (share.open), whom the change is shown to all the same.
func (a access) checkShared(kind, id string, s share) error {
	if err := a.checkRecipients(s.people); err != nil {
		return err
	}
	if len(s.open) == 0 || len(a.sendTo) == 0 {
		return nil
	}
	w := strings.Join(s.open, ", ")
	return refuse(strings.ToUpper(kind[:1])+kind[1:]+" "+id+" is shared with "+w+", whom Kivali cannot check against the addresses the owner allows you to send to, and they would see this change; ask the owner if your work needs it.",
		"send_to cannot be checked: "+kind+" "+id+" is shared with "+w)
}

// union is a, then each of b that a does not hold.
func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// aclRule is one rule of a calendar's access control list.
type aclRule struct {
	Role  string `json:"role"`
	Scope struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"scope"`
}

// calendarShare is who besides the owner can see the events on the
// calendar cal (acl.list, 250 rules a page): each rule's user other
// than the owner's own addresses (calOwner.self), group, domain, or the
// public (scope default). A rule of role none gives nothing, and one of
// freeBusyReader shows busy times alone, none of an event's text, so
// neither counts.
func (cc *calCall) calendarShare(ctx context.Context, cal string, o calOwner) (share, error) {
	var s share
	page := ""
	for range sharePages {
		q := url.Values{"maxResults": {"250"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		var list struct {
			Items         []aclRule `json:"items"`
			NextPageToken string    `json:"nextPageToken"`
		}
		if err := cc.g.get(ctx, cc.token, googleAPI, "/calendar/v3/calendars/"+url.PathEscape(cal)+"/acl?"+q.Encode(), &list); err != nil {
			return share{}, err
		}
		for _, r := range list.Items {
			if r.Role == "none" || r.Role == "freeBusyReader" {
				continue
			}
			switch r.Scope.Type {
			case "user":
				if addr := calAddress(r.Scope.Value); addr == "" || !o.self(addr) {
					s.person(addr, false)
				}
			case "group":
				s.person(calAddress(r.Scope.Value), true)
			case "domain":
				s.domain(r.Scope.Value, "")
			case "default":
				s.openTo("anyone, as the calendar is public")
			default:
				s.openTo(unknownSharer)
			}
		}
		if page = list.NextPageToken; page == "" {
			return s, nil
		}
	}
	s.openTo(unlistedSharer)
	return s, nil
}

// drivePermission is one of a Drive file's permissions.
type drivePermission struct {
	Type               string `json:"type"`
	EmailAddress       string `json:"emailAddress"`
	Domain             string `json:"domain"`
	AllowFileDiscovery bool   `json:"allowFileDiscovery"`
	Deleted            bool   `json:"deleted"`
}

// permissionFields are the fields Kivali asks Drive for of a file's
// permissions.
const permissionFields = "nextPageToken,permissions(type,emailAddress,domain,allowFileDiscovery,deleted)"

// fileShare is who besides the signed-in account can see the Drive
// file id, a document or a folder (permissions.list, 100 a page, a
// shared drive's members included): each permission's user other than
// the signed-in account, group, domain or anyone. Every role sees the
// file; a deleted account's permission reaches no one.
func (c *call) fileShare(ctx context.Context, id string) (share, error) {
	me, err := c.account(ctx)
	if err != nil {
		return share{}, err
	}
	var s share
	page := ""
	for range sharePages {
		q := url.Values{"fields": {permissionFields}, "pageSize": {"100"}, "supportsAllDrives": {"true"}}
		if page != "" {
			q.Set("pageToken", page)
		}
		var list struct {
			Permissions   []drivePermission `json:"permissions"`
			NextPageToken string            `json:"nextPageToken"`
		}
		if err := c.g.get(ctx, c.token, googleAPI, filePath(id)+"/permissions?"+q.Encode(), &list); err != nil {
			return share{}, err
		}
		for _, p := range list.Permissions {
			if p.Deleted {
				continue
			}
			switch p.Type {
			case "user":
				if addr := calAddress(p.EmailAddress); addr != me.address {
					s.person(addr, false)
				}
			case "group":
				s.person(calAddress(p.EmailAddress), true)
			case "domain":
				s.domain(p.Domain, linkWords(p.AllowFileDiscovery))
			case "anyone":
				if p.AllowFileDiscovery {
					s.openTo("anyone, as it is public on the web")
				} else {
					s.openTo("anyone with the link")
				}
			default:
				s.openTo(unknownSharer)
			}
		}
		if page = list.NextPageToken; page == "" {
			return s, nil
		}
	}
	s.openTo(unlistedSharer)
	return s, nil
}

// linkWords follows "everyone at <domain>" for a Drive permission that
// lets them find the file in a search (found) or only open it by its
// link.
func linkWords(found bool) string {
	if found {
		return ""
	}
	return " with the link"
}
