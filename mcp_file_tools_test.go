package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/devshell"
)

// Without DEV_SHELL_SOCKET the file tools refuse unless the local tree
// is asked for, whatever is on disk: a pod whose spec lost the variable
// refuses to start rather than answering file calls from whatever tree
// its container has.
func TestFileToolsRefuseWithoutSidecar(t *testing.T) {
	root := t.TempDir()
	if _, _, err := agentFileTools("", false, "alice", root); err == nil || !strings.Contains(err.Error(), "DEV_SHELL_SOCKET") || !strings.Contains(err.Error(), LocalFilesEnv) {
		t.Errorf("agentFileTools with no socket, not asked for local: err = %v, want a refusal naming DEV_SHELL_SOCKET and %s", err, LocalFilesEnv)
	}
	if _, _, err := subagentFileTools("", false, "alice", root); err == nil {
		t.Error("subagentFileTools with no socket, not asked for local: want a refusal")
	}
	missing := filepath.Join(root, "files")
	if _, _, err := agentFileTools("", true, "alice", missing); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("agentFileTools asked for local with no tree: err = %v, want a refusal", err)
	}
	if _, _, err := subagentFileTools("", true, "alice", filepath.Join(missing, "subagents", "s1")); err == nil {
		t.Error("subagentFileTools asked for local with no tree: want a refusal")
	}
}

// Asked for, the local tree serves tests and fixtures.
func TestFileToolsLocalWhenAskedAndTreeExists(t *testing.T) {
	root := t.TempDir()
	shell, disp, err := agentFileTools("", true, "alice", root)
	if err != nil || disp == nil {
		t.Fatalf("agentFileTools = %v, %v", disp, err)
	}
	if ls, ok := shell.(*agentpod.LocalShell); !ok || ls.ScratchRoot != root {
		t.Errorf("shell = %#v, want a LocalShell at %s", shell, root)
	}
	if _, _, err := subagentFileTools("", true, "alice", root); err != nil {
		t.Errorf("subagentFileTools with an existing tree: %v", err)
	}
}

// With the socket set, both go to the sidecar, whatever is on disk
// here or asked for; a subagent's view must lie below /files/.
func TestFileToolsUseTheSidecarWhenSocketSet(t *testing.T) {
	shell, disp, err := agentFileTools("/run/sock", true, "alice", t.TempDir())
	if err != nil || disp == nil {
		t.Fatalf("agentFileTools = %v, %v", disp, err)
	}
	if _, ok := shell.(*devshell.SidecarShell); !ok {
		t.Errorf("shell = %T, want *devshell.SidecarShell", shell)
	}
	shell, _, err = subagentFileTools("/run/sock", false, "alice", "/files/subagents/s1")
	if err != nil {
		t.Fatal(err)
	}
	if ss := shell.(*devshell.SidecarShell); ss.CwdPrefix != "subagents/s1" || ss.OverlayPrivate != "/files/subagents/s1/artifacts/private" {
		t.Errorf("subagent shell = %+v", ss)
	}
	for _, bad := range []string{"/files", "/elsewhere/s1"} {
		if _, _, err := subagentFileTools("/run/sock", false, "alice", bad); err == nil {
			t.Errorf("subagentFileTools(%q): want refused", bad)
		}
	}
}
