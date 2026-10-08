package agentpod

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
)

func skipIfNoBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("LocalShell uses bash; skipping on Windows")
	}
}

// TestLocalShellRunSimpleCommand: a command that writes to stdout
// returns exit 0 and the output.
func TestLocalShellRunSimpleCommand(t *testing.T) {
	skipIfNoBash(t)
	root := t.TempDir()
	s := &LocalShell{ScratchRoot: root, Slug: "alice"}
	res, err := s.Exec(context.Background(), "alice", agent.ShellRequest{
		Command: "echo hi",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0; stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "hi") {
		t.Errorf("Stdout = %q, want to contain hi", res.Stdout)
	}
}

// TestLocalShellNonZeroExit: a command that exits non-zero returns
// the exit code (no error) — same contract as the chat-loop expects.
func TestLocalShellNonZeroExit(t *testing.T) {
	skipIfNoBash(t)
	s := &LocalShell{ScratchRoot: t.TempDir()}
	res, err := s.Exec(context.Background(), "alice", agent.ShellRequest{
		Command: "exit 7",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.Err != "" {
		t.Errorf("Err = %q, want empty (non-zero exit isn't an env error)", res.Err)
	}
}

// TestLocalShellTimeout: a command that exceeds the timeout is
// killed and TimedOut is set.
func TestLocalShellTimeout(t *testing.T) {
	skipIfNoBash(t)
	s := &LocalShell{ScratchRoot: t.TempDir()}
	res, err := s.Exec(context.Background(), "alice", agent.ShellRequest{
		Command:        "sleep 5",
		TimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true")
	}
}

// TestLocalShellWritesPersistInCwd: bytes the command writes to the
// cwd are visible on disk afterward — the unified-files contract.
func TestLocalShellWritesPersistInCwd(t *testing.T) {
	skipIfNoBash(t)
	root := t.TempDir()
	s := &LocalShell{ScratchRoot: root}
	res, err := s.Exec(context.Background(), "alice", agent.ShellRequest{
		Command: "echo -n produced > out.txt",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
	got, rerr := os.ReadFile(filepath.Join(root, "out.txt"))
	if rerr != nil {
		t.Fatalf("ReadFile out.txt: %v", rerr)
	}
	if string(got) != "produced" {
		t.Errorf("out.txt = %q, want produced", string(got))
	}
}

// TestLocalShellCwdResolution: req.Cwd is joined under ScratchRoot.
// Existence is auto-created. Empty cwd → ScratchRoot itself.
func TestLocalShellCwdResolution(t *testing.T) {
	skipIfNoBash(t)
	root := t.TempDir()
	s := &LocalShell{ScratchRoot: root}

	// nested cwd auto-created
	res, err := s.Exec(context.Background(), "alice", agent.ShellRequest{
		Command: "pwd",
		Cwd:     "deep/nest",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := filepath.Join(root, "deep", "nest")
	if !strings.Contains(res.Stdout, want) {
		t.Errorf("Stdout = %q, want it to contain %q", res.Stdout, want)
	}
}

// TestLocalShellCwdRejectsAbsolute guards path safety.
func TestLocalShellCwdRejectsAbsolute(t *testing.T) {
	s := &LocalShell{ScratchRoot: t.TempDir()}
	_, err := s.Exec(context.Background(), "alice", agent.ShellRequest{
		Command: "true",
		Cwd:     "/etc",
	})
	if err == nil {
		t.Error("expected error for absolute cwd")
	}
}

// TestLocalShellCwdRejectsTraversal: ../ escapes are rejected.
func TestLocalShellCwdRejectsTraversal(t *testing.T) {
	s := &LocalShell{ScratchRoot: t.TempDir()}
	_, err := s.Exec(context.Background(), "alice", agent.ShellRequest{
		Command: "true",
		Cwd:     "../escape",
	})
	if err == nil {
		t.Error("expected error for cwd traversal")
	}
}

// TestLocalShellMissingScratchRoot is a defensive check — empty
// ScratchRoot is a programming error, not user input.
func TestLocalShellMissingScratchRoot(t *testing.T) {
	s := &LocalShell{} // ScratchRoot empty
	_, err := s.Exec(context.Background(), "alice", agent.ShellRequest{Command: "true"})
	if err == nil {
		t.Error("expected error when ScratchRoot is empty")
	}
}

// TestLocalShellSyncSkillsNoOp: SyncSkills does nothing and succeeds.
func TestLocalShellSyncSkillsNoOp(t *testing.T) {
	s := &LocalShell{ScratchRoot: t.TempDir()}
	if err := s.SyncSkills(context.Background(), "alice"); err != nil {
		t.Errorf("SyncSkills should be a no-op: %v", err)
	}
}

// TestLocalShellImplementsShellExecutor compile-time assertion that
// LocalShell satisfies the contract the existing chat path expects.
func TestLocalShellImplementsShellExecutor(t *testing.T) {
	var _ agent.ShellExecutor = (*LocalShell)(nil)
	_ = os.PathSeparator // silence unused import in case other tests skip
}
