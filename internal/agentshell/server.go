package agentshell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/files"
)

// ServerConfig configures the agent-shell daemon's HTTP-over-UDS handler.
// Root is the absolute path bash starts in by default — typically
// /files/ in production so cwd-relative paths resolve under the
// agent's writable workspace. Tests pin a temp dir.
//
// MaxBodyBytes caps a /v1/exec request body to fail closed against an
// inadvertent large upload (the 256 KiB output cap is enforced
// downstream in RunCommand). 64 KiB is generous for a single
// command-line. FilesMaxBodyBytes is the /v1/files cap, which has to
// admit a whole file_create; zero means FilesMaxBodyBytes.
//
// ReadRoots are the host directories outside Root that the /files/
// symlink farms resolve through, read-only: in an agent pod the
// /data/* mounts of the agent-shell container (files.PodReadRoots). The
// file_* tools follow a link into Root or one of these and nowhere
// else.
type ServerConfig struct {
	Root              string
	MaxBodyBytes      int64
	FilesMaxBodyBytes int64
	ReadRoots         []string

	// Packages is the image's installed package list (ReadPackages),
	// served by GET /v1/packages. Nil when the image records none.
	Packages []string

	// Install is the software install the daemon started with, nil
	// when there is none. While it runs, POST /v1/exec waits for it
	// (up to ExecInstallWait) so commands see the software, and
	// GET /v1/packages waits up to PackagesInstallWait. POST /v1/files
	// never waits.
	Install *Install

	// Clock drives the two waits. Nil means the real clock.
	Clock clock.Clock

	// ExecInstallWait and PackagesInstallWait bound the waits; zero
	// means DefaultExecInstallWait and DefaultPackagesInstallWait.
	ExecInstallWait     time.Duration
	PackagesInstallWait time.Duration

	// ProxyCredentialPath is the shell's egress proxy credential, the
	// ProxyCredentialFile beside the socket. POST /v1/exec reads it for
	// each command: present, the command's proxy variables carry it and
	// its output is scrubbed of it; missing or malformed, the command
	// gets the bare proxy URL. Empty means never read.
	ProxyCredentialPath string

	// CommandUser is the uid and gid POST /v1/exec runs each command
	// as, set when the daemon dropped root to a user of its own. Nil
	// runs commands as the daemon.
	CommandUser *CommandUser

	// Home is the shell's home directory ($HOME, /scratch/home in an
	// agent pod): POST /v1/git works in a directory under Root or under
	// it. Empty, only Root.
	Home string

	// GitProxyURL is the daemon's bare egress proxy URL, from its own
	// environment: POST /v1/git gives it to go-git with the request's
	// proxy credential. Empty (no proxy in the environment), an
	// operation connects directly.
	GitProxyURL string

	// GitUnavailable, when set, is why POST /v1/git runs nothing here:
	// the daemon did not drop to a user of its own, so a command could
	// read its memory, or the socket's directory is not protected
	// (ProtectSocketDir), so a command could stand in for the daemon.
	// Either would hand a git job's token to the agent's commands.
	GitUnavailable string

	// GitInsecureHTTP lets POST /v1/git reach a repository over http://.
	// For tests only, whose git server is an httptest one; an agent pod
	// never sets it, and anything but https is refused.
	GitInsecureHTTP bool
}

// DefaultPackagesInstallWait bounds how long GET /v1/packages waits for
// a running install. The agent's MCP server fetches the list while it
// answers initialize and gives up after 20s (agent.ShellPackages), so
// the daemon answers before that with the packages still installing
// named as such.
const DefaultPackagesInstallWait = 15 * time.Second

// DefaultExecInstallWait bounds how long POST /v1/exec waits for a
// running install before answering that the shell is still installing.
// The client allows for it on top of the command's own timeout
// (SidecarShell.Exec), so it answers rather than giving up first.
const DefaultExecInstallWait = 10 * time.Minute

// NewHandler returns the http.Handler that backs the daemon.
//
// POST /v1/exec — runs one command. Each request spawns a fresh
// bash subprocess; concurrent requests run in parallel because
// each is its own process and each Goroutine.
//
// POST /v1/files — runs one file_* tool call (files.Run) in this
// process, against a Backend built from Root, the request's root
// and ReadRoots.
//
// GET /v1/packages — the packages the shell has, and the state of the
// install the daemon started with.
//
// POST /v1/git — one clone or fetch through an integration's git
// connection, in this process (git.go).
//
// GET /healthz — k8s liveness probe.
//
// Anything else returns 404.
func NewHandler(cfg ServerConfig) http.Handler {
	if cfg.Root == "" {
		cfg.Root = RootDefault
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 64 << 10 // 64 KiB
	}
	if cfg.FilesMaxBodyBytes <= 0 {
		cfg.FilesMaxBodyBytes = FilesMaxBodyBytes
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.New()
	}
	if cfg.ExecInstallWait <= 0 {
		cfg.ExecInstallWait = DefaultExecInstallWait
	}
	if cfg.PackagesInstallWait <= 0 {
		cfg.PackagesInstallWait = DefaultPackagesInstallWait
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/exec", handleExec(cfg))
	mux.HandleFunc("POST /v1/files", handleFiles(cfg))
	mux.HandleFunc("GET /v1/packages", handlePackages(cfg))
	mux.HandleFunc("POST /v1/git", handleGit(cfg))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func handleExec(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxBodyBytes)
		var req ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "decode ExecRequest: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Command == "" {
			http.Error(w, "command is required", http.StatusBadRequest)
			return
		}
		// The command runs once the software the shell starts with is
		// in place, so it can use it.
		switch waitInstall(r.Context(), cfg.Clock, cfg.Install, cfg.ExecInstallWait) {
		case waitCanceled:
			return // the caller is gone
		case waitTimedOut:
			writeJSON(w, http.StatusOK, ExecResponse{
				ExitCode: -1,
				Err:      "the shell is still installing software (" + strings.Join(cfg.Install.Requested(), ", ") + "); run the command again in a few minutes",
			})
			return
		}
		cwd, err := ResolveCwd(cfg.Root, req.Cwd)
		if err != nil {
			writeJSON(w, http.StatusOK, ExecResponse{
				ExitCode: -1,
				Err:      fmt.Sprintf("resolve cwd %q: %v", req.Cwd, err),
			})
			return
		}
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			writeJSON(w, http.StatusOK, ExecResponse{
				ExitCode: -1,
				Err:      fmt.Sprintf("mkdir cwd %q: %v", cwd, err),
			})
			return
		}
		// Validate OverlayPrivate is under cfg.Root before passing it to
		// the bind-mount wrapper. Defense in depth — the client and
		// daemon share a pod, so a path outside Root is necessarily a
		// caller bug rather than a real isolation request.
		var overlayDst string
		if req.OverlayPrivate != "" {
			clean := filepath.Clean(req.OverlayPrivate)
			if !filepath.IsAbs(clean) || (clean != cfg.Root && !strings.HasPrefix(clean, cfg.Root+string(filepath.Separator))) {
				writeJSON(w, http.StatusOK, ExecResponse{
					ExitCode: -1,
					Err:      fmt.Sprintf("overlay_private %q must be an absolute path under %q", req.OverlayPrivate, cfg.Root),
				})
				return
			}
			// Destination is fixed by daemon root: <Root>/artifacts/private
			// is the path /v1/files resolves under the overlay for a
			// subagent root, so bash-side and tool-side paths agree.
			overlayDst = filepath.Join(cfg.Root, "artifacts", "private")
		}
		// Read for each command: the runtime writes the file once core
		// has the credential, which can be after the daemon starts.
		cred := readShellCredential(cfg.ProxyCredentialPath)
		res, runErr := RunCommand(r.Context(), cwd, req.Command, req.OverlayPrivate, overlayDst, TimeoutFor(req.TimeoutSeconds), cred.url, cfg.CommandUser)
		if runErr != nil {
			res.Err = runErr.Error()
		}
		cred.scrub(&res)
		// The notice is for the agent itself, once: a subagent's command
		// (OverlayPrivate set) or a caller that has gone away leaves it
		// for the agent's next command.
		if req.OverlayPrivate == "" && r.Context().Err() == nil {
			res.Notice = cfg.Install.TakeNotice()
		}
		writeJSON(w, http.StatusOK, res)
	}
}

func handlePackages(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if waitInstall(r.Context(), cfg.Clock, cfg.Install, cfg.PackagesInstallWait) == waitCanceled {
			return
		}
		installing, installed, failed := cfg.Install.State()
		pkgs := cfg.Packages
		if len(installed) > 0 {
			pkgs = mergePackages(cfg.Packages, installed)
		}
		if pkgs == nil {
			pkgs = []string{} // "packages": [], never null
		}
		writeJSON(w, http.StatusOK, PackagesResponse{Packages: pkgs, Installing: installing, Failed: failed})
	}
}

type waitOutcome int

const (
	waitFinished waitOutcome = iota
	waitTimedOut
	waitCanceled
)

// waitInstall waits for inst to finish, for at most bound on c, or
// until ctx ends.
func waitInstall(ctx context.Context, c clock.Clock, inst *Install, bound time.Duration) waitOutcome {
	if inst.Finished() {
		return waitFinished
	}
	t := c.NewTimer(bound)
	defer t.Stop()
	select {
	case <-inst.Done():
		return waitFinished
	case <-t.C():
		return waitTimedOut
	case <-ctx.Done():
		return waitCanceled
	}
}

func handleFiles(cfg ServerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, cfg.FilesMaxBodyBytes)
		var req FilesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			status := http.StatusBadRequest
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "decode FilesRequest: "+err.Error(), status)
			return
		}
		if !files.IsToolName(req.Tool) {
			http.Error(w, fmt.Sprintf("unknown file tool %q", req.Tool), http.StatusBadRequest)
			return
		}
		b, err := filesBackend(cfg, req.Root)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body, isError, img, runErr := files.Run(b, req.Tool, req.Input)
		out := FilesResponse{Body: body, IsError: isError}
		if runErr != nil {
			out = FilesResponse{Err: runErr.Error()}
		}
		if img != nil {
			out.Image = &FilesImage{Data: img.Data, MIME: img.MIME}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// filesBackend builds the Backend for one file_* call. The root is ""
// (the agent's own /files/, which is cfg.Root) or "subagents/<id>"
// (the view core built for that subagent) and nothing else, so no
// request can point the tools at an arbitrary directory.
//
// A subagent's view links into its parent's tree (background/,
// artifacts/public/), so the parent's root is a WriteRoot, as it is
// for the subagent's shell. Both views read through the same RO
// mounts: the subagent runs in its parent's pod.
func filesBackend(cfg ServerConfig, root string) (*files.Backend, error) {
	if root == "" {
		return &files.Backend{Root: cfg.Root, ReadRoots: cfg.ReadRoots}, nil
	}
	id, ok := strings.CutPrefix(root, "subagents/")
	if !ok || id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`) {
		return nil, fmt.Errorf("root %q: want \"\" or \"subagents/<id>\"", root)
	}
	overlay := filepath.Join(cfg.Root, "subagents", id)
	return &files.Backend{
		Root:       overlay,
		ReadRoots:  cfg.ReadRoots,
		WriteRoots: []string{files.SubagentParentRoot(overlay)},
	}, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
