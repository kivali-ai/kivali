package agentpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ReadSessionID returns the persisted claude CLI session_id for this
// runtime's slug. Empty string when no session has been established
// yet (first-turn case). The claudeagent.Client uses this to decide
// whether to pass --resume.
func (c *Client) ReadSessionID(ctx context.Context) (string, error) {
	url := fmt.Sprintf("http://core/v1/agent/%s/session-id", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("agentpod: ReadSessionID: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("agentpod: ReadSessionID HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("agentpod: decode session id: %w", err)
	}
	return body.SessionID, nil
}

// WriteSessionID persists id as this runtime's claude CLI session_id.
// Empty id is a no-op (the claudeagent runner only writes after the
// first system event arrives). Idempotent on repeat with same id.
func (c *Client) WriteSessionID(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"session_id": id})
	url := fmt.Sprintf("http://core/v1/agent/%s/session-id", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentpod: WriteSessionID: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentpod: WriteSessionID HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// ClearSessionID removes the persisted session_id so the next chat
// turn starts a fresh CLI session. Called on rotation (New chat) and
// on detected stale sessions (corrupt CLI session log).
func (c *Client) ClearSessionID(ctx context.Context) error {
	url := fmt.Sprintf("http://core/v1/agent/%s/session-id", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentpod: ClearSessionID: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentpod: ClearSessionID HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return nil
}

// SessionStore is the slug-bound view of the agentpod Client that
// satisfies claudeagent.SessionStore. claudeagent.SessionStore takes
// a slug arg on each call (single FSStore + N agents); the agentpod
// runtime is bound to one slug for its lifetime, so we capture it.
//
// Construct via Client.SessionStore() and pass to claudeagent.New.
type SessionStore struct{ c *Client }

// SessionStore returns a claudeagent.SessionStore-compatible view of
// this client. Reads/writes round-trip to core via UDS.
func (c *Client) SessionStore() *SessionStore { return &SessionStore{c: c} }

// ReadClaudeSessionID matches claudeagent.SessionStore. The slug arg
// must equal the bound slug; mismatches return an error so a stray
// caller against a different agent's session id surfaces loudly.
func (s *SessionStore) ReadClaudeSessionID(slug string) (string, error) {
	if err := s.checkSlug(slug); err != nil {
		return "", err
	}
	return s.c.ReadSessionID(context.Background())
}

// WriteClaudeSessionID matches claudeagent.SessionStore.
func (s *SessionStore) WriteClaudeSessionID(slug, id string) error {
	if err := s.checkSlug(slug); err != nil {
		return err
	}
	return s.c.WriteSessionID(context.Background(), id)
}

// ClearClaudeSessionID matches claudeagent.SessionStore.
func (s *SessionStore) ClearClaudeSessionID(slug string) error {
	if err := s.checkSlug(slug); err != nil {
		return err
	}
	return s.c.ClearSessionID(context.Background())
}

func (s *SessionStore) checkSlug(slug string) error {
	if slug != "" && slug != s.c.slug {
		return fmt.Errorf("agentpod.SessionStore: slug %q does not match bound %q", slug, s.c.slug)
	}
	return nil
}
