package apitypes

// ---- pending messages (agent stream events and /api/v1/agents/{slug}/pending) ----
//
// A pending message is one the CEO sent while the agent was mid-turn:
// the server holds it until the turn takes it (a fold at the next tool
// round) or ends. It is not in the transcript yet, because the model
// does not have it yet. The agent stream narrates its life:
//
//	pending_message    staged (or restored after a delete)
//	pending_offered    handed to the running turn; Delete no longer works
//	pending_delivered  the model has it; ALWAYS before the chat_message
//	                   for the same row, when one follows
//	pending_deleted    removed by the CEO, or dropped by an offboard
//
// Every timestamp is unix milliseconds.

// PendingAttachment is a file a pending message carries. The bytes are
// already stored; restoring a deleted message never re-uploads.
type PendingAttachment struct {
	SHA  string `json:"sha"`
	Name string `json:"name"`
}

// PendingMessage is the payload of the `pending_message` stream event,
// one entry of GET …/chat's `pending`, and the body of a restore.
type PendingMessage struct {
	// ID is stable for the message's whole pending life; every later
	// event and action names it.
	ID   string `json:"id"`
	Text string `json:"text"`
	// QueuedAt is when the server received the message. It is also the
	// ts the delivered row carries in the transcript.
	QueuedAt    int64               `json:"queued_at"`
	Attachments []PendingAttachment `json:"attachments"`
	// Deletable is false once the message has been handed to the
	// running turn: from then on only the agent can consume it.
	Deletable bool `json:"deletable"`
}

// PendingRef is the payload of `pending_offered` and `pending_deleted`.
type PendingRef struct {
	ID string `json:"id"`
}

// PendingDelivered is the payload of `pending_delivered`: the message
// is in the transcript now, keyed by TS (the same data-ts the
// chat_message that may follow carries).
type PendingDelivered struct {
	ID string `json:"id"`
	TS int64  `json:"ts"`
}

// ChatMarker is the payload of the `chat_marker` stream event: a
// system row in the transcript. Kind is the row's kind on disk
// ("user-interruption", "runtime-disruption", "turn-error",
// "paused-to-deliver"), or "rotation" for a new chat that was asked
// for: that one has no row of its own kind, TS is its rotation_prompt
// row's and Content is empty.
type ChatMarker struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
	TS      int64  `json:"ts"`
}

// ChatMarkerPausedToDeliver is the kind of the marker a Send now
// leaves: the turn output above it, the delivered messages below it.
const ChatMarkerPausedToDeliver = "paused-to-deliver"

// PendingRestoreResponse is POST …/pending/{id}/restore: the message as
// `pending_message` describes it. When the agent was idle by the time
// of the restore the message went straight into the transcript instead
// of back into the queue; DeliveredTS is then set, and no
// pending_message event is sent.
type PendingRestoreResponse struct {
	PendingMessage `tstype:",extends,required"`
	DeliveredTS    *int64 `json:"delivered_ts,omitempty"`
}

// MessagePostRequest is the JSON body of POST /agents/{slug}/messages
// (and the /api/v1 alias) for a message without attachments; one with
// attachments is sent as multipart/form-data (text, attachment).
type MessagePostRequest struct {
	Text string `json:"text"`
}

// MessagePostResponse is POST /agents/{slug}/messages (and the
// /api/v1 alias). PendingID is set when the agent was mid-turn and the
// message is waiting rather than in the transcript; the client paints
// a pending bubble keyed on it. Attachments is absent when the message
// carried none.
type MessagePostResponse struct {
	TS          int64               `json:"ts"`
	Attachments []PendingAttachment `json:"attachments,omitempty"`
	PendingID   string              `json:"pending_id,omitempty"`
}
