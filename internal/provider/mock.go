package provider

import (
	"context"
	"errors"
	"sync"
)

// MockClient is a test double for Client. Scripted behavior is set via
// the *Fn fields; unset fields return an error when called.
//
// MockClient is safe for concurrent callers.
type MockClient struct {
	CompleteFn    func(ctx context.Context, req CompleteRequest) (*CompleteResponse, error)
	StreamFn      func(ctx context.Context, req CompleteRequest) (Stream, error)
	RunSubagentFn func(ctx context.Context, req SubagentRequest) (Stream, error)

	// HandlesLoop toggles the capability flag reported by
	// HandlesToolLoop(). Defaults to false so tests drive the tool
	// loop themselves; the CLI driver reports true.
	HandlesLoop bool

	mu    sync.Mutex
	Calls []CompleteRequest // every request passed to Complete, in order

	// SubagentCalls is every request passed to RunSubagent, in order.
	SubagentCalls []SubagentRequest
}

// RunSubagent returns the scripted subagent stream and records the
// request.
func (m *MockClient) RunSubagent(ctx context.Context, req SubagentRequest) (Stream, error) {
	m.mu.Lock()
	m.SubagentCalls = append(m.SubagentCalls, req)
	m.mu.Unlock()
	if m.RunSubagentFn == nil {
		return nil, errors.New("provider.MockClient: RunSubagentFn not set")
	}
	return m.RunSubagentFn(ctx, req)
}

// SubagentRequests returns a copy of the requests RunSubagent saw.
func (m *MockClient) SubagentRequests() []SubagentRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SubagentRequest(nil), m.SubagentCalls...)
}

// Complete returns the scripted response and records the request.
func (m *MockClient) Complete(ctx context.Context, req CompleteRequest) (*CompleteResponse, error) {
	m.mu.Lock()
	m.Calls = append(m.Calls, req)
	m.mu.Unlock()
	if m.CompleteFn == nil {
		return nil, errors.New("provider.MockClient: CompleteFn not set")
	}
	return m.CompleteFn(ctx, req)
}

// Stream returns the scripted stream.
func (m *MockClient) Stream(ctx context.Context, req CompleteRequest) (Stream, error) {
	m.mu.Lock()
	m.Calls = append(m.Calls, req)
	m.mu.Unlock()
	if m.StreamFn == nil {
		return nil, errors.New("provider.MockClient: StreamFn not set")
	}
	return m.StreamFn(ctx, req)
}

// FormatUsage returns an empty display for mock tests. Real transports
// populate fields meaningful to their transport.
func (m *MockClient) FormatUsage(u TokenUsage) UsageDisplay {
	return UsageDisplay{}
}

// HandlesToolLoop returns the scripted HandlesLoop flag.
func (m *MockClient) HandlesToolLoop() bool { return m.HandlesLoop }

// MockStream is a simple Stream implementation used by tests. Events are
// delivered in order; Final() returns the response passed at construction.
type MockStream struct {
	events []StreamEvent
	final  *CompleteResponse
	err    error
	ch     chan StreamEvent
	once   sync.Once
	done   chan struct{}
}

// NewMockStream returns a Stream that emits events in order and terminates
// with final (which may be nil).
func NewMockStream(events []StreamEvent, final *CompleteResponse) *MockStream {
	s := &MockStream{
		events: events,
		final:  final,
		ch:     make(chan StreamEvent, len(events)+1),
		done:   make(chan struct{}),
	}
	for _, e := range events {
		s.ch <- e
	}
	close(s.ch)
	return s
}

// NewMockStreamErr returns a Stream that emits events in order, then
// reports err from Err(). Used to test caller behavior on stream
// failure modes — most importantly subprocess-death (wrap err with
// ErrSubprocessExited and assert callers handle the disruption path).
func NewMockStreamErr(events []StreamEvent, err error) *MockStream {
	s := NewMockStream(events, nil)
	s.err = err
	return s
}

// Events returns the scripted event channel.
func (s *MockStream) Events() <-chan StreamEvent { return s.ch }

// Err returns the scripted error.
func (s *MockStream) Err() error { return s.err }

// Close closes the stream. Safe to call multiple times.
func (s *MockStream) Close() error {
	s.once.Do(func() { close(s.done) })
	return nil
}

// Final returns the scripted final response.
func (s *MockStream) Final() *CompleteResponse { return s.final }
