package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/debian"
	"github.com/kivali-ai/kivali/internal/store"
)

// knownPackages is a debian.Checker over a fixed set of names; with
// err set, Debian's list cannot be read.
type knownPackages struct {
	names map[string]bool
	err   error
}

func (k knownPackages) Check(_ context.Context, names []string) ([]debian.Unknown, error) {
	if k.err != nil {
		return nil, k.err
	}
	var out []debian.Unknown
	for _, n := range names {
		if !k.names[n] {
			out = append(out, debian.Unknown{Name: n, Suggestions: []string{"poppler-utils"}})
		}
	}
	return out, nil
}

func (knownPackages) Lookup(names []string) []debian.Package {
	out := make([]debian.Package, 0, len(names))
	for _, n := range names {
		out = append(out, debian.Package{Name: n})
	}
	return out
}

func (knownPackages) Warm(context.Context) {}

// A propose_software_install naming a package Debian does not have is
// refused with what the agent may have meant, and never reaches the
// owner. When Debian's list cannot be read, the request goes through.
func TestPublishProposeSoftwareInstallChecksDebian(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checker knownPackages
		refused bool
	}{
		{"unknown name", knownPackages{names: map[string]bool{"ffmpeg": true}}, true},
		{"list unreadable", knownPackages{err: debian.ErrUnavailable}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			if err := s.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "ceo"}, "role"); err != nil {
				t.Fatalf("create alice: %v", err)
			}
			tools := PublishTools(PublishToolsConfig{
				Dispatcher: NewStorePublishDispatcher(StorePublishDeps{Store: s, From: "alice", Packages: tc.checker}),
				From:       "alice",
			})
			c := startServer(t, tools)
			result := c.call(t, "tools/call", map[string]any{
				"name": "propose_software_install",
				"arguments": map[string]any{
					"title":    "Install software: tools",
					"packages": []string{"ffmpeg", "popler-utils"},
					"reason":   "I need them.",
				},
			})
			var r toolsCallResult
			if err := json.Unmarshal(result, &r); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			msgs, _ := s.ListMessages(store.MessageFilter{Type: store.MsgCEOApprovalRequest})
			if !tc.refused {
				if r.IsError || len(msgs) != 1 {
					t.Fatalf("should publish unchecked; result %+v, messages %+v", r, msgs)
				}
				return
			}
			if !r.IsError || len(r.Content) == 0 {
				t.Fatalf("should be refused; got %+v", r)
			}
			text := r.Content[0].Text
			for _, want := range []string{"Debian 13 has no package called popler-utils. Did you mean poppler-utils?", "Nothing was sent to the owner."} {
				if !strings.Contains(text, want) {
					t.Errorf("refusal %q does not say %q", text, want)
				}
			}
			if len(msgs) != 0 {
				t.Fatalf("a refused request reached the owner: %+v", msgs)
			}
		})
	}
}
