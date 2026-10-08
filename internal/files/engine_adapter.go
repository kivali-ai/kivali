package files

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
)

// Dispatcher adapts per-agent Backends to the file-tool dispatch
// surface: given a slug, it resolves the correct FS root and runs
// the tool call through the shared dispatch handlers.
//
// DataDir is the Kivali store root (…/data).
type Dispatcher struct {
	DataDir string
}

// BackendFor returns the filesystem Backend for one agent, as core
// runs it. Core mounts the whole data volume, so what a symlink in
// the agent's tree may reach is spelled out here: the org-wide
// project files and skills and this agent's own archived chats —
// the targets of the farms Sync builds. Attachments are not a read
// root: on core the farm points into the org-wide blob store, and
// reaching it would let a planted link read any agent's upload by SHA.
// A read that resolves there is served instead from this agent's own
// hardlink directory, agents/<slug>/attachments/<sha>/, which holds
// only the SHAs its chat history references — the same directory the
// agent pod mounts at /data/attachments (see PodReadRoots) — so
// body_path can name /files/attachments/<file> on core as it can in
// the pod. The published trees are Mounts, as they are in the pod.
func (d Dispatcher) BackendFor(slug string) *Backend {
	agentRoot := filepath.Join(d.DataDir, "agents", slug)
	return &Backend{
		Root: StorageRoot(agentRoot),
		ReadRoots: []string{
			filepath.Join(d.DataDir, "project_files"),
			filepath.Join(d.DataDir, "skills"),
			filepath.Join(agentRoot, "chats"),
		},
		ReadAliases: map[string]string{
			filepath.Join(d.DataDir, "attachments"): filepath.Join(agentRoot, "attachments"),
		},
		// The published trees, where the agent pod mounts them: this
		// agent's own at artifacts/public, everyone's at artifacts/shared.
		Mounts: map[string]string{
			PublishedOwnDir:   PublishedDir(d.DataDir, slug),
			PublishedPeersDir: PublishedRoot(d.DataDir),
		},
	}
}

// Dispatch routes one file_* tool call to the per-slug Backend. Used
// by the agent-pod files dispatch UDS endpoint.
func (d Dispatcher) Dispatch(slug, toolName string, rawInput []byte) (string, bool, error) {
	return Dispatch(d.BackendFor(slug), toolName, json.RawMessage(rawInput))
}

// ResolveBodyPathFor returns a message.ParseContext-compatible callback
// that reads an artifact from the given agent's memory. Used by the
// publish-tool parsers to support the body_path pattern: the agent
// drafts a message under /files/artifacts/private/ via small
// file_create/file_str_replace calls, then publishes with
// body_path set. The backend reads the file here and substitutes it
// in as the message body, following symlinks only as far as the
// backend's roots.
func (d Dispatcher) ResolveBodyPathFor(slug string) func(string) (string, error) {
	b := d.BackendFor(slug)
	return func(modelPath string) (string, error) {
		abs, err := b.Resolve(modelPath)
		if err != nil {
			return "", err
		}
		data, err := b.readFile(abs, modelPath)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return "", fmt.Errorf("not found: %s", modelPath)
			}
			return "", err
		}
		return string(data), nil
	}
}
