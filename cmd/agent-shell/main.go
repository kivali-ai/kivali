// agent-shell is the per-agent shell-exec sidecar.
//
// One process per agent pod, listening on a UDS shared with the agent
// runtime container via an emptyDir mounted at /run/kivali-agent-shell/.
// POST /v1/exec runs a single bash command and returns
// stdout/stderr/exit. POST /v1/files runs one file_* tool call against
// the agent's /files/ (or a subagent's view of it). POST /v1/git runs
// one clone or fetch through an integration's git connection, in this
// process. /healthz returns 200 for liveness probes.
//
// Each request runs in a fresh bash subprocess. Persistence across
// calls is filesystem-based: HOME points into /scratch/, this
// container's half of the per-agent PVC (the agent container mounts
// the other half; neither can reach the other's), BASH_ENV at a shim
// file every bash sources at startup. Agent
// state — pip --user installs, pyenv configuration, exported vars,
// cd-style helpers — survives chat rotation, pod recreation, and
// concurrent fan-out from subagents.
//
// Users. The container starts as root and the daemon drops itself to
// a user of its own, --daemon-uid (65533, the image's agent-shell
// user), with group --run-as's gid (65532) and filesystem uid --run-as's
// uid (65532), keeping only CAP_SETUID, CAP_SETGID and CAP_KILL, with
// no_new_privs set (agentshell.DropPrivileges). Every command runs as
// --run-as, 65532, the image's kivali user and the agent container's.
// So the daemon reads and writes files as 65532, exactly as the
// commands do, while a command cannot trace, signal or read the /proc
// of the daemon that runs it.
//
// Trust boundary: the socket lives on a pod-local emptyDir (no other
// agent can reach it). It is the daemon's (65533), group 65532, mode
// 0o660: uid 65532 can connect to it from either container, the agent
// container's `kivali mcp` processes and the daemon's own commands
// alike. A daemon that did not drop root keeps the socket to itself,
// mode 0o600: its only clients are its own uid. Single-tenant. It is
// the only container in the pod that mounts /files/: run_shell and the
// file_* tools both write it from here.
//
// Network. A command's proxy variables carry the shell's egress proxy
// credential, which the agent runtime writes beside the socket
// (agentshell.ProxyCredentialFile), and its output is scrubbed of it;
// before the file exists they carry the bare proxy URL. The software
// install below keeps the bare URL.
//
// Start-up. The order is fixed:
//
//  0. set the sticky bit on the socket's directory
//     (agentshell.ProtectSocketDir), so no command can stand in for the
//     daemon's socket; a failure leaves POST /v1/git running nothing;
//  1. with --install, start `agent-shell install-software <pkgs...>`
//     (this binary, via /proc/self/exe) as a root child, which runs
//     apt-get and reports on a pipe;
//  2. drop this process to its own user as above and verify every user
//     and group ID and capability set; a failure is fatal;
//  3. only then set up HOME, create the socket and serve.
//
// The daemon never serves or runs a command as root. Started as anyone
// else, it changes nothing and runs commands as itself. Commands wait
// for the install to finish; see agentshell.ServerConfig.Install.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kivali-ai/kivali/internal/agentshell"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/files"
)

// shellEnvDefault is the contents written to BASH_ENV on first
// daemon start when the file doesn't yet exist. Sourced at the top
// of every non-interactive bash. Agents own this file: they can
// edit it freely (echo, file_str_replace, etc.) and the changes
// stick across restarts because /scratch is a PVC.
const shellEnvDefault = `# Sourced by every run_shell command (BASH_ENV). Edit freely —
# additions persist across run_shell calls, chat rotations, and
# pod restarts because $HOME lives on the per-agent PVC.
#
# Common patterns:
#   export FOO=bar              — every later run_shell sees FOO=bar
#   export PATH="$HOME/bin:$PATH"  — make a custom dir's binaries available
#   eval "$(pyenv init -)"      — bring pyenv into every shell

# pip install --user lands binaries here; put it on PATH so agents
# can invoke them by name from any later command.
export PATH="$HOME/.local/bin:$PATH"
`

func main() {
	if len(os.Args) > 1 && os.Args[1] == agentshell.InstallSubcommand {
		os.Exit(installSoftware(os.Args[2:]))
	}

	socketPath := flag.String("socket", agentshell.SocketDefault, "UDS path to listen on")
	rootPath := flag.String("root", agentshell.RootDefault, "default cwd for /v1/exec; the agent's /files/ for /v1/files")
	agentSlug := flag.String("agent", "", "slug of the agent this pod runs; names its read-only /data mounts for /v1/files")
	packagesFile := flag.String("packages-file", agentshell.PackagesFileDefault, "image's installed package list, one per line; served at GET /v1/packages")
	install := flag.String("install", "", "comma-separated Debian packages to install at start; needs the daemon to start as root")
	runAs := flag.String("run-as", agentshell.RunAsDefault, "uid:gid every command runs as when the daemon started as root; also the daemon's gid and filesystem uid")
	daemonUID := flag.Int("daemon-uid", agentshell.DaemonUIDDefault, "uid the daemon itself drops to when started as root")
	flag.Parse()

	logger := log.New(os.Stderr, "agent-shell ", log.LstdFlags|log.Lmicroseconds)

	uid, gid, err := agentshell.ParseRunAs(*runAs)
	if err != nil {
		logger.Fatalf("%v", err)
	}
	id := agentshell.Identity{DaemonUID: *daemonUID, UID: uid, GID: gid}
	creds := agentshell.HostCreds{}
	// While root: the socket's directory gets its sticky bit, so no
	// command can replace the daemon's socket with its own. Without it,
	// POST /v1/git runs nothing.
	gitUnavailable := ""
	if os.Geteuid() == 0 {
		if err := agentshell.ProtectSocketDir(filepath.Dir(*socketPath)); err != nil {
			logger.Printf("warning: protect the socket's directory: %v; git through a connection will not run", err)
			gitUnavailable = "the shell's socket is not protected, so git through the connection does not run in this shell"
		}
	}
	// Then: install as a root child if asked, and stop being root.
	// Nothing below runs with privileges.
	inst, err := agentshell.BecomeUnprivileged(creds, id,
		agentshell.ParseInstallList(*install), startInstall, logger.Printf)
	if err != nil {
		logger.Fatalf("privileges: %v", err)
	}
	ids, idsErr := creds.IDs()
	caps, capsErr := creds.Caps()
	if idsErr == nil && capsErr == nil {
		logger.Printf("running as %s caps %s", ids, caps)
	}
	// Dropped to its own user, the daemon starts every command as
	// --run-as; otherwise commands run as the daemon does.
	var commandUser *agentshell.CommandUser
	dropped := agentshell.VerifyDaemon(creds, id) == nil
	if dropped {
		commandUser = &agentshell.CommandUser{UID: uint32(uid), GID: uint32(gid)}
	} else {
		// A command of the daemon's own uid could read the daemon's
		// memory, and a git job's token in it.
		gitUnavailable = "the shell runs commands as its own user, so git through the connection does not run in this shell"
	}
	if inst != nil {
		logger.Printf("installing software: %v", inst.Requested())
		go func() {
			<-inst.Done()
			_, installed, failed := inst.State()
			logger.Printf("software installed: %v; failed: %v", installed, failed)
		}()
	}

	// The package list the image recorded at build time. Without it the
	// daemon still serves, with an empty list, and the run_shell
	// description says the list is unknown.
	packages, err := agentshell.ReadPackages(*packagesFile)
	if err != nil {
		logger.Printf("warning: read %s: %v; serving an empty package list", *packagesFile, err)
	}

	// The file_* tools follow a /files/ farm link only into the
	// read-only /data mounts this container has, and one of them is
	// per agent (its archived chats), named by the slug. Without it
	// the daemon still serves, with every read root but that one: a
	// sidecar that refused to start would take run_shell down with it.
	readRoots := files.PodReadRoots(*agentSlug)
	if *agentSlug == "" {
		logger.Print("warning: --agent not set; /files/past-chats/ will not resolve until the pod is recreated with it")
	}

	// Ensure HOME exists + the BASH_ENV shim is in place. Both are
	// load-bearing for the persistence model — without HOME, bash
	// crashes on `cd ~` or shell-startup writes; without BASH_ENV,
	// agent-edited env vars don't propagate.
	if err := bootstrapHome(logger); err != nil {
		logger.Fatalf("bootstrap home: %v", err)
	}

	// The socket is the daemon's own file: dropped, it is made with the
	// daemon's uid as its filesystem uid, so it is 65533:65532. Dropped,
	// the agent container's uid 65532 connects through the
	// socket's group; otherwise the socket is the same uid as its only
	// clients.
	socketMode := os.FileMode(0o600)
	if dropped {
		socketMode = 0o660
	}
	var ln net.Listener
	listen := func() error {
		var err error
		ln, err = listenSocket(*socketPath, socketMode)
		return err
	}
	if dropped {
		err = agentshell.AsDaemonUser(creds, id, listen)
	} else {
		err = listen()
	}
	if err != nil {
		logger.Fatalf("socket: %v", err)
	}

	srv := &http.Server{
		Handler: agentshell.NewHandler(agentshell.ServerConfig{
			Root:      *rootPath,
			ReadRoots: readRoots,
			Packages:  packages,
			Install:   inst,
			// The agent runtime writes the shell's proxy credential
			// beside the socket, on the emptyDir both containers mount.
			ProxyCredentialPath: filepath.Join(filepath.Dir(*socketPath), agentshell.ProxyCredentialFile),
			CommandUser:         commandUser,
			// POST /v1/git works under the root or the home, and goes
			// through the proxy the daemon's environment names, with the
			// connection's credential on it.
			Home:           os.Getenv("HOME"),
			GitProxyURL:    gitProxyURL(os.Getenv),
			GitUnavailable: gitUnavailable,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("serve: %v", err)
		}
	}()
	logger.Printf("listening on %s (root=%s home=%s)", *socketPath, *rootPath, os.Getenv("HOME"))

	<-ctx.Done()
	logger.Print("shutdown signal received")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	_ = os.Remove(*socketPath)
	logger.Print("shutdown complete")
}

// gitProxyURL is the daemon's bare egress proxy URL: the first proxy
// variable of its environment that is set, without user info. Empty
// when there is none.
func gitProxyURL(getenv func(string) string) string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := egresscred.BareProxyURL(getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// listenSocket removes any stale socket at path from a crashed prior
// run, listens on path and makes the socket mode: 0o660 lets its owner
// and group connect, 0o600 its owner alone, nobody else either way.
func listenSocket(path string, mode os.FileMode) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket %q: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("chmod %q: %w", path, err)
	}
	return ln, nil
}

// startInstall starts the install child: this binary, run again as
// `agent-shell install-software <pkgs...>` with the daemon's current
// (root) credentials. /proc/self/exe is the running binary itself,
// whatever its path.
func startInstall(pkgs []string) (*agentshell.Install, error) {
	return agentshell.StartInstallChild("/proc/self/exe", pkgs, agentshell.InstallEnv(os.Getenv), os.Stderr, clock.New())
}

// installSoftware is the install-software subcommand: apt-get for pkgs,
// its result written as JSON to InstallResultFD.
func installSoftware(pkgs []string) int {
	result, err := agentshell.OpenInstallResult()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-shell install-software: %v\n", err)
		return 2
	}
	in := agentshell.Installer{Env: os.Environ(), Log: os.Stderr}
	if err := agentshell.RunInstallSubcommand(in, pkgs, result); err != nil {
		fmt.Fprintf(os.Stderr, "agent-shell install-software: write result: %v\n", err)
		return 1
	}
	return 0
}

// bootstrapHome creates $HOME if it doesn't exist and writes the
// default BASH_ENV shim if no file is there yet. Both are idempotent
// — on every boot AFTER the first, this is a no-op.
//
// HOME comes from the env (Dockerfile sets it; tests can override).
// If unset, falls back to /tmp so the daemon doesn't crash, but
// persistence is broken in that case.
func bootstrapHome(logger *log.Logger) error {
	home := os.Getenv("HOME")
	if home == "" {
		logger.Print("HOME unset; falling back to /tmp (persistence will not survive pod restart)")
		home = "/tmp"
		_ = os.Setenv("HOME", home)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	bashEnv := os.Getenv("BASH_ENV")
	if bashEnv == "" {
		bashEnv = filepath.Join(home, ".shell_env")
		_ = os.Setenv("BASH_ENV", bashEnv)
	}
	if _, err := os.Stat(bashEnv); err == nil {
		// Already exists — leave the agent's edits alone.
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(bashEnv), 0o755); err != nil {
		return err
	}
	return os.WriteFile(bashEnv, []byte(shellEnvDefault), 0o644)
}
