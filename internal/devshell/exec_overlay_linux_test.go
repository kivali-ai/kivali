//go:build linux

package devshell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestRunCommandOverlayBindIsolatesPrivateDir: a subagent that writes
// to /files/artifacts/private/<x> from bash must land in its own
// overlay, not its caller's private dir — the overlay has to apply to
// bash as well as to the MCP file_view backend.
//
// When overlaySrc/overlayDst are
// set, RunCommand wraps bash in `unshare -mUr` + bind-mount, so a
// write to <root>/artifacts/private inside the namespace lands at
// overlaySrc on disk and the original path under root is untouched.
//
// Linux-only: user namespaces + bind-mounts aren't a thing on darwin.
// Skips when the binary or kernel feature is unavailable rather than
// failing — Mac developer machines run other tests; CI runs on Linux.
func TestRunCommandOverlayBindIsolatesPrivateDir(t *testing.T) {
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare binary not on PATH")
	}
	// Probe: does the kernel allow unprivileged user-namespace + bind?
	// Common in modern distros, disabled in some hardened kernels and
	// in rootless containers without /proc/sys/user/max_user_namespaces.
	probe := exec.Command("unshare", "-mUr", "true")
	if err := probe.Run(); err != nil {
		t.Skipf("unshare -mUr unavailable in this environment: %v", err)
	}

	// Simulated layout: <root>/artifacts/private holds the parent's file
	// (must NOT be touched), <overlaySrc> is the subagent's overlay
	// private dir (where writes must land).
	root := t.TempDir()
	parentPriv := filepath.Join(root, "artifacts", "private")
	if err := os.MkdirAll(parentPriv, 0o755); err != nil {
		t.Fatalf("mkdir parent priv: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parentPriv, "PARENT_FILE"), []byte("untouched"), 0o644); err != nil {
		t.Fatalf("seed parent file: %v", err)
	}
	overlaySrc := filepath.Join(root, "subagents", "abc", "artifacts", "private")
	if err := os.MkdirAll(overlaySrc, 0o755); err != nil {
		t.Fatalf("mkdir overlay src: %v", err)
	}

	// The subagent's bash writes to the parent-shaped path. The kernel
	// bind-mount must redirect to the overlay.
	cmd := "echo SUBAGENT_WROTE > " + parentPriv + "/note.txt && ls " + parentPriv
	res, err := RunCommand(context.Background(), root, cmd, overlaySrc, parentPriv, 10*time.Second)
	if err != nil {
		t.Fatalf("RunCommand: %v (stderr=%q)", err, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Outside the namespace, parent's file is untouched.
	got, err := os.ReadFile(filepath.Join(parentPriv, "PARENT_FILE"))
	if err != nil {
		t.Fatalf("read PARENT_FILE: %v", err)
	}
	if string(got) != "untouched" {
		t.Errorf("parent PARENT_FILE got=%q, want %q", got, "untouched")
	}
	// note.txt must NOT exist in the parent's private dir — the bind-mount
	// shadowed the path during the bash run.
	if _, err := os.Stat(filepath.Join(parentPriv, "note.txt")); !os.IsNotExist(err) {
		t.Errorf("parent priv leaked note.txt: stat err=%v", err)
	}
	// note.txt SHOULD exist in the overlay source (the bind-mount routed
	// the write there).
	got, err = os.ReadFile(filepath.Join(overlaySrc, "note.txt"))
	if err != nil {
		t.Fatalf("read overlay note.txt: %v", err)
	}
	if string(got) != "SUBAGENT_WROTE\n" {
		t.Errorf("overlay note.txt got=%q, want %q", got, "SUBAGENT_WROTE\n")
	}
}

// TestRunCommandWithoutOverlayUnchanged guards the full-agent path:
// when overlaySrc is empty, RunCommand must not invoke unshare and
// write straight to the on-disk path.
func TestRunCommandWithoutOverlayUnchanged(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "out.txt")
	res, err := RunCommand(context.Background(), root, "echo hi > "+target, "", "", 5*time.Second)
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "hi\n" {
		t.Errorf("target got=%q, want %q", got, "hi\n")
	}
}
