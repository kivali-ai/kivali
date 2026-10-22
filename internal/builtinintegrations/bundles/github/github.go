// Package github is the GitHub integration that ships with Kivali: a
// bundle embedded in the binary, manifest.json and server/main.py, a
// stdio MCP server of Python's standard library that runs GitHub's gh
// for each action. Core installs it at boot as a shipped bundle
// (builtins.Shipped) and runs it in a pod of its own like an uploaded
// bundle; the pod installs gh (kivali.packages) before the server
// starts.
//
// Its connection is made by signing in with a GitHub App the owner
// registers (kivali.connection, method device): core runs the sign-in
// and hands the server a current user access token on each call
// (kivali/credential, env GH_TOKEN). The server gives it to gh in gh's
// environment alone, and holds every call to the agent's access: the
// repositories it names and whether it may create issues, comments and
// pull requests. It moves no commits; pushing is the Git integration's.
package github

import (
	"embed"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// Name is the integration's name, the one its manifest gives.
const Name = "github"

// files is the bundle: manifest.json at the root and the server beside
// it.
//
//go:embed manifest.json all:server
var files embed.FS

// Bundle is the GitHub integration as builtins.Shipped lists it.
func Bundle() builtinintegrations.ShippedBundle {
	return builtinintegrations.ShippedBundle{Name: Name, FS: files}
}
