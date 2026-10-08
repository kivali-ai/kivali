package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/devshell"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/mcp"
)

// Where an MCP server's run_shell and file_* tools execute.
//
// In an agent pod, DEV_SHELL_SOCKET names the dev-shell sidecar's
// socket, and both execute there: the sidecar is the only container
// that mounts the agent's /files/. Running them in this process
// instead, against a local tree, is for tests and fixtures only, and
// only when LocalFilesEnv asks for it (and the tree exists). A pod
// whose spec lost the socket variable refuses to start, naming it,
// rather than answering every file call from whatever tree this
// container happens to have.

// LocalFilesEnv, set to "1", runs run_shell and the file_* tools in
// this process against a local tree when DEV_SHELL_SOCKET is not set.
const LocalFilesEnv = "KIVALI_MCP_LOCAL_FILES"

// LocalFilesRootEnv names the durable agent's local tree when
// LocalFilesEnv asks for one and it is not /files: a test on a host
// that has no /files (the web e2e suite) points it at a directory of
// its own.
const LocalFilesRootEnv = "KIVALI_MCP_LOCAL_FILES_ROOT"

// localFilesRequested reports whether LocalFilesEnv asks for the
// local tree.
func localFilesRequested() bool { return os.Getenv(LocalFilesEnv) == "1" }

// agentFilesRoot is the durable agent's /files/: LocalFilesRootEnv
// when the local tree is asked for and it is set.
func agentFilesRoot() string {
	if root := os.Getenv(LocalFilesRootEnv); root != "" && localFilesRequested() {
		return root
	}
	return files.ModelRootPath
}

// fileToolsRefusal is why there is nothing to run the file tools
// against: no sidecar, and either no local tree asked for or none
// there.
func fileToolsRefusal(local bool, root string) error {
	if !local {
		return fmt.Errorf("DEV_SHELL_SOCKET is not set: run_shell and the file_* tools execute in the dev-shell sidecar (%s=1 runs them in this process against a local tree, for tests)", LocalFilesEnv)
	}
	return fmt.Errorf("DEV_SHELL_SOCKET is not set and %s=1, but %s does not exist: there is no local tree to run run_shell and the file_* tools against", LocalFilesEnv, root)
}

// localTreeExists reports whether root is a directory.
func localTreeExists(root string) bool {
	info, err := os.Stat(root)
	return err == nil && info.IsDir()
}

// agentFileTools returns the durable agent's shell and file tools: the
// sidecar's when sock is set, else, when local asks for it, in-process
// against root.
func agentFileTools(sock string, local bool, slug, root string) (agent.ShellExecutor, mcp.FilesDispatcher, error) {
	if sock != "" {
		return &devshell.SidecarShell{SocketPath: sock, Slug: slug},
			mcp.NewSidecarFilesDispatcher(&devshell.SidecarFiles{SocketPath: sock}),
			nil
	}
	if !local || !localTreeExists(root) {
		return nil, nil, fileToolsRefusal(local, root)
	}
	return &agentpod.LocalShell{ScratchRoot: root, Slug: slug},
		mcp.NewLocalFilesDispatcher(&files.Backend{Root: root, ReadRoots: files.PodReadRoots(slug)}),
		nil
}

// subagentFileTools returns a subagent's shell and file tools, both
// scoped to its narrowed view filesRoot (/files/subagents/<id>), from
// the sidecar or the local tree as agentFileTools decides.
func subagentFileTools(sock string, local bool, parent, filesRoot string) (agent.ShellExecutor, mcp.FilesDispatcher, error) {
	if sock == "" {
		if !local || !localTreeExists(filesRoot) {
			return nil, nil, fileToolsRefusal(local, filesRoot)
		}
		return &agentpod.LocalShell{ScratchRoot: filesRoot, Slug: parent},
			mcp.NewLocalFilesDispatcher(&files.Backend{
				Root:       filesRoot,
				ReadRoots:  files.PodReadRoots(parent),
				WriteRoots: []string{files.SubagentParentRoot(filesRoot)},
			}),
			nil
	}
	// Daemon resolves cwd and the file_* root against /files/; the
	// subagent's narrowed view is files-root-relative. Strip the
	// /files/ prefix so both are daemon-root-relative.
	const filesPrefix = files.ModelRootPath + "/"
	prefix := strings.TrimPrefix(filesRoot, filesPrefix)
	if !strings.HasPrefix(filesRoot, filesPrefix) || prefix == "" {
		// Out-of-shape filesRoot — refuse. /files itself would hand the
		// subagent its parent's whole view; anything outside /files/
		// would not match the narrowed view, leading to silent
		// cross-subagent leakage.
		return nil, nil, fmt.Errorf("subagent filesRoot %q must live under %s", filesRoot, filesPrefix)
	}
	shell := &devshell.SidecarShell{
		SocketPath: sock,
		Slug:       parent,
		CwdPrefix:  prefix,
		// Daemon bind-mounts this over /files/artifacts/private inside
		// a per-call mount namespace so absolute "/files/artifacts/private/…"
		// writes from bash land in the subagent's overlay rather than
		// the parent's private dir — matching what the file_* tools
		// do (rooted at filesRoot). Without this the model can
		// silently overwrite the parent's role / handbook drafts.
		OverlayPrivate: filepath.Join(filesRoot, "artifacts", "private"),
	}
	return shell,
		mcp.NewSidecarFilesDispatcher(&devshell.SidecarFiles{SocketPath: sock, Root: prefix}),
		nil
}
