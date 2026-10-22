package agentshell

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// childAptEnv names the fake apt-get the test binary uses when it runs
// as the install child.
const childAptEnv = "AGENTSHELL_TEST_CHILD_APT"

// childAptCacheEnv names the fake apt-cache it uses.
const childAptCacheEnv = "AGENTSHELL_TEST_CHILD_APT_CACHE"

// TestMain lets the test binary stand in for the agent-shell binary:
// run as `<test binary> install-software <pkgs...>` with childAptEnv
// set, it is the install subcommand, writing its result to
// InstallResultFD, opened as cmd/agent-shell opens it.
func TestMain(m *testing.M) {
	if apt := os.Getenv(childAptEnv); apt != "" && len(os.Args) > 1 && os.Args[1] == InstallSubcommand {
		result, err := OpenInstallResult()
		if err != nil {
			os.Exit(2)
		}
		in := Installer{AptGet: apt, AptCache: os.Getenv(childAptCacheEnv), Env: os.Environ(), Log: os.Stderr}
		if err := RunInstallSubcommand(in, os.Args[2:], result); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The daemon's side and the child's side of the install, end to end
// through a real child process: packages go over argv untouched, apt's
// output goes to the log, and the result comes back on the pipe.
func TestStartInstallChildRoundTrip(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	apt := newFakeApt(t)
	fd3 := filepath.Join(t.TempDir(), "fd3.log")
	env := append(apt.envVars(),
		childAptEnv+"="+apt.bin,
		childAptCacheEnv+"="+apt.cache,
		"FAKE_APT_MISSING=nosuch",
		"FAKE_APT_FD3="+fd3,
	)
	var log bytes.Buffer
	inst, err := StartInstallChild(exe, []string{"g++", "nosuch"}, env, &log, clock.NewFake())
	if err != nil {
		t.Fatalf("StartInstallChild: %v", err)
	}
	<-inst.Done()
	_, installed, failed := inst.State()
	if want := []string{"g++"}; !reflect.DeepEqual(installed, want) {
		t.Errorf("installed = %v, want %v", installed, want)
	}
	if want := []PackageFailure{{Name: "nosuch", Error: "Unable to locate package nosuch"}}; !reflect.DeepEqual(failed, want) {
		t.Errorf("failed = %v, want %v", failed, want)
	}
	if !strings.Contains(log.String(), "Unable to locate package nosuch") {
		t.Errorf("apt-get's output did not reach the log: %q", log.String())
	}
	runs := apt.runs(t)
	if len(runs) == 0 || runs[len(runs)-1][len(runs[len(runs)-1])-1] != "nosuch" {
		t.Errorf("apt-get runs = %q", runs)
	}
	// The result pipe is the child's alone: apt-get does not inherit it.
	if b, err := os.ReadFile(fd3); err == nil {
		t.Errorf("apt-get inherited the result pipe: %s", b)
	}
}

// A child that never reports is given up on once the install's bound
// and its grace have passed: every package fails, and commands stop
// waiting. The child is reaped whenever it does end.
func TestAwaitInstallResultGivesUp(t *testing.T) {
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	clk := clock.NewFake()
	inst := NewInstall([]string{"jq", "zip"})
	reaped := make(chan struct{})
	awaitInstallResult(inst, inst.Requested(), r, func() error { close(reaped); return nil }, clk, InstallTimeout+installResultGrace)

	clk.BlockUntil(1)
	clk.Advance(InstallTimeout + installResultGrace - time.Nanosecond)
	if inst.Finished() {
		t.Fatal("gave up before the bound")
	}
	clk.Advance(time.Nanosecond)
	<-inst.Done()
	_, installed, failed := inst.State()
	if want := failAll([]string{"jq", "zip"}, reasonInstallTooLong).Failed; len(installed) != 0 || !reflect.DeepEqual(failed, want) {
		t.Errorf("installed %v, failed %v, want %v", installed, failed, want)
	}
	<-reaped
}

// A result that arrives in time wins.
func TestAwaitInstallResultInTime(t *testing.T) {
	r, w := io.Pipe()
	clk := clock.NewFake()
	inst := NewInstall([]string{"jq"})
	awaitInstallResult(inst, inst.Requested(), r, func() error { return nil }, clk, time.Hour)
	if _, err := io.WriteString(w, `{"installed":["jq"],"failed":[]}`); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	<-inst.Done()
	if _, installed, _ := inst.State(); !reflect.DeepEqual(installed, []string{"jq"}) {
		t.Errorf("installed = %v", installed)
	}
}

// A child that dies without reporting leaves every package failed,
// and the install finished, so commands stop waiting.
func TestStartInstallChildWithoutAResult(t *testing.T) {
	inst, err := StartInstallChild("/bin/sh", []string{"-c", "exit 3"}, nil, &bytes.Buffer{}, clock.NewFake())
	if err != nil {
		t.Fatalf("StartInstallChild: %v", err)
	}
	<-inst.Done()
	_, installed, failed := inst.State()
	if len(installed) != 0 || len(failed) != 2 || failed[0].Error != "the installer stopped without reporting a result" {
		t.Errorf("installed %v, failed %v", installed, failed)
	}
}

// A child that reports and exits while something it started still
// holds its output finishes the install once installOutputGrace has
// passed, not when that process ends: otherwise a leftover from a
// package's scripts would keep the daemon's commands, or an
// integration's start, waiting with nothing in any log.
func TestStartInstallChildWithALeftoverHoldingItsOutput(t *testing.T) {
	script := `(sleep 30 3>&-) & echo '{"installed":["jq"],"failed":[]}' >&3; exit 0`
	inst, err := StartInstallCommand([]string{"/bin/sh", "-c", script}, []string{"jq"}, nil, &bytes.Buffer{}, clock.NewFake())
	if err != nil {
		t.Fatalf("StartInstallCommand: %v", err)
	}
	select {
	case <-inst.Done():
	case <-time.After(installOutputGrace + 10*time.Second):
		t.Fatal("the install waited on the leftover's output")
	}
	if _, installed, _ := inst.State(); !reflect.DeepEqual(installed, []string{"jq"}) {
		t.Errorf("installed = %v", installed)
	}
}

func TestStartInstallChildCannotStart(t *testing.T) {
	if _, err := StartInstallChild("/nonexistent/agent-shell", []string{"jq"}, nil, &bytes.Buffer{}, clock.NewFake()); err == nil {
		t.Error("StartInstallChild of a missing binary succeeded")
	}
}

func TestInstallEnv(t *testing.T) {
	vars := map[string]string{"PATH": "/p", "https_proxy": "http://proxy:3128", "HOME": "/scratch/home", "SECRET": "x"}
	got := InstallEnv(func(k string) string { return vars[k] })
	want := []string{"PATH=/p", "HOME=/root", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "https_proxy=http://proxy:3128"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InstallEnv = %q, want %q", got, want)
	}
}
