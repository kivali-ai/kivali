//go:build linux

package agentshell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Run as root, RunCommand with a CommandUser really starts the command
// as that uid and gid, with no groups and no capabilities left after
// exec; and `unshare -mUr` still makes its overlay as that user. Only
// root can start a process as another user, so elsewhere this skips.
func TestRunCommandAsCommandUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to start a command as another user")
	}
	user := &CommandUser{UID: 65532, GID: 65532}
	res, err := RunCommand(context.Background(), "/", "id -u; id -g; id -G; grep CapEff /proc/self/status", "", "", 10*time.Second, "", user)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("RunCommand = %+v, %v", res, err)
	}
	if want := "65532\n65532\n65532\nCapEff:\t0000000000000000\n"; res.Stdout != want {
		t.Errorf("stdout = %q, want %q", res.Stdout, want)
	}

	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("unshare binary not on PATH")
	}
	root := t.TempDir()
	priv := filepath.Join(root, "artifacts", "private")
	overlay := filepath.Join(root, "subagents", "a", "artifacts", "private")
	for _, d := range []string{priv, overlay} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The command's user must reach the tree, as in the pod: t.TempDir's
	// parent is root's alone.
	if err := os.Chmod(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(p, 65532, 65532)
	}); err != nil {
		t.Fatal(err)
	}
	res, err = RunCommand(context.Background(), root, "echo x > "+priv+"/note && id -u", overlay, priv, 10*time.Second, "", user)
	if err != nil {
		t.Fatalf("overlay RunCommand: %v", err)
	}
	if res.ExitCode != 0 {
		t.Skipf("unshare -mUr unavailable here as uid 65532: exit %d, %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	// Inside the user namespace the command is its root.
	if res.Stdout != "0\n" {
		t.Errorf("overlay stdout = %q, want 0 (root in its own namespace)", res.Stdout)
	}
	st, err := os.Stat(filepath.Join(overlay, "note"))
	if err != nil {
		t.Fatalf("overlay note: %v", err)
	}
	if uid := st.Sys().(*syscall.Stat_t).Uid; uid != 65532 {
		t.Errorf("overlay note owned by %d, want 65532", uid)
	}
}
