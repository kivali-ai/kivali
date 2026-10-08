package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
)

// ArtifactRequest is one artifact_publish (Source, Dest) or
// artifact_unpublish (Path) call.
type ArtifactRequest struct {
	Source string
	Dest   string
	Path   string
}

// ArtifactDispatcher executes artifact_publish and artifact_unpublish.
// Core is the only writer of the published trees, so the production
// implementation forwards to it over the agent pod's UDS; the body is
// core's rendered reply.
type ArtifactDispatcher interface {
	DispatchArtifact(ctx context.Context, tool string, req ArtifactRequest) (body string, isError bool, err error)
}

// NewAgentpodArtifactDispatcher returns the UDS-backed dispatcher.
// subagentID is "" for the durable agent and the subagent's id for a
// subagent: its client is bound to the parent's slug, and core needs
// the id to find the view its source paths are in.
func NewAgentpodArtifactDispatcher(c *agentpod.Client, subagentID string) ArtifactDispatcher {
	return agentpodArtifactDispatcher{c: c, subagentID: subagentID}
}

type agentpodArtifactDispatcher struct {
	c          *agentpod.Client
	subagentID string
}

func (d agentpodArtifactDispatcher) DispatchArtifact(ctx context.Context, tool string, req ArtifactRequest) (string, bool, error) {
	resp, err := d.c.ArtifactDispatch(ctx, agentpod.ArtifactDispatchRequest{
		Tool:       tool,
		SubagentID: d.subagentID,
		Source:     req.Source,
		Dest:       req.Dest,
		Path:       req.Path,
	})
	if err != nil {
		return "", false, err
	}
	return resp.Body, resp.IsError, nil
}

// ArtifactTools returns artifact_publish and artifact_unpublish, or nil
// without a dispatcher.
func ArtifactTools(d ArtifactDispatcher) []Tool {
	if d == nil {
		return nil
	}
	pub := agent.ArtifactPublishTool()
	unpub := agent.ArtifactUnpublishTool()
	return []Tool{
		{
			Name:        pub.Name,
			Description: pub.Description,
			InputSchema: pub.InputSchema,
			Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
				var in struct {
					Source string `json:"source"`
					Dest   string `json:"dest"`
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return &ToolResult{IsError: true, Content: []string{"invalid artifact_publish input: " + err.Error()}}, nil
				}
				if strings.TrimSpace(in.Source) == "" {
					return &ToolResult{IsError: true, Content: []string{"artifact_publish: source is required"}}, nil
				}
				return artifactResult(d.DispatchArtifact(ctx, pub.Name, ArtifactRequest{Source: in.Source, Dest: in.Dest}))
			},
		},
		{
			Name:        unpub.Name,
			Description: unpub.Description,
			InputSchema: unpub.InputSchema,
			Handler: func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
				var in struct {
					Path string `json:"path"`
				}
				if err := json.Unmarshal(raw, &in); err != nil {
					return &ToolResult{IsError: true, Content: []string{"invalid artifact_unpublish input: " + err.Error()}}, nil
				}
				if strings.TrimSpace(in.Path) == "" {
					return &ToolResult{IsError: true, Content: []string{"artifact_unpublish: path is required"}}, nil
				}
				return artifactResult(d.DispatchArtifact(ctx, unpub.Name, ArtifactRequest{Path: in.Path}))
			},
		},
	}
}

func artifactResult(body string, isErr bool, err error) (*ToolResult, error) {
	if err != nil {
		return &ToolResult{IsError: true, Content: []string{err.Error()}}, nil
	}
	return &ToolResult{IsError: isErr, Content: []string{body}}, nil
}
