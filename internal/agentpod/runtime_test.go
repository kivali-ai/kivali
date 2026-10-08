package agentpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
)

// fakeCore is a tiny stand-in for Kivali central wired against a
// Unix socket: it accepts an SSE subscription on
// /v1/agent/{slug}/events and exposes a Publish helper to push
// events to the (single) connected client. It also accepts the
// turn-event POST and records each TurnEvent for the test to
// assert on.
type fakeCore struct {
	mu        sync.Mutex
	subs      map[string]chan Event  // slug → event channel
	posted    map[string][]TurnEvent // turnID → events posted, in order
	sessions  map[string]string      // slug → claude session id
	addr      string                 // socket path
	srv       *http.Server
	doneClose chan struct{}
}

func startFakeCore(t *testing.T) *fakeCore {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wos-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	fc := &fakeCore{
		subs:      map[string]chan Event{},
		posted:    map[string][]TurnEvent{},
		sessions:  map[string]string{},
		addr:      filepath.Join(dir, "s"),
		doneClose: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent/{slug}/events", fc.handleEvents)
	mux.HandleFunc("POST /v1/agent/{slug}/chat-turn/{turn_id}/event", fc.handleTurnEvent)
	mux.HandleFunc("/v1/agent/{slug}/session-id", fc.handleSession)

	ln, err := net.Listen("unix", fc.addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fc.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := fc.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = fc.srv.Shutdown(ctx)
		close(fc.doneClose)
	})
	return fc
}

func (f *fakeCore) handleEvents(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}
	ch := make(chan Event, 8)
	f.mu.Lock()
	f.subs[slug] = ch
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.subs, slug)
		f.mu.Unlock()
	}()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, string(ev.Data))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (f *fakeCore) handleTurnEvent(w http.ResponseWriter, r *http.Request) {
	turnID := r.PathValue("turn_id")
	var ev TurnEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.posted[turnID] = append(f.posted[turnID], ev)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeCore) handleSession(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"session_id": f.sessions[slug]})
	case http.MethodPost:
		var body struct {
			SessionID string `json:"session_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.sessions[slug] = body.SessionID
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		delete(f.sessions, slug)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *fakeCore) publish(slug string, ev Event) {
	f.mu.Lock()
	ch, ok := f.subs[slug]
	f.mu.Unlock()
	if !ok {
		return
	}
	ch <- ev
}

func (f *fakeCore) postedFor(turnID string) []TurnEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]TurnEvent, len(f.posted[turnID]))
	copy(out, f.posted[turnID])
	return out
}

// scriptedClaudeClient is the test seam between the runtime and a
// real claudeagent.Client. We script the events the stream emits +
// the final usage so the runtime drives a turn deterministically.
type scriptedClaudeClient struct {
	streams func(ctx context.Context, req provider.CompleteRequest) (provider.Stream, error)
}

func (s *scriptedClaudeClient) Complete(_ context.Context, _ provider.CompleteRequest) (*provider.CompleteResponse, error) {
	return nil, errors.New("unused in tests")
}
func (s *scriptedClaudeClient) Stream(ctx context.Context, req provider.CompleteRequest) (provider.Stream, error) {
	return s.streams(ctx, req)
}
func (s *scriptedClaudeClient) FormatUsage(_ provider.TokenUsage) provider.UsageDisplay {
	return provider.UsageDisplay{}
}
func (s *scriptedClaudeClient) HandlesToolLoop() bool { return true }
func (s *scriptedClaudeClient) RunSubagent(_ context.Context, _ provider.SubagentRequest) (provider.Stream, error) {
	return nil, errors.New("unused in tests")
}

// resettableClaudeClient is a scriptedClaudeClient that also satisfies
// the runtime's sessionResetter capability, recording the slug it was
// asked to reset so EventSessionReset handling can be asserted.
type resettableClaudeClient struct {
	scriptedClaudeClient
	resetCh chan string
}

func (r *resettableClaudeClient) ResetSession(slug string) error {
	r.resetCh <- slug
	return nil
}

// startRuntime constructs a Runtime against the fake core + a
// scripted claude client and starts Run on a goroutine. Returns
// the runtime + a cancel func.
func startRuntime(t *testing.T, fc *fakeCore, slug string, cc provider.Client) (*Runtime, context.CancelFunc) {
	t.Helper()
	c := NewClient(fc.addr, slug)
	rt := &Runtime{Client: c, Claude: cc, Logger: log.New(io.Discard, "", 0)}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = rt.Run(ctx) }()
	// Wait for the runtime to subscribe.
	deadline := time.Now().Add(2 * time.Second)
	for {
		fc.mu.Lock()
		_, ok := fc.subs[slug]
		fc.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("runtime never subscribed to events")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return rt, cancel
}

// TestRuntimeChatTurnHappyPath: a chat-turn event drives a turn,
// the runtime translates StreamEvents to TurnEvents, POSTs each,
// and emits a final "done" event with usage.
func TestRuntimeChatTurnHappyPath(t *testing.T) {
	fc := startFakeCore(t)
	cc := &scriptedClaudeClient{
		streams: func(_ context.Context, _ provider.CompleteRequest) (provider.Stream, error) {
			return provider.NewMockStream(
				[]provider.StreamEvent{
					{Kind: provider.StreamDelta, Text: "hi"},
					{Kind: provider.StreamToolUseStart, ToolUseID: "tu1", ToolName: "file_view"},
					{Kind: provider.StreamToolUseEnd, ToolUseID: "tu1", ToolName: "file_view", ToolInput: []byte(`{"path":"/files/x"}`)},
					{Kind: provider.StreamToolResult, ToolUseID: "tu1", ToolResultText: "ok"},
					{Kind: provider.StreamEnd},
				},
				&provider.CompleteResponse{
					StopReason: "end_turn",
					Usage:      provider.TokenUsage{InputTokens: 10, OutputTokens: 20},
				},
			), nil
		},
	}
	_, cancel := startRuntime(t, fc, "alice", cc)
	defer cancel()

	body, _ := json.Marshal(ChatTurnEvent{
		TurnID: "t1", Slug: "alice", Source: "chat", Model: "test",
	})
	fc.publish("alice", Event{Type: EventChatTurn, Data: body})

	deadline := time.Now().Add(2 * time.Second)
	for {
		evs := fc.postedFor("t1")
		if len(evs) > 0 && evs[len(evs)-1].Kind == TurnEventDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never received done; got: %v", fc.postedFor("t1"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := fc.postedFor("t1")
	wantKinds := []TurnEventKind{
		TurnEventDelta,
		TurnEventToolUseStart,
		TurnEventToolUseEnd,
		TurnEventToolResult,
		TurnEventDone,
	}
	if len(got) != len(wantKinds) {
		t.Fatalf("got %d events, want %d: %v", len(got), len(wantKinds), got)
	}
	for i, want := range wantKinds {
		if got[i].Kind != want {
			t.Errorf("[%d] kind = %q, want %q", i, got[i].Kind, want)
		}
	}
	last := got[len(got)-1]
	if last.StopReason != "end_turn" {
		t.Errorf("done StopReason = %q, want end_turn", last.StopReason)
	}
	if last.InputTokens != 10 || last.OutputTokens != 20 {
		t.Errorf("done tokens = (%d/%d), want (10/20)", last.InputTokens, last.OutputTokens)
	}
}

// TestRuntimeChatTurnSubprocessDeath: the resumption-bug carrier
// flows through cleanly — an ErrSubprocessExited surfaces as a
// failed/subprocess-died TurnEvent, which core's handler turns
// into a KindRuntimeDisruption row (asserted in
// internal/web/agentpod_socket_test.go::TestAgentpodTurnEventFailedSubprocessDied).
func TestRuntimeChatTurnSubprocessDeath(t *testing.T) {
	fc := startFakeCore(t)
	cc := &scriptedClaudeClient{
		streams: func(_ context.Context, _ provider.CompleteRequest) (provider.Stream, error) {
			return provider.NewMockStreamErr(
				[]provider.StreamEvent{{Kind: provider.StreamDelta, Text: "starting"}},
				fmt.Errorf("claudeagent: %w (cause: signal: killed)", provider.ErrSubprocessExited),
			), nil
		},
	}
	_, cancel := startRuntime(t, fc, "alice", cc)
	defer cancel()

	body, _ := json.Marshal(ChatTurnEvent{TurnID: "t-die", Slug: "alice"})
	fc.publish("alice", Event{Type: EventChatTurn, Data: body})

	deadline := time.Now().Add(2 * time.Second)
	for {
		evs := fc.postedFor("t-die")
		if len(evs) > 0 && evs[len(evs)-1].Kind == TurnEventFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never received failed; got: %v", fc.postedFor("t-die"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	last := fc.postedFor("t-die")[len(fc.postedFor("t-die"))-1]
	if last.FailedReason != FailedReasonSubprocessDied {
		t.Errorf("FailedReason = %q, want %q", last.FailedReason, FailedReasonSubprocessDied)
	}
}

// TestRuntimeChatTurnCancelled: EventCancelTurn arrives mid-stream,
// the runtime cancels the per-turn context, the stream reports
// context.Canceled, and the runtime POSTs a failed event with
// FailedReasonCancelled. Locks in the wire-cancel handshake so
// Stop fires immediately rather than waiting on the model's chunk
// boundary.
func TestRuntimeChatTurnCancelled(t *testing.T) {
	fc := startFakeCore(t)
	streamReady := make(chan struct{})
	cc := &scriptedClaudeClient{
		streams: func(ctx context.Context, _ provider.CompleteRequest) (provider.Stream, error) {
			s := &cancellableStream{
				events: make(chan provider.StreamEvent, 4),
				ctx:    ctx,
			}
			go func() {
				// Emit one delta so we know we're mid-stream, then
				// block on ctx.Done() — that's what a real CLI stream
				// would do, parking on stdin.
				s.events <- provider.StreamEvent{Kind: provider.StreamDelta, Text: "starting"}
				close(streamReady)
				<-ctx.Done()
				s.errMu.Lock()
				s.err = ctx.Err()
				s.errMu.Unlock()
				close(s.events)
			}()
			return s, nil
		},
	}
	_, cancel := startRuntime(t, fc, "alice", cc)
	defer cancel()

	body, _ := json.Marshal(ChatTurnEvent{TurnID: "t-cancel", Slug: "alice"})
	fc.publish("alice", Event{Type: EventChatTurn, Data: body})
	<-streamReady

	cancelBody, _ := json.Marshal(CancelTurnEvent{TurnID: "t-cancel", Slug: "alice"})
	fc.publish("alice", Event{Type: EventCancelTurn, Data: cancelBody})

	deadline := time.Now().Add(2 * time.Second)
	for {
		evs := fc.postedFor("t-cancel")
		if len(evs) > 0 && evs[len(evs)-1].Kind == TurnEventFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never received failed; got: %v", fc.postedFor("t-cancel"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	last := fc.postedFor("t-cancel")[len(fc.postedFor("t-cancel"))-1]
	if last.FailedReason != FailedReasonCancelled {
		t.Errorf("FailedReason = %q, want %q", last.FailedReason, FailedReasonCancelled)
	}
}

// TestRuntimeCancelTurnStaleNoOp: a cancel for a turn that's already
// done (or never existed) is a logged no-op — no event posted, no
// error to the events loop.
func TestRuntimeCancelTurnStaleNoOp(t *testing.T) {
	fc := startFakeCore(t)
	_, cancel := startRuntime(t, fc, "alice", nil)
	defer cancel()

	cancelBody, _ := json.Marshal(CancelTurnEvent{TurnID: "t-stale", Slug: "alice"})
	fc.publish("alice", Event{Type: EventCancelTurn, Data: cancelBody})

	// Give the runtime a moment to process; assert no events posted.
	time.Sleep(100 * time.Millisecond)
	if got := fc.postedFor("t-stale"); len(got) != 0 {
		t.Errorf("stale cancel posted events: %v", got)
	}
}

// cancellableStream is a tiny provider.Stream that parks on ctx.Done()
// after emitting one event. Used by the cancel test to simulate a
// long-running model stream that should be aborted by Stop.
type cancellableStream struct {
	events chan provider.StreamEvent
	ctx    context.Context
	errMu  sync.Mutex
	err    error
}

func (s *cancellableStream) Events() <-chan provider.StreamEvent { return s.events }
func (s *cancellableStream) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}
func (s *cancellableStream) Close() error                      { return nil }
func (s *cancellableStream) Final() *provider.CompleteResponse { return &provider.CompleteResponse{} }

// TestRuntimeChatTurnGenericError: a non-subprocess transport
// failure surfaces as failed/other-error — does NOT trip the
// disruption marker.
func TestRuntimeChatTurnGenericError(t *testing.T) {
	fc := startFakeCore(t)
	cc := &scriptedClaudeClient{
		streams: func(_ context.Context, _ provider.CompleteRequest) (provider.Stream, error) {
			return provider.NewMockStreamErr(nil, errors.New("claudeagent: parse stream-json: invalid")), nil
		},
	}
	_, cancel := startRuntime(t, fc, "alice", cc)
	defer cancel()

	body, _ := json.Marshal(ChatTurnEvent{TurnID: "t-bad", Slug: "alice"})
	fc.publish("alice", Event{Type: EventChatTurn, Data: body})

	deadline := time.Now().Add(2 * time.Second)
	for {
		evs := fc.postedFor("t-bad")
		if len(evs) > 0 && evs[len(evs)-1].Kind == TurnEventFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never received failed; got: %v", fc.postedFor("t-bad"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	last := fc.postedFor("t-bad")[len(fc.postedFor("t-bad"))-1]
	if last.FailedReason != FailedReasonOtherError {
		t.Errorf("FailedReason = %q, want %q", last.FailedReason, FailedReasonOtherError)
	}
}

// TestRuntimeChatTurnNoClaudeReportsFailed: a runtime started
// without a provider.Client (skip-claude / dev path) reports failed
// for chat-turn so the core-side hub doesn't hang waiting on a
// turn that will never produce events.
func TestRuntimeChatTurnNoClaudeReportsFailed(t *testing.T) {
	fc := startFakeCore(t)
	_, cancel := startRuntime(t, fc, "alice", nil) // no claude client
	defer cancel()

	body, _ := json.Marshal(ChatTurnEvent{TurnID: "t-nc", Slug: "alice"})
	fc.publish("alice", Event{Type: EventChatTurn, Data: body})

	deadline := time.Now().Add(2 * time.Second)
	for {
		evs := fc.postedFor("t-nc")
		if len(evs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never received any event")
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := fc.postedFor("t-nc")
	if len(got) != 1 || got[0].Kind != TurnEventFailed {
		t.Fatalf("expected single failed event, got %v", got)
	}
	if got[0].FailedReason != FailedReasonOtherError {
		t.Errorf("FailedReason = %q, want %q", got[0].FailedReason, FailedReasonOtherError)
	}
}

// TestRuntimeSessionResetEvictsRunner: an EventSessionReset (sent by
// core after a chat rotation clears the stored session id) drives the
// runtime to call ResetSession on its claude client, tearing down the
// warm `claude -p` runner so the next turn starts a fresh session.
// This is the in-pod half of the rotation-amnesia fix — without it the
// runner keeps the whole pre-rotation conversation in memory forever.
func TestRuntimeSessionResetEvictsRunner(t *testing.T) {
	fc := startFakeCore(t)
	resetCh := make(chan string, 1)
	cc := &resettableClaudeClient{resetCh: resetCh}
	_, cancel := startRuntime(t, fc, "alice", cc)
	defer cancel()

	body, _ := json.Marshal(SessionResetEvent{Slug: "alice"})
	fc.publish("alice", Event{Type: EventSessionReset, Data: body})

	select {
	case got := <-resetCh:
		if got != "alice" {
			t.Errorf("ResetSession slug = %q, want alice", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime never called ResetSession on EventSessionReset")
	}
}

// TestRuntimeSessionResetNoOpWhenUnsupported: a claude client that
// doesn't implement the reset capability (a mock) makes
// EventSessionReset a logged no-op rather than a crash, and the events
// loop stays alive — proven by driving a normal chat turn to done
// immediately after the reset event.
func TestRuntimeSessionResetNoOpWhenUnsupported(t *testing.T) {
	fc := startFakeCore(t)
	cc := &scriptedClaudeClient{ // no ResetSession method
		streams: func(_ context.Context, _ provider.CompleteRequest) (provider.Stream, error) {
			return provider.NewMockStream(
				[]provider.StreamEvent{{Kind: provider.StreamDelta, Text: "hi"}, {Kind: provider.StreamEnd}},
				&provider.CompleteResponse{StopReason: "end_turn"},
			), nil
		},
	}
	_, cancel := startRuntime(t, fc, "alice", cc)
	defer cancel()

	// Unsupported reset — must not wedge or crash the events loop.
	resetBody, _ := json.Marshal(SessionResetEvent{Slug: "alice"})
	fc.publish("alice", Event{Type: EventSessionReset, Data: resetBody})

	// Loop is still alive: a following chat turn still completes.
	turnBody, _ := json.Marshal(ChatTurnEvent{TurnID: "t-after-reset", Slug: "alice"})
	fc.publish("alice", Event{Type: EventChatTurn, Data: turnBody})

	deadline := time.Now().Add(2 * time.Second)
	for {
		evs := fc.postedFor("t-after-reset")
		if len(evs) > 0 && evs[len(evs)-1].Kind == TurnEventDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("events loop appears wedged after an unsupported session-reset; got %v", fc.postedFor("t-after-reset"))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRuntimeSessionStoreRoundtrip: the Client.SessionStore() view
// reads/writes/clears via UDS round-trip to fake core. Locks the
// claudeagent.SessionStore contract this satisfies.
func TestRuntimeSessionStoreRoundtrip(t *testing.T) {
	fc := startFakeCore(t)
	c := NewClient(fc.addr, "alice")
	ss := c.SessionStore()

	id, err := ss.ReadClaudeSessionID("alice")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if id != "" {
		t.Errorf("initial id = %q, want empty", id)
	}
	if err := ss.WriteClaudeSessionID("alice", "sess-xyz"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	id, err = ss.ReadClaudeSessionID("alice")
	if err != nil {
		t.Fatalf("Read after write: %v", err)
	}
	if id != "sess-xyz" {
		t.Errorf("read after write = %q, want sess-xyz", id)
	}
	if err := ss.ClearClaudeSessionID("alice"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	id, _ = ss.ReadClaudeSessionID("alice")
	if id != "" {
		t.Errorf("read after clear = %q, want empty", id)
	}
}

// TestRuntimeSessionStoreSlugMismatchRejected guards the bound-slug
// check on the SessionStore wrapper.
func TestRuntimeSessionStoreSlugMismatchRejected(t *testing.T) {
	fc := startFakeCore(t)
	ss := NewClient(fc.addr, "alice").SessionStore()
	if _, err := ss.ReadClaudeSessionID("bob"); err == nil {
		t.Error("expected error on slug mismatch; got nil")
	}
}

// TestStreamToTurnTranslation locks in the per-event mapping so a
// drift between provider.StreamEvent and agentpod.TurnEvent doesn't
// silently break the wire shape.
func TestStreamToTurnTranslation(t *testing.T) {
	cases := []struct {
		in   provider.StreamEvent
		want *TurnEvent
	}{
		{
			provider.StreamEvent{Kind: provider.StreamDelta, Text: "x"},
			&TurnEvent{Kind: TurnEventDelta, Text: "x"},
		},
		{
			provider.StreamEvent{Kind: provider.StreamToolUseStart, ToolUseID: "t1", ToolName: "n"},
			&TurnEvent{Kind: TurnEventToolUseStart, ToolUseID: "t1", ToolName: "n"},
		},
		{
			provider.StreamEvent{Kind: provider.StreamToolInputDelta, ToolUseID: "t1", Text: "delta"},
			&TurnEvent{Kind: TurnEventToolInputDelta, ToolUseID: "t1", Text: "delta"},
		},
		{
			provider.StreamEvent{Kind: provider.StreamToolUseEnd, ToolUseID: "t1", ToolName: "n", ToolInput: []byte(`{}`)},
			&TurnEvent{Kind: TurnEventToolUseEnd, ToolUseID: "t1", ToolName: "n", ToolInput: []byte(`{}`)},
		},
		{
			provider.StreamEvent{Kind: provider.StreamToolResult, ToolUseID: "t1", ToolResultText: "r", ToolResultIsError: true},
			&TurnEvent{Kind: TurnEventToolResult, ToolUseID: "t1", ToolResultText: "r", ToolResultIsError: true},
		},
		{
			provider.StreamEvent{Kind: provider.StreamError, Text: "API Error: 529 overloaded"},
			&TurnEvent{Kind: TurnEventError, Text: "API Error: 529 overloaded"},
		},
		{
			provider.StreamEvent{Kind: provider.StreamEnd},
			nil, // folded into post-loop "done" event
		},
	}
	for i, c := range cases {
		got := streamToTurn(c.in)
		if (got == nil) != (c.want == nil) {
			t.Errorf("[%d] nilness mismatch: got=%s want=%s", i, streamToTurnFmt(got), streamToTurnFmt(c.want))
			continue
		}
		if got == nil {
			continue
		}
		if got.Kind != c.want.Kind ||
			got.Text != c.want.Text ||
			got.ToolUseID != c.want.ToolUseID ||
			got.ToolName != c.want.ToolName ||
			string(got.ToolInput) != string(c.want.ToolInput) ||
			got.ToolResultText != c.want.ToolResultText ||
			got.ToolResultIsError != c.want.ToolResultIsError {
			t.Errorf("[%d] mismatch:\n got  %+v\n want %+v", i, got, c.want)
		}
	}
}

// TestRuntimeSubagentTurnReportsFailedWhenNotConfigured proves a
// runtime missing the subagent fork plumbing reports failed for
// Kind=subagent so SubagentService doesn't hang.
func TestRuntimeSubagentTurnReportsFailedWhenNotConfigured(t *testing.T) {
	fc := startFakeCore(t)
	c := NewClient(fc.addr, "alice")
	rt := &Runtime{
		Client: c,
		Logger: log.New(io.Discard, "", 0),
		// KivaliBinary / CoreUDS / ScratchRoot all empty.
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rt.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fc.mu.Lock()
		_, ok := fc.subs["alice"]
		fc.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime never subscribed")
		}
		time.Sleep(5 * time.Millisecond)
	}

	body, _ := json.Marshal(ChatTurnEvent{
		TurnID: "t-nc", Slug: "alice", Kind: ChatTurnKindSubagent,
		Subagent: &SubagentSpec{SubagentID: "x"},
	})
	fc.publish("alice", Event{Type: EventChatTurn, Data: body})

	deadline = time.Now().Add(2 * time.Second)
	for {
		evs := fc.postedFor("t-nc")
		if len(evs) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never received any event")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := fc.postedFor("t-nc")
	if got[0].Kind != TurnEventFailed {
		t.Errorf("kind = %q, want %q", got[0].Kind, TurnEventFailed)
	}
	if got[0].FailedReason != FailedReasonOtherError {
		t.Errorf("failed reason = %q, want %q", got[0].FailedReason, FailedReasonOtherError)
	}
}

// (smoke check — connects fakeCore + atomic counters to confirm the
// events stream subscription path is reentrant under reconnect.)
func TestRuntimeReconnectsAfterStreamDrop(t *testing.T) {
	fc := startFakeCore(t)
	var subs atomic.Int32
	// Wrap the original handler to count subscriptions. Replace the
	// mux to capture the reconnect cycles.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent/{slug}/events", func(w http.ResponseWriter, r *http.Request) {
		subs.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Close immediately to force reconnect.
	})
	mux.HandleFunc("POST /v1/agent/{slug}/chat-turn/{turn_id}/event", fc.handleTurnEvent)
	mux.HandleFunc("/v1/agent/{slug}/session-id", fc.handleSession)
	fc.srv.Handler = mux

	c := NewClient(fc.addr, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rt := &Runtime{Client: c, Claude: nil, Logger: log.New(io.Discard, "", 0)}
	go func() { _ = rt.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for subs.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("subs = %d after timeout, want >=2", subs.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
