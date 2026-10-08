package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// ListProjectFilesToolName is the registered MCP name for the rich
// project-files catalog.
const ListProjectFilesToolName = "list_project_files"

// ProjectFilesLister abstracts the read side of project files for
// the list_project_files tool. agentpod.Client.ListProjectFiles
// satisfies it directly; *store.FSStore is wrapped via
// NewStoreProjectFilesLister (its ListProjectFiles takes no ctx).
type ProjectFilesLister interface {
	ListProjectFiles(ctx context.Context) ([]store.ProjectFile, error)
}

// NewStoreProjectFilesLister wraps *store.FSStore so it satisfies
// ProjectFilesLister. The FSStore method is synchronous, ignores ctx,
// and just walks the on-disk index.
func NewStoreProjectFilesLister(s *store.FSStore) ProjectFilesLister {
	return storeProjectFilesLister{s: s}
}

type storeProjectFilesLister struct {
	s *store.FSStore
}

func (l storeProjectFilesLister) ListProjectFiles(_ context.Context) ([]store.ProjectFile, error) {
	return l.s.ListProjectFiles()
}

// ListProjectFilesTool returns a tool that lists project files with
// their one-sentence Haiku-generated summaries, so agents can triage
// the catalog without loading every file.
//
// file_view /files/project/ also works and returns just the
// filenames (no summaries) — that's the fallback when an agent
// already knows which file they want. list_project_files is the
// browse path: "what's available, and what's each one about?"
//
// Summaries are generated asynchronously after upload. A file with
// no summary shows "(no summary yet)" — either the generator hasn't
// run yet, the file has no extractable text (image/binary), or the
// Haiku call errored. Either way the catalog still works.
func ListProjectFilesTool(l ProjectFilesLister) Tool {
	return Tool{
		Name:        ListProjectFilesToolName,
		Description: listProjectFilesDescription,
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		ReadOnly:    true,
		Handler:     listProjectFilesHandler(l),
	}
}

func listProjectFilesHandler(l ProjectFilesLister) func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
	return func(ctx context.Context, raw json.RawMessage) (*ToolResult, error) {
		pfs, err := l.ListProjectFiles(ctx)
		if err != nil {
			return &ToolResult{IsError: true, Content: []string{"list project files: " + err.Error()}}, nil
		}
		return &ToolResult{Content: []string{renderProjectFilesListing(pfs)}}, nil
	}
}

func renderProjectFilesListing(pfs []store.ProjectFile) string {
	if len(pfs) == 0 {
		return "No project files uploaded yet. The owner can upload via /settings/files."
	}
	sort.Slice(pfs, func(i, j int) bool { return pfs[i].OriginalName < pfs[j].OriginalName })

	// Three tiers:
	//  - Files with a summary (text-readable via file_view OR
	//    images summarized via Haiku vision) — main listing.
	//  - Images without a summary yet (just uploaded, summarizer
	//    still running) — main listing with placeholder.
	//  - Other binary / unknown (no canonical, not an image) — a
	//    separate "not viewable" section at the bottom.
	var listed, unviewable []store.ProjectFile
	for _, f := range pfs {
		if f.CanonicalName != "" || files.IsImageMIME(f.MIME) {
			listed = append(listed, f)
		} else {
			unviewable = append(unviewable, f)
		}
	}

	var b strings.Builder
	b.WriteString("Project files available. Open a file with `file_view /files/project/<name>` — text files come back as text, images (PNG/JPEG/GIF/WebP) come back as a vision content block (Claude reads the pixels directly).\n")
	b.WriteString("\n")
	for _, f := range listed {
		summary := strings.TrimSpace(f.Summary)
		if summary == "" {
			summary = "(no summary yet)"
		}
		marker := ""
		if files.IsImageMIME(f.MIME) {
			marker = " [image]"
		}
		fmt.Fprintf(&b, "  - `%s`%s — %s\n", f.OriginalName, marker, summary)
	}
	if len(unviewable) > 0 {
		b.WriteString("\nOpaque files (no extractable text — not viewable via file_view, but reachable from run_shell at /files/project/<name>):\n")
		for _, f := range unviewable {
			fmt.Fprintf(&b, "  - `%s` (%s)\n", f.OriginalName, f.MIME)
		}
	}
	return b.String()
}

// Description kept verbatim aligned with internal/agent/state_tools.go.
const listProjectFilesDescription = `List the project files available to you, with a one-sentence summary per file. No input parameters. Use to triage the catalog without loading everything; open a specific file with file_view /files/project/<filename>. Output is grouped: text and image files first (with summaries), then opaque binaries (not viewable via file_view, but reachable from run_shell at /files/project/<name>).`
