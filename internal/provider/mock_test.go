package provider

import (
	"context"
	"strings"
	"testing"
)

func TestMockCompleteRecordsCalls(t *testing.T) {
	m := &MockClient{
		CompleteFn: func(ctx context.Context, req CompleteRequest) (*CompleteResponse, error) {
			return &CompleteResponse{Content: []ContentBlock{{Type: ContentText, Text: "hi"}}}, nil
		},
	}
	req := CompleteRequest{Model: "claude-opus-4-7", Purpose: "test", Agent: "alice"}
	resp, err := m.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "hi" {
		t.Errorf("content = %+v", resp.Content)
	}
	if len(m.Calls) != 1 || m.Calls[0].Agent != "alice" {
		t.Errorf("Calls = %+v", m.Calls)
	}
}

func TestMockCompleteErrorsIfUnset(t *testing.T) {
	m := &MockClient{}
	_, err := m.Complete(context.Background(), CompleteRequest{Model: "x"})
	if err == nil {
		t.Fatal("expected error when CompleteFn is nil")
	}
}

func TestMockStreamEmits(t *testing.T) {
	events := []StreamEvent{
		{Kind: StreamDelta, Text: "hel"},
		{Kind: StreamDelta, Text: "lo"},
		{Kind: StreamEnd},
	}
	final := &CompleteResponse{Content: []ContentBlock{{Type: ContentText, Text: "hello"}}}
	s := NewMockStream(events, final)
	defer func() { _ = s.Close() }()
	var got strings.Builder
	for ev := range s.Events() {
		if ev.Kind == StreamDelta {
			got.WriteString(ev.Text)
		}
	}
	if got.String() != "hello" {
		t.Errorf("got = %q", got.String())
	}
	if s.Final() != final {
		t.Errorf("Final mismatch")
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err = %v", err)
	}
}
