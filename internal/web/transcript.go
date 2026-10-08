package web

import (
	"cmp"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The transcript as JSON: chat.jsonl rows mapped to the rows the web
// app renders. One mapper for the live chat, an archived chat and a
// background task's transcript, carrying what the chat shows for each
// kind:
//
//   - tool_result rows fold into their tool_use (by tool_use_id); a
//     result whose call is not in the transcript stands alone.
//   - a subagent tool_use becomes a subagents row, its tasks read off
//     disk; a subagent_result is a task_result row where it landed, and
//     its outcome folds into the task it reports on.
//   - doc_published finishes its publish_* call, and becomes a
//     ceo_queue row when what was published was addressed to you.
//   - inbox_delivery becomes a delivery card from its sender.

// transcriptRows maps slug's chat rows. Never nil.
func (s *Server) transcriptRows(slug string, msgs []store.ChatMessage) []apitypes.TranscriptRow {
	b := s.newTranscriptBuilder(slug)
	return b.rows(msgs)
}

// currentChatRows is transcriptRows for the current chat, which the
// page refetches whole after every turn: each tool call's input and
// output are capped (capToolRow).
func (s *Server) currentChatRows(slug string, msgs []store.ChatMessage) []apitypes.TranscriptRow {
	b := s.newTranscriptBuilder(slug)
	b.capTools = true
	return b.rows(msgs)
}

// subagentTranscriptRows maps a background task's transcript: its own
// replies are from the task, and what it received came from the agent
// that dispatched it.
func (s *Server) subagentTranscriptRows(parent, id, description string, msgs []store.ChatMessage) []apitypes.TranscriptRow {
	b := s.newTranscriptBuilder(parent)
	b.self = apitypes.ChatParty{Kind: apitypes.PartyKindAgent, Slug: id, Name: cmp.Or(description, "Background task")}
	b.receivedFrom = b.agentParty(parent)
	return b.rows(msgs)
}

// transcriptBuilder resolves what rows name, reading each thing once.
type transcriptBuilder struct {
	s    *Server
	slug string
	// self writes the RoleSent rows; receivedFrom the received chat
	// messages.
	self         apitypes.ChatParty
	receivedFrom apitypes.ChatParty
	view         *homeView

	ceoReplied     map[string]bool
	ceoRepliedRead bool

	// capTools cuts tool calls' input and output (capToolRow).
	capTools bool
}

func (s *Server) newTranscriptBuilder(slug string) *transcriptBuilder {
	b := &transcriptBuilder{s: s, slug: slug, view: s.newHomeView()}
	b.self = b.agentParty(slug)
	b.receivedFrom = apitypes.ChatParty{Kind: apitypes.PartyKindPerson, Slug: agent.CEOSlug, Name: "You"}
	return b
}

// agentParty names slug as the rest of the app does; the CEO is a
// person.
func (b *transcriptBuilder) agentParty(slug string) apitypes.ChatParty {
	p := b.view.person(slug)
	kind := apitypes.PartyKindAgent
	if slug == agent.CEOSlug {
		kind = apitypes.PartyKindPerson
	}
	return apitypes.ChatParty{Kind: kind, Slug: p.Slug, Name: p.Name}
}

func (b *transcriptBuilder) rows(msgs []store.ChatMessage) []apitypes.TranscriptRow {
	results := toolResultsByID(msgs)
	calls := map[string]bool{}
	published := map[string]store.ChatMessage{}
	for _, m := range msgs {
		switch {
		case m.Kind == "tool_use" && m.ToolUseID != "":
			calls[m.ToolUseID] = true
		case m.Kind == "doc_published" && m.ToolUseID != "":
			published[m.ToolUseID] = m
		}
	}
	inbox := b.s.loadInboxViews(msgs)

	out := make([]apitypes.TranscriptRow, 0, len(msgs))
	// tasks locates each background task already in out, by id, so a
	// subagent_result's outcome can fold into it.
	tasks := map[string]taskSlot{}
	for i, m := range msgs {
		switch m.Kind {
		case "tool_use":
			res, hasRes := results[m.ToolUseID]
			if isSubagentToolName(m.ToolName) {
				row := b.subagentsRow(m, res.Content)
				for j, t := range *row.Tasks {
					if t.ID != "" {
						tasks[t.ID] = taskSlot{row: len(out), task: j}
					}
				}
				out = append(out, row)
				continue
			}
			out = append(out, b.capToolRow(toolUseRow(m, res, hasRes, published, msgs[i+1:])))
		case "tool_result":
			if calls[m.ToolUseID] {
				continue
			}
			out = append(out, b.capToolRow(orphanToolResultRow(m)))
		case "doc_published":
			out = append(out, b.publishedRow(m))
		case store.KindSubagentResult:
			out = append(out, b.taskResult(m, out, tasks))
		case "inbox_delivery":
			out = append(out, b.deliveryRow(m, inbox))
		case "file_shared":
			out = append(out, apitypes.TranscriptRow{
				Kind:        apitypes.TranscriptKindFileShared,
				TS:          unixMS(m.TS),
				From:        ptr(b.self),
				BodyMD:      ptr(m.Content),
				Attachments: ptr(b.view.attachments(m.Attachments)),
			})
		case store.KindUserInterruption, store.KindRuntimeDisruption, store.KindTurnError, store.KindPausedToDeliver:
			out = append(out, markerRow(m)) // isMarkerKind
		case store.KindWakeUpdate:
			out = append(out, bodyRow(apitypes.TranscriptKindWakeUpdate, m))
		case "rotation_prompt":
			out = append(out, bodyRow(apitypes.TranscriptKindRotationPrompt, m))
		default:
			out = append(out, b.messageRow(m))
		}
	}
	return out
}

// taskSlot is where a background task sits in the rows built so far.
type taskSlot struct{ row, task int }

// messageRow is a chat bubble: a direct chat, or any kind without a
// row of its own (named in source_kind).
func (b *transcriptBuilder) messageRow(m store.ChatMessage) apitypes.TranscriptRow {
	from := b.receivedFrom
	if m.Role == store.RoleSent {
		from = b.self
	}
	role := apitypes.MessageRoleReceived
	if from.Kind == apitypes.PartyKindPerson {
		role = apitypes.MessageRoleSent
	}
	row := apitypes.TranscriptRow{
		Kind:        apitypes.TranscriptKindMessage,
		TS:          unixMS(m.TS),
		Role:        &role,
		From:        &from,
		BodyMD:      ptr(m.Content),
		Attachments: ptr(b.view.attachments(m.Attachments)),
		Pending:     ptr(false),
		Quiet:       m.Quiet && m.Role == store.RoleReceived,
	}
	if m.Kind != "" && m.Kind != "direct_chat" {
		row.SourceKind = ptr(m.Kind)
	}
	// The model and effort badge: only on the agent's own reply, and
	// only when the turn recorded them.
	if m.Role == store.RoleSent && m.Kind == "direct_chat" && m.Model != "" {
		row.Model = ptr(b.s.modelLabel(m.Model))
		if m.Effort != "" {
			row.Effort = ptr(m.Effort)
		}
	}
	return row
}

// toolUseRow folds a call's result into it. A call with no result is
// running, unless the turn moved on past it: then a publish_* call's
// doc_published finished it, a marker cut it short, and the agent
// saying anything means it finished without a recorded result. Rows
// that land inside a turn (a message delivered mid-turn, a background
// task's report, the wake note) say nothing about the call: it may
// still be running, and the stream's tool_result finishes it by id.
func toolUseRow(m store.ChatMessage, res store.ChatMessage, hasRes bool, published map[string]store.ChatMessage, after []store.ChatMessage) apitypes.TranscriptRow {
	row := apitypes.TranscriptRow{
		Kind:      apitypes.TranscriptKindToolUse,
		TS:        unixMS(m.TS),
		ToolUseID: ptr(m.ToolUseID),
		Name:      ptr(m.ToolName),
		Input:     ptr(m.ToolInput),
		StartedTS: ptr(unixMS(m.TS)),
	}
	status := apitypes.ToolStatusRunning
	switch doc, isDoc := published[m.ToolUseID]; {
	case hasRes:
		row.Output = ptr(res.Content)
		row.EndedTS = ptr(unixMS(res.TS))
		row.IsError = res.IsError
		status = apitypes.ToolStatusDone
		if res.IsError {
			status = apitypes.ToolStatusError
		}
	case isDoc:
		row.EndedTS = ptr(unixMS(doc.TS))
		status = apitypes.ToolStatusDone
	default:
		for _, next := range after {
			if !endsTurn(next) {
				continue
			}
			status = apitypes.ToolStatusDone
			if isMarkerKind(next.Kind) {
				status = apitypes.ToolStatusError
			}
			break
		}
	}
	row.Status = &status
	return row
}

// capToolRow cuts a tool_use row's input and output to the cap when
// the builder caps, flagging what it cut.
func (b *transcriptBuilder) capToolRow(row apitypes.TranscriptRow) apitypes.TranscriptRow {
	if !b.capTools {
		return row
	}
	if row.Input != nil {
		in, cut := capToolInput(*row.Input)
		row.Input, row.InputTruncated = &in, cut
	}
	if row.Output != nil {
		out, cut := capToolOutput(*row.Output)
		row.Output, row.OutputTruncated = &out, cut
	}
	return row
}

// endsTurn reports whether m could only have been written once the
// turn before it was over: a marker, the rotation prompt (written
// while the agent is idle), or the agent's own words.
// Its tool rows, publish and share records, and every received row
// (a message folded into the running turn, a delivery, a task's
// report, the wake note) ride inside a turn.
func endsTurn(m store.ChatMessage) bool {
	if isMarkerKind(m.Kind) || m.Kind == "rotation_prompt" {
		return true
	}
	if m.Role != store.RoleSent {
		return false
	}
	switch m.Kind {
	case "tool_use", "doc_published", "file_shared":
		return false
	}
	return true
}

// isMarkerKind reports whether kind is one of the four stored markers,
// each of which ends the turn without the tool in flight finishing.
func isMarkerKind(kind string) bool {
	switch kind {
	case store.KindUserInterruption, store.KindRuntimeDisruption, store.KindTurnError, store.KindPausedToDeliver:
		return true
	}
	return false
}

// orphanToolResultRow is a result whose call is not in the transcript
// (a call made before the chat rotated, say).
func orphanToolResultRow(m store.ChatMessage) apitypes.TranscriptRow {
	status := apitypes.ToolStatusDone
	if m.IsError {
		status = apitypes.ToolStatusError
	}
	return apitypes.TranscriptRow{
		Kind:      apitypes.TranscriptKindToolUse,
		TS:        unixMS(m.TS),
		ToolUseID: ptr(m.ToolUseID),
		Name:      ptr(m.ToolName),
		Input:     ptr(""),
		Output:    ptr(m.Content),
		IsError:   m.IsError,
		Status:    &status,
		StartedTS: ptr(unixMS(m.TS)),
		EndedTS:   ptr(unixMS(m.TS)),
	}
}

// subagentsRow is one dispatched batch: the tasks the call asked for,
// each with the id its receipt named and the state its meta.json
// records.
func (b *transcriptBuilder) subagentsRow(m store.ChatMessage, receipt string) apitypes.TranscriptRow {
	views := b.s.subagentTaskViews(b.slug, m.ToolInput, receipt)
	tasks := make([]apitypes.SubagentTask, 0, len(views))
	for i, v := range views {
		tasks = append(tasks, subagentTask(b.s.Provider, b.slug, i, v))
	}
	return apitypes.TranscriptRow{
		Kind:      apitypes.TranscriptKindSubagents,
		TS:        unixMS(m.TS),
		ToolUseID: ptr(m.ToolUseID),
		Tasks:     &tasks,
	}
}

// subagentTask is one of parent's background tasks as the app shows it.
func subagentTask(p provider.Provider, parent string, index int, v SubagentTaskView) apitypes.SubagentTask {
	t := apitypes.SubagentTask{
		Index:  index,
		ID:     v.ID,
		Title:  v.Description,
		Model:  provider.Label(p, v.Model),
		Effort: v.Effort,
	}
	t.State, t.Error = subagentState(v.Status, v.Error)
	if v.FinalText != "" && t.State != apitypes.SubagentTaskStateRunning {
		t.OutputMD = ptr(v.FinalText)
	}
	if v.ID != "" {
		t.URL = ptr(subagentAPIURL(parent, v.ID))
	}
	return t
}

// subagentState maps a task's recorded status (meta.json's, or the
// chip's "running" before the receipt names an id) to the three the
// app shows. A cancelled task did not finish, so it reads as errored
// with the reason.
func subagentState(status, errText string) (apitypes.SubagentTaskState, *string) {
	switch status {
	case "completed":
		return apitypes.SubagentTaskStateDone, nil
	case "errored", subagentJobFailed:
		return apitypes.SubagentTaskStateErrored, ptr(cmp.Or(errText, "The task failed without saying why"))
	case subagentJobCancelled:
		return apitypes.SubagentTaskStateErrored, ptr(cmp.Or(errText, "Cancelled"))
	case "unknown":
		return apitypes.SubagentTaskStateErrored, ptr("This task left no record")
	default:
		return apitypes.SubagentTaskStateRunning, nil
	}
}

func subagentAPIURL(parent, id string) string {
	return "/api/v1/agents/" + url.PathEscape(parent) + "/subagents/" + url.PathEscape(id)
}

// subagentResultPattern reads the job id, description and outcome off
// the first line of a subagent_result (renderSubagentResultMessage).
var subagentResultPattern = regexp.MustCompile(`^Background task ([0-9a-f]+) \((.*)\) (finished|was cancelled|FAILED)`)

// reportedTask is the task a subagent_result reports on: its id,
// description and outcome read off the report's first line, the rest
// off disk. ok is false for a report that names no task.
func reportedTask(storeRoot, parent, report string) (SubagentTaskView, bool) {
	match := subagentResultPattern.FindStringSubmatch(report)
	if match == nil {
		return SubagentTaskView{}, false
	}
	id, desc, outcome := match[1], match[2], match[3]
	v := deriveSubagentTaskView(storeRoot, parent, id, desc)
	// The report is the outcome; meta.json may lag it or be gone.
	v.Status = "completed"
	switch outcome {
	case "was cancelled":
		v.Status = subagentJobCancelled
	case "FAILED":
		v.Status = "errored"
	}
	return v, true
}

// taskResultRow is a finished task's report as the row the transcript
// shows where it landed: the task as it ended. A report that names no
// task stands as its text.
func taskResultRow(p provider.Provider, storeRoot, parent string, m store.ChatMessage) apitypes.TranscriptRow {
	row := apitypes.TranscriptRow{Kind: apitypes.TranscriptKindTaskResult, TS: unixMS(m.TS)}
	v, ok := reportedTask(storeRoot, parent, m.Content)
	if !ok {
		row.Tasks = &[]apitypes.SubagentTask{}
		row.BodyMD = ptr(m.Content)
		return row
	}
	row.Tasks = &[]apitypes.SubagentTask{subagentTask(p, parent, 0, v)}
	return row
}

// taskResult is m's taskResultRow, its outcome folded into the task it
// reports on when the batch that dispatched the task is in the
// transcript (it is not when the dispatch was in an earlier chat).
func (b *transcriptBuilder) taskResult(m store.ChatMessage, out []apitypes.TranscriptRow, tasks map[string]taskSlot) apitypes.TranscriptRow {
	row := taskResultRow(b.s.Provider, b.s.Store.Root(), b.slug, m)
	if len(*row.Tasks) == 0 {
		return row
	}
	reported := &(*row.Tasks)[0]
	slot, ok := tasks[reported.ID]
	if !ok {
		return row
	}
	dispatched := &(*out[slot.row].Tasks)[slot.task]
	// The dispatch named what the task runs on; keep it when the
	// meta.json the view read lacks it.
	reported.Model = cmp.Or(reported.Model, dispatched.Model)
	reported.Effort = cmp.Or(reported.Effort, dispatched.Effort)
	index := dispatched.Index
	*dispatched = *reported
	dispatched.Index = index
	return row
}

// deliveryRow is an inbox delivery as a card from its sender, built
// from the same InboxView the model's text is rendered from. A message
// file that can no longer be read leaves the text the model saw.
func (b *transcriptBuilder) deliveryRow(m store.ChatMessage, views map[string]agent.InboxView) apitypes.TranscriptRow {
	row := apitypes.TranscriptRow{
		Kind:        apitypes.TranscriptKindDelivery,
		TS:          unixMS(m.TS),
		DeliveredTo: ptr(b.view.person(b.slug)),
		AlsoTo:      ptr([]apitypes.PersonRef{}),
		Attachments: ptr(b.view.attachments(m.Attachments)),
		Quiet:       m.Quiet,
	}
	if m.MessageRef != "" {
		row.RawURL = ptr("/" + m.MessageRef)
	}
	iv, ok := views[m.MessageRef]
	if !ok {
		row.Title = ptr("")
		row.BodyMD = ptr(m.Content)
		row.KindBadge = ptr("delivery")
		return row
	}
	row.Title = ptr(cmp.Or(iv.Title, iv.Label))
	row.BodyMD = ptr(iv.Body)
	row.From = ptr(b.agentParty(iv.From))
	row.RepliesTo = ptr(b.view.person(iv.From))
	row.MessageType = ptr(iv.Type)
	row.KindBadge = ptr(deliveryBadge(iv))
	row.Attachments = ptr(b.view.attachments(iv.Attachments))
	also := make([]apitypes.PersonRef, 0, len(iv.To))
	for _, to := range iv.To {
		if to != b.slug {
			also = append(also, b.view.person(to))
		}
	}
	row.AlsoTo = &also
	if iv.Assignment != nil {
		row.AssignmentRef = b.view.assignment(iv.Assignment.ID)
	}
	if iv.InReplyTo != "" {
		row.InReplyTo = ptr("/" + iv.InReplyTo)
	}
	return row
}

// deliveryBadge is the card's kind badge: an assignment event is an
// assignment update, and every other type is its label in lower case
// ("notice", "approved", "handoff").
func deliveryBadge(iv agent.InboxView) string {
	if iv.Type == string(store.MsgAssignmentEvent) {
		return "assignment update"
	}
	return strings.ToLower(iv.Label)
}

// publishedRow is what the agent published: to you, a ceo_queue row
// you answer from Home; to anyone else, a chip naming it.
func (b *transcriptBuilder) publishedRow(m store.ChatMessage) apitypes.TranscriptRow {
	var msg store.Message
	readErr := errors.New("no message path")
	if m.MessageRef != "" {
		msg, readErr = b.s.Store.ReadMessage(filepath.Join(b.s.Store.Root(), m.MessageRef))
	}
	if readErr == nil && msg.Type.IsCEOBound() {
		return apitypes.TranscriptRow{
			Kind:        apitypes.TranscriptKindCEOQueue,
			TS:          unixMS(m.TS),
			From:        ptr(b.self),
			Title:       ptr(msg.Title),
			BodyMD:      ptr(strings.TrimRight(msg.Body, "\n")),
			Attachments: ptr(b.view.attachments(msg.Attachments)),
			MessageType: ptr(string(msg.Type)),
			Path:        ptr(m.MessageRef),
			RawURL:      ptr("/" + m.MessageRef),
			Resolved:    ptr(b.ceoAnswered(m.MessageRef)),
		}
	}
	row := apitypes.TranscriptRow{
		Kind:        apitypes.TranscriptKindDocPublished,
		TS:          unixMS(m.TS),
		ToolUseID:   ptr(m.ToolUseID),
		Title:       ptr(m.Content),
		To:          ptr([]apitypes.PersonRef{}),
		Attachments: ptr(b.view.attachments(m.Attachments)),
	}
	if m.MessageRef != "" {
		row.RawURL = ptr("/" + m.MessageRef)
	}
	if readErr == nil {
		row.Title = ptr(msg.Title)
		row.BodyMD = ptr(strings.TrimRight(msg.Body, "\n"))
		row.MessageType = ptr(string(msg.Type))
		row.To = ptr(b.view.people(msg.To))
		row.Attachments = ptr(b.view.attachments(msg.Attachments))
		if msg.Assignment != nil {
			row.AssignmentRef = b.view.assignment(msg.Assignment.ID)
		}
	}
	return row
}

// ceoAnswered reports whether you replied to the message at path: a
// ceo_reply in your chat that names it (ReplyToMessageRef, which
// handleCEOReply records). Your chat is read once per transcript, and
// only when something asks.
func (b *transcriptBuilder) ceoAnswered(path string) bool {
	if path == "" {
		return false
	}
	if !b.ceoRepliedRead {
		b.ceoRepliedRead = true
		b.ceoReplied = map[string]bool{}
		hist, _ := b.s.Store.ReadChatHistory(agent.CEOSlug)
		for _, e := range hist {
			if e.Kind == "ceo_reply" && e.ReplyToMessageRef != "" {
				b.ceoReplied[e.ReplyToMessageRef] = true
			}
		}
	}
	return b.ceoReplied[path]
}

// markerText is each stored marker's label, the words its marker row
// shows.
var markerText = map[string]string{
	store.KindUserInterruption:  "Chat interrupted by Stop",
	store.KindRuntimeDisruption: "Interrupted by a runtime restart",
	store.KindTurnError:         "Stopped on an error",
	store.KindPausedToDeliver:   "Paused to deliver your message",
}

func markerRow(m store.ChatMessage) apitypes.TranscriptRow {
	kind := apitypes.MarkerKind(m.Kind)
	return apitypes.TranscriptRow{
		Kind:       apitypes.TranscriptKindMarker,
		TS:         unixMS(m.TS),
		MarkerKind: &kind,
		Text:       ptr(markerText[m.Kind]),
		BodyMD:     ptr(m.Content),
	}
}

func bodyRow(kind apitypes.TranscriptKind, m store.ChatMessage) apitypes.TranscriptRow {
	return apitypes.TranscriptRow{Kind: kind, TS: unixMS(m.TS), BodyMD: ptr(m.Content)}
}

// unixMS is t in unix milliseconds; 0 for the zero time, which a row
// written before rows were stamped carries.
func unixMS(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func ptr[T any](v T) *T { return &v }
