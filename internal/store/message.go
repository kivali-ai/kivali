package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MessageType enumerates the standard inter-agent message types. A
// message file of a type not listed here still parses: readers render
// it generically by its type string.
type MessageType string

const (
	MsgNotice              MessageType = "notice"
	MsgCEOApprovalRequest  MessageType = "ceo_approval_request"
	MsgCEOApprovalResponse MessageType = "ceo_approval_response"
	MsgCEONotification     MessageType = "ceo_notification"
	MsgCEONotificationAck  MessageType = "ceo_notification_ack"
	// MsgAssignmentEvent is a wake from the assignment tracker:
	// something about an assignment changed and the recipient has to
	// act on it. Written by the tracker, never by an agent; From is the
	// agent whose change it reports. It rides the same queue as every
	// other message and takes no reply: the assignment is the place to
	// act. See docs/developers/assignments.md §Wakes.
	MsgAssignmentEvent MessageType = "assignment_event"
)

// AssignmentRef is the front matter an assignment event carries: which
// assignment, which log entry, and the op it reports. Stored under the
// message front-matter key `assignment`.
type AssignmentRef struct {
	ID  int    `yaml:"id"`
	Seq int64  `yaml:"seq"`
	Op  string `yaml:"op"`
}

// KnownMessageTypes is every type this binary writes.
func KnownMessageTypes() []MessageType {
	return []MessageType{
		MsgNotice,
		MsgCEOApprovalRequest, MsgCEOApprovalResponse, MsgCEONotification,
		MsgCEONotificationAck, MsgAssignmentEvent,
	}
}

// IsCEOBound reports whether a message type is addressed to the CEO via
// the dedicated CEO-flow (instant-delivered to the CEO inbox, bypasses
// the release system).
func (t MessageType) IsCEOBound() bool {
	return t == MsgCEOApprovalRequest || t == MsgCEONotification
}

// IsCEOReply reports whether a message type is a CEO response emitted
// from the inbox UI (instant-delivered to the original sender).
func (t MessageType) IsCEOReply() bool {
	return t == MsgCEOApprovalResponse || t == MsgCEONotificationAck
}

// TakesReplies reports whether a message of this type may be named
// by another message's InReplyTo. A notice is a tell: nothing is owed
// back and nothing can be sent back, so no message may point at one.
// An assignment event is the tracker speaking: the reply is a change
// to the assignment, not a message. Everything else is repliable
// (ceo_approval_request by the CEO's response, and so on).
func (t MessageType) TakesReplies() bool {
	return t != MsgNotice && t != MsgAssignmentEvent
}

// DeliversToCEO reports whether the message lands in the CEO's inbox
// instantly rather than in an agent's release queue: the two CEO-bound
// types always, and an assignment event addressed to the CEO (an
// assignment given to them, or one of theirs that moved).
func (d Message) DeliversToCEO() bool {
	if d.Type.IsCEOBound() {
		return true
	}
	return d.Type == MsgAssignmentEvent && d.To.Contains("ceo")
}

// Recipients is a message's `to` field: one or more agent slugs.
//
// A notice may address several agents at once (one ledger file, one
// inbox pointer per recipient); every other type has exactly one
// recipient. The YAML codec is asymmetric on purpose:
//
//   - Unmarshal accepts a bare scalar (`to: alice`) or a sequence
//     (`to: [alice, bob]`).
//   - Marshal emits a bare scalar when there is exactly one
//     recipient, the common case.
//
// String() renders the list for display and for %s verbs, which keeps
// log lines, tool-result bodies, and HTML templates reading naturally
// without each of them having to know about the list.
type Recipients []string

// Primary returns the first recipient, or "" when there are none.
// Use it where exactly one slug is meaningful — the "who was this
// addressed to" of a CEO-bound message. For anything that must reach
// everyone, range over the
// slice.
func (r Recipients) Primary() string {
	if len(r) == 0 {
		return ""
	}
	return r[0]
}

// Contains reports whether slug is one of the recipients.
func (r Recipients) Contains(slug string) bool {
	for _, s := range r {
		if s == slug {
			return true
		}
	}
	return false
}

// String renders the recipients as a comma-separated list.
func (r Recipients) String() string { return strings.Join(r, ", ") }

// UnmarshalYAML accepts both the scalar and sequence forms. See the
// type comment for why both exist.
func (r *Recipients) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var one string
		if err := n.Decode(&one); err != nil {
			return err
		}
		if one == "" {
			*r = nil
			return nil
		}
		*r = Recipients{one}
		return nil
	case yaml.SequenceNode:
		var many []string
		if err := n.Decode(&many); err != nil {
			return err
		}
		*r = Recipients(many)
		return nil
	default:
		return fmt.Errorf("to: expected a slug or a list of slugs, got yaml kind %v", n.Kind)
	}
}

// MarshalYAML collapses a single recipient back to a scalar so
// single-recipient files keep the shape they have always had.
func (r Recipients) MarshalYAML() (any, error) {
	if len(r) == 1 {
		return r[0], nil
	}
	return []string(r), nil
}

// Hire is the embedded proposal on a `ceo_approval_request` of type
// "hire". When non-nil, the CEO approving the request triggers the
// server to provision the agent BEFORE writing the approval response
// — so the approval delivery landing in the requester's chat is
// itself the signal that the new agent is live. When nil, the
// approval request is a generic decision; no provisioning happens.
//
// Body and InitialAgentMemory are resolved at publish time from the
// publish tool's `body_path` / `initial_agent_memory_path` against
// the sender's /files/ view, and the resolved markdown is
// persisted here on the approval request file.
type Hire struct {
	Slug               string `yaml:"slug"`
	Role               string `yaml:"role"`
	Icon               string `yaml:"icon,omitempty"`
	ReportsTo          string `yaml:"reports_to"`
	Body               string `yaml:"body,omitempty"`
	InitialAgentMemory string `yaml:"initial_agent_memory,omitempty"`
}

// RoleUpdate is the embedded proposal on a `ceo_approval_request` of
// type "role update". When non-nil, the CEO approving the request
// triggers the server to overwrite the target agent's role.md BEFORE
// writing the approval response — same atomicity rule as Hire. Slug
// names the existing agent whose role.md is being replaced; Body
// holds the proposed new content (resolved at publish time from
// `body_path` against the sender's /files/ view).
type RoleUpdate struct {
	Slug string `yaml:"slug"`
	Body string `yaml:"body,omitempty"`
}

// HandbookUpdate is the embedded proposal on a
// `ceo_approval_request` of type "handbook update". Same atomicity
// rule as RoleUpdate / Hire: when the CEO approves, the server
// overwrites the org-wide handbook BEFORE writing the approval
// response, so the ack landing in the proposer's chat is itself the
// signal that the new handbook is live. There is no slug — the
// handbook is a singleton owned by the org.
type HandbookUpdate struct {
	Body string `yaml:"body,omitempty"`
}

// ReorgMove is one element of a reorg proposal — moving Slug under
// NewManager. NewManager may be "ceo" (the root) or any active agent
// slug.
type ReorgMove struct {
	Slug       string `yaml:"slug" json:"slug"`
	NewManager string `yaml:"new_manager" json:"new_manager"`
}

// ReorgFailure pairs a move that the apply step refused (or that
// errored mid-flight) with a human-readable reason. Carried on the
// approval response so the proposer can converge stragglers without
// re-fetching the org chart.
type ReorgFailure struct {
	Move   ReorgMove `yaml:"move" json:"move"`
	Reason string    `yaml:"reason" json:"reason"`
}

// Reorg is the embedded proposal on a `ceo_approval_request` of type
// "reorg". Unlike Hire / RoleUpdate / HandbookUpdate, application
// is best-effort per move: each move is applied independently and the
// approval response carries Applied + Failed lists so the Chief of
// Staff can detect stragglers and converge.
//
// Field usage by message direction:
//
//   - On the approval request: Moves is the proposal; Applied/Failed
//     are empty.
//   - On the approval response: Moves is dropped (it lived on the
//     request file the response replies to); Applied + Failed describe
//     what landed.
type Reorg struct {
	Moves   []ReorgMove    `yaml:"moves,omitempty" json:"moves,omitempty"`
	Applied []ReorgMove    `yaml:"applied,omitempty" json:"applied,omitempty"`
	Failed  []ReorgFailure `yaml:"failed,omitempty" json:"failed,omitempty"`
}

// Offboard is the embedded proposal on a `ceo_approval_request` of
// type "offboard" — letting one agent go. Same atomicity rule as Hire
// / RoleUpdate / HandbookUpdate: on approve the server archives
// the agent BEFORE writing the approval response, so the ack landing
// in the proposer's chat is itself the signal that the departure is
// done.
//
// Deliberately one slug, not a list. A departure is higher-consequence
// than a reporting-line change, so each one earns its own approval
// card and its own record on disk; a team shutdown is N approvals, and
// that friction is the point. Contrast Reorg, which batches because
// moving five people is one decision.
//
// Archiving is not deletion: the agent's chat history, role, memory,
// and messages are preserved under agents/_archived/<slug>, and the
// per-agent PVC is retained so the scratch filesystem survives for
// forensics.
type Offboard struct {
	Slug string `yaml:"slug" json:"slug"`
	// Reason is the proposer's short justification, kept on the
	// request for the forensic record. The CEO-facing prose lives in
	// the message body; this is the one-liner that stays attached to
	// the structured proposal.
	Reason string `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// Message is one inter-agent message, persisted as markdown with a
// YAML frontmatter header. The body is free-form markdown.
// InReplyTo, Approved, Hire are CEO-flow fields.
type Message struct {
	Type  MessageType `yaml:"type"`
	Title string      `yaml:"title"`
	From  string      `yaml:"from"`
	// To is the recipient list. Exactly one slug for every type but
	// `notice`, which may address several agents at once. See the
	// Recipients type for the scalar/sequence encoding.
	To   Recipients `yaml:"to"`
	Date time.Time  `yaml:"date"`
	// InReplyTo is the relative path of the message this one is
	// responding to (ceo_approval_response → ceo_approval_request,
	// ceo_notification_ack → ceo_notification). Structured so the
	// inbox can pair a reply with its request without parsing bodies.
	InReplyTo string `yaml:"in_reply_to,omitempty"`
	// Quiet is the producer's declaration that this message asks for
	// no turn of its own: deliver it at once, fold it into a turn the
	// recipient is already running, but never wake them for it alone;
	// an idle recipient reads it with whatever wakes them next. It
	// changes nothing about routing: an agent's quiet message still
	// waits in the inbox for release. The tracker sets it on a hold
	// (an idle agent has nothing to stop); any type may set it.
	Quiet bool `yaml:"quiet,omitempty"`
	// Approved is non-nil only on ceo_approval_response — true =
	// approved, false = denied. The inbox UI sets it; agents read it
	// to decide whether to proceed.
	Approved *bool `yaml:"approved,omitempty"`
	// Hire is non-nil only on ceo_approval_request messages that
	// propose instantiating a new agent. The server provisions the
	// agent on approve before writing the approval response.
	Hire *Hire `yaml:"hire,omitempty"`
	// RoleUpdate is non-nil only on ceo_approval_request messages that
	// propose overwriting an existing agent's role.md. The server
	// applies the update on approve before writing the response.
	RoleUpdate *RoleUpdate `yaml:"role_update,omitempty"`
	// HandbookUpdate is non-nil only on ceo_approval_request
	// messages that propose overwriting the org-wide handbook.
	// The server applies the update on approve before writing the
	// response, mirroring RoleUpdate.
	HandbookUpdate *HandbookUpdate `yaml:"handbook_update,omitempty"`
	// Reorg is non-nil on ceo_approval_request messages that propose
	// moving one or more agents to a new manager, and on the
	// corresponding ceo_approval_response (carrying Applied + Failed
	// instead of Moves). Application is best-effort per move; the
	// proposer reads Failed to converge stragglers.
	Reorg *Reorg `yaml:"reorg,omitempty"`
	// Offboard is non-nil only on ceo_approval_request messages that
	// propose archiving an existing agent. The server archives the
	// agent on approve before writing the approval response. It is
	// the ONLY path that archives an agent — there is no CEO-side
	// button; see internal/web/ceo.go.
	Offboard *Offboard `yaml:"offboard,omitempty"`
	// Assignment is non-nil only on an assignment event: the
	// assignment and log entry the wake reports. The inbox links the
	// card to the assignment with it.
	Assignment  *AssignmentRef      `yaml:"assignment,omitempty"`
	Attachments []MessageAttachment `yaml:"attachments,omitempty"`
	Body        string              `yaml:"-"`
	Path        string              `yaml:"-"`
}

// MessageDateBucket returns the UTC date bucket name for a message
// timestamp — the directory under messages/ where a message published
// at t lives. Always UTC: a message published at 2026-04-27T23:30:00Z
// buckets to "2026-04-27" regardless of the writer's local timezone.
// Single source of truth so every write and read path agrees on the
// layout.
func MessageDateBucket(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// MessagePath returns the absolute path WriteMessage would write d
// to, derived deterministically from d.Type, d.Date, d.From, d.To,
// and d.Title via messageFilename. Date must be non-zero — callers
// that stage a message in two phases (parse → write) pin Date at
// stage time so this path is stable across retries.
//
// Used by the two-phase publish flow's commit phase to re-derive the
// body's "saved to <path>" suffix on an idempotent replay without
// re-invoking WriteMessage.
func (s *FSStore) MessagePath(d Message) string {
	return filepath.Join(s.path("messages", MessageDateBucket(d.Date)), messageFilename(d))
}

// WriteMessage persists the message under messages/<YYYY-MM-DD>/ where
// the date bucket is the UTC date of d.Date. Returns the absolute path
// on disk.
func (s *FSStore) WriteMessage(d Message) (string, error) {
	if d.Type == "" || d.Title == "" || d.From == "" || len(d.To) == 0 {
		return "", errors.New("message: type, title, from, and to are required")
	}
	for _, to := range d.To {
		if to == "" {
			return "", errors.New("message: to must not contain an empty recipient")
		}
	}
	if d.Date.IsZero() {
		d.Date = time.Now().UTC()
	}
	dir := s.path("messages", MessageDateBucket(d.Date))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, messageFilename(d))
	header, err := yaml.Marshal(struct {
		Type           MessageType         `yaml:"type"`
		Title          string              `yaml:"title"`
		From           string              `yaml:"from"`
		To             Recipients          `yaml:"to"`
		Date           time.Time           `yaml:"date"`
		InReplyTo      string              `yaml:"in_reply_to,omitempty"`
		Quiet          bool                `yaml:"quiet,omitempty"`
		Approved       *bool               `yaml:"approved,omitempty"`
		Hire           *Hire               `yaml:"hire,omitempty"`
		RoleUpdate     *RoleUpdate         `yaml:"role_update,omitempty"`
		HandbookUpdate *HandbookUpdate     `yaml:"handbook_update,omitempty"`
		Reorg          *Reorg              `yaml:"reorg,omitempty"`
		Offboard       *Offboard           `yaml:"offboard,omitempty"`
		Assignment     *AssignmentRef      `yaml:"assignment,omitempty"`
		Attachments    []MessageAttachment `yaml:"attachments,omitempty"`
	}{d.Type, d.Title, d.From, d.To, d.Date, d.InReplyTo, d.Quiet, d.Approved, d.Hire, d.RoleUpdate, d.HandbookUpdate, d.Reorg, d.Offboard, d.Assignment, d.Attachments})
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	buf.WriteString("---\n")
	buf.Write(header)
	buf.WriteString("---\n\n")
	buf.WriteString(d.Body)
	if !strings.HasSuffix(d.Body, "\n") {
		buf.WriteByte('\n')
	}
	if err := writeAtomic(path, []byte(buf.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ReadMessage reads the message at the given absolute path.
func (s *FSStore) ReadMessage(path string) (Message, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	return parseMessage(path, b)
}

var frontmatterRE = regexp.MustCompile(`(?s)\A---\n(.*?)\n---\n?(.*)\z`)

// ParseMessage parses a message file's bytes the way ReadMessage does.
// path is only used in errors and copied to Message.Path.
func ParseMessage(path string, raw []byte) (Message, error) { return parseMessage(path, raw) }

func parseMessage(path string, raw []byte) (Message, error) {
	m := frontmatterRE.FindSubmatch(raw)
	if m == nil {
		return Message{}, fmt.Errorf("message %s: no frontmatter", path)
	}
	var meta struct {
		Type           MessageType         `yaml:"type"`
		Title          string              `yaml:"title"`
		From           string              `yaml:"from"`
		To             Recipients          `yaml:"to"`
		Date           time.Time           `yaml:"date"`
		InReplyTo      string              `yaml:"in_reply_to"`
		Quiet          bool                `yaml:"quiet"`
		Approved       *bool               `yaml:"approved"`
		Hire           *Hire               `yaml:"hire"`
		RoleUpdate     *RoleUpdate         `yaml:"role_update"`
		HandbookUpdate *HandbookUpdate     `yaml:"handbook_update"`
		Reorg          *Reorg              `yaml:"reorg"`
		Offboard       *Offboard           `yaml:"offboard"`
		Assignment     *AssignmentRef      `yaml:"assignment"`
		Attachments    []MessageAttachment `yaml:"attachments"`
	}
	if err := yaml.Unmarshal(m[1], &meta); err != nil {
		return Message{}, fmt.Errorf("message %s: frontmatter: %w", path, err)
	}
	body := strings.TrimLeft(string(m[2]), "\n")
	return Message{
		Type:           meta.Type,
		Title:          meta.Title,
		From:           meta.From,
		To:             meta.To,
		Date:           meta.Date,
		InReplyTo:      meta.InReplyTo,
		Quiet:          meta.Quiet,
		Approved:       meta.Approved,
		Hire:           meta.Hire,
		RoleUpdate:     meta.RoleUpdate,
		HandbookUpdate: meta.HandbookUpdate,
		Reorg:          meta.Reorg,
		Offboard:       meta.Offboard,
		Assignment:     meta.Assignment,
		Attachments:    meta.Attachments,
		Body:           body,
		Path:           path,
	}, nil
}

// MessageFilter narrows a ListMessages query. Zero-value fields are unfiltered.
type MessageFilter struct {
	From string
	To   string
	Type MessageType
}

// ListMessages returns messages matching the filter, sorted by path
// (which sorts first by release, then by timestamp embedded in the filename).
func (s *FSStore) ListMessages(f MessageFilter) ([]Message, error) {
	root := s.path("messages")
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() {
			// Skip archive / reset-backup subdirs (dot-prefix). Otherwise
			// stale messages from prior test cycles show up as "pending
			// work" and confuse the model. Only the root itself can have
			// an empty base; the dot rule applies below the root.
			if p != root && strings.HasPrefix(filepath.Base(p), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".md") {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []Message
	for _, p := range paths {
		message, err := s.ReadMessage(p)
		if err != nil {
			return nil, err
		}
		if f.From != "" && message.From != f.From {
			continue
		}
		if f.To != "" && !message.To.Contains(f.To) {
			continue
		}
		if f.Type != "" && message.Type != f.Type {
			continue
		}
		out = append(out, message)
	}
	return out, nil
}

// MoveToRedacted relocates a message from messages/<bucket>/foo.md to
// messages/.redacted/<bucket>/foo.md and returns the new absolute path.
// The dot-prefix subdir is the same convention archive/reset-backup
// scopes use, so ListMessages and CountMessages skip it for free.
//
// Used by a CEO bounce and by the tracker when it pulls a queued wake
// the assignment has outgrown: the contract is "leave the file on disk for
// audit, but pull it out of any view that might rediscover it as
// live." Caller is expected to have already dropped the recipient's
// inbox pointer to this message; MoveToRedacted only handles the file
// move.
func (s *FSStore) MoveToRedacted(absPath string) (string, error) {
	messagesRoot := s.path("messages")
	rel, err := filepath.Rel(messagesRoot, absPath)
	if err != nil {
		return "", fmt.Errorf("MoveToRedacted: %w", err)
	}
	if rel == "." || rel == "" || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("MoveToRedacted: %q is not under messages/", absPath)
	}
	newPath := filepath.Join(messagesRoot, ".redacted", rel)
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return "", fmt.Errorf("MoveToRedacted: mkdir: %w", err)
	}
	if err := os.Rename(absPath, newPath); err != nil {
		return "", fmt.Errorf("MoveToRedacted: rename: %w", err)
	}
	return newPath, nil
}

var slugSanitizeRE = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func messageFilename(d Message) string {
	ts := d.Date.UTC().Format("20060102T150405.000000000Z")
	safe := func(s string) string {
		s = slugSanitizeRE.ReplaceAllString(s, "-")
		s = strings.Trim(s, "-")
		if s == "" {
			s = "x"
		}
		return s
	}
	return fmt.Sprintf("%s-%s-%s--to--%s.md", ts, d.Type, safe(d.From), safe(recipientsForFilename(d.To)))
}

// recipientFilenameCap bounds the "--to--" segment of a message
// filename. A notice can name every agent in the org; joining a dozen
// slugs would push the filename past what some filesystems accept and
// make the path unreadable in a log line. Past the cap we keep the
// first recipient and summarize the rest by count, which stays
// deterministic (Recipients order is fixed at parse time) so
// MessagePath still re-derives the same path on an idempotent commit.
const recipientFilenameCap = 60

func recipientsForFilename(to Recipients) string {
	joined := strings.Join(to, "_")
	if len(joined) <= recipientFilenameCap || len(to) < 2 {
		return joined
	}
	return fmt.Sprintf("%s_and_%d_more", to[0], len(to)-1)
}
