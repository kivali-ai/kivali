package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// configPaths are an allowlist and an agents file in a fresh directory,
// neither written yet.
func configPaths(t *testing.T) (allowlist, agents string) {
	dir := t.TempDir()
	return filepath.Join(dir, "allowlist.yaml"), filepath.Join(dir, "agents.json")
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Guards the boot race that cost agents their software: the proxy
// listened with an empty allowlist before the server had written one,
// and refused deb.debian.org to the agent shells installing at start.
// It must keep waiting until the server has written it.
func TestAwaitConfigWaitsForTheServersWrite(t *testing.T) {
	allowlist, agents := configPaths(t)
	clk := clock.NewFake()
	done := make(chan bool, 1)
	go func() { done <- awaitConfig(allowlist, agents, time.Minute, clk) }()
	for i := 0; i < 3; i++ {
		clk.BlockUntil(1)
		clk.Advance(allowlistPollInterval)
	}
	// Armed again: it looked after the last Advance and found nothing.
	clk.BlockUntil(1)
	select {
	case got := <-done:
		t.Fatalf("awaitConfig returned %v before the file was written", got)
	default:
	}
	writeFile(t, allowlist, "patterns: [deb.debian.org]\n")
	clk.Advance(allowlistPollInterval)
	if !<-done {
		t.Error("awaitConfig = false, want true once the allowlist exists")
	}
}

// Guards a proxy restart within a running pod: the file is already in
// the volume, and the proxy must listen at once rather than wait.
func TestAwaitConfigReturnsAtOnceWhenTheFileIsThere(t *testing.T) {
	allowlist, agents := configPaths(t)
	writeFile(t, allowlist, "patterns: []\n")
	clk := clock.NewFake()
	if !awaitConfig(allowlist, agents, time.Minute, clk) {
		t.Error("awaitConfig = false, want true")
	}
	if n := clk.Waiters(); n != 0 {
		t.Errorf("%d timers armed, want none", n)
	}
}

// Guards a server that could not write the allowlist (an unreadable
// list, say): it writes the agents file anyway, and the proxy must then
// listen without the allowlist at once, not hold agents' model calls
// for the whole bound.
func TestAwaitConfigStopsWhenOnlyTheAgentsFileComes(t *testing.T) {
	allowlist, agents := configPaths(t)
	writeFile(t, agents, "{}")
	clk := clock.NewFake()
	if awaitConfig(allowlist, agents, time.Minute, clk) {
		t.Error("awaitConfig = true, want false with no allowlist")
	}
	if n := clk.Waiters(); n != 0 {
		t.Errorf("%d timers armed, want none", n)
	}
}

// Guards against a server that writes neither file: the proxy still
// listens after its bound, as it did before the wait.
func TestAwaitConfigGivesUpAfterItsBound(t *testing.T) {
	allowlist, agents := configPaths(t)
	clk := clock.NewFake()
	done := make(chan bool, 1)
	go func() { done <- awaitConfig(allowlist, agents, time.Second, clk) }()
	for i := 0; i < int(time.Second/allowlistPollInterval); i++ {
		clk.BlockUntil(1)
		clk.Advance(allowlistPollInterval)
	}
	if <-done {
		t.Error("awaitConfig = true, want false with no file")
	}
}
