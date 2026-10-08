package agentpod

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/kivali-ai/kivali/internal/store"
)

// ListSkills fetches the catalog of skills (app-wide; not slug-scoped).
// Not cached — listings tend to drift, and the per-call cost is small
// (no large blobs in the response).
func (c *Client) ListSkills(ctx context.Context) ([]store.Skill, error) {
	var skills []store.Skill
	if err := c.getJSON(ctx, "http://core/v1/skills", &skills); err != nil {
		return nil, err
	}
	return skills, nil
}

// ReadSkill fetches one skill's full metadata + sorted file tree.
// Not cached — same rationale as ListSkills (metadata, no blobs).
func (c *Client) ReadSkill(ctx context.Context, name string) (store.Skill, error) {
	var sk store.Skill
	if err := c.getJSON(ctx, fmt.Sprintf("http://core/v1/skills/%s", name), &sk); err != nil {
		return store.Skill{}, err
	}
	return sk, nil
}

// ReadSkillManifest fetches the raw SKILL.md body of one skill. Used
// by the system-prompt builder to surface description + when_to_use.
//
// Cached path-addressed: ETag/If-None-Match short-circuits unchanged
// manifests (hot path during system-prompt assembly).
func (c *Client) ReadSkillManifest(ctx context.Context, name string) (string, error) {
	body, err := c.getBytesCached(ctx,
		skillManifestCacheKey(name),
		fmt.Sprintf("http://core/v1/skills/%s/manifest", name))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ReadSkillFile fetches the raw bytes of one file inside a skill
// directory. rel is forward-slash relative to the skill root
// (e.g. "scripts/foo.sh"). The cache layer materializes the bytes
// locally under /scratch/cache/path/... on demand so run_shell
// can exec them without re-fetching every call.
//
// Cached path-addressed: ETag/If-None-Match.
func (c *Client) ReadSkillFile(ctx context.Context, name, rel string) ([]byte, error) {
	return c.getBytesCached(ctx,
		skillFileCacheKey(name, rel),
		fmt.Sprintf("http://core/v1/skills/%s/files/%s", name, rel))
}

// skillManifestCacheKey / skillFileCacheKey are the canonical cache
// keys for skill resources. Centralized so the runtime's
// cache-invalidate handler can drop the same shape on a server
// invalidation event.
func skillManifestCacheKey(name string) string { return "skills/" + name + "/manifest" }
func skillFileCacheKey(name, rel string) string {
	return "skills/" + name + "/files/" + rel
}

// getBytesCached is the cache-aware reader for raw-body endpoints
// (skill manifest, skill file). Sends If-None-Match when the cache
// has an entry for key; on 304 returns the cached body, on 200
// stores the fresh body + ETag.
func (c *Client) getBytesCached(ctx context.Context, key, url string) ([]byte, error) {
	cachedBody, cachedETag, hit := c.cache.LookupPath(key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(SlugHeader, c.slug)
	if hit && cachedETag != "" {
		req.Header.Set("If-None-Match", cachedETag)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified && hit {
		return cachedBody, nil
	}
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("agentpod: GET %s HTTP %d: %s", url, resp.StatusCode, string(errBody))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("agentpod: read response: %w", err)
	}
	if c.cache != nil {
		etag := resp.Header.Get("ETag")
		if etag == "" {
			etag = ETag(body)
		}
		_ = c.cache.StorePath(key, etag, body)
	}
	return body, nil
}
