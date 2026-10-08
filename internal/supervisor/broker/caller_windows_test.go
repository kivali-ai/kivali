//go:build windows

package broker

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// callerPipe is a pipe with this account at both ends: the server side
// (servePipe's one instance) connected, the client side dialled as Dial
// does (at impersonation level, with clientAccess), and one byte sent
// and read, since a server can impersonate a client only once it has
// read from it. The server end is what winCaller impersonates through.
func callerPipe(t *testing.T) (server *os.File, client net.Conn) {
	t.Helper()
	name := testPipeName("caller")
	served := servePipe(t, name, pipeDACL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := dialPipe(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server = connected(t, served)
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	return server, client
}

// The impersonating caller sees the client's SID and opens the client's
// own files: the checks the broker makes before it touches anything
// (WritableDir, Absent, File), here with this account at both ends of
// the pipe.
func TestCallerOpensAsTheClient(t *testing.T) {
	server, _ := callerPipe(t)
	pipe := windows.Handle(server.Fd())
	c := &winCaller{pipe: pipe}
	err := onCallerThread(pipe, func() error {
		u, err := windows.GetCurrentThreadToken().GetTokenUser()
		if err != nil {
			return err
		}
		c.sid = u.User.Sid.String()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.sid != ourSID(t) {
		t.Fatalf("the caller's SID is %s, this account is %s", c.sid, ourSID(t))
	}

	// The temp directory as its final path: a hosted runner's %TEMP% is
	// an 8.3 name (C:\Users\RUNNER~1\...), which the caller's opens would
	// refuse as not canonical.
	dir, err := winFiles{}.Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	canon, err := c.WritableDir(dir)
	if err != nil {
		t.Fatalf("WritableDir(%s): %v", dir, err)
	}
	if canon != filepath.Clean(dir) {
		t.Errorf("WritableDir(%s) = %s", dir, canon)
	}
	f := filepath.Join(dir, "data.vhdx")
	if err := c.Absent(f); err != nil {
		t.Errorf("Absent(%s) = %v", f, err)
	}
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Absent(f); !errors.Is(err, ErrExists) {
		t.Errorf("Absent(%s) after creation = %v, want ErrExists", f, err)
	}
	// A name-only open pins nothing (an attributes-only handle takes no
	// part in sharing); a read or write check pins the file against a
	// rename while it is held, and still lets a writer that shares
	// reading, as Hyper-V opens a disk it attaches or starts, open it.
	if got, err := c.File(f, AccessName); err != nil || got != f {
		t.Errorf("File(%s, name) = %q, %v", f, got, err)
	}
	if err := os.Rename(f, f+".moved"); err != nil {
		t.Errorf("a name-only check held the file: %v", err)
	}
	if err := os.Rename(f+".moved", f); err != nil {
		t.Fatal(err)
	}
	for _, a := range []Access{AccessRead, AccessWrite} {
		if got, err := c.File(f, a); err != nil || got != f {
			t.Errorf("File(%s, %d) = %q, %v", f, a, got, err)
		}
		if err := os.Rename(f, f+".moved"); err == nil {
			t.Errorf("File(%s, %d) did not pin the file: it could be renamed", f, a)
			_ = os.Rename(f+".moved", f)
		}
		name, _ := windows.UTF16PtrFromString(f)
		h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ, nil,
			windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			t.Errorf("File(%s, %d): a writer sharing reads cannot open the file: %v", f, a, err)
		} else {
			_ = windows.CloseHandle(h)
		}
	}
	if _, err := c.WritableDir(filepath.Join(dir, "missing")); !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Errorf("WritableDir(missing) = %v, want ERROR_FILE_NOT_FOUND", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
