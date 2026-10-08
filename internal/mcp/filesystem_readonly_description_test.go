package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
)

// nopFilesDispatcher satisfies FilesDispatcher for tests that only
// inspect tool definitions and never invoke a handler.
type nopFilesDispatcher struct{}

func (nopFilesDispatcher) DispatchFilesTool(context.Context, string, json.RawMessage) (string, bool, *FilesImage, error) {
	return "", false, nil, nil
}

// The MCP twin of TestFileCreateDescriptionNamesEveryReadOnlyRoot in
// internal/agent. The two transports must tell an agent the same
// read-only set — an agent on the pod path would otherwise be handed a
// different list than one on the native path, and only one of them
// would be right. Driven off files.DefaultReadOnlyRoots so a new
// subtree fails here until the description names it.
func TestMCPFileCreateDescriptionNamesEveryReadOnlyRoot(t *testing.T) {
	var desc string
	for _, tool := range FilesystemTools(nopFilesDispatcher{}) {
		if tool.Name == files.ToolCreate {
			desc = tool.Description
			break
		}
	}
	if desc == "" {
		t.Fatalf("no %s tool found in FilesystemTools()", files.ToolCreate)
	}
	for _, root := range files.DefaultReadOnlyRoots {
		if !strings.Contains(desc, root) {
			t.Errorf("%s description does not list read-only root %q; the agent is told a read-only set that omits a subtree its writes will be rejected from", files.ToolCreate, root)
		}
	}
}

// The MCP twin of TestFilesystemSurfacesNameTheSharedWorkspace. The
// shared workspace is writable by omission from DefaultReadOnlyRoots,
// so nothing drives it into these descriptions automatically, and
// file_create must not say the two artifact roots are the only
// writable ones.
func TestMCPFileToolDescriptionsNameTheSharedWorkspace(t *testing.T) {
	for _, tool := range FilesystemTools(nopFilesDispatcher{}) {
		switch tool.Name {
		case files.ToolCreate, files.ToolCopy:
			if !strings.Contains(tool.Description, files.SharedWorkspaceDir+"/") {
				t.Errorf("%s description does not name %s/ as writable; the agent is told a writable set that omits it", tool.Name, files.SharedWorkspaceDir)
			}
		}
	}
}
