package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// LoadImages imports an image archive, r (a docker-archive or OCI tar,
// as `docker save` or `ctr images export` write; the names inside are
// kept), into the guest's containerd, and restarts the org onto the
// new images (restartLocked). The development loop on a machine with a
// Kivali VM does not need it: `make dev-load` builds the images inside
// the team's own VM, straight into that containerd, and calls
// `restart` alone.
func (s *Supervisor) LoadImages(ctx context.Context, r io.Reader, logf Logf) error {
	ctx, end, err := s.begin(ctx, "load-images", true)
	if err != nil {
		return err
	}
	defer end()
	logf = s.tee(ctx, logf)
	g, err := s.running()
	if err != nil {
		return err
	}
	logf("importing the image archive into k3s")
	res, err := g.ImportImages(ctx, r)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("import: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Output))
	}
	logf("imported")
	return s.restartLocked(ctx, g, logf)
}

// Restart puts the org onto the images now in the guest's containerd:
// it deletes the agent pods and restarts the server deployment (a
// mutable tag such as :dev leaves the manifest unchanged, so nothing
// else would pick new images up), then waits for the server.
func (s *Supervisor) Restart(ctx context.Context, logf Logf) error {
	ctx, end, err := s.begin(ctx, "restart", true)
	if err != nil {
		return err
	}
	defer end()
	logf = s.tee(ctx, logf)
	g, err := s.running()
	if err != nil {
		return err
	}
	return s.restartLocked(ctx, g, logf)
}

func (s *Supervisor) restartLocked(ctx context.Context, g Guest, logf Logf) error {
	installed, err := s.installedLocked(ctx)
	if err != nil {
		return err
	}
	if !installed {
		logf("Kivali is not installed yet; nothing to restart")
		return nil
	}
	if _, err := kubectl(ctx, g, "-n", Namespace, "delete", "pod", "-l", "app=kivali-agentpod", "--ignore-not-found", "--wait", "--timeout=120s"); err != nil {
		return err
	}
	logf("agent pods deleted")
	if _, err := kubectl(ctx, g, "-n", Namespace, "rollout", "restart", "deployment/kivali"); err != nil {
		return err
	}
	logf("deployment/kivali restarted")
	return s.waitServing(ctx, "", "", logf)
}

// ServerExec is the argv prefix that runs a command in the server
// container.
func ServerExec(tty bool) []string {
	argv := []string{"k3s", "kubectl", "-n", Namespace, "exec"}
	if tty {
		argv = append(argv, "-it")
	} else {
		argv = append(argv, "-i")
	}
	return append(argv, "deploy/kivali", "-c", "kivali", "--")
}

// Backup streams the org's backup to w: the zip the app's Org page
// downloads, from the same endpoint, signed in as the owner. The
// archive excludes the Claude credential, as every backup does. A
// download the server aborts part way is an error, never a short zip.
func (s *Supervisor) Backup(ctx context.Context, w io.Writer, logf Logf) error {
	logf = s.tee(ctx, logf)
	c, err := s.signIn(ctx)
	if err != nil {
		return err
	}
	resp, err := c.post(ctx, "/api/v1/org/backup", "", nil)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return orgError("backup", resp)
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return fmt.Errorf("backup: the download stopped after %d bytes: %w", n, err)
	}
	logf("backup: %d bytes", n)
	return nil
}

// RestoreRequest is the body of POST /v1/restore.
type RestoreRequest struct {
	// Path is the backup zip: an absolute path inside the config
	// directory or the home directory of the user serve runs as.
	Path string `json:"path"`
}

// RestoreFromFile restores the backup zip at path into the org through
// the app's own restore (setup's "Restore from a backup"), signed in as
// the owner: refused, as there, unless the org is freshly set up. It
// answers once the server has restored the archive and finished the
// way a boot does.
func (s *Supervisor) RestoreFromFile(ctx context.Context, path string, logf Logf) error {
	// Under the op lock, refusing while another operation runs: a down,
	// destroy or upgrade stopping the VM part way through the app's
	// restore would leave a half-restored org (and an upgrade would
	// snapshot it as the rollback point).
	ctx, done, err := s.beginNow(ctx, "restore", false)
	if err != nil {
		return err
	}
	defer done()
	if s.State().Upgrade != nil {
		return errors.New("an interrupted upgrade must be finished first (run `kivali-supervisor up`)")
	}
	logf = s.tee(ctx, logf)
	src, err := s.backupPathAllowed(path)
	if err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	c, err := s.signIn(ctx)
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("archive", filepath.Base(src))
		if err == nil {
			_, err = io.Copy(part, f)
		}
		if err == nil {
			err = mw.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	logf("restoring %s into the org", src)
	resp, err := c.post(ctx, "/api/v1/org/restore", mw.FormDataContentType(), pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return fmt.Errorf("restore: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return orgError("restore", resp)
	}
	logf("restored %s", src)
	return nil
}

// Exec starts argv in the guest (as root, with k3s's tools on PATH) and
// returns the guest agent's exec stream. It is the debugging escape
// hatch of the command line; the shell does not need it.
func (s *Supervisor) Exec(ctx context.Context, argv []string) (net.Conn, error) {
	g, err := s.running()
	if err != nil {
		return nil, err
	}
	// Never the arguments: they may carry secrets.
	s.o.Logf("exec: %s (%d args)", argv[0], len(argv)-1)
	return g.OpenExec(ctx, guestapi.ExecSpec{Argv: argv})
}

// BackupRequest is the body of POST /v1/backup.
type BackupRequest struct {
	// Path is where the archive goes: an absolute path inside the
	// config directory or the home directory of the user serve runs as.
	Path string `json:"path"`
}

// BackupResult is the answer to a backup.
type BackupResult struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// backupPathAllowed resolves p and checks it lies under the config
// directory or the home directory (symlinks resolved), so an RPC client
// cannot make serve write anywhere else.
func (s *Supervisor) backupPathAllowed(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("backup path %q is not absolute", p)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(filepath.Clean(p)))
	if err != nil {
		return "", fmt.Errorf("backup path: %w", err)
	}
	var roots []string
	for _, r := range []string{s.o.ConfigDir, s.o.Home} {
		if r == "" {
			continue
		}
		if rr, err := filepath.EvalSymlinks(r); err == nil {
			roots = append(roots, rr)
		}
	}
	for _, r := range roots {
		if within(r, dir) {
			return filepath.Join(dir, filepath.Base(p)), nil
		}
	}
	return "", fmt.Errorf("backup path %s is outside the config directory and the home directory", p)
}

// within reports whether dir is root or lies under it. Both are clean
// absolute paths. A path on another volume has no relative path; a
// parent reference counts with either separator, whatever the OS.
func within(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil || filepath.IsAbs(rel) || rel == ".." {
		return false
	}
	for _, up := range []string{".." + string(filepath.Separator), "../", `..\`} {
		if strings.HasPrefix(rel, up) {
			return false
		}
	}
	return true
}

// BackupToFile writes a backup to path, atomically: the archive appears
// only once the download completed and wrote something.
func (s *Supervisor) BackupToFile(ctx context.Context, path string, logf Logf) (BackupResult, error) {
	logf = s.tee(ctx, logf)
	dst, err := s.backupPathAllowed(path)
	if err != nil {
		return BackupResult{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".kivali-backup-*")
	if err != nil {
		return BackupResult{}, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	logf("backing up the org to %s", dst)
	if err := s.Backup(ctx, tmp, nil); err != nil {
		_ = tmp.Close()
		return BackupResult{}, err
	}
	fi, err := tmp.Stat()
	if err == nil && fi.Size() == 0 {
		err = errors.New("backup: the download was empty")
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return BackupResult{}, err
	}
	if err := s.o.Host.Rename(tmp.Name(), dst); err != nil {
		return BackupResult{}, err
	}
	logf("backup written: %s (%d bytes)", dst, fi.Size())
	return BackupResult{Path: dst, Bytes: fi.Size()}, nil
}

// OpenURLMarker starts what the BROWSER helper writes to its terminal:
// the page Claude Code asks a browser to open, then BEL. The
// `kivali-supervisor terminal` client removes it from what it shows and
// opens the page in this computer's browser (cmd/kivali-supervisor,
// browser.go). An OSC sequence no terminal acts on.
const OpenURLMarker = "\x1b]1337;KivaliOpenURL="

// browserHelper is the BROWSER Claude Code runs with in the container:
// there is no browser there, so it hands the page to the terminal.
const browserHelper = "/tmp/kivali-open-browser"

// TerminalArgv is what `terminal` attaches in the server container, in a
// writable directory: Claude Code itself, so the person lands straight
// in it (its first run asks them to log in; the login lands in the
// container's HOME on the data volume, where the server's own runs of
// the CLI find it), or bash when shell is set. Claude Code runs with
// BROWSER set to a helper that passes its sign-in page out through the
// terminal (OpenURLMarker), so the person's own browser opens it rather
// than their copying a link Claude wraps across lines.
func TerminalArgv(shell bool) []string {
	if shell {
		return append(ServerExec(true), "sh", "-c", "cd /tmp && exec bash")
	}
	script := "cd /tmp && printf '%s\\n' '#!/bin/sh' " +
		`'printf "\033]1337;KivaliOpenURL=%s\007" "$1" > /dev/tty'` +
		" > " + browserHelper + " && chmod 700 " + browserHelper +
		" && BROWSER=" + browserHelper + " CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 exec claude"
	return append(ServerExec(true), "sh", "-c", script)
}

// Terminal opens a PTY running TerminalArgv in the guest; the returned
// stream speaks the guest agent's frames (stdin and resize in, stdout
// and exit out).
func (s *Supervisor) Terminal(ctx context.Context, rows, cols uint16, shell bool) (net.Conn, error) {
	g, err := s.running()
	if err != nil {
		return nil, err
	}
	argv := TerminalArgv(shell)
	s.o.Logf("terminal: attaching %q", argv)
	return g.OpenPTY(ctx, guestapi.ExecSpec{Argv: argv, Rows: rows, Cols: cols})
}

// terminalAttached counts an open terminal session (status's
// terminals) until the returned func is called.
func (s *Supervisor) terminalAttached() func() {
	s.terminalsChanged(s.terminals.Add(1))
	var once sync.Once
	return func() { once.Do(func() { s.terminalsChanged(s.terminals.Add(-1)) }) }
}

func (s *Supervisor) terminalsChanged(n int32) {
	if s.o.terminalsChanged != nil {
		s.o.terminalsChanged(int(n))
	}
}
