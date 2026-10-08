package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
)

// ShellToolName is the stable name of the shell-exec tool agents use.
// Kept here rather than in the message package because this
// is not a message-producing tool — its output feeds back to the model
// as a tool_result rather than persisting as a Kivali Message.
const ShellToolName = "run_shell"

// ShellTool is the Anthropic-compatible tool definition that exposes
// an agent's run_shell capability — commands run inside the agent's
// own pod against the unified /files/ filesystem. The file_* tools
// execute in the same container with the same privileges, so
// file_view paths and shell paths point at the same physical bytes.
//
// The toolkit paragraph is not written by hand: it names the packages
// the running dev-shell reports (see ShellPackageLister), because a
// description that disagrees with the image sends agents to plan
// workflows around tools that are not there. With
// no list to name, ShellTool states that instead of guessing.
func ShellTool() provider.Tool { return ShellToolWithPackages(nil) }

// ShellPackageLister is implemented by a ShellExecutor that can say
// which packages its shell has installed: the dev-shell sidecar client.
// Executors that cannot (tests, the in-process local shell) simply do
// not implement it.
type ShellPackageLister interface {
	Packages(ctx context.Context) ([]string, error)
}

// shellPackagesTimeout bounds the package fetch. The fetch runs while
// the MCP server answers initialize, so it must finish before the
// Claude Code CLI gives up on the server (its MCP connect timeout is 30s
// by default); the sidecar client's own 30s dial grace plus backoff
// would outlast that and leave the agent with no kivali tools at all,
// which is worse than the fallback sentence. 20s covers a cold image
// pull of the dev-shell.
const shellPackagesTimeout = 20 * time.Second

// ShellPackages asks exec for its installed packages, once, bounded by
// shellPackagesTimeout; the caller's context otherwise passes through.
// It returns nil when exec cannot say or the lookup fails or times out;
// ShellToolWithPackages(nil) then words the toolkit paragraph
// accordingly.
func ShellPackages(ctx context.Context, exec ShellExecutor) []string {
	l, ok := exec.(ShellPackageLister)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, shellPackagesTimeout)
	defer cancel()
	pkgs, err := l.Packages(ctx)
	if err != nil {
		return nil
	}
	return pkgs
}

// shellToolkitUnknown is the toolkit paragraph when the dev-shell's
// package list is not available (daemon unreachable, or an image with
// no list file).
const shellToolkitUnknown = "The shell is bash on Debian. Its package list could not be read, so check for a tool with \"command -v <tool>\" before relying on it."

// shellCommandNames are the well-known packages whose command is not
// the package name. Only these are annotated; nothing else is guessed.
var shellCommandNames = map[string]string{
	"ripgrep":     "rg",
	"fd-find":     "fd",
	"imagemagick": "magick, convert",
	"python3-pip": "pip3",
}

// shellToolkitText is the toolkit paragraph for a known package list.
func shellToolkitText(pkgs []string) string {
	if len(pkgs) == 0 {
		return shellToolkitUnknown
	}
	names := make([]string, len(pkgs))
	for i, p := range pkgs {
		names[i] = p
		if cmd, ok := shellCommandNames[p]; ok {
			names[i] = p + " (" + cmd + ")"
		}
	}
	return "The shell is bash. Preinstalled packages: " + strings.Join(names, ", ") + "."
}

// ShellToolWithPackages is ShellTool with the toolkit paragraph naming
// pkgs, the dev-shell's installed package list. An empty list gets the
// fallback sentence.
func ShellToolWithPackages(pkgs []string) provider.Tool {
	return provider.Tool{
		Name: ShellToolName,
		Description: `Run a shell command in your agent pod. The command runs against your /files/ workspace — the same filesystem you read with file_view and write with file_create, with the same privileges. Read /files/project/foo.csv with bash, write /files/artifacts/private/out.json with bash, and the next file_view sees it. Default cwd is /files/; pass a cwd to start somewhere else under that root.

Read-only here, as for file_create: /files/project/, skills/, attachments/, past-chats/, episodes/, and the published trees (/files/artifacts/public/, /files/artifacts/shared/). Everything else under /files/ is your workspace. A command cannot publish by writing into the published trees; artifact_publish copies a workspace file or directory there.

` + shellToolkitText(pkgs) + `

PERSISTENT ENVIRONMENT. Each run_shell call is a fresh bash subprocess, but $HOME is on a per-agent persistent disk and $BASH_ENV (~/.shell_env) is sourced before every command. So:

- "pip install --user <pkg>" persists across calls + chat rotations + pod restarts. Future commands find the binaries on $PATH (~/.shell_env adds $HOME/.local/bin to PATH by default).
- Edit ~/.shell_env to export env vars, prepend PATH, source pyenv init, etc. Changes stick.
- Cwd does NOT persist between calls — each command starts at the cwd you pass (or /files/). If you want a series of commands in one place, prepend "cd <path> && " to each, or chain inside one command with && / newlines.

Apt-get is NOT available — the container runs as a non-root user and cannot install system packages at runtime. For Python use "pip install --user <pkg>" or a venv. If you need a system package that is not installed, publish a ceo_notification describing what you need and why; an operator adds it by rebuilding the dev-shell image with the package named in the DEV_SHELL_EXTRA_PACKAGES build arg.

Use this for: running tests, experimenting with libraries, document conversion, image processing, processing data, prototyping scripts, anything that wants a shell. Chain steps inside a single "command" with && / ; / newlines. stdout and stderr are each capped at 256KB with a truncation marker. Both are persisted in your chat history as a tool_result and replay on every subsequent turn until your chat rotates — treat each large output like spending tokens. For commands that produce a lot, redirect to a file in /files/artifacts/private/ and read just the section you need (grep -n, head, sed -n 'A,Bp') on a subsequent call. Default timeout is 60 seconds; raise it with "timeout_seconds" up to 600.

` + ShellHandOffText + `

` + ShellClosingText,
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command":         {"type": "string", "description": "the shell command, executed as bash -c"},
    "cwd":             {"type": "string", "description": "working directory relative to /files/ (created if missing); empty defaults to /files/"},
    "timeout_seconds": {"type": "integer", "description": "max runtime, default 60, max 600"}
  },
  "required": ["command"]
}`),
	}
}

// ShellHandOffText is run_shell's paragraph on getting a produced file
// to someone else, for an agent that has publish_* and share_file.
const ShellHandOffText = "To hand a file the command produced to another agent, write it under /files/artifacts/private/ and reference its path in publish_*'s attachments[], or make it durable for everyone with artifact_publish. share_file shows a file to the user in this chat. No need to base64-encode or content-address it yourself."

// ShellClosingText is run_shell's last paragraph, for an agent that
// reports through assignments.
const ShellClosingText = "You see the tool_result and can iterate — a typical flow is: run → inspect output → fix something → run again. When you are done and have a result to report, close the assignment you were working with assignment_close, or send a notice, as usual."
