// Package devshell is the per-agent shell-exec sidecar.
//
// The agent runtime container is a slim Go runtime carrying no dev
// tooling — no compilers, no python, no jq. Putting dev tooling into
// the agent runtime image would couple toolkit version to Kivali
// release cadence and bloat every pod with utilities the runtime never
// invokes.
//
// Instead, each agent pod carries a second container ("dev-shell")
// built from a richer Debian image. run_shell in the agent runtime
// dispatches commands to that sidecar over a Unix-domain socket on a
// pod-local emptyDir. The file_* tools execute there too
// (POST /v1/files), so the dev-shell container is the only one that
// mounts the agent's /files/ tree: file_* and bash are one writer with
// the same mounts and privileges, and the agent container, which holds
// the Claude credentials, mounts nothing an agent can write.
//
// Wire shape: HTTP-over-UDS, mirroring the agentpod core socket. The
// daemon binary lives at cmd/dev-shell; SidecarShell implements
// agent.ShellExecutor and SidecarFiles backs the file_* tools, both
// wired in mcp_cmd.go's full-agent and subagent paths.
package devshell

import "encoding/json"

// ExecRequest is the body of POST /v1/exec. Mirrors agent.ShellRequest
// at the wire level so the in-pod client can forward without
// transformation. Cwd is resolved against the daemon's --root
// (typically /files/).
//
// Each request runs in a fresh bash subprocess. Persistence across
// calls (env exports, pip --user, pyenv state, .bashrc edits) lives
// on disk: HOME points at the per-agent PVC, BASH_ENV at a shim
// file bash sources before every command. Concurrent requests are
// independent processes — fan-out from subagents Just Works.
type ExecRequest struct {
	Command        string `json:"command"`
	Cwd            string `json:"cwd,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`

	// OverlayPrivate, when set, is the absolute host path that should
	// shadow <Root>/artifacts/private inside the bash process's view —
	// implemented by spawning bash inside `unshare -mUr` and bind-
	// mounting OverlayPrivate over /files/artifacts/private. Used by
	// subagent run_shell calls so absolute "/files/artifacts/private/…"
	// writes from the model land in the subagent's overlay rather than
	// the parent's private dir. The file_* tools get the same view by
	// rooting at the subagent's overlay (FilesRequest.Root); this is
	// the matching view for bash. Empty for full-agent calls.
	//
	// Daemon validates OverlayPrivate is under cfg.Root before using it
	// (defense in depth — the client and daemon share a pod, but a
	// path outside Root makes no sense for any caller).
	OverlayPrivate string `json:"overlay_private,omitempty"`
}

// ExecResponse mirrors agent.ShellResult. Stdout/Stderr each carry the
// 256 KiB-capped buffer with a truncation marker appended when full;
// TimedOut is true when the daemon's per-command timeout fired (vs a
// client-side cancel, which surfaces as ctx.Err on the client). Err
// is set only for environment failures (cmd.Start fail) the model
// should see as is_error=true; non-zero ExitCode alone is normal.
type ExecResponse struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
	Err        string `json:"err,omitempty"`
}

// FilesRequest is the body of POST /v1/files: one file_* tool call,
// executed by the daemon with its own mounts and privileges (the
// shell's), so file_* and run_shell are one writer with two
// interfaces. Tool is a files.Tool* name and Input the raw arguments
// the model emitted.
//
// Root names whose /files/ view the call runs against, relative to
// the daemon's --root: "" for the agent itself, "subagents/<id>" for a
// subagent, whose view core builds there (its own artifacts/private/,
// links into the parent's tree for the rest). It is the same narrowing
// run_shell's CwdPrefix gives a subagent's shell. The daemon accepts
// nothing else: no absolute path, no "..", no other shape.
type FilesRequest struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
	Root  string          `json:"root,omitempty"`
}

// FilesResponse carries a file_* result. Body is the model-visible
// text (the header line for an image), IsError the tool-level failure
// flag (not found, read-only, no unique match), and Image the bytes of
// a file_view of an image. Err is set when files.Run itself failed
// (malformed arguments); the client returns it as an error, as an
// in-process dispatcher would.
type FilesResponse struct {
	Body    string      `json:"body,omitempty"`
	IsError bool        `json:"is_error,omitempty"`
	Image   *FilesImage `json:"image,omitempty"`
	Err     string      `json:"err,omitempty"`
}

// FilesImage is an image returned by file_view. Data is the raw bytes
// (encoding/json base64-encodes a []byte); MIME its content type.
type FilesImage struct {
	Data []byte `json:"data"`
	MIME string `json:"mime"`
}

// PackagesResponse is the body of GET /v1/packages: the apt packages
// the dev-shell image has installed, one entry per package, as recorded
// at build time in PackagesFileDefault. Empty when the image carries no
// such file; the client then reports the list as unknown.
type PackagesResponse struct {
	Packages []string `json:"packages"`
}

// PackagesFileDefault is where Dockerfile.dev-shell records the
// installed package list, one package per line. The daemon reads it once
// at start (--packages-file overrides the path).
const PackagesFileDefault = "/etc/kivali/dev-shell-packages"

// FilesMaxBodyBytes caps a POST /v1/files body. The arguments arrive
// on one JSON-RPC line from the CLI, and the MCP server reads lines of
// up to 16 MiB (internal/mcp/server.go), so no file_create the server
// accepts can exceed it; the extra 64 KiB covers the envelope.
// /v1/exec keeps its own, much smaller, cap.
const FilesMaxBodyBytes = 16<<20 + 64<<10

// SocketDefault is the conventional in-pod socket path. Both the
// daemon and the client default to this; flag overrides exist for
// tests that pin a temp dir. Lives on a pod-local emptyDir mount
// distinct from /scratch so agents browsing the shell's working
// directories don't see dispatch machinery.
const SocketDefault = "/run/kivali-dev-shell/sock"

// RootDefault is the daemon's default cwd root — the agent's writable
// /files/ workspace. Cwd values from the request are resolved relative
// to this, matching the unified-files contract.
const RootDefault = "/files"
