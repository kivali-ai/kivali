//go:build linux

package agentshell

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/kivali-ai/kivali/internal/clock"
)

// A process apt-get leaves running (a service a maintainer script
// started) does not outlive the install child: it is killed before the
// child reports, so nothing keeps running as root.
func TestInstallChildKillsWhatItLeftRunning(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	apt := newFakeApt(t)
	pidFile := filepath.Join(t.TempDir(), "linger.pid")
	t.Cleanup(func() { killPidFile(pidFile) })
	env := append(apt.envVars(),
		childAptEnv+"="+apt.bin,
		childAptCacheEnv+"="+apt.cache,
		"FAKE_APT_LINGER="+pidFile,
		"FAKE_APT_LINGER_QUIET=1",
	)
	inst, err := StartInstallChild(exe, []string{"jq"}, env, &bytes.Buffer{}, clock.NewFake())
	if err != nil {
		t.Fatalf("StartInstallChild: %v", err)
	}
	<-inst.Done()
	_, installed, _ := inst.State()
	if len(installed) != 1 {
		t.Fatalf("installed = %v", installed)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the fake apt-get left no process: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
		t.Errorf("process %d left by apt-get is still there (kill 0: %v)", pid, err)
	}
}
