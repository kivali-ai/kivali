package agentpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kivali-ai/kivali/internal/store"
)

// ReadChatHistory fetches the agent's chat.jsonl content from core
// and returns it as a slice of store.ChatMessage. Empty slice (not
// nil) when the agent has no chat history yet — matches the
// in-process Store.ReadChatHistory contract.
//
// Used by any tool handler that needs to walk history
// (search_past_chats, etc.).
func (c *Client) ReadChatHistory(ctx context.Context) ([]store.ChatMessage, error) {
	url := fmt.Sprintf("http://core/v1/agent/%s/chat/history", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: ReadChatHistory: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: ReadChatHistory HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var hist []store.ChatMessage
	if err := json.NewDecoder(resp.Body).Decode(&hist); err != nil {
		return nil, fmt.Errorf("agentpod: decode chat history: %w", err)
	}
	return hist, nil
}

// AppendChatMessage routes one ChatMessage append to core. Used by
// the agent runtime's chat-turn driver to land tool_use / tool_result
// / direct_chat rows on chat.jsonl. The single-writer invariant
// holds because core's handler calls store.AppendChatMessage which
// takes the per-agent chatLock.
func (c *Client) AppendChatMessage(ctx context.Context, msg store.ChatMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("agentpod: marshal ChatMessage: %w", err)
	}
	url := fmt.Sprintf("http://core/v1/agent/%s/chat/append", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentpod: AppendChatMessage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentpod: AppendChatMessage HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// ListArchivedChats fetches the list of archived chat generations
// for this agent. Mirrors store.FSStore.ListArchivedChats — returns
// (Slug, Timestamp) per chat, no message content.
func (c *Client) ListArchivedChats(ctx context.Context) ([]store.ArchivedChat, error) {
	url := fmt.Sprintf("http://core/v1/agent/%s/past-chats", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: ListArchivedChats: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: ListArchivedChats HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out []store.ArchivedChat
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agentpod: decode archived chats: %w", err)
	}
	return out, nil
}

// ReadArchivedChat fetches the full chat.jsonl content of one
// archived generation by timestamp. Mirrors
// store.FSStore.ReadArchivedChat. ts comes from one of the entries
// returned by ListArchivedChats.
func (c *Client) ReadArchivedChat(ctx context.Context, ts string) ([]store.ChatMessage, error) {
	url := fmt.Sprintf("http://core/v1/agent/%s/past-chats/%s", c.slug, ts)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: ReadArchivedChat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: ReadArchivedChat HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out []store.ChatMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agentpod: decode archived chat: %w", err)
	}
	return out, nil
}

// ReadRole fetches the agent's role.md content from core. Cached
// path-addressed; ETag/If-None-Match short-circuits unchanged
// reads. Local writes (WriteRole) invalidate the cache.
func (c *Client) ReadRole(ctx context.Context) (string, error) {
	return c.getStringCached(ctx, roleCacheKey(c.slug),
		fmt.Sprintf("http://core/v1/agent/%s/role", c.slug))
}

// WriteRole replaces the agent's role.md content via core. The
// cached entry is dropped — the next ReadRole will refetch
// (current store contents may differ from the body we just sent
// after server-side normalization, so we don't pre-populate).
func (c *Client) WriteRole(ctx context.Context, body string) error {
	if err := c.postString(ctx, fmt.Sprintf("http://core/v1/agent/%s/role", c.slug), map[string]string{"body": body}); err != nil {
		return err
	}
	c.cache.InvalidatePath(roleCacheKey(c.slug))
	return nil
}

// ReadMemory fetches the agent's agent_memory.md content from core.
// Cached path-addressed; mutations via WriteMemory / AppendMemory /
// MemoryDispatch invalidate the entry.
func (c *Client) ReadMemory(ctx context.Context) (string, error) {
	return c.getStringCached(ctx, memoryCacheKey(c.slug),
		fmt.Sprintf("http://core/v1/agent/%s/memory", c.slug))
}

// WriteMemory replaces the agent's agent_memory.md content via core.
func (c *Client) WriteMemory(ctx context.Context, body string) error {
	if err := c.postString(ctx, fmt.Sprintf("http://core/v1/agent/%s/memory", c.slug), map[string]any{"body": body}); err != nil {
		return err
	}
	c.cache.InvalidatePath(memoryCacheKey(c.slug))
	return nil
}

// AppendMemory appends to the agent's agent_memory.md via core.
// Mirrors store.AppendAgentMemory's semantics — newline-terminated
// concatenation.
func (c *Client) AppendMemory(ctx context.Context, body string) error {
	if err := c.postString(ctx, fmt.Sprintf("http://core/v1/agent/%s/memory", c.slug), map[string]any{"body": body, "append": true}); err != nil {
		return err
	}
	c.cache.InvalidatePath(memoryCacheKey(c.slug))
	return nil
}

// roleCacheKey / memoryCacheKey produce the canonical cache key for
// the per-slug role.md / agent_memory.md entries. Centralized so
// the runtime's cache-invalidate handler can drop the same key
// shape from a server-pushed event.
func roleCacheKey(slug string) string   { return "agent/" + slug + "/role" }
func memoryCacheKey(slug string) string { return "agent/" + slug + "/memory" }

// ShareFileDispatch routes a share_file tool invocation to core.
// The in-pod MCP forwards (sha, file_path, caption, display_name);
// core resolves the attachment, appends the chat-bubble row to
// chat.jsonl, and returns the rendered ack body.
func (c *Client) ShareFileDispatch(ctx context.Context, req ShareFileDispatchRequest) (*ShareFileDispatchResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: marshal ShareFileDispatchRequest: %w", err)
	}
	url := fmt.Sprintf("http://core/v1/agent/%s/share-file/dispatch", c.slug)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("agentpod: build ShareFileDispatch request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("agentpod: ShareFileDispatch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: ShareFileDispatch HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out ShareFileDispatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agentpod: decode ShareFileDispatchResponse: %w", err)
	}
	return &out, nil
}

// ArtifactDispatch routes one artifact_publish / artifact_unpublish
// call to core, which copies or removes the files and answers with
// what the knowledge graph made of them.
func (c *Client) ArtifactDispatch(ctx context.Context, req ArtifactDispatchRequest) (*ArtifactDispatchResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: marshal ArtifactDispatchRequest: %w", err)
	}
	url := fmt.Sprintf("http://core/v1/agent/%s/artifact/dispatch", c.slug)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("agentpod: build ArtifactDispatch request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("agentpod: ArtifactDispatch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: ArtifactDispatch HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out ArtifactDispatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agentpod: decode ArtifactDispatchResponse: %w", err)
	}
	return &out, nil
}

// StagePublish runs phase 1 of the two-phase publish: parse + role-
// conflict check + attachment resolve, persisted as a staged-publish
// record. The returned StageID is consumed by a subsequent
// CommitPublish call.
//
// Stage failures (parse error, role conflict, unreachable attachment)
// surface as IsError=true on the response with no StageID. The bridge
// surfaces the body to the model directly without retry — the inputs
// are deterministic and won't parse differently on retry.
//
// Transport failures return a non-nil error. The bridge SHOULD treat
// stage transport failures as "definitely not committed" and either
// surface to the model OR retry the stage (parse is deterministic, so
// at most one retry is meaningful before giving up).
func (c *Client) StagePublish(ctx context.Context, tool string, input json.RawMessage) (*PublishStageResponse, error) {
	body, err := json.Marshal(PublishStageRequest{Tool: tool, Input: input})
	if err != nil {
		return nil, fmt.Errorf("agentpod: marshal PublishStageRequest: %w", err)
	}
	url := fmt.Sprintf("http://core/v1/agent/%s/publish/stage", c.slug)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("agentpod: build StagePublish request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("agentpod: StagePublish: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: StagePublish HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out PublishStageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agentpod: decode PublishStageResponse: %w", err)
	}
	return &out, nil
}

// ErrCommitStageNotFound is returned by CommitPublish when the server
// responds with 404 — the staged-publish record is missing (typically
// expired by the janitor, or a stale bridge against a fresh core).
// The bridge MUST treat this as "definitely not committed" and either
// re-stage or surface a failure to the model. Distinct from generic
// transport errors so the bridge's retry loop can short-circuit.
var ErrCommitStageNotFound = errors.New("agentpod: stage_id not found at commit time")

// CommitPublish runs phase 2 of the two-phase publish: the server
// writes the message file + atomically updates message_queue.json
// (appending the inbox entry AND recording stageID in
// CommittedStageIDs under one lock acquisition). Idempotent on
// stageID — safe to retry on any transport failure short of
// ErrCommitStageNotFound.
//
// On 200: returns the rendered tool_result body + IsError flag.
// On 404: returns ErrCommitStageNotFound.
// On 5xx / transport error: returns a wrapped error. Retry safe.
func (c *Client) CommitPublish(ctx context.Context, stageID string) (*PublishCommitResponse, error) {
	body, err := json.Marshal(PublishCommitRequest{StageID: stageID})
	if err != nil {
		return nil, fmt.Errorf("agentpod: marshal PublishCommitRequest: %w", err)
	}
	url := fmt.Sprintf("http://core/v1/agent/%s/publish/commit", c.slug)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("agentpod: build CommitPublish request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("agentpod: CommitPublish: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrCommitStageNotFound
	}
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: CommitPublish HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out PublishCommitResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agentpod: decode PublishCommitResponse: %w", err)
	}
	return &out, nil
}

// StateDispatch routes one state-tool invocation (get_org_chart /
// list_skills / read_agent_role / the graph reads / the assignment tools)
// to core. The server runs the rendering logic
// against the authoritative store and returns the model-visible
// body. Tool-level failures (CoS-only gate, redact-target not
// pending, unknown tool) ride on IsError; non-2xx HTTP indicates
// transport.
func (c *Client) StateDispatch(ctx context.Context, tool string, input json.RawMessage) (*StateDispatchResponse, error) {
	body, err := json.Marshal(StateDispatchRequest{Tool: tool, Input: input})
	if err != nil {
		return nil, fmt.Errorf("agentpod: marshal StateDispatchRequest: %w", err)
	}
	url := fmt.Sprintf("http://core/v1/agent/%s/state/dispatch", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("agentpod: build StateDispatch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: StateDispatch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: StateDispatch HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out StateDispatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agentpod: decode StateDispatchResponse: %w", err)
	}
	return &out, nil
}

// MemoryDispatch routes one agent_memory_* tool invocation to core.
// The in-pod MCP handler doesn't mutate
// agent_memory.md locally (no /data mount); it forwards (tool, input)
// here, and core applies the change under its single-writer lock.
//
// Returns Body / IsError on every 2xx response — IsError captures
// tool-level failures (validation, str-replace not-found / multiple
// matches). Non-2xx surfaces as an error.
func (c *Client) MemoryDispatch(ctx context.Context, tool string, input json.RawMessage) (*MemoryDispatchResponse, error) {
	body, err := json.Marshal(MemoryDispatchRequest{Tool: tool, Input: input})
	if err != nil {
		return nil, fmt.Errorf("agentpod: marshal MemoryDispatchRequest: %w", err)
	}
	url := fmt.Sprintf("http://core/v1/agent/%s/memory/dispatch", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("agentpod: build MemoryDispatch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: MemoryDispatch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: MemoryDispatch HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out MemoryDispatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("agentpod: decode MemoryDispatchResponse: %w", err)
	}
	// Tool-level "is_error" responses didn't actually mutate the
	// store (validation failed), so the cache is still valid. Only
	// invalidate on a successful mutation.
	if !out.IsError {
		c.cache.InvalidatePath(memoryCacheKey(c.slug))
	}
	return &out, nil
}

// getStringCached is the cache-aware sibling of getString for the
// {"body": "..."} JSON endpoints (role, memory). On cache hit, it
// sends the stored ETag as If-None-Match and on 304 returns the
// cached body without re-decoding the envelope; on 200 it stores
// the fresh body + new ETag.
//
// Cache key is the caller-supplied logical key (e.g. "agent/<slug>/role")
// so cross-call invalidation has a stable handle. The HTTP URL is
// the wire address — same string we'd hit without the cache.
func (c *Client) getStringCached(ctx context.Context, key, url string) (string, error) {
	cachedBody, cachedETag, hit := c.cache.LookupPath(key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(SlugHeader, c.slug)
	if hit && cachedETag != "" {
		req.Header.Set("If-None-Match", cachedETag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("agentpod: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified && hit {
		return string(cachedBody), nil
	}
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("agentpod: GET %s HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var out struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("agentpod: decode body: %w", err)
	}
	if c.cache != nil {
		etag := resp.Header.Get("ETag")
		if etag == "" {
			// Fallback: the server didn't emit one. Compute it
			// locally so future If-None-Match still gets a value.
			etag = ETag([]byte(out.Body))
		}
		_ = c.cache.StorePath(key, etag, []byte(out.Body))
	}
	return out.Body, nil
}

// postString runs a POST with the given JSON body. Helper for the
// simple write endpoints (role, memory).
func (c *Client) postString(ctx context.Context, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("agentpod: marshal body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentpod: POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentpod: POST %s HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}
