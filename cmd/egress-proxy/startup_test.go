package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// Guards the boot race that cost agents their software: the proxy
// listened with an empty allowlist before the server had written one,
// and refused deb.debian.org to the agent shells installing at start.
// It must not report the file there until the server has written it.
func TestAwaitFileWaitsForTheServersWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allowlist.yaml")
	clk := clock.NewFake()
	done := make(chan bool, 1)
	go func() { done <- awaitFile(path, time.Minute, clk) }()
	for i := 0; i < 3; i++ {
		clk.BlockUntil(1)
		clk.Advance(allowlistPollInterval)
	}
	select {
	case got := <-done:
		t.Fatalf("awaitFile returned %v before the file was written", got)
	default:
	}
	if err := os.WriteFile(path, []byte("patterns: [deb.debian.org]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk.BlockUntil(1)
	clk.Advance(allowlistPollInterval)
	if !<-done {
		t.Error("awaitFile = false, want true once the file exists")
	}
}

// Guards a proxy restart within a running pod: the file is already in
// the volume, and the proxy must listen at once rather than wait.
func TestAwaitFileReturnsAtOnceWhenTheFileIsThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allowlist.yaml")
	if err := os.WriteFile(path, []byte("patterns: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake()
	if !awaitFile(path, time.Minute, clk) {
		t.Error("awaitFile = false, want true")
	}
	if n := clk.Waiters(); n != 0 {
		t.Errorf("%d timers armed, want none", n)
	}
}

// Guards against a server that never writes the file: the proxy still
// listens after its bound, as it did before the wait, so agents'
// credentialed traffic is not cut off with it.
func TestAwaitFileGivesUpAfterItsBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allowlist.yaml")
	clk := clock.NewFake()
	done := make(chan bool, 1)
	go func() { done <- awaitFile(path, time.Second, clk) }()
	for i := 0; i < int(time.Second/allowlistPollInterval); i++ {
		clk.BlockUntil(1)
		clk.Advance(allowlistPollInterval)
	}
	if <-done {
		t.Error("awaitFile = true, want false with no file")
	}
}
