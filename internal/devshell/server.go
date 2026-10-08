package devshell

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/kivali-ai/kivali/internal/files"
)

// ServerConfig configures the dev-shell daemon's HTTP-over-UDS handler.
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
// /data/* mounts of the dev-shell container (files.PodReadRoots). The
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
}

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
// GET /v1/packages — the image's installed package list.
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
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/exec", handleExec(cfg))
	mux.HandleFunc("POST /v1/files", handleFiles(cfg))
	mux.HandleFunc("GET /v1/packages", func(w http.ResponseWriter, _ *http.Request) {
		pkgs := cfg.Packages
		if pkgs == nil {
			pkgs = []string{} // "packages": [], never null
		}
		writeJSON(w, http.StatusOK, PackagesResponse{Packages: pkgs})
	})
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
		res, runErr := RunCommand(r.Context(), cwd, req.Command, req.OverlayPrivate, overlayDst, TimeoutFor(req.TimeoutSeconds))
		if runErr != nil {
			res.Err = runErr.Error()
		}
		writeJSON(w, http.StatusOK, res)
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
