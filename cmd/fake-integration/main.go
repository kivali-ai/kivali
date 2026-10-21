// Command fake-integration is a stdio MCP server that stands in for an
// integration's server in tests and in end-to-end runs: see
// internal/integrationpod/fakeintegration for what it answers.
package main

import (
	"os"

	"github.com/kivali-ai/kivali/internal/integrationpod/fakeintegration"
)

func main() {
	os.Exit(fakeintegration.Main())
}
