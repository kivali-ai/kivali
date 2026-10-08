package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kivali-ai/kivali/internal/store"
)

// ProjectFilesResourceProvider exposes the Kivali project_files/
// corpus as MCP resources. URIs are "kivali://project/<original_name>"
// and resolve to the canonical form on disk (text canonical if
// available, otherwise the original bytes for things like image PDFs
// that the converter left as-is).
//
// Project PDFs land on disk once, and the MCP server serves them to
// whichever transport consumes them, with no Files-API round-trip. Text files are still reachable via
// file_view; resources give binaries and larger messages a clean
// read path without being inlined into the system prompt.
type ProjectFilesResourceProvider struct {
	Store *store.FSStore
}

// List returns every project file as a resource entry. We use the
// ORIGINAL filename as the human-readable name and as the URI
// suffix, because that's what agents cite when discussing project
// context. Collisions across distinct SHAs are disambiguated by
// using the SHA as a fallback when names repeat.
func (p *ProjectFilesResourceProvider) List(ctx context.Context) ([]ResourceDescriptor, error) {
	files, err := p.Store.ListProjectFiles()
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	out := make([]ResourceDescriptor, 0, len(files))
	for _, f := range files {
		name := f.OriginalName
		if _, dup := seen[name]; dup {
			name = f.SHA[:8] + "-" + name
		}
		seen[name] = struct{}{}
		out = append(out, ResourceDescriptor{
			URI:         resourceURIFor(name),
			Name:        name,
			Description: fmt.Sprintf("Project file (%s)", mimeForFile(f)),
			MIMEType:    mimeForFile(f),
			Size:        f.Size,
		})
	}
	return out, nil
}

// Read resolves a project URI to its canonical bytes. Text canonical
// when available (.md / .csv / extracted .txt); otherwise the
// original bytes (raw PDF, image, etc.) for the client to consume
// natively.
func (p *ProjectFilesResourceProvider) Read(ctx context.Context, uri string) (*ResourceContents, error) {
	name := resourceNameFromURI(uri)
	if name == "" {
		return nil, fmt.Errorf("unknown resource uri: %q", uri)
	}
	// Deal with our disambiguation prefix ("<short-sha>-<name>"),
	// which List emits on collisions.
	files, err := p.Store.ListProjectFiles()
	if err != nil {
		return nil, err
	}
	var pf *store.ProjectFile
	for i := range files {
		f := files[i]
		if f.OriginalName == name {
			pf = &f
			break
		}
		if len(f.SHA) >= 8 && f.SHA[:8]+"-"+f.OriginalName == name {
			pf = &f
			break
		}
	}
	if pf == nil {
		return nil, fmt.Errorf("resource not found: %s", uri)
	}
	// Prefer canonical text when the converter left one; fall back
	// to the original bytes.
	dir := filepath.Join(p.Store.Root(), "project_files", pf.SHA)
	if pf.CanonicalName != "" && !strings.EqualFold(filepath.Ext(pf.CanonicalName), ".pdf") {
		// Text-shaped canonical: return as text.
		body, rerr := os.ReadFile(filepath.Join(dir, pf.CanonicalName))
		if rerr == nil {
			return &ResourceContents{
				URI:      uri,
				MIMEType: mimeForFile(*pf),
				Text:     string(body),
			}, nil
		}
	}
	// Fall back to original bytes (PDF / image / etc.).
	origName := "original" + pf.OriginalExt
	body, rerr := os.ReadFile(filepath.Join(dir, origName))
	if rerr != nil {
		return nil, rerr
	}
	return &ResourceContents{
		URI:      uri,
		MIMEType: mimeForFile(*pf),
		Blob:     body,
	}, nil
}

const resourceURIPrefix = "kivali://project/"

func resourceURIFor(name string) string {
	return resourceURIPrefix + name
}

func resourceNameFromURI(uri string) string {
	return strings.TrimPrefix(uri, resourceURIPrefix)
}

func mimeForFile(f store.ProjectFile) string {
	if f.MIME != "" {
		return f.MIME
	}
	if f.CanonicalName != "" && strings.HasSuffix(strings.ToLower(f.CanonicalName), ".md") {
		return "text/markdown"
	}
	if f.CanonicalName != "" && strings.HasSuffix(strings.ToLower(f.CanonicalName), ".txt") {
		return "text/plain"
	}
	return "application/octet-stream"
}
