//go:build unix

package guestapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Server is the guest agent. The zero value is not usable; NewServer
// fills the production paths, which tests override.
type Server struct {
	// Version is the agent's own version (ldflags).
	Version string
	// RunDir holds the boot scripts' markers: booted, ready (this
	// boot's id once READY was printed) and fatal (the reason).
	RunDir string
	// DataDir is the data disk's mount point; write-file is confined
	// to it.
	DataDir string
	// ImagesDir and ChartsDir are the root disk's baked tarballs.
	ImagesDir string
	ChartsDir string
	// VMVersionFile names the root disk's version.
	VMVersionFile string
	// BootIDFile is /proc/sys/kernel/random/boot_id.
	BootIDFile string
	// KernelFile is /proc/sys/kernel/osrelease.
	KernelFile string
	// MountsFile is /proc/mounts and Ext4SysDir /sys/fs/ext4, where the
	// data disk's health is read (diskhealth.go).
	MountsFile string
	Ext4SysDir string
	// Env is the base environment of every command.
	Env []string
	// NodeReadyArgv prints "<bootID> <Ready status>" for the node.
	NodeReadyArgv []string
	// K3sVersionArgv prints k3s's version on its first line.
	K3sVersionArgv []string
	// ImportArgv reads an image tar on stdin and imports it.
	ImportArgv []string
	// PoweroffArgv starts the guest's clean shutdown.
	PoweroffArgv []string
	// Logf logs; nil discards.
	Logf func(format string, args ...any)
	// KillGrace is how long a hung-up terminal gets before SIGKILL, and
	// how long a finished exec's stragglers may hold its output pipes.
	KillGrace time.Duration

	// execDone, for tests, is called when an exec stream ends.
	execDone func(Exit)

	bakedOnce   sync.Once
	bakedImages []string
	bakedCharts []BakedChart
	k3sOnce     sync.Once
	k3sVersion  string
}

// K3sBin is where k3s unpacks its bundled binaries (kubectl, iptables)
// inside its data directory.
const K3sBin = DataDir + "/k3s/data/current/bin"

// NewServer returns the agent configured for the Kivali Desktop guest.
func NewServer(version string) *Server {
	return &Server{
		Version:       version,
		RunDir:        "/run/kivali",
		DataDir:       DataDir,
		ImagesDir:     "/usr/share/kivali/images",
		ChartsDir:     "/usr/share/kivali/charts",
		VMVersionFile: "/usr/share/kivali/vm-version",
		BootIDFile:    "/proc/sys/kernel/random/boot_id",
		KernelFile:    "/proc/sys/kernel/osrelease",
		MountsFile:    "/proc/mounts",
		Ext4SysDir:    "/sys/fs/ext4",
		Env: []string{
			"PATH=/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:" + K3sBin + ":" + K3sBin + "/aux",
			"HOME=/root",
			"TERM=xterm-256color",
		},
		NodeReadyArgv: []string{"k3s", "kubectl", "get", "node", "kivali", "-o",
			`jsonpath={.status.nodeInfo.bootID} {range .status.conditions[?(@.type=="Ready")]}{.status}{end}`},
		K3sVersionArgv: []string{"k3s", "--version"},
		ImportArgv:     []string{"k3s", "ctr", "-n", "k8s.io", "images", "import", "-"},
		PoweroffArgv:   []string{"poweroff"},
		KillGrace:      5 * time.Second,
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Handler returns the agent's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathStatus, s.handleStatus)
	mux.HandleFunc("GET "+PathExec, s.handleExec)
	mux.HandleFunc("GET "+PathPTY, s.handlePTY)
	mux.HandleFunc("PUT "+PathFile, s.handleFile)
	mux.HandleFunc("POST "+PathImport, s.handleImport)
	mux.HandleFunc("GET "+PathProxy, s.handleProxy)
	mux.HandleFunc("POST "+PathShutdown, s.handleShutdown)
	return mux
}

// Serve answers requests on l until it fails.
func (s *Server) Serve(l net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 30 * time.Second}
	return srv.Serve(l)
}

func readTrim(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Server) command(ctx context.Context, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append([]string(nil), s.Env...)
	return cmd
}

// lookPath resolves argv[0] against the agent's PATH, not its own.
func (s *Server) resolve(argv []string) []string {
	if strings.Contains(argv[0], "/") {
		return argv
	}
	for _, e := range s.Env {
		if p, ok := strings.CutPrefix(e, "PATH="); ok {
			for _, dir := range filepath.SplitList(p) {
				cand := filepath.Join(dir, argv[0])
				if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
					out := append([]string{cand}, argv[1:]...)
					return out
				}
			}
		}
	}
	return argv
}

// State reads the boot scripts' markers.
func (s *Server) State() (state, fatal string) {
	if f := readTrim(filepath.Join(s.RunDir, "fatal")); f != "" {
		return StateFatal, f
	}
	bootID := readTrim(s.BootIDFile)
	if r := readTrim(filepath.Join(s.RunDir, "ready")); r != "" && r == bootID {
		return StateReady, ""
	}
	return StateBooting, ""
}

func (s *Server) baked() ([]string, []BakedChart) {
	s.bakedOnce.Do(func() {
		ents, _ := os.ReadDir(s.ImagesDir)
		seen := map[string]bool{}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			refs, err := ImageRefsFile(filepath.Join(s.ImagesDir, e.Name()))
			if err != nil {
				s.logf("baked images: %v", err)
				continue
			}
			for _, r := range refs {
				if !seen[r] {
					seen[r] = true
					s.bakedImages = append(s.bakedImages, r)
				}
			}
		}
		charts, _ := filepath.Glob(filepath.Join(s.ChartsDir, "*.tgz"))
		for _, c := range charts {
			m, err := ChartInfoFile(c)
			if err != nil {
				s.logf("baked chart %s: %v", c, err)
				continue
			}
			s.bakedCharts = append(s.bakedCharts, BakedChart{Path: c, Name: m.Name, Version: m.Version, AppVersion: m.AppVersion})
		}
	})
	return s.bakedImages, s.bakedCharts
}

func (s *Server) k3s(ctx context.Context) string {
	s.k3sOnce.Do(func() {
		out, err := s.command(ctx, s.resolve(s.K3sVersionArgv)).Output()
		if err == nil {
			s.k3sVersion, _, _ = strings.Cut(strings.TrimSpace(string(out)), "\n")
		}
	})
	return s.k3sVersion
}

func (s *Server) nodeReady(ctx context.Context, bootID string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := s.command(ctx, s.resolve(s.NodeReadyArgv)).Output()
	if err != nil {
		return false
	}
	f := strings.Fields(string(out))
	return len(f) == 2 && f[0] == bootID && f[1] == "True"
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := Status{BootID: readTrim(s.BootIDFile), Time: time.Now().UTC()}
	st.State, st.Fatal = s.State()
	st.NodeReady = s.nodeReady(r.Context(), st.BootID)
	st.BakedImages, st.BakedCharts = s.baked()
	st.DataDisk = s.dataDiskHealth()
	st.Versions = Versions{
		Agent:   s.Version,
		VMImage: readTrim(s.VMVersionFile),
		Kernel:  readTrim(s.KernelFile),
		K3s:     s.k3s(r.Context()),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// hijack answers 101 and returns the raw connection, with anything the
// server had already buffered.
func hijack(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), UpgradeToken) {
		http.Error(w, "this endpoint needs Upgrade: "+UpgradeToken, http.StatusBadRequest)
		return nil, nil, errors.New("no upgrade")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return nil, nil, errors.New("no hijacker")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + UpgradeToken + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, rw, nil
}

func parseSpec(r *http.Request) (ExecSpec, error) {
	var spec ExecSpec
	if err := json.Unmarshal([]byte(r.URL.Query().Get("spec")), &spec); err != nil {
		return spec, fmt.Errorf("bad spec: %w", err)
	}
	if len(spec.Argv) == 0 {
		return spec, errors.New("bad spec: empty argv")
	}
	return spec, nil
}

func exitOf(err error) Exit {
	if err == nil {
		return Exit{Code: 0}
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return Exit{Code: 128 + int(ws.Signal())}
		}
		return Exit{Code: ee.ExitCode()}
	}
	return Exit{Code: -1, Error: err.Error()}
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	spec, err := parseSpec(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	conn, rw, err := hijack(w, r)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	// The request context ends at hijack; the stream's own end kills
	// the process.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fw := NewFrameWriter(conn)

	cmd := s.command(ctx, s.resolve(spec.Argv))
	cmd.Env = append(cmd.Env, spec.Env...)
	cmd.Dir = spec.Dir
	// Its own process group, killed whole when the stream ends, so a
	// shell's children do not outlive it; WaitDelay bounds a straggler
	// that still holds the output pipes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = s.KillGrace
	cmd.Stdout = fw.Writer(FrameStdout)
	cmd.Stderr = fw.Writer(FrameStderr)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = fw.JSON(FrameExit, Exit{Code: -1, Error: err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		_ = fw.JSON(FrameExit, Exit{Code: -1, Error: err.Error()})
		return
	}
	go func() {
		defer func() { _ = stdin.Close() }()
		for {
			typ, p, err := ReadFrame(rw)
			if err != nil {
				// The host went away: kill the process.
				cancel()
				return
			}
			switch typ {
			case FrameStdin:
				if _, err := stdin.Write(p); err != nil {
					_ = stdin.Close()
				}
			case FrameStdinClose:
				_ = stdin.Close()
			}
		}
	}()
	ex := exitOf(cmd.Wait())
	_ = fw.JSON(FrameExit, ex)
	if s.execDone != nil {
		s.execDone(ex)
	}
}

func (s *Server) handlePTY(w http.ResponseWriter, r *http.Request) {
	spec, err := parseSpec(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	conn, rw, err := hijack(w, r)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	fw := NewFrameWriter(conn)

	cmd := s.command(context.Background(), s.resolve(spec.Argv))
	cmd.Env = append(cmd.Env, spec.Env...)
	cmd.Dir = spec.Dir
	size := &pty.Winsize{Rows: spec.Rows, Cols: spec.Cols}
	if size.Rows == 0 || size.Cols == 0 {
		size = &pty.Winsize{Rows: 24, Cols: 80}
	}
	tty, err := pty.StartWithSize(cmd, size)
	if err != nil {
		_ = fw.JSON(FrameExit, Exit{Code: -1, Error: err.Error()})
		return
	}
	// Never the arguments: they may carry secrets.
	s.logf("pty: started %s (%d args)", spec.Argv[0], len(spec.Argv)-1)
	outDone := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(outDone)
		_, _ = io.Copy(fw.Writer(FrameStdout), tty)
	}()
	go func() {
		for {
			typ, p, err := ReadFrame(rw)
			if err != nil {
				// The host went away: hang up the terminal's session
				// (pty.Start made the child its leader), and kill it
				// if it has not gone after a grace period.
				pid := cmd.Process.Pid
				_ = syscall.Kill(-pid, syscall.SIGHUP)
				_ = tty.Close()
				select {
				case <-exited:
				case <-time.After(s.KillGrace):
					_ = syscall.Kill(-pid, syscall.SIGKILL)
				}
				return
			}
			switch typ {
			case FrameStdin:
				_, _ = tty.Write(p)
			case FrameResize:
				var rs Resize
				if json.Unmarshal(p, &rs) == nil && rs.Rows > 0 && rs.Cols > 0 {
					_ = pty.Setsize(tty, &pty.Winsize{Rows: rs.Rows, Cols: rs.Cols})
				}
			case FrameStdinClose:
				// A terminal has no EOF of its own; send ^D.
				_, _ = tty.Write([]byte{4})
			}
		}
	}()
	werr := cmd.Wait()
	close(exited)
	// The output copy ends when the PTY drains (EIO after the last
	// writer closes). A grandchild that kept the terminal open must not
	// hold the stream forever.
	select {
	case <-outDone:
	case <-time.After(2 * time.Second):
	}
	_ = tty.Close()
	_ = fw.JSON(FrameExit, exitOf(werr))
	s.logf("pty: %s exited", spec.Argv[0])
}

// confine resolves rel inside root, refusing anything that escapes it.
func confine(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be relative to the data directory", rel)
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q escapes the data directory", rel)
	}
	return filepath.Join(root, clean), nil
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	dst, err := confine(s.DataDir, r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mode := os.FileMode(0o644)
	if m := r.URL.Query().Get("mode"); m != "" {
		v, err := strconv.ParseUint(m, 8, 32)
		if err != nil {
			http.Error(w, "bad mode", http.StatusBadRequest)
			return
		}
		mode = os.FileMode(v).Perm()
	}
	if err := writeAtomic(dst, mode, r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logf("write-file: %s", dst)
	w.WriteHeader(http.StatusNoContent)
}

func writeAtomic(dst string, mode os.FileMode, r io.Reader) (err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".tmp-"+filepath.Base(dst)+"-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = io.Copy(f, r); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), dst); err != nil {
		return err
	}
	// The rename is a directory entry: until the directory is synced, a
	// power cut can still undo it, leaving the old file (or none) after
	// the host was told the write succeeded.
	return syncDir(filepath.Dir(dst))
}

// syncDir fsyncs the directory dir, making the entries renamed or
// created in it durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	cmd := s.command(r.Context(), s.resolve(s.ImportArgv))
	cmd.Stdin = r.Body
	out, err := cmd.CombinedOutput()
	ex := exitOf(err)
	res := ImportResult{ExitCode: ex.Code, Output: string(out)}
	if ex.Error != "" {
		res.Output += ex.Error
	}
	s.logf("images import: exit %d", res.ExitCode)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.URL.Query().Get("port"))
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "bad port", http.StatusBadRequest)
		return
	}
	// Loopback only: the forward reaches the node's own NodePort, the
	// way traffic from the node itself does.
	up, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 10*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	conn, rw, err := hijack(w, r)
	if err != nil {
		_ = up.Close()
		return
	}
	Splice(conn, rw.Reader, up)
}

func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusAccepted)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	s.logf("shutdown requested by the host")
	cmd := s.command(context.Background(), s.resolve(s.PoweroffArgv))
	if err := cmd.Start(); err != nil {
		log.Printf("poweroff: %v", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}
