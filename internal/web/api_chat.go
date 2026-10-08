package web

import (
	"bytes"
	"cmp"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The agent chat API: the transcript, the composer's actions, a
// background task's transcript, and past chats.
func (s *Server) wireAPIChatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agents/{slug}/chat", s.handleAPIChat)
	mux.HandleFunc("POST /api/v1/agents/{slug}/messages", s.handleAPIMessagePost)
	mux.HandleFunc("POST /api/v1/agents/{slug}/stop", s.handleAPIStop)
	mux.HandleFunc("POST /api/v1/agents/{slug}/model", s.handleAPIModel)
	mux.HandleFunc("POST /api/v1/agents/{slug}/effort", s.handleAPIEffort)
	mux.HandleFunc("POST /api/v1/agents/{slug}/new-chat", s.handleAPINewChat)
	mux.HandleFunc("GET /api/v1/agents/{slug}/subagents/{id}", s.handleAPISubagent)
	mux.HandleFunc("GET /api/v1/agents/{slug}/chats", s.handleAPIPastChats)
	mux.HandleFunc("GET /api/v1/agents/{slug}/chats/{ts}", s.handleAPIPastChat)
	mux.HandleFunc("GET /api/v1/agents/{slug}/tool-calls/{id}", s.handleAPIToolCall)
}

const whoLink = "whoever sent you the link"

// chatAgent resolves {slug} for a chat route: an active agent, or an
// archived one (archived true). It writes the error itself and reports
// ok false when there is no such agent. The CEO has no chat here: 400,
// as the stream answers.
func (s *Server) chatAgent(w http.ResponseWriter, r *http.Request) (a store.Agent, archived, ok bool) {
	slug := r.PathValue("slug")
	if slug == agent.CEOSlug {
		writeAPIError(w, http.StatusBadRequest, "you have no chat of your own", whoDevelopers)
		return a, false, false
	}
	if !plainSlug(slug) {
		writeAPIError(w, http.StatusNotFound, "no agent is called "+slug, whoLink)
		return a, false, false
	}
	a, err := s.Store.GetAgent(slug)
	if errors.Is(err, store.ErrNotFound) {
		a, err = s.Store.GetArchivedAgent(slug)
		archived = true
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "no agent is called "+slug, whoLink)
		return a, false, false
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "the agent could not be read", whoServer)
		return a, false, false
	}
	return a, archived, true
}

// activeChatAgent is chatAgent for an action: an archived agent has
// left, so nothing can be done to its chat.
func (s *Server) activeChatAgent(w http.ResponseWriter, r *http.Request) (store.Agent, bool) {
	a, archived, ok := s.chatAgent(w, r)
	if ok && archived {
		writeAPIError(w, http.StatusConflict, a.Slug+" has left the team", whoNoOne)
		return a, false
	}
	return a, ok
}

// handleAPIChat serves GET /api/v1/agents/{slug}/chat.
func (s *Server) handleAPIChat(w http.ResponseWriter, r *http.Request) {
	a, archived, ok := s.chatAgent(w, r)
	if !ok {
		return
	}
	hist, err := s.Store.ReadChatHistory(a.Slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusInternalServerError, "the agent's chat could not be read", whoServer)
		return
	}
	// The same numbers as the agent summary's context and the old
	// page's ring: an empty chat reads as empty whatever the sidecar
	// last recorded.
	cw, _ := s.Store.ReadContextWindow(a.Slug)
	stats := s.computeChatFillStats(a.Model, s.AgentModel, hist, cw.ContextTokens)
	fill := apitypes.ChatFill{
		Limit:         stats.ContextLimit,
		Bucket:        stats.FillBucket,
		LongThreshold: stats.LongThresh,
		ResolvedModel: stats.ResolvedModel,
	}
	if len(hist) > 0 {
		fill.Pct, fill.Tokens = stats.FillPct, stats.TokenEst
	}
	fill.Bucket = fillBucket(fill.Pct)

	gens, err := s.Store.ListArchivedChats(a.Slug) // newest first
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the agent's chat could not be read", whoServer)
		return
	}
	generation := ""
	if len(gens) > 0 {
		generation = gens[0].Timestamp
	}

	current := cmp.Or(a.Model, s.AgentModel)
	running, waiting := s.agentWorking(a.Slug)
	models := s.modelOptions(current, a.Effort)
	writeJSON(w, http.StatusOK, apitypes.Chat{
		Rows:          s.currentChatRows(a.Slug, hist),
		Pending:       s.pendingMessagesFor(a.Slug),
		Fill:          fill,
		Models:        models,
		CurrentModel:  current,
		CurrentEffort: s.effectiveEffort(current, a.Effort),
		Running:       running,
		WaitingTasks:  waiting,
		Archived:      archived,
		Rotating:      s.rotating(a.Slug),
		Generation:    generation,
	})
}

// handleAPIToolCall serves GET /api/v1/agents/{slug}/tool-calls/{id}:
// a call in the current chat in full, for a row the chat API cut. A
// call the chat no longer holds (it rotated since the page loaded) is
// a 404.
func (s *Server) handleAPIToolCall(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.chatAgent(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	hist, err := s.Store.ReadChatHistory(a.Slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusInternalServerError, "the agent's chat could not be read", whoServer)
		return
	}
	call := apitypes.ToolCallDetail{ToolUseID: id}
	found := false
	for _, m := range hist {
		if m.ToolUseID != id {
			continue
		}
		switch m.Kind {
		case "tool_use":
			call.Input, found = m.ToolInput, true
		case "tool_result":
			call.Output, found = m.Content, true
		}
	}
	if id == "" || !found {
		writeAPIError(w, http.StatusNotFound, "this tool call is no longer in "+a.Slug+"'s current chat", "you: reload the page to see the chat as it is now")
		return
	}
	writeJSON(w, http.StatusOK, call)
}

// handleAPIMessagePost serves POST /api/v1/agents/{slug}/messages with
// handleAgentMessagePost (POST /agents/{slug}/messages), which already
// answers JSON on success; its plain-text refusals are rewritten into
// the API's error shape.
func (s *Server) handleAPIMessagePost(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !plainSlug(slug) {
		writeAPIError(w, http.StatusNotFound, "no agent is called "+slug, whoLink)
		return
	}
	var c capturedResponse
	s.handleAgentMessagePost(&c, r)
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.status < http.StatusBadRequest {
		for k, v := range c.header {
			w.Header()[k] = v
		}
		w.WriteHeader(c.status)
		_, _ = w.Write(c.body.Bytes())
		return
	}
	what := strings.TrimSpace(c.body.String())
	who := whoDevelopers
	switch {
	case c.status == http.StatusNotFound:
		what, who = "no agent is called "+slug, whoLink
	case c.status >= http.StatusInternalServerError:
		who = whoServer
	}
	writeAPIError(w, c.status, what, who)
}

// handleAPIStop serves POST /api/v1/agents/{slug}/stop through the
// page's Stop handler: 202 from it is stopped, 204 is nothing to stop.
func (s *Server) handleAPIStop(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.activeChatAgent(w, r); !ok {
		return
	}
	var c capturedResponse
	s.handleAgentStop(&c, r)
	if c.status >= http.StatusBadRequest {
		writeAPIError(w, c.status, strings.TrimSpace(c.body.String()), whoDevelopers)
		return
	}
	writeJSON(w, http.StatusOK, apitypes.StopResponse{Stopped: c.status == http.StatusAccepted})
}

// handleAPIModel serves POST /api/v1/agents/{slug}/model.
func (s *Server) handleAPIModel(w http.ResponseWriter, r *http.Request) {
	a, ok := s.activeChatAgent(w, r)
	if !ok {
		return
	}
	var req apitypes.ModelRequest
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	s.writeSettingResult(w, a.Slug, s.setAgentModel(a.Slug, req.Model))
}

// handleAPIEffort serves POST /api/v1/agents/{slug}/effort.
func (s *Server) handleAPIEffort(w http.ResponseWriter, r *http.Request) {
	a, ok := s.activeChatAgent(w, r)
	if !ok {
		return
	}
	var req apitypes.EffortRequest
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	s.writeSettingResult(w, a.Slug, s.setAgentEffort(a.Slug, req.Effort))
}

// writeSettingResult answers a model or effort post: the agent's
// settings as they now stand, or why the change was refused.
func (s *Server) writeSettingResult(w http.ResponseWriter, slug string, err error) {
	switch {
	case errors.Is(err, errUnsupportedSetting):
		// The picker offers only what is supported, so a refusal here
		// is the client and server disagreeing.
		writeAPIError(w, http.StatusBadRequest, err.Error(), whoDevelopers)
		return
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "no agent is called "+slug, whoLink)
		return
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "the setting could not be saved", whoServer)
		return
	}
	a, err := s.Store.GetAgent(slug)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the agent could not be read", whoServer)
		return
	}
	writeJSON(w, http.StatusOK, apitypes.ChatSettings{
		CurrentModel:  cmp.Or(a.Model, s.AgentModel),
		CurrentEffort: s.effectiveEffort(cmp.Or(a.Model, s.AgentModel), a.Effort),
	})
}

// handleAPINewChat serves POST /api/v1/agents/{slug}/new-chat.
func (s *Server) handleAPINewChat(w http.ResponseWriter, r *http.Request) {
	a, ok := s.activeChatAgent(w, r)
	if !ok {
		return
	}
	started, err := s.startNewChat(a.Slug)
	switch {
	case errors.Is(err, errNewChatBusy):
		writeAPIError(w, http.StatusConflict, a.Slug+" is still replying, so a new chat cannot start yet", whoYou+", once the reply ends")
	case errors.Is(err, errNewChatNoClaude):
		writeAPIError(w, http.StatusServiceUnavailable, "this server has no model configured", whoServer)
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "no agent is called "+a.Slug, whoLink)
	case err != nil:
		writeAPIError(w, http.StatusInternalServerError, "the new chat could not start", whoServer)
	default:
		writeJSON(w, http.StatusOK, apitypes.NewChatResponse{Started: started})
	}
}

// handleAPISubagent serves GET /api/v1/agents/{slug}/subagents/{id}:
// one background task's header and transcript. Tasks of an agent that
// has left stay readable: its files moved under agents/_archived.
func (s *Server) handleAPISubagent(w http.ResponseWriter, r *http.Request) {
	a, archived, ok := s.chatAgent(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	notFound := func() {
		writeAPIError(w, http.StatusNotFound, "no background task is called "+id, whoLink)
	}
	if !plainSlug(id) {
		notFound()
		return
	}
	dir := agentDirName(a.Slug, archived)
	hist, err := readSubagentHistory(subagentChatPath(s.Store.Root(), dir, id))
	if err != nil {
		notFound()
		return
	}
	meta := s.subagentMetaView(a.Slug, dir, id, hist)
	writeJSON(w, http.StatusOK, apitypes.SubagentTranscript{
		Meta: meta,
		Rows: s.subagentTranscriptRows(a.Slug, id, meta.Description, hist),
	})
}

// agentDirName is slug's directory under agents/: the slug itself, or
// _archived/<slug> once the agent has left (store.ArchiveAgent moves
// the whole directory, background tasks included).
func agentDirName(slug string, archived bool) string {
	if archived {
		return filepath.Join("_archived", slug)
	}
	return slug
}

// subagentMetaView is a task's header: the live job when core still
// holds it, else what meta.json and the transcript recorded. dir is
// parent's directory under agents/ (agentDirName).
func (s *Server) subagentMetaView(parent, dir, id string, hist []store.ChatMessage) apitypes.SubagentMeta {
	meta, metaErr := readSubagentMeta(subagentMetaPath(s.Store.Root(), dir, id))
	status := meta.Status
	if metaErr != nil {
		status = "unknown"
	}
	errText := meta.Error
	out := apitypes.SubagentMeta{
		ID:          id,
		Parent:      parent,
		Description: meta.Description,
		Model:       s.modelLabel(resolveSubagentModel(s.Provider, meta.Model)),
		Effort:      resolveSubagentEffort(s.Provider, resolveSubagentModel(s.Provider, meta.Model), meta.Effort),
	}
	if len(hist) > 0 {
		out.Started = unixMS(hist[0].TS)
	}
	if job, ok := s.subagentJobByID(parent, id); ok {
		status, errText = job.State, job.Err
		out.Description = cmp.Or(out.Description, job.Description)
		if !job.StartedAt.IsZero() {
			out.Started = unixMS(job.StartedAt)
		}
		if !job.EndedAt.IsZero() {
			out.Ended = ptr(unixMS(job.EndedAt))
		}
		if job.Activity != "" && !job.terminal() {
			out.Activity = ptr(job.Activity)
		}
	}
	out.State, out.Error = subagentState(status, errText)
	if out.State != apitypes.SubagentTaskStateRunning && out.Ended == nil {
		if t, err := time.Parse(time.RFC3339, meta.UpdatedAt); err == nil {
			out.Ended = ptr(unixMS(t))
		}
	}
	return out
}

// subagentJobByID is core's record of one of parent's jobs, while it
// still holds it (until the chat rotates or core restarts).
func (s *Server) subagentJobByID(parent, id string) (subagentJob, bool) {
	if s.SubagentService == nil {
		return subagentJob{}, false
	}
	for _, j := range s.SubagentService.listJobs(parent) {
		if j.ID == id {
			return j, true
		}
	}
	return subagentJob{}, false
}

// handleAPIPastChats serves GET /api/v1/agents/{slug}/chats, newest
// first.
func (s *Server) handleAPIPastChats(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.chatAgent(w, r)
	if !ok {
		return
	}
	gens, err := s.Store.ListArchivedChats(a.Slug)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the past chats could not be read", whoServer)
		return
	}
	views := s.archivedChatViews(a.Slug, gens)
	resp := apitypes.PastChats{Chats: make([]apitypes.PastChat, 0, len(gens))}
	for i, g := range gens {
		hist, _ := s.Store.ReadArchivedChat(a.Slug, g.Timestamp)
		resp.Chats = append(resp.Chats, pastChat(g.Timestamp, views[i].Excerpt, hist))
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAPIPastChat serves GET /api/v1/agents/{slug}/chats/{ts}: what
// the rotation left behind, the transcript, and the chats either side.
func (s *Server) handleAPIPastChat(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.chatAgent(w, r)
	if !ok {
		return
	}
	ts := r.PathValue("ts")
	notFound := func() {
		writeAPIError(w, http.StatusNotFound, "no past chat is called "+ts, whoLink)
	}
	if !plainSlug(ts) {
		notFound()
		return
	}
	hist, err := s.Store.ReadArchivedChat(a.Slug, ts)
	if err != nil {
		notFound()
		return
	}
	gens, err := s.Store.ListArchivedChats(a.Slug) // newest first
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the past chats could not be read", whoServer)
		return
	}
	title := ""
	if v := s.archivedChatViews(a.Slug, []store.ArchivedChat{{Slug: a.Slug, Timestamp: ts}}); len(v) == 1 {
		title = v[0].Excerpt
	}
	outcome := s.rotationOutcomeFor(a.Slug, ts)
	detail := apitypes.PastChatDetail{
		Summary: apitypes.PastChatSummary{
			PastChat:    pastChat(ts, title, hist),
			DigestMD:    episodeDigest(outcome.Episode),
			MemoryAdded: addedLines(outcome.MemoryBefore, outcome.MemoryAfter),
			HabitsDiff:  apitypes.HabitsDiff{Before: outcome.HabitsBefore, After: outcome.HabitsAfter},
		},
		Rows: s.transcriptRows(a.Slug, hist),
	}
	for i, g := range gens {
		if g.Timestamp != ts {
			continue
		}
		if i > 0 {
			detail.NextTS = ptr(gens[i-1].Timestamp)
		}
		if i+1 < len(gens) {
			detail.PrevTS = ptr(gens[i+1].Timestamp)
		}
		break
	}
	writeJSON(w, http.StatusOK, detail)
}

// pastChat is one archived chat's list row. Messages counts what was
// said (chat messages, deliveries, shared files), not tool plumbing or
// markers.
func pastChat(ts, title string, hist []store.ChatMessage) apitypes.PastChat {
	pc := apitypes.PastChat{TS: ts, Title: title, Tokens: estimateChatTokens(hist)}
	for _, m := range hist {
		switch m.Kind {
		case "", "direct_chat", "inbox_delivery", "file_shared":
			pc.Messages++
		}
	}
	if len(hist) > 0 {
		pc.Range = apitypes.ChatRange{From: unixMS(hist[0].TS), To: unixMS(hist[len(hist)-1].TS)}
	}
	return pc
}

// episodeDigest is an episode file's digest without its frontmatter
// (renderEpisodeFile writes "---", the fields, "---", then the body).
func episodeDigest(file string) string {
	if rest, ok := strings.CutPrefix(file, "---\n"); ok {
		if _, body, found := strings.Cut(rest, "\n---\n"); found {
			return strings.TrimSpace(body)
		}
	}
	return strings.TrimSpace(file)
}

// addedLines is the memory notes present in after and not in before:
// non-blank lines that are not headings, with any list marker removed.
func addedLines(before, after string) []string {
	had := map[string]bool{}
	for _, l := range strings.Split(before, "\n") {
		had[memoryNote(l)] = true
	}
	out := []string{}
	for _, l := range strings.Split(after, "\n") {
		n := memoryNote(l)
		if n == "" || strings.HasPrefix(n, "#") || had[n] {
			continue
		}
		had[n] = true
		out = append(out, n)
	}
	return out
}

func memoryNote(line string) string {
	n := strings.TrimSpace(line)
	for _, marker := range []string{"- ", "* "} {
		n = strings.TrimPrefix(n, marker)
	}
	return strings.TrimSpace(n)
}

// subagentChatPath is a task's transcript, beside its meta.json. dir
// is the parent's directory under agents/ (agentDirName).
func subagentChatPath(storeRoot, dir, id string) string {
	return filepath.Join(filepath.Dir(subagentMetaPath(storeRoot, dir, id)), "chat.jsonl")
}

// capturedResponse records what a page handler wrote, so a JSON route
// can reuse the handler and answer in the API's shape.
type capturedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *capturedResponse) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}

func (c *capturedResponse) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *capturedResponse) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p)
}
