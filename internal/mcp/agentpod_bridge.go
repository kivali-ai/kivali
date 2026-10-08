package mcp

// agentpod_bridge.go — UDS-backed implementations of every MCP
// dispatcher interface. Used by the in-pod MCP subprocess (Kivali
// agent runtime) which has no FSStore: each adapter forwards the
// call to core via an *agentpod.Client, where the matching
// store-backed dispatcher runs against the authoritative store.
//
// One file per interface would be tidier but the adapters are tiny
// and colocating them lets a reader see the full agent-pod plumb at
// a glance.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/store"
)

// NewAgentpodAgentMemoryDispatcher returns the UDS-backed
// AgentMemoryDispatcher used by the in-pod MCP subprocess.
func NewAgentpodAgentMemoryDispatcher(c *agentpod.Client) AgentMemoryDispatcher {
	return agentpodAgentMemoryDispatcher{c: c}
}

type agentpodAgentMemoryDispatcher struct{ c *agentpod.Client }

func (d agentpodAgentMemoryDispatcher) DispatchAgentMemoryTool(ctx context.Context, name string, raw json.RawMessage) (string, bool, error) {
	resp, err := d.c.MemoryDispatch(ctx, name, raw)
	if err != nil {
		return "", false, err
	}
	return resp.Body, resp.IsError, nil
}

// NewAgentpodProjectFilesLister returns the UDS-backed lister.
func NewAgentpodProjectFilesLister(c *agentpod.Client) ProjectFilesLister {
	return agentpodProjectFilesLister{c: c}
}

type agentpodProjectFilesLister struct{ c *agentpod.Client }

func (l agentpodProjectFilesLister) ListProjectFiles(ctx context.Context) ([]store.ProjectFile, error) {
	return l.c.ListProjectFiles(ctx)
}

// NewAgentpodShareFileDispatcher returns the UDS-backed
// ShareFileDispatcher.
func NewAgentpodShareFileDispatcher(c *agentpod.Client) ShareFileDispatcher {
	return agentpodShareFileDispatcher{c: c}
}

type agentpodShareFileDispatcher struct{ c *agentpod.Client }

func (d agentpodShareFileDispatcher) DispatchShareFile(ctx context.Context, files []ShareFileEntry, caption string) (string, bool, error) {
	wire := make([]agentpod.ShareFileEntry, len(files))
	for i, f := range files {
		wire[i] = agentpod.ShareFileEntry{Path: f.Path, Name: f.Name}
	}
	resp, err := d.c.ShareFileDispatch(ctx, agentpod.ShareFileDispatchRequest{
		Files:   wire,
		Caption: caption,
	})
	if err != nil {
		return "", false, err
	}
	return resp.Body, resp.IsError, nil
}

// NewAgentpodPublishDispatcher returns the UDS-backed PublishDispatcher.
// DispatchPublish issues a /publish/stage followed by /publish/commit,
// retrying commit on transport failure (commit is idempotent on
// StageID, so a retry is safe and won't double-route).
func NewAgentpodPublishDispatcher(c *agentpod.Client) PublishDispatcher {
	return agentpodPublishDispatcher{c: c}
}

type agentpodPublishDispatcher struct {
	c   *agentpod.Client
	clk clock.Clock
}

// clock returns the dispatcher's time source, defaulting to the real
// one. The commit retry is the only thing it drives.
func (d agentpodPublishDispatcher) clock() clock.Clock {
	if d.clk == nil {
		return clock.New()
	}
	return d.clk
}

// commitRetryAttempts and commitRetryBackoff bound how many times the
// bridge will retry CommitPublish on transport failure. Three
// attempts at 50/200/500ms is plenty for a same-node UDS link — if
// three back-to-back round-trips fail, core is genuinely unreachable
// and "fail loudly" beats "hang silently."
//
// Stage is NOT retried at this layer: parse / conflict / attachment
// checks are deterministic, so a stage transport failure either
// surfaces immediately to the model or a different failure mode
// (which the bridge can't fix).
const commitRetryAttempts = 3

var commitRetryBackoff = []time.Duration{
	50 * time.Millisecond,
	200 * time.Millisecond,
	500 * time.Millisecond,
}

func (d agentpodPublishDispatcher) DispatchPublish(ctx context.Context, tool string, raw json.RawMessage) (string, bool, error) {
	stageResp, err := d.c.StagePublish(ctx, tool, raw)
	if err != nil {
		// Stage transport failure → nothing committed; surface to the
		// caller. publishHandler converts this into IsError=true with
		// the error string in the body.
		return "", false, err
	}
	if stageResp.IsError {
		// Deterministic input-side failure (parse error, role
		// conflict, attachment unreachable). No staged record exists;
		// no commit possible. Surface to the model.
		return stageResp.Body, true, nil
	}
	if stageResp.StageID == "" {
		// Defensive: server gave us no stage_id and no error. Treat
		// as a transport-class failure.
		return "", false, errors.New("agentpod: StagePublish returned empty stage_id without IsError")
	}

	var lastErr error
	for attempt := 0; attempt < commitRetryAttempts; attempt++ {
		if attempt > 0 {
			wait := d.clock().NewTimer(commitRetryBackoff[attempt-1])
			select {
			case <-ctx.Done():
				wait.Stop()
				return "", false, ctx.Err()
			case <-wait.C():
			}
		}
		commitResp, cerr := d.c.CommitPublish(ctx, stageResp.StageID)
		if cerr == nil {
			return commitResp.Body, commitResp.IsError, nil
		}
		// ErrCommitStageNotFound means the staged record is gone —
		// expired by janitor or wiped by a fresh core. Retrying
		// won't help; surface immediately.
		if errors.Is(cerr, agentpod.ErrCommitStageNotFound) {
			return "", false, cerr
		}
		lastErr = cerr
	}
	return "", false, fmt.Errorf("agentpod: CommitPublish exhausted %d attempts: %w", commitRetryAttempts, lastErr)
}

// NewAgentpodStateDispatcher returns the UDS-backed StateDispatcher.
func NewAgentpodStateDispatcher(c *agentpod.Client) StateDispatcher {
	return agentpodStateDispatcher{c: c}
}

type agentpodStateDispatcher struct{ c *agentpod.Client }

func (d agentpodStateDispatcher) DispatchStateTool(ctx context.Context, tool string, raw json.RawMessage) (string, bool, error) {
	resp, err := d.c.StateDispatch(ctx, tool, raw)
	if err != nil {
		return "", false, err
	}
	return resp.Body, resp.IsError, nil
}

// NewAgentpodPastChatsReader returns the UDS-backed PastChatsReader.
func NewAgentpodPastChatsReader(c *agentpod.Client) PastChatsReader {
	return agentpodPastChatsReader{c: c}
}

type agentpodPastChatsReader struct{ c *agentpod.Client }

func (r agentpodPastChatsReader) ListArchivedChats(ctx context.Context) ([]store.ArchivedChat, error) {
	return r.c.ListArchivedChats(ctx)
}

func (r agentpodPastChatsReader) ReadArchivedChat(ctx context.Context, ts string) ([]store.ChatMessage, error) {
	return r.c.ReadArchivedChat(ctx, ts)
}
