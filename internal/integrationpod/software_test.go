package integrationpod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agentshell"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/integration"
)

// steps is an ordered record shared by a fake core and a fake
// Software, so a test sees what happened in which order. A nil *steps
// records nothing.
type steps struct {
	mu   sync.Mutex
	list []string
}

func (s *steps) add(step string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list = append(s.list, step)
}

func (s *steps) get() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.list...)
}

// upTo is the steps up to and including the first one equal to last,
// and whether there was one.
func (s *steps) upTo(last string) ([]string, bool) {
	all := s.get()
	for i, st := range all {
		if st == last {
			return all[:i+1], true
		}
	}
	return all, false
}

// fakeSoftware stands in for RootSoftware: it records the install and
// the leave, writes output as apt-get would, and answers result.
type fakeSoftware struct {
	steps  *steps
	output string
	result func(pkgs []string) agentshell.InstallResult
	leave  error
}

func (f *fakeSoftware) Install(_ context.Context, pkgs []string, log io.Writer) agentshell.InstallResult {
	f.steps.add("install " + strings.Join(pkgs, " "))
	_, _ = io.WriteString(log, f.output)
	if f.result != nil {
		return f.result(pkgs)
	}
	return agentshell.InstallResult{Installed: pkgs, Failed: []agentshell.PackageFailure{}}
}

func (f *fakeSoftware) LeaveRoot() error {
	f.steps.add("leave root")
	return f.leave
}

// softwareManifest is the fake bundle's manifest declaring pkgs.
func softwareManifest(pkgs ...string) *integration.Manifest {
	m := testManifest(nil)
	m.Kivali.Packages = pkgs
	return m
}

// The start of a version that installs software, in order: the log
// says what is being installed, apt-get's output follows it, the log
// says it was installed, the shim leaves root, and only then does it
// dial core's events stream, so the integration is not running until
// the install is over and nothing of the bundle runs as root; the
// server starts on core's start as it always does. A shim that dialled
// first would show the integration running while it installs; one
// that left root after starting the server would run the owner's
// bundle as root.
func TestShimInstallsThenLeavesRootThenStarts(t *testing.T) {
	m := softwareManifest("gh", "jq")
	c := newFakeCore(t, m)
	rec := &steps{}
	c.setSteps(rec)
	sw := &fakeSoftware{steps: rec, output: "Get:1 http://deb.debian.org/debian trixie/main gh\nSetting up gh ...\nno newline at the end"}
	_, _, _ = runShimWith(t, c, m, func(s *Shim) { s.Software = sw })
	rc := &recorder{c: c}

	st := c.stream()
	got, ok := rec.upTo("events")
	want := []string{
		"log: Installing gh, jq",
		"install gh jq",
		"log: Get:1 http://deb.debian.org/debian trixie/main gh",
		"log: Setting up gh ...",
		"log: no newline at the end",
		"log: Installed gh, jq",
		"leave root",
		"events",
	}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("start = %q\nwant    %q", got, want)
	}
	if p := rc.log("Installing gh, jq"); p.Level != "info" {
		t.Errorf("Installing level = %q", p.Level)
	}
	if p := rc.log("Setting up gh"); p.Level != "debug" {
		t.Errorf("apt-get output level = %q", p.Level)
	}
	if p := rc.log("Installed gh, jq"); p.Level != "info" {
		t.Errorf("Installed level = %q", p.Level)
	}
	st.send(EventStart, "{}")
	rc.log("server started")
}

// An install that fails stops the shim before it dials core or starts
// anything of the bundle, with the failure's words in the log, and
// with root never left, since nothing runs after it: the pod restarts
// and tries again. A shim that went on would start a server whose
// actions fail on a missing CLI, with nothing in the log to say why.
func TestShimStopsWhenTheInstallFails(t *testing.T) {
	m := softwareManifest("gh", "nosuch")
	c := newFakeCore(t, m)
	rec := &steps{}
	c.setSteps(rec)
	sw := &fakeSoftware{steps: rec, result: func(pkgs []string) agentshell.InstallResult {
		return agentshell.InstallResult{Installed: []string{"gh"}, Failed: []agentshell.PackageFailure{{Name: "nosuch", Error: "no package has this name"}}}
	}}
	_, run, _ := runShimWith(t, c, m, func(s *Shim) { s.Software = sw })
	if code := exitOf(t, run); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	want := []string{
		"log: Installing gh, nosuch",
		"install gh nosuch",
		"log: Could not install nosuch (no package has this name). The integration stops and tries again when it restarts.",
	}
	if got := rec.get(); !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %q\nwant    %q", got, want)
	}
	rc := &recorder{c: c}
	if p := rc.log("Could not install nosuch"); p.Level != "error" {
		t.Errorf("failure level = %q", p.Level)
	}
	if n := len(c.streams); n != 0 {
		t.Errorf("the shim opened %d events streams", n)
	}
}

// A shim that cannot leave root stops before it dials core, so the
// server never runs as root; one started as root for a version with no
// packages installs nothing and still leaves root first.
func TestShimLeavesRootBeforeAnything(t *testing.T) {
	m := softwareManifest()
	c := newFakeCore(t, m)
	rec := &steps{}
	c.setSteps(rec)
	_, _, _ = runShimWith(t, c, m, func(s *Shim) { s.Software = &fakeSoftware{steps: rec} })
	c.stream()
	if got, _ := rec.upTo("events"); !reflect.DeepEqual(got, []string{"leave root", "events"}) {
		t.Errorf("start without packages = %q", got)
	}

	c2 := newFakeCore(t, m)
	rec2 := &steps{}
	c2.setSteps(rec2)
	_, run, _ := runShimWith(t, c2, m, func(s *Shim) {
		s.Software = &fakeSoftware{steps: rec2, leave: errors.New("verify privileges: uid=0")}
	})
	if code := exitOf(t, run); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	want := []string{"leave root", "log: Could not stop running as root, so the integration did not start: verify privileges: uid=0"}
	if got := rec2.get(); !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %q\nwant    %q", got, want)
	}
}

// A shim that did not start as root cannot install anything: a version
// that declares packages stops with that in its log rather than
// starting a server without its software.
func TestShimWithPackagesButNotRoot(t *testing.T) {
	m := softwareManifest("gh")
	c := newFakeCore(t, m)
	rec := &steps{}
	c.setSteps(rec)
	_, run, _ := runShim(t, c, m)
	if code := exitOf(t, run); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	want := []string{"log: Could not install gh: the integration does not start as root, so it cannot install software. It stops and tries again when it restarts."}
	if got := rec.get(); !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %q\nwant    %q", got, want)
	}
}

// installChildEnv makes the test binary the shim's install child, in
// the mode it names: "ok" reports every package installed but the one
// installChildFailEnv names, after a line of output for each; "hang"
// blocks on the FIFO installChildFIFOEnv names until the test writes
// to it, then exits with no result.
const (
	installChildEnv     = "INTEGRATIONPOD_TEST_INSTALL_CHILD"
	installChildFailEnv = "INTEGRATIONPOD_TEST_INSTALL_FAIL"
	installChildFIFOEnv = "INTEGRATIONPOD_TEST_INSTALL_FIFO"
)

func installChildMain(mode string, pkgs []string) int {
	result, err := agentshell.OpenInstallResult()
	if err != nil {
		return 2
	}
	if mode == "hang" {
		f, err := os.Open(os.Getenv(installChildFIFOEnv))
		if err == nil {
			_, _ = io.Copy(io.Discard, f)
		}
		return 0
	}
	res := agentshell.InstallResult{Installed: []string{}, Failed: []agentshell.PackageFailure{}}
	for _, p := range pkgs {
		fmt.Printf("Setting up %s ...\n", p)
		if p == os.Getenv(installChildFailEnv) {
			fmt.Fprintf(os.Stderr, "E: Unable to locate package %s\n", p)
			res.Failed = append(res.Failed, agentshell.PackageFailure{Name: p, Error: "Unable to locate package " + p})
		} else {
			res.Installed = append(res.Installed, p)
		}
	}
	if err := json.NewEncoder(result).Encode(res); err != nil {
		return 1
	}
	return 0
}

// RootSoftware runs the shim's own binary again as the install child,
// with the packages as arguments after InstallArgs and nothing else
// between, its output to the log and its result read back from the
// pipe. A wrong command line would run an install the pod's binary
// does not have; output lost would leave the owner's log silent about
// why an install failed.
func TestRootSoftwareRunsTheInstallChild(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r := RootSoftware{Exe: exe, Env: []string{installChildEnv + "=ok", installChildFailEnv + "=nosuch"}, Clock: clock.NewFake()}
	var log bytes.Buffer
	res := r.Install(context.Background(), []string{"gh", "nosuch"}, &log)
	if !reflect.DeepEqual(res.Installed, []string{"gh"}) ||
		!reflect.DeepEqual(res.Failed, []agentshell.PackageFailure{{Name: "nosuch", Error: "Unable to locate package nosuch"}}) {
		t.Errorf("result = %+v", res)
	}
	for _, want := range []string{"Setting up gh ...", "Setting up nosuch ...", "E: Unable to locate package nosuch"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log lacks %q: %q", want, log.String())
		}
	}
}

// An install that never reports is given up on once the agent shell's
// bound has passed on the shim's clock, every package failed with the
// reason, so the shim stops and the pod tries again rather than
// waiting for ever, not running, with nothing in the log.
func TestRootSoftwareGivesUpOnTime(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(t.TempDir(), "release")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake()
	r := RootSoftware{Exe: exe, Env: []string{installChildEnv + "=hang", installChildFIFOEnv + "=" + fifo}, Clock: clk}
	got := make(chan agentshell.InstallResult, 1)
	go func() { got <- r.Install(context.Background(), []string{"gh"}, io.Discard) }()
	t.Cleanup(func() {
		// Let the child go: a blocking open for writing pairs with the
		// child's open for reading whenever it gets there, and closing it
		// is the child's end of file. It runs on a goroutine bounded by
		// the failsafe, so a child that never opens the FIFO cannot hang
		// the test.
		released := make(chan struct{})
		go func() {
			defer close(released)
			if f, err := os.OpenFile(fifo, os.O_WRONLY, 0); err == nil {
				_ = f.Close()
			}
		}()
		select {
		case <-released:
		case <-time.After(failsafe):
			t.Error("the install child never opened its FIFO")
		}
	})

	clk.BlockUntil(1)
	clk.Advance(agentshell.InstallTimeout - time.Nanosecond)
	select {
	case res := <-got:
		t.Fatalf("gave up before the bound: %+v", res)
	default:
	}
	clk.Advance(agentshell.InstallTimeout)
	select {
	case res := <-got:
		if len(res.Installed) != 0 || len(res.Failed) != 1 || res.Failed[0].Name != "gh" || res.Failed[0].Error != "the installation did not finish in time" {
			t.Errorf("result = %+v", res)
		}
	case <-time.After(failsafe):
		t.Fatal("Install did not give up")
	}
}
