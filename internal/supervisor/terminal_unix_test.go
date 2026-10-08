//go:build unix

package supervisor

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/creack/pty"
)

// The script TerminalArgv runs in the container, run here under a real
// terminal with a stand-in `claude` that does what Claude Code does at
// sign-in: run $BROWSER with the page. What reaches the terminal must be
// exactly OpenURLMarker, the page and BEL.
func TestTerminalBrowserHelperWritesTheMarker(t *testing.T) {
	argv := TerminalArgv(false)
	script := argv[len(argv)-1]
	if argv[len(argv)-3] != "sh" || argv[len(argv)-2] != "-c" {
		t.Fatalf("argv %q", argv)
	}
	dir := t.TempDir()
	// The container's /tmp and its `claude`, moved under the test's dir.
	script = strings.ReplaceAll(script, "/tmp", dir)
	fake := filepath.Join(dir, "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n\"$BROWSER\" 'https://claude.com/cai/oauth/authorize?a=1&b=2'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(fake)+":/usr/bin:/bin")
	f, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, _ = io.Copy(&out, f)
	_ = cmd.Wait()
	want := OpenURLMarker + "https://claude.com/cai/oauth/authorize?a=1&b=2\a"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("terminal got %q, want it to contain %q", out.String(), want)
	}
	if fi, err := os.Stat(filepath.Join(dir, "kivali-open-browser")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("helper: %v %v", fi, err)
	}
}

func TestTerminalShellHasNoHelper(t *testing.T) {
	argv := TerminalArgv(true)
	if got := argv[len(argv)-1]; got != "cd /tmp && exec bash" {
		t.Fatalf("got %q", got)
	}
}
