package main

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/supervisor"
)

// The supervisor reads the server's version from its boot line after an
// install or an upgrade; the line the server logs must be one it reads.
func TestBootLineNamesTheVersionTheSupervisorReads(t *testing.T) {
	line := "2026/01/02 03:04:05 " + bootLine("v1.7.0", "prod", "claude", "Claude Max") + "\n"
	if got, ok := supervisor.ServerBootVersion([]byte(line)); !ok || got != "1.7.0" {
		t.Fatalf("ServerBootVersion(%q) = %q, %v", line, got, ok)
	}
}
