package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/provider"
)

// fakeAgentPod stands in for an agent-pod runtime in tests. After
// installing one for a slug, every chat-turn event published for that
// slug gets intercepted: the fake runs a per-test response function
// (or a default single-done response) and POSTs the resulting
// TurnEvents back via handleAgentpodTurnEvent — the same shape a real
// agent runtime uses over UDS. Lets the in-process chat loop go
// without leaving the chat tests with no driver.
//
// Each chat-turn handles in its own goroutine so a blocking response
// (used by the "delivery arrives mid-stream" race tests) doesn't stall
// the events stream. The handleAgentpodTurnEvent calls themselves are
// synchronous within one turn so chat.jsonl ordering matches what core
// would observe over the wire.
type fakeAgentPod struct {
	srv  *Server
	slug string
	sub  *agentpodSubscriber

	mu   sync.Mutex
	next func(ct agentpod.ChatTurnEvent) []agentpod.TurnEvent

	// turns fires non-blockingly on each received chat-turn event so
	// tests can AwaitTurn() to inspect the inlined CompleteRequest.
	// Buffered; surplus events drop on the floor.
	turns chan agentpod.ChatTurnEvent

	// finished fires non-blockingly once a turn's events have all
	// been POSTed back, i.e. once core has run finalizeAgentpodTurn
	// for it. A test that ends right after AwaitTurn leaves that
	// finalize racing its own TempDir cleanup, which shows up as
	// "directory not empty" the moment finalize writes anything (the
	// knowledge-graph pass does). AwaitFinished is the deterministic
	// wait for that.
	finished chan struct{}

	// inflight holds one channel per turn the fake has started
	// handling, closed when that turn's last event has been POSTed.
	// Close waits on them: the fake's Cleanup runs before the
	// TempDir's, and a test that ends while a turn is still posting
	// would otherwise leave core's finalize writing into a directory
	// being removed. Guarded by mu.
	inflight []chan struct{}

	done      chan struct{}
	closeOnce sync.Once
}

// fakeTurnDrain bounds how long Close waits for a turn still posting
// events. A response func that blocks on a channel the test never
// releases must not hang the whole suite; the wait is generous
// because a turn's finalize runs an index pass and a usage write.
const fakeTurnDrain = 5 * time.Second

// installFakeAgentPod registers a fake agent pod for slug. The default
// response is a single done event with stop=end_turn. Tests override behavior
// via SetResponse / SetResponseFunc.
//
// AgentpodHub is auto-created if nil so callers don't need to remember.
func installFakeAgentPod(t *testing.T, srv *Server, slug string) *fakeAgentPod {
	t.Helper()
	if srv.AgentpodHub == nil {
		srv.AgentpodHub = newAgentpodHub()
	}
	p := &fakeAgentPod{
		srv:      srv,
		slug:     slug,
		sub:      srv.AgentpodHub.Subscribe(slug),
		turns:    make(chan agentpod.ChatTurnEvent, 16),
		finished: make(chan struct{}, 16),
		done:     make(chan struct{}),
	}
	go p.run()
	t.Cleanup(p.Close)
	return p
}

// Close waits for every turn the fake has started to finish posting,
// then unsubscribes from the hub and stops the run loop. Idempotent.
//
// The wait is what keeps a test's TempDir cleanup from racing core's
// finalize: t.Cleanup runs last-registered first, so this runs before
// the store's directory is removed, and once a turn's done event has
// been POSTed the writes finalize makes (chat, usage, the graph pass)
// have all returned. A turn that arrives after the snapshot below
// (a follow-up spawned by the finalize itself) is the one shape this
// does not cover; tests that expect one wait for it explicitly.
func (p *fakeAgentPod) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		turns := append([]chan struct{}(nil), p.inflight...)
		p.mu.Unlock()
		deadline := time.After(fakeTurnDrain)
		for _, turn := range turns {
			select {
			case <-turn:
			case <-deadline:
				// A blocked response func the test never released;
				// nothing more to wait for here.
				turns = nil
			}
			if turns == nil {
				break
			}
		}
		close(p.done)
		p.srv.AgentpodHub.Unsubscribe(p.sub)
	})
}

// SetResponse scripts a fixed event sequence to send back for every
// future chat-turn. Use SetResponseFunc when behavior depends on call
// count, the request, or test-side synchronization.
func (p *fakeAgentPod) SetResponse(events []agentpod.TurnEvent) {
	p.SetResponseFunc(func(_ agentpod.ChatTurnEvent) []agentpod.TurnEvent { return events })
}

// SetResponseFunc installs a per-turn callback. The fn is called once
// per chat-turn; its returned events are POSTed back in order. May
// block (e.g. waiting on a release channel for race tests).
func (p *fakeAgentPod) SetResponseFunc(fn func(ct agentpod.ChatTurnEvent) []agentpod.TurnEvent) {
	p.mu.Lock()
	p.next = fn
	p.mu.Unlock()
}

// AwaitTurn blocks until the next chat-turn arrives or timeout fires.
// Returns the captured event so tests can assert on the inlined
// CompleteRequest.
func (p *fakeAgentPod) AwaitTurn(t *testing.T, timeout time.Duration) agentpod.ChatTurnEvent {
	t.Helper()
	select {
	case ct := <-p.turns:
		return ct
	case <-time.After(timeout):
		t.Fatalf("fakeAgentPod %s: no chat-turn within %v", p.slug, timeout)
		return agentpod.ChatTurnEvent{}
	}
}

func (p *fakeAgentPod) run() {
	for {
		select {
		case <-p.done:
			return
		case <-p.sub.closed:
			return
		case ev, ok := <-p.sub.ch:
			if !ok {
				return
			}
			if ev.Type != agentpod.EventChatTurn {
				continue
			}
			var ct agentpod.ChatTurnEvent
			if err := json.Unmarshal(ev.Data, &ct); err != nil {
				continue
			}
			select {
			case p.turns <- ct:
			default:
			}
			turnDone := make(chan struct{})
			p.mu.Lock()
			p.inflight = append(p.inflight, turnDone)
			p.mu.Unlock()
			go p.handleTurn(ct, turnDone)
		}
	}
}

func (p *fakeAgentPod) handleTurn(ct agentpod.ChatTurnEvent, turnDone chan struct{}) {
	defer close(turnDone)
	p.mu.Lock()
	fn := p.next
	p.mu.Unlock()
	var events []agentpod.TurnEvent
	if fn != nil {
		events = fn(ct)
	}
	if len(events) == 0 {
		events = []agentpod.TurnEvent{{Kind: agentpod.TurnEventDone, StopReason: "end_turn"}}
	}
	for _, e := range events {
		p.postEvent(ct.Slug, ct.TurnID, e)
	}
	select {
	case p.finished <- struct{}{}:
	default:
	}
}

// AwaitFinished blocks until the fake has POSTed back every event of
// one turn, so core's finalize for that turn has returned. Pair with
// AwaitTurn in tests that assert on the request and then end.
func (p *fakeAgentPod) AwaitFinished(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-p.finished:
	case <-time.After(timeout):
		t.Fatalf("fake agent pod %s: turn did not finish within %v", p.slug, timeout)
	}
}

func (p *fakeAgentPod) postEvent(slug, turnID string, ev agentpod.TurnEvent) {
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/v1/agent/%s/chat-turn/%s/event", slug, turnID),
		bytes.NewReader(body))
	req.Header.Set(agentpod.SlugHeader, slug)
	rr := httptest.NewRecorder()
	p.srv.handleAgentpodTurnEvent(rr, req)
}

// doneEvent returns a single TurnEventDone with stop=end_turn and the
// supplied usage. Most tests only need the fixed-end variant.
func doneEvent() agentpod.TurnEvent {
	return agentpod.TurnEvent{Kind: agentpod.TurnEventDone, StopReason: "end_turn"}
}

// doneEventWithUsage is doneEvent + usage counts for tests that assert
// on token bookkeeping flowing through to the chat-hub "done" event.
func doneEventWithUsage(in, out int) agentpod.TurnEvent {
	return agentpod.TurnEvent{
		Kind:         agentpod.TurnEventDone,
		StopReason:   "end_turn",
		InputTokens:  in,
		OutputTokens: out,
	}
}

// deltaResponse is the equivalent of immediateEndStream() with text:
// one delta + done.
func deltaResponse(text string) []agentpod.TurnEvent {
	return []agentpod.TurnEvent{
		{Kind: agentpod.TurnEventDelta, Text: text},
		doneEvent(),
	}
}

// memoryToolDispatchResponse runs the agent_memory_* tool dispatch
// against srv.Store (the same path the agent pod's MCP subprocess
// would take over UDS) and returns the TurnEvents for the full
// tool_use_start / tool_use_end / tool_result triple. Used by rotation
// tests where the agent memory has to actually be written.
func memoryToolDispatchResponse(srv *Server, slug, toolName, toolUseID string, input []byte) []agentpod.TurnEvent {
	body, isErr := agent.DispatchAgentMemoryTool(srv.Store, slug, toolName, input)
	return []agentpod.TurnEvent{
		{Kind: agentpod.TurnEventToolUseStart, ToolUseID: toolUseID, ToolName: toolName},
		{Kind: agentpod.TurnEventToolUseEnd, ToolUseID: toolUseID, ToolName: toolName, ToolInput: input},
		{Kind: agentpod.TurnEventToolResult, ToolUseID: toolUseID, ToolResultText: body, ToolResultIsError: isErr},
		doneEvent(),
	}
}

// requestFromTurn unmarshals the inlined provider.CompleteRequest from a
// captured ChatTurnEvent. Tests use this to assert on the payload core
// shipped to the agent runtime — same shape the in-process loop saw on
// its mockFn argument.
func requestFromTurn(t *testing.T, ct agentpod.ChatTurnEvent) provider.CompleteRequest {
	t.Helper()
	var req provider.CompleteRequest
	if err := json.Unmarshal(ct.Request, &req); err != nil {
		t.Fatalf("decode CompleteRequest from chat-turn: %v", err)
	}
	return req
}

// sendMessage posts a chat message and returns the chat-turn the fake
// agent pod received, so the test can inspect the request core built.
func sendMessage(t *testing.T, srv *Server, fake *fakeAgentPod, text string) provider.CompleteRequest {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/agents/alice/messages",
		strings.NewReader("text="+text))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	authedHandler(t, srv).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("post message = %d: %s", rr.Code, rr.Body.String())
	}
	captured := requestFromTurn(t, fake.AwaitTurn(t, 5*time.Second))
	// The fake answers with a done event on its own goroutine; let
	// core finish the turn before the caller asserts and returns, or
	// finalize races the test's TempDir cleanup.
	fake.AwaitFinished(t, 5*time.Second)
	return captured
}
