// Command kivali-supervisor runs a local Kivali org in the Kivali
// Desktop VM, from the command line: `serve` owns the VM and an
// owner-only RPC endpoint (a Unix socket, or a named pipe on Windows);
// every other subcommand is a client of that endpoint (`up` starts
// `serve` in the background when none is running). See
// docs/developers/supervisor.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kivali-ai/kivali/internal/claudeagent"
	"github.com/kivali-ai/kivali/internal/supervisor"
	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

var version = "dev"

const usage = `usage: kivali-supervisor [--config-dir DIR] <command> [flags]

commands:
  serve        own the VM and serve the RPC endpoint (started by up if needed)
  up           boot the VM, forward the port, install Kivali if needed
  down         shut the VM down cleanly (--exit also stops serve)
  status       show the VM, guest and local.json
  install      install Kivali into the running VM
  load-images  import an image archive (--from FILE|-, a docker or OCI tar) and restart onto it
  restart      restart the agent pods and the server onto the images now in the VM (the dev loop)
  forward      move the host port forward (--port)
  check        check the release feed for a new version
  upgrade      upgrade to the feed's release, with snapshot and rollback
  backup       write a backup of the org to a file (--out), the zip the app downloads
  restore      restore a backup zip into a freshly set up org (--in), as the app does
  credential   show how the org's Claude CLI is signed in (sign in with terminal: Claude opens its sign-in menu)
  handoff      print a one-time token that signs the owner in at /auth/handoff (2 minutes)
  destroy      delete the org and its data disk (--yes; --exit also stops serve)
  terminal     open Claude Code in the server container (--shell for bash)
  exec         run a command in the guest as root, e.g. exec -- k3s kubectl get pods -A
  broker       the Hyper-V broker (Windows): --console, install/uninstall the service, status/setup
  version      print the version
`

// defaultConfigDir is $KIVALI_CONFIG_DIR, else the host's (Application
// Support on macOS, %LOCALAPPDATA% on Windows, $XDG_DATA_HOME on Linux).
func defaultConfigDir(h supervisor.Host) string {
	if d := os.Getenv("KIVALI_CONFIG_DIR"); d != "" {
		return d
	}
	d, err := h.DefaultConfigDir()
	if err != nil {
		return "Kivali"
	}
	return d
}

func main() {
	log.SetFlags(0)
	global := flag.NewFlagSet("kivali-supervisor", flag.ExitOnError)
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	var h supervisor.Host = host.Default()
	configDir := global.String("config-dir", defaultConfigDir(h), "configuration directory (local.json, data disk, RPC endpoint)")
	_ = global.Parse(os.Args[1:])
	args := global.Args()
	if len(args) == 0 {
		global.Usage()
		os.Exit(2)
	}
	dir, err := filepath.Abs(*configDir)
	if err != nil {
		log.Fatal(err)
	}
	cmd, rest := args[0], args[1:]
	c := &cli{configDir: dir, host: h, backend: newBackend(), client: &supervisor.Client{Host: h, Dir: dir}}
	var code int
	switch cmd {
	case "serve":
		code = c.serve(rest)
	case "up":
		code = c.up(rest)
	case "down":
		code = c.down(rest)
	case "status":
		code = c.status(rest)
	case "install":
		code = c.install(rest)
	case "load-images":
		code = c.loadImages(rest)
	case "restart":
		code = c.restart(rest)
	case "forward":
		code = c.forward(rest)
	case "check":
		code = c.check(rest)
	case "upgrade":
		code = c.upgrade(rest)
	case "backup":
		code = c.backup(rest)
	case "restore":
		code = c.restore(rest)
	case "credential":
		code = c.credential(rest)
	case "handoff":
		code = c.handoff(rest)
	case "destroy":
		code = c.destroy(rest)
	case "terminal":
		code = c.terminal(rest)
	case "exec":
		code = c.exec(rest)
	case "broker":
		code = c.broker(rest)
	case "version":
		fmt.Println(version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		code = 2
	}
	os.Exit(code)
}

type cli struct {
	configDir string
	host      supervisor.Host
	backend   supervisor.Backend // nil where this build has none
	client    *supervisor.Client
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "kivali-supervisor:", err)
	return 1
}

// errNoBackend is serve's answer where this build has no VM backend.
var errNoBackend = errors.New("this build has no VM backend: serve needs macOS on Apple Silicon (Virtualization.framework) or Windows (Hyper-V)")

// vmDir resolves where the VM image lives: the flag, $KIVALI_VM_DIR,
// a vm directory next to the executable, or vm/build/out under the
// current directory (a repository checkout); the first the backend
// recognises as an image.
func (c *cli) vmDir(flagVal string) (string, error) {
	if c.backend == nil {
		return "", errNoBackend
	}
	cands := []string{flagVal, os.Getenv("KIVALI_VM_DIR")}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "vm"))
	}
	cands = append(cands, filepath.Join("vm", "build", "out"))
	for _, d := range cands {
		if d == "" {
			continue
		}
		_, err := c.backend.Image(d)
		if err == nil {
			return filepath.Abs(d)
		}
		if d == flagVal {
			return "", fmt.Errorf("--vm-dir: %w", err)
		}
	}
	return "", fmt.Errorf("no VM image found (%s): pass --vm-dir (the directory holding the VM image) or build one with `make -C vm image`", vmImageHint)
}

func (c *cli) serve(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	vmFlag := fs.String("vm-dir", "", "directory holding the VM image")
	_ = fs.Parse(args)
	vd, err := c.vmDir(*vmFlag)
	if err != nil {
		return fail(err)
	}
	// One serve per config directory, decided by the lock before the
	// RPC endpoint is touched.
	unlock, err := c.host.LockServe(c.configDir)
	if errors.Is(err, host.ErrServeRunning) {
		fmt.Fprintln(os.Stderr, "kivali-supervisor serve:", err, "; leaving it to that one")
		return 3
	}
	if err != nil {
		return fail(err)
	}
	defer unlock()
	logger := log.New(os.Stderr, "", log.LstdFlags)
	sup, err := supervisor.New(supervisor.Options{
		ConfigDir: c.configDir,
		VMDir:     vd,
		Backend:   c.backend,
		Host:      c.host,
		Version:   version,
		Logf:      logger.Printf,
		Models:    claudeagent.SelectableModelIDs(),
	})
	if err != nil {
		return fail(err)
	}
	ln, err := c.host.Listen(c.configDir)
	if err != nil {
		return fail(err)
	}
	rpc := supervisor.NewRPCServer(sup)
	srv := &http.Server{Handler: rpc.Handler(), ReadHeaderTimeout: 30 * time.Second}
	logger.Printf("kivali-supervisor %s serving on %s (config %s, VM image %s)", version, c.client.Endpoint(), c.configDir, vd)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	// On Windows a detached serve has no console, so no console control
	// event ever arrives: it exits only through `down --exit` (or being
	// terminated, which leaves the journal for the next up).
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigs:
		logger.Printf("%v: shutting the VM down", sig)
	case <-rpc.Quit():
	case err := <-errc:
		logger.Printf("serve: %v", err)
	}
	// A running upgrade is waited for, however long (serve keeps
	// answering status meanwhile); only the stop itself is bounded.
	if err := sup.Shutdown(5*time.Minute, nil); err != nil {
		logger.Printf("shutdown: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Closing the listener also removes the socket file (a pipe goes
	// with its last handle).
	_ = srv.Shutdown(ctx)
	return 0
}

// ensureServe starts `serve` in the background if nothing answers on
// the RPC endpoint, and waits for it.
func (c *cli) ensureServe(vmFlag string) error {
	ctx := context.Background()
	if c.client.Ping(ctx) {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(c.configDir, "logs"), 0o700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"--config-dir", c.configDir, "serve"}
	if vmFlag != "" {
		abs, err := filepath.Abs(vmFlag)
		if err != nil {
			return err
		}
		args = append(args, "--vm-dir", abs)
	} else {
		vd, err := c.vmDir("")
		if err != nil {
			return err
		}
		args = append(args, "--vm-dir", vd)
	}
	logPath := filepath.Join(c.configDir, "logs", "supervisor.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lf.Close() }()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	c.host.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start serve: %w", err)
	}
	fmt.Fprintf(os.Stderr, "started serve (pid %d, log %s)\n", cmd.Process.Pid, logPath)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// The serve started here may lose the lock to one started at the
	// same moment (it then exits 3); either way, whichever serve owns the
	// directory answers on the endpoint.
	var exitErr error
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c.client.Ping(ctx) {
			return nil
		}
		select {
		case err := <-exited:
			exitErr = err
			exited = nil
		case <-time.After(100 * time.Millisecond):
		}
	}
	if exitErr != nil {
		return fmt.Errorf("serve exited (%v) and no other serve answers on %s; see %s", exitErr, c.client.Endpoint(), logPath)
	}
	return fmt.Errorf("serve did not open %s; see %s", c.client.Endpoint(), logPath)
}

func printLog(line string) { fmt.Fprintln(os.Stderr, "  "+line) }

// op runs an operation and prints its log.
func (c *cli) op(name string, body any) (json.RawMessage, int) {
	if !c.client.Ping(context.Background()) {
		return nil, fail(fmt.Errorf("serve is not running (no answer on %s); run `kivali-supervisor up`", c.client.Endpoint()))
	}
	res, err := c.client.Op(context.Background(), name, body, printLog)
	if err != nil {
		return nil, fail(err)
	}
	return res, 0
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func installFlags(fs *flag.FlagSet) func() (supervisor.InstallOptions, error) {
	owner := fs.String("owner", "", "Google account that owns the org (required on the first up)")
	chart := fs.String("chart", "", "chart tarball on the host (default: the chart baked into the VM image)")
	clientID := fs.String("public-client-id", "", "OAUTH_PUBLIC_CLIENT_ID for the server (oauth.publicClientID)")
	var env, set multiFlag
	fs.Var(&env, "env", "extra server environment NAME=VALUE, for names the chart does not set (repeatable)")
	fs.Var(&set, "set", "chart value override path.to.key=value (repeatable)")
	return func() (supervisor.InstallOptions, error) {
		o := supervisor.InstallOptions{Owner: *owner, PublicClientID: *clientID, Env: env, Set: set}
		if *chart != "" {
			abs, err := filepath.Abs(*chart)
			if err != nil {
				return o, err
			}
			o.Chart = abs
		}
		return o, nil
	}
}

func (c *cli) up(args []string) int {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	inst := installFlags(fs)
	port := fs.Int("port", 0, "host loopback port for the org (default 8080, then whatever local.json says)")
	portHint := fs.Int("port-hint", 0, "first port of an org with none recorded yet; moved if busy")
	mem := fs.Uint64("memory-mb", 0, "VM memory, MiB (default 4096)")
	cpus := fs.Uint("cpus", 0, "VM CPUs (default 4)")
	prepare := fs.Bool("prepare", false, "boot and forward only; install on a later up, once the owner is known")
	vmFlag := fs.String("vm-dir", "", "VM image directory, used when up starts serve")
	_ = fs.Parse(args)
	o, err := inst()
	if err != nil {
		return fail(err)
	}
	if err := c.ensureServe(*vmFlag); err != nil {
		return fail(err)
	}
	start := time.Now()
	res, code := c.op("up", supervisor.UpOptions{Install: o, Port: *port, PortHint: *portHint, MemoryMB: *mem, CPUs: *cpus, Prepare: *prepare})
	if code != 0 {
		return code
	}
	var r supervisor.Report
	_ = json.Unmarshal(res, &r)
	fmt.Printf("Kivali %s is up at %s (%s)\n", r.State.Kivali, r.URL, time.Since(start).Round(time.Second))
	return 0
}

func (c *cli) down(args []string) int {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	exit := fs.Bool("exit", false, "also stop serve")
	_ = fs.Parse(args)
	_, code := c.op("down", supervisor.DownRequest{Exit: *exit})
	return code
}

func (c *cli) status(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the full report as JSON")
	_ = fs.Parse(args)
	ctx := context.Background()
	if !c.client.Ping(ctx) {
		dataDisk := ""
		if c.backend != nil {
			dataDisk = c.backend.DataDiskPath(c.configDir)
		}
		st, err := supervisor.LoadState(c.configDir, dataDisk)
		if err != nil {
			return fail(err)
		}
		if *asJSON {
			return printJSON(supervisor.Report{State: st, ConfigDir: c.configDir})
		}
		fmt.Printf("serve: not running\nkivali: %s\n", orNone(st.Kivali))
		printJournal(st.Upgrade)
		return 0
	}
	r, err := c.client.Status(ctx)
	if err != nil {
		return fail(err)
	}
	if *asJSON {
		return printJSON(r)
	}
	fmt.Printf("serve: running (supervisor %s, config %s)\n", r.Version, r.ConfigDir)
	if !r.Running {
		fmt.Printf("vm: stopped\nkivali: %s\n", orNone(r.State.Kivali))
		printJournal(r.State.Upgrade)
		return 0
	}
	fmt.Printf("vm: running (%d CPUs, %d MiB), forward %s\n", r.State.CPUs, r.State.MemoryMB, orNone(r.Forward))
	if g := r.Guest; g != nil {
		fmt.Printf("guest: %s, node ready %v, boot %s\n", g.State, g.NodeReady, g.BootID)
		if g.Fatal != "" {
			fmt.Printf("guest fatal: %s\n", g.Fatal)
		}
		fmt.Printf("versions: vm image %s, agent %s, kernel %s, %s\n", orNone(g.Versions.VMImage), g.Versions.Agent, g.Versions.Kernel, g.Versions.K3s)
	} else {
		fmt.Printf("guest: unreachable: %s\n", r.GuestError)
	}
	fmt.Printf("kivali: %s\n", orNone(r.State.Kivali))
	if r.State.LastCheck != nil {
		fmt.Printf("last check: %s (ok %v)\n", r.State.LastCheck.Format(time.RFC3339), r.State.LastCheckOK)
	}
	printJournal(r.State.Upgrade)
	if r.Busy {
		fmt.Println("an operation is running")
	}
	return 0
}

func printJournal(j *supervisor.Journal) {
	if j != nil {
		fmt.Printf("upgrade journal: %s -> %s, step %s, snapshot complete %v\n", j.From, j.To, j.Step, j.SnapshotComplete)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(err)
	}
	return 0
}

func (c *cli) install(args []string) int {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	inst := installFlags(fs)
	_ = fs.Parse(args)
	o, err := inst()
	if err != nil {
		return fail(err)
	}
	_, code := c.op("install", o)
	return code
}

func (c *cli) loadImages(args []string) int {
	fs := flag.NewFlagSet("load-images", flag.ExitOnError)
	from := fs.String("from", "", "the image archive to import, a docker or OCI tar (docker save, ctr images export); - reads it from stdin")
	_ = fs.Parse(args)
	if *from == "" {
		return fail(errors.New("load-images: --from FILE|- is required"))
	}
	path := *from
	if path == "-" {
		// serve reads a file, not this stdin: it lands in the config
		// directory, which serve can read as this user.
		f, err := os.CreateTemp(c.configDir, ".load-images-*.tar")
		if err != nil {
			return fail(err)
		}
		defer func() { _ = os.Remove(f.Name()) }()
		if _, err := io.Copy(f, os.Stdin); err != nil {
			_ = f.Close()
			return fail(fmt.Errorf("load-images: reading stdin: %w", err))
		}
		if err := f.Close(); err != nil {
			return fail(err)
		}
		path = f.Name()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fail(err)
	}
	_, code := c.op("load-images", supervisor.LoadImagesRequest{Path: abs})
	return code
}

func (c *cli) restart(args []string) int {
	fs := flag.NewFlagSet("restart", flag.ExitOnError)
	_ = fs.Parse(args)
	_, code := c.op("restart", supervisor.RestartRequest{})
	return code
}

func (c *cli) forward(args []string) int {
	fs := flag.NewFlagSet("forward", flag.ExitOnError)
	port := fs.Int("port", 0, "host loopback port")
	_ = fs.Parse(args)
	_, code := c.op("forward", supervisor.ForwardRequest{Port: *port})
	return code
}

func (c *cli) check(args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	feed := fs.String("feed", "", "release.json URL or path (default: local.json's feed)")
	_ = fs.Parse(args)
	res, code := c.op("check", supervisor.CheckRequest{Feed: *feed})
	if code != 0 {
		return code
	}
	var r supervisor.CheckResult
	_ = json.Unmarshal(res, &r)
	fmt.Printf("%s: %s\n", r.Status, r.Message)
	return 0
}

func (c *cli) upgrade(args []string) int {
	fs := flag.NewFlagSet("upgrade", flag.ExitOnError)
	feed := fs.String("feed", "", "release.json URL or path (default: local.json's feed)")
	force := fs.Bool("force", false, "upgrade even when the release is not newer")
	_ = fs.Parse(args)
	f := *feed
	if f != "" && !strings.Contains(f, "://") {
		abs, err := filepath.Abs(f)
		if err != nil {
			return fail(err)
		}
		f = abs
	}
	_, code := c.op("upgrade", supervisor.UpgradeOptions{Feed: f, Force: *force})
	return code
}

func (c *cli) exec(args []string) int {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	stdin := fs.Bool("i", false, "pass stdin to the command")
	_ = fs.Parse(args)
	if fs.NArg() == 0 {
		return fail(errors.New("exec: no command"))
	}
	ctx := context.Background()
	if !c.client.Ping(ctx) {
		return fail(errors.New("serve is not running; run `kivali-supervisor up`"))
	}
	var in io.Reader
	if *stdin {
		in = os.Stdin
	}
	code, err := c.client.Exec(ctx, fs.Args(), in, os.Stdout, os.Stderr)
	if err != nil {
		return fail(err)
	}
	return code
}

func (c *cli) backup(args []string) int {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	out := fs.String("out", "", "file to write the backup to (.zip), inside the config directory or your home directory")
	_ = fs.Parse(args)
	if *out == "" {
		return fail(errors.New("backup: --out is required"))
	}
	abs, err := filepath.Abs(*out)
	if err != nil {
		return fail(err)
	}
	// serve writes the file itself and reports success only once the
	// archive is complete; a failure can never look like a backup.
	res, code := c.op("backup", supervisor.BackupRequest{Path: abs})
	if code != 0 {
		return code
	}
	var r supervisor.BackupResult
	_ = json.Unmarshal(res, &r)
	fmt.Printf("backup written to %s (%d bytes)\n", r.Path, r.Bytes)
	return 0
}

func (c *cli) restore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	in := fs.String("in", "", "backup zip to restore (the one the app or `backup` writes), inside the config directory or your home directory")
	_ = fs.Parse(args)
	if *in == "" {
		return fail(errors.New("restore: --in is required"))
	}
	abs, err := filepath.Abs(*in)
	if err != nil {
		return fail(err)
	}
	// The org's own restore runs it: refused unless the org is freshly
	// set up, checked whole before anything is written.
	if _, code := c.op("restore", supervisor.RestoreRequest{Path: abs}); code != 0 {
		return code
	}
	fmt.Printf("restored %s\n", abs)
	return 0
}

func (c *cli) credential(args []string) int {
	fs := flag.NewFlagSet("credential", flag.ExitOnError)
	_ = fs.Parse(args)
	ctx := context.Background()
	if !c.client.Ping(ctx) {
		return fail(errors.New("serve is not running; run `kivali-supervisor up`"))
	}
	st, err := c.client.Credential(ctx)
	if err != nil {
		return fail(err)
	}
	printCredential(st)
	return 0
}

// handoff prints a one-time sign-in token for the owner, for debugging
// the desktop app's first open of a team: open
// http://127.0.0.1:<port>/auth/handoff?t=<token> within two minutes.
func (c *cli) handoff(args []string) int {
	fs := flag.NewFlagSet("handoff", flag.ExitOnError)
	_ = fs.Parse(args)
	ctx := context.Background()
	if !c.client.Ping(ctx) {
		return fail(errors.New("serve is not running; run `kivali-supervisor up`"))
	}
	res, err := c.client.Handoff(ctx)
	if err != nil {
		return fail(err)
	}
	fmt.Println(res.Token)
	return 0
}

func printCredential(st supervisor.CredentialStatus) {
	if !st.SignedIn {
		fmt.Println("Claude: not signed in (run `kivali-supervisor terminal`; Claude opens its sign-in menu)")
		return
	}
	line := "Claude: signed in"
	for _, part := range []string{st.Email, st.Billing} {
		if part != "" {
			line += " · " + part
		}
	}
	fmt.Println(line)
}

func (c *cli) destroy(args []string) int {
	fs := flag.NewFlagSet("destroy", flag.ExitOnError)
	yes := fs.Bool("yes", false, "really delete the org: its data disk, local.json and logs")
	exit := fs.Bool("exit", false, "also stop serve")
	_ = fs.Parse(args)
	if !*yes {
		return fail(errors.New("destroy deletes the org and everything in it; pass --yes"))
	}
	res, code := c.op("destroy", supervisor.DestroyRequest{Exit: *exit})
	if code != 0 {
		return code
	}
	var r supervisor.DestroyResult
	_ = json.Unmarshal(res, &r)
	fmt.Printf("deleted (%d MiB freed)\n", r.FreedBytes>>20)
	return 0
}
