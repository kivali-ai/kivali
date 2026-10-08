package apitypes

// ---- agent chat: GET /api/v1/agents/{slug}/chat and its actions ----
//
// Every timestamp in this file is unix milliseconds, the same unit the
// agent stream's events carry, so a row loaded here and the event that
// announced it can be matched on ts.

// TranscriptKind discriminates TranscriptRow.
type TranscriptKind string

const (
	// TranscriptKindMessage is a chat bubble: you writing to the agent, or
	// the agent writing back.
	TranscriptKindMessage TranscriptKind = "message"
	// TranscriptKindDelivery is a message another agent (or the tracker)
	// sent, delivered into this chat. It renders as a card from its
	// sender.
	TranscriptKindDelivery TranscriptKind = "delivery"
	// TranscriptKindCEOQueue is something the agent sent you: an approval
	// request or a notification, answered from Home.
	TranscriptKindCEOQueue TranscriptKind = "ceo_queue"
	// TranscriptKindToolUse is one tool call with its result folded in.
	TranscriptKindToolUse TranscriptKind = "tool_use"
	// TranscriptKindDocPublished is a message the agent published to
	// another agent.
	TranscriptKindDocPublished TranscriptKind = "doc_published"
	// TranscriptKindFileShared is files the agent shared into the chat.
	TranscriptKindFileShared TranscriptKind = "file_shared"
	// TranscriptKindSubagents is one batch of background tasks.
	TranscriptKindSubagents TranscriptKind = "subagents"
	// TranscriptKindTaskResult is a background task's report, delivered
	// to the agent when the task ended. It renders where it landed, as
	// the task it reports on.
	TranscriptKindTaskResult TranscriptKind = "task_result"
	// TranscriptKindMarker is a turn boundary the runtime recorded.
	TranscriptKindMarker TranscriptKind = "marker"
	// TranscriptKindWakeUpdate is the runtime's note at a wake. The model
	// reads it; the UI hides it by default.
	TranscriptKindWakeUpdate TranscriptKind = "wake_update"
	// TranscriptKindRotationPrompt is the request to fold the chat into
	// memory before a new chat starts.
	TranscriptKindRotationPrompt TranscriptKind = "rotation_prompt"
)

// MessageRole is which side of the conversation a message is on, seen
// from the person using the app: sent is yours, received is an
// agent's.
type MessageRole string

const (
	MessageRoleSent     MessageRole = "sent"
	MessageRoleReceived MessageRole = "received"
)

// PartyKind says whether a ChatParty is a person or an agent.
type PartyKind string

const (
	PartyKindPerson PartyKind = "person"
	PartyKindAgent  PartyKind = "agent"
)

// ChatParty is who wrote a row. You are {person, "ceo", "You"}; an
// agent is named as the rest of the app names it; a background task
// is an agent whose slug is its task id.
type ChatParty struct {
	Kind PartyKind `json:"kind"`
	Slug string    `json:"slug"`
	Name string    `json:"name"`
}

// ToolStatus is a tool call's state.
type ToolStatus string

const (
	ToolStatusRunning ToolStatus = "running"
	ToolStatusDone    ToolStatus = "done"
	ToolStatusError   ToolStatus = "error"
)

// SubagentTaskState is a background task's state.
type SubagentTaskState string

const (
	SubagentTaskStateRunning SubagentTaskState = "running"
	SubagentTaskStateDone    SubagentTaskState = "done"
	SubagentTaskStateErrored SubagentTaskState = "errored"
)

// MarkerKind is the boundary a marker row records. The four stored
// kinds keep their on-disk spelling. rotation is never stored: the
// stream sends it, as a chat_marker, when the turn that folds the chat
// into memory starts, so an open page shows that a new chat was asked
// for. Its ts is the rotation_prompt row's, which is what the chat
// API's rows carry for the same moment.
type MarkerKind string

const (
	MarkerKindUserInterruption  MarkerKind = "user-interruption"
	MarkerKindRuntimeDisruption MarkerKind = "runtime-disruption"
	MarkerKindTurnError         MarkerKind = "turn-error"
	MarkerKindPausedToDeliver   MarkerKind = "paused-to-deliver"
	MarkerKindRotation          MarkerKind = "rotation"
)

// SubagentTask is one task in a subagents row.
type SubagentTask struct {
	// Index is the task's position in the batch the agent dispatched.
	Index int `json:"index"`
	// ID is empty until the dispatch receipt names it.
	ID    string `json:"id"`
	Title string `json:"title"`
	// Model is the friendly name ("Opus 5.5"); Effort the level.
	Model  string            `json:"model"`
	Effort string            `json:"effort"`
	State  SubagentTaskState `json:"state"`
	Error  *string           `json:"error,omitempty"`
	// OutputMD is the task's final answer, once it has one.
	OutputMD *string `json:"output_md,omitempty"`
	// URL is the task transcript's JSON, present once ID is known.
	URL *string `json:"url,omitempty"`
}

// TranscriptRow is one row of a transcript. Kind says which fields are
// set; a field not listed for a kind is absent.
//
//   - message: role, from, body_md, attachments, pending, and for an
//     agent's reply model and effort; quiet on a message that woke no
//     one; is_error on a reply that is the error text of a failed model
//     call; source_kind when the row is not a plain chat message
//     (ceo_reply, …).
//   - delivery: from, title, delivered_to, also_to, body_md,
//     attachments, kind_badge, message_type, assignment_ref, replies_to,
//     in_reply_to, raw_url. When the message file can no longer be
//     read, body_md is the text the model saw, kind_badge is
//     "delivery", and from, replies_to and message_type are absent.
//   - ceo_queue: from, title, body_md, attachments, message_type, path,
//     raw_url, resolved.
//   - tool_use: tool_use_id, name, input, output, status, is_error,
//     started_ts, ended_ts, input_truncated, output_truncated. A call
//     without a result is running while the turn it belongs to may
//     still be going; the stream's tool_result, matched on tool_use_id,
//     finishes it. Input is the call's JSON as the model wrote it,
//     output the result text. The current chat (GET .../chat) cuts
//     each long string in the input, and a long output, ending the cut
//     with "…" and setting input_truncated or output_truncated;
//     GET .../tool-calls/{tool_use_id} answers the call in full. Past
//     chats and task transcripts send both in full.
//   - doc_published: tool_use_id, title, message_type, to, body_md,
//     attachments, assignment_ref (when the message names one),
//     raw_url (the stored markdown file, not a page). When the message
//     file can no longer be read, title is the one the agent gave and
//     body_md, message_type and assignment_ref are absent.
//   - file_shared: from, body_md, attachments.
//   - subagents: tool_use_id, tasks.
//   - task_result: tasks, holding the one task that reported, as it
//     ended. When the report names no task, tasks is empty and body_md
//     is the report's text.
//   - marker: marker_kind, text, body_md (the detail).
//   - wake_update, rotation_prompt: body_md.
type TranscriptRow struct {
	Kind TranscriptKind `json:"kind"`
	TS   int64          `json:"ts"`

	Role        *MessageRole     `json:"role,omitempty"`
	From        *ChatParty       `json:"from,omitempty"`
	BodyMD      *string          `json:"body_md,omitempty"`
	Attachments *[]AttachmentRef `json:"attachments,omitempty"`
	Model       *string          `json:"model,omitempty"`
	Effort      *string          `json:"effort,omitempty"`
	Pending     *bool            `json:"pending,omitempty"`
	Quiet       bool             `json:"quiet,omitempty"`
	IsError     bool             `json:"is_error,omitempty"`
	SourceKind  *string          `json:"source_kind,omitempty"`

	Title         *string        `json:"title,omitempty"`
	DeliveredTo   *PersonRef     `json:"delivered_to,omitempty"`
	AlsoTo        *[]PersonRef   `json:"also_to,omitempty"`
	KindBadge     *string        `json:"kind_badge,omitempty"`
	MessageType   *string        `json:"message_type,omitempty"`
	AssignmentRef *AssignmentRef `json:"assignment_ref,omitempty"`
	RepliesTo     *PersonRef     `json:"replies_to,omitempty"`
	InReplyTo     *string        `json:"in_reply_to,omitempty"`
	RawURL        *string        `json:"raw_url,omitempty"`

	Path     *string      `json:"path,omitempty"`
	Resolved *bool        `json:"resolved,omitempty"`
	To       *[]PersonRef `json:"to,omitempty"`

	ToolUseID *string     `json:"tool_use_id,omitempty"`
	Name      *string     `json:"name,omitempty"`
	Input     *string     `json:"input,omitempty"`
	Output    *string     `json:"output,omitempty"`
	Status    *ToolStatus `json:"status,omitempty"`
	StartedTS *int64      `json:"started_ts,omitempty"`
	EndedTS   *int64      `json:"ended_ts,omitempty"`

	InputTruncated  bool `json:"input_truncated,omitempty"`
	OutputTruncated bool `json:"output_truncated,omitempty"`

	Tasks *[]SubagentTask `json:"tasks,omitempty"`

	MarkerKind *MarkerKind `json:"marker_kind,omitempty"`
	Text       *string     `json:"text,omitempty"`
}

// ChatMessageEvent is the payload of the `chat_message` stream event:
// a message the agent received, now in the transcript. Kind is the
// row's kind on disk: direct_chat is a message you sent, painted from
// Content and Attachments; subagent_result is a background task's
// report, painted from Row, the task_result row the chat API serves for
// it. Content is the text the model read.
type ChatMessageEvent struct {
	Role        MessageRole         `json:"role"`
	Kind        string              `json:"kind"`
	Content     string              `json:"content"`
	TS          int64               `json:"ts"`
	Attachments []PendingAttachment `json:"attachments"`
	Row         *TranscriptRow      `json:"row,omitempty"`
}

// ChatFill is how full the agent's context window is. LongThreshold is
// the token count past which a new chat is worth suggesting;
// ResolvedModel is the model the limit was sized for.
type ChatFill struct {
	Pct           int    `json:"pct"`
	Tokens        int    `json:"tokens"`
	Limit         int    `json:"limit"`
	Bucket        string `json:"bucket"`
	LongThreshold int    `json:"long_threshold"`
	ResolvedModel string `json:"resolved_model"`
}

// ToolCallDetail is GET /api/v1/agents/{slug}/tool-calls/{tool_use_id}: one
// call in the current chat with its input and output in full. Output
// is empty while the call has no result.
type ToolCallDetail struct {
	ToolUseID string `json:"tool_use_id"`
	Input     string `json:"input"`
	Output    string `json:"output"`
}

// Chat is GET /api/v1/agents/{slug}/chat.
type Chat struct {
	Rows []TranscriptRow `json:"rows"`
	// Pending is what you sent while the agent was mid-turn, not yet in
	// Rows. The stream narrates what happens to each.
	Pending       []PendingMessage `json:"pending"`
	Fill          ChatFill         `json:"fill"`
	Models        []ModelOption    `json:"models"`
	CurrentModel  string           `json:"current_model"`
	CurrentEffort string           `json:"current_effort"`
	// Running is true while a turn streams or background tasks run;
	// WaitingTasks counts the tasks.
	Running      bool `json:"running"`
	WaitingTasks int  `json:"waiting_tasks"`
	Archived     bool `json:"archived"`
	// Rotating is true while a new chat is on its way: the agent is
	// folding this chat into memory, after which it is archived. The
	// stream's rotation_done says when that has happened.
	Rotating bool `json:"rotating"`
	// Generation names this chat among the agent's chats: the archive
	// id of the chat before it, empty for the agent's first. It changes
	// when a new chat starts, and only then, so rows a page holds under
	// another generation belong to a chat that is now a past chat.
	Generation string `json:"generation"`
}

// ModelRequest is POST /api/v1/agents/{slug}/model. "default" clears
// the agent's own pin.
type ModelRequest struct {
	Model string `json:"model"`
}

// EffortRequest is POST /api/v1/agents/{slug}/effort. "default" clears
// the agent's own level.
type EffortRequest struct {
	Effort string `json:"effort"`
}

// ChatSettings answers the model and effort posts: what the agent runs
// on from its next turn.
type ChatSettings struct {
	CurrentModel  string `json:"current_model"`
	CurrentEffort string `json:"current_effort"`
}

// StopResponse is POST /api/v1/agents/{slug}/stop. Stopped is false
// when nothing was running.
type StopResponse struct {
	Stopped bool `json:"stopped"`
}

// NewChatResponse is POST /api/v1/agents/{slug}/new-chat. Started is
// false when the chat was empty and there was nothing to fold.
type NewChatResponse struct {
	Started bool `json:"started"`
}

// SubagentMeta is a background task's header.
type SubagentMeta struct {
	ID          string            `json:"id"`
	Parent      string            `json:"parent"`
	State       SubagentTaskState `json:"state"`
	Model       string            `json:"model"`
	Effort      string            `json:"effort"`
	Description string            `json:"description"`
	// Started is 0 when nothing recorded it.
	Started int64   `json:"started"`
	Ended   *int64  `json:"ended,omitempty"`
	Error   *string `json:"error,omitempty"`
	// Step is the plan step the task works on. Nothing links a task to
	// a step yet, so it is always absent.
	Step *string `json:"step,omitempty"`
	// Activity is what a running task is doing now ("running Bash").
	Activity *string `json:"activity,omitempty"`
}

// SubagentTranscript is GET /api/v1/agents/{slug}/subagents/{id}.
type SubagentTranscript struct {
	Meta SubagentMeta    `json:"meta"`
	Rows []TranscriptRow `json:"rows"`
}

// ChatRange is the first and last timestamp in a chat; both 0 for an
// empty chat.
type ChatRange struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// PastChat is one row of GET /api/v1/agents/{slug}/chats. TS is the
// archive id, used in the detail URL. Title is the episode's title, or
// the chat's opening line while the episode is being written. Tokens
// is estimated from the transcript's length.
type PastChat struct {
	TS       string    `json:"ts"`
	Title    string    `json:"title"`
	Range    ChatRange `json:"range"`
	Messages int       `json:"messages"`
	Tokens   int       `json:"tokens"`
}

// PastChats is GET /api/v1/agents/{slug}/chats, newest first.
type PastChats struct {
	Chats []PastChat `json:"chats"`
}

// HabitsDiff is the agent's habits (operating principles) when the
// new chat was asked for and after it had folded the chat in.
type HabitsDiff struct {
	Before string `json:"before"`
	After  string `json:"after"`
}

// PastChatSummary is what a rotation left behind. DigestMD is empty
// until the episode is written. MemoryAdded is the lines of memory
// present after the rotation and not before it.
type PastChatSummary struct {
	PastChat    `tstype:",extends,required"`
	DigestMD    string     `json:"digest_md"`
	MemoryAdded []string   `json:"memory_added"`
	HabitsDiff  HabitsDiff `json:"habits_diff"`
}

// PastChatDetail is GET /api/v1/agents/{slug}/chats/{ts}. PrevTS is the
// chat before this one, NextTS the one after.
type PastChatDetail struct {
	Summary PastChatSummary `json:"summary"`
	Rows    []TranscriptRow `json:"rows"`
	PrevTS  *string         `json:"prev_ts,omitempty"`
	NextTS  *string         `json:"next_ts,omitempty"`
}
