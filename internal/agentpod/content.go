package agentpod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kivali-ai/kivali/internal/store"
)

// GetAttachment fetches metadata for one attachment by SHA.
func (c *Client) GetAttachment(ctx context.Context, sha string) (store.Attachment, error) {
	var att store.Attachment
	if err := c.getJSON(ctx, fmt.Sprintf("http://core/v1/attachments/%s", sha), &att); err != nil {
		return store.Attachment{}, err
	}
	return att, nil
}

// OpenAttachmentBlob streams the original bytes of an attachment.
// The returned ReadCloser is the raw HTTP response body — caller
// closes when done. MIME isn't on the wire here (the GET endpoint
// returns octet-stream); call GetAttachment for the typed metadata.
//
// Cache: content-addressed (attachments are immutable per SHA). On
// hit, returns a ReadCloser around the cached bytes; on miss,
// fetches + stores then returns. A nil Cache short-circuits to a
// direct stream.
func (c *Client) OpenAttachmentBlob(ctx context.Context, sha string) (io.ReadCloser, error) {
	return c.openSHACached(ctx, sha, "blob",
		fmt.Sprintf("http://core/v1/attachments/%s/blob", sha))
}

// ReadAttachmentText returns the canonical text form of an attachment,
// or "" when no canonical exists (image / unknown binary).
//
// Cache: content-addressed; same shape as OpenAttachmentBlob. The
// "no canonical" case caches an empty body so repeated lookups for
// the same SHA short-circuit (the negative result is also immutable).
func (c *Client) ReadAttachmentText(ctx context.Context, sha string) (string, error) {
	if cached, ok := c.cache.LookupSHA(sha, "canonical"); ok {
		return string(cached), nil
	}
	rc, err := c.openStream(ctx, fmt.Sprintf("http://core/v1/attachments/%s/canonical", sha))
	if err != nil {
		return "", err
	}
	body, err := readAllAndClose(rc)
	if err != nil {
		return "", err
	}
	if c.cache != nil {
		_ = c.cache.StoreSHA(sha, "canonical", body)
	}
	return string(body), nil
}

// AddAttachment uploads bytes as an attachment for this client's
// slug. Returns the resulting Attachment metadata (sha + canonical
// info + size). Slug-scoped because core records which agent
// originated the upload.
func (c *Client) AddAttachment(ctx context.Context, name string, data []byte) (store.Attachment, error) {
	body, _ := json.Marshal(map[string]any{"name": name, "data": data})
	url := fmt.Sprintf("http://core/v1/agent/%s/attachments", c.slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return store.Attachment{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return store.Attachment{}, fmt.Errorf("agentpod: AddAttachment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return store.Attachment{}, fmt.Errorf("agentpod: AddAttachment HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	var att store.Attachment
	if err := json.NewDecoder(resp.Body).Decode(&att); err != nil {
		return store.Attachment{}, fmt.Errorf("agentpod: decode Attachment: %w", err)
	}
	return att, nil
}

// ListProjectFiles fetches the catalog of project files (CEO-uploaded
// reference material). Empty slice when none uploaded.
func (c *Client) ListProjectFiles(ctx context.Context) ([]store.ProjectFile, error) {
	var pfs []store.ProjectFile
	if err := c.getJSON(ctx, "http://core/v1/project-files", &pfs); err != nil {
		return nil, err
	}
	return pfs, nil
}

// GetProjectFile fetches metadata for one project file by SHA.
func (c *Client) GetProjectFile(ctx context.Context, sha string) (store.ProjectFile, error) {
	var pf store.ProjectFile
	if err := c.getJSON(ctx, fmt.Sprintf("http://core/v1/project-files/%s", sha), &pf); err != nil {
		return store.ProjectFile{}, err
	}
	return pf, nil
}

// OpenProjectFileBlob streams the original bytes of a project file.
// Cache: content-addressed (immutable per SHA).
func (c *Client) OpenProjectFileBlob(ctx context.Context, sha string) (io.ReadCloser, error) {
	return c.openSHACached(ctx, sha, "blob",
		fmt.Sprintf("http://core/v1/project-files/%s/blob", sha))
}

// OpenProjectFileCanonical streams the canonical text form of a
// project file. Empty body when no canonical exists.
// Cache: content-addressed.
func (c *Client) OpenProjectFileCanonical(ctx context.Context, sha string) (io.ReadCloser, error) {
	return c.openSHACached(ctx, sha, "canonical",
		fmt.Sprintf("http://core/v1/project-files/%s/canonical", sha))
}

// openSHACached is the shared cache wrapper for content-addressed
// streaming reads. On cache hit, returns a ReadCloser around the
// cached bytes (no transport hop). On miss, fetches from core,
// drains the response into memory, stores, and returns a
// ReadCloser around the captured bytes. A nil cache degrades to a
// direct stream — the caller still gets a working ReadCloser, just
// without the local copy.
//
// Materializing on cache miss costs an extra in-memory copy of the
// blob versus the streaming-passthrough path; that's the price of
// being able to cache. For Kivali's use case (attachments are
// typically <1 MB), this is fine. If a future agent uses very
// large blobs we can switch the miss path to tee-and-store but the
// shape stays the same.
func (c *Client) openSHACached(ctx context.Context, sha, kind, url string) (io.ReadCloser, error) {
	if cached, ok := c.cache.LookupSHA(sha, kind); ok {
		return io.NopCloser(bytes.NewReader(cached)), nil
	}
	rc, err := c.openStream(ctx, url)
	if err != nil {
		return nil, err
	}
	body, err := readAllAndClose(rc)
	if err != nil {
		return nil, err
	}
	if c.cache != nil {
		_ = c.cache.StoreSHA(sha, kind, body)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// getJSON runs a GET expecting a JSON body and decodes into out.
func (c *Client) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agentpod: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("agentpod: GET %s HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("agentpod: decode response: %w", err)
	}
	return nil
}

// openStream runs a GET and returns the raw response body for streaming.
// Caller closes when done. Used for blob + canonical endpoints.
func (c *Client) openStream(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(SlugHeader, c.slug)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentpod: GET %s: %w", url, err)
	}
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("agentpod: GET %s HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}
	return resp.Body, nil
}
