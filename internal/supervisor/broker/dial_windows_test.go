//go:build windows

package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// pipeSerial keeps two test pipes made within the clock's resolution
// from sharing a name (a second instance of a pipe with the broker's
// DACL is refused to this account).
var pipeSerial atomic.Int64

func testPipeName(kind string) string {
	return fmt.Sprintf(`\\.\pipe\kivali-broker-test-%s-%d-%d-%d`, kind, os.Getpid(), time.Now().UnixNano(), pipeSerial.Add(1))
}

// skipIfElevated skips a test whose point is what an ordinary account
// cannot do: an elevated test process can name the Administrators group
// as owner and add instances to the broker's pipe.
func skipIfElevated(t *testing.T) {
	t.Helper()
	if windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("the test process is elevated")
	}
}

// servePipe creates a pipe's one instance with the security descriptor
// sddl, as this process (so owned by this account), and sends its server
// end on the channel once a client connects. It is a single first
// instance, as the desktop app's tests make theirs, rather than
// go-winio's listener, which adds an instance for each client: that
// needs FILE_CREATE_PIPE_INSTANCE, which pipeDACL gives only SYSTEM and
// Administrators. Buffered, so a write by a client completes into the
// pipe (and can be read back after it has gone) rather than waiting for
// a reader.
func servePipe(t *testing.T, name, sddl string) <-chan *os.File {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateNamedPipe(n, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT, 1, 4096, 4096, 0, sa)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	f := os.NewFile(uintptr(h), name)
	connected := make(chan *os.File, 1)
	go func() {
		// A client may connect before this call (ERROR_PIPE_CONNECTED), or
		// connect and close before it (ERROR_NO_DATA): either way it came,
		// and what it wrote is still there to read, ahead of the end.
		err := windows.ConnectNamedPipe(h, nil)
		if err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) && !errors.Is(err, windows.ERROR_NO_DATA) {
			close(connected)
			return
		}
		connected <- f
	}()
	return connected
}

// connected waits for servePipe's client.
func connected(t *testing.T, c <-chan *os.File) *os.File {
	t.Helper()
	select {
	case f, ok := <-c:
		if !ok {
			t.Fatal("the server's connect failed")
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("the server saw no connection")
	}
	return nil
}

// A pipe served by this test process, with the broker's own DACL, is
// owned by this ordinary account, which is what a squatter's pipe looks
// like: Dial refuses it, and refuses it before writing a byte, so the
// squatter could never have impersonated the client. (Dial reaching the
// owner check at all shows its access fits the broker's DACL.) The
// server reads nothing until Dial has returned and then finds the pipe
// empty and closed. The refusal is logged once however often it
// happens. Accepting the real broker (a pipe owned by Administrators)
// needs the service or an elevated terminal, and is the spike's.
func TestDialRefusesAPipeThisAccountOwns(t *testing.T) {
	skipIfElevated(t)
	var (
		mu     sync.Mutex
		logged []string
	)
	oldLogf := dialLogf
	dialLogf = func(format string, args ...any) {
		mu.Lock()
		logged = append(logged, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	refusalLogged.Store(false)
	t.Cleanup(func() {
		dialLogf = oldLogf
		refusalLogged.Store(false)
	})

	// Two pipes, since this account cannot add a second instance to one.
	for i := range 2 {
		name := testPipeName("owned")
		server := servePipe(t, name, pipeDACL)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := Dial(ctx, name)
		cancel()
		if err == nil {
			_ = c.Close()
			t.Fatalf("dial %d: a pipe owned by an ordinary account was accepted", i+1)
		}
		if !errors.Is(err, ErrNotBroker) || !strings.Contains(err.Error(), "the broker pipe is not served by the Kivali broker") ||
			!strings.Contains(err.Error(), "owned by "+ourSID(t)) {
			t.Fatalf("dial %d: err = %v", i+1, err)
		}
		t.Logf("dial %d: %v", i+1, err)
		srv := connected(t, server)
		// Only now, with Dial's answer given and its end closed, does the
		// server read: anything the client had written would still be in
		// the pipe ahead of the end.
		n, rerr := srv.Read(make([]byte, 64))
		if n != 0 || !errors.Is(rerr, io.EOF) {
			t.Fatalf("dial %d: the server received %d bytes (read error %v); want none and the end", i+1, n, rerr)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(logged) != 1 || !strings.Contains(logged[0], ErrNotBroker.Error()) {
		t.Fatalf("logged %q; want the refusal once", logged)
	}
}

// Dial's client access is enough to carry a request and its answer
// over a pipe with the broker's DACL, while a client asking for
// GENERIC_WRITE, which includes adding an instance, is refused there.
func TestDialAccessFitsTheBrokersDACL(t *testing.T) {
	skipIfElevated(t)
	name := testPipeName("io")
	server := servePipe(t, name, pipeDACL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if c, err := winio.DialPipeAccessImpLevel(ctx, name, windows.GENERIC_READ|windows.GENERIC_WRITE, winio.PipeImpLevelImpersonation); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		if err == nil {
			_ = c.Close()
		}
		t.Fatalf("GENERIC_WRITE on the broker's DACL: err = %v, want access denied", err)
	}

	c, err := dialPipe(ctx, name)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	srv := connected(t, server)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(c, "ping"); err != nil {
		t.Fatalf("write: %v", err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(srv, b); err != nil || string(b) != "ping" {
		t.Fatalf("server read %q, %v", b, err)
	}
	if _, err := io.WriteString(srv, "pong"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "pong" {
		t.Fatalf("client read %q, %v", b, err)
	}
}

// The broker's DACL lets an ordinary account connect but not add an
// instance to the pipe, so it cannot serve clients under the broker's
// owner; with GENERIC_WRITE granted to Authenticated Users (the DACL
// before this rule) it could.
func TestBrokerPipeTakesNoInstanceFromAnOrdinaryAccount(t *testing.T) {
	skipIfElevated(t)
	addInstance := func(name string) error {
		n, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return err
		}
		// No FILE_FLAG_FIRST_PIPE_INSTANCE: another instance of the pipe.
		h, err := windows.CreateNamedPipe(n, windows.PIPE_ACCESS_DUPLEX, windows.PIPE_TYPE_BYTE, windows.PIPE_UNLIMITED_INSTANCES, 4096, 4096, 0, nil)
		if err != nil {
			return err
		}
		return windows.CloseHandle(h)
	}
	for _, tc := range []struct {
		dacl  string
		added bool
	}{
		{pipeDACL, false},
		{"D:P(A;;GA;;;AU)(A;;GA;;;SY)(A;;GA;;;BA)", true},
	} {
		name := testPipeName("instance")
		ln, err := winio.ListenPipe(name, &winio.PipeConfig{SecurityDescriptor: tc.dacl})
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		err = addInstance(name)
		_ = ln.Close()
		if added := err == nil; added != tc.added {
			t.Errorf("DACL %s: an ordinary account added an instance: %v (err %v)", tc.dacl, added, err)
		}
		if !tc.added && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Errorf("DACL %s: err = %v, want access denied", tc.dacl, err)
		}
	}
}

// Listen names the Administrators group as the pipe's owner, which an
// ordinary account may not: its broker cannot even start, and nor can
// a squatter's pipe show that owner.
func TestListenNeedsAnOwnerAnOrdinaryAccountCannotName(t *testing.T) {
	skipIfElevated(t)
	ln, err := Listen(testPipeName("listen"))
	if err == nil {
		_ = ln.Close()
		t.Fatal("an ordinary account created a pipe owned by the Administrators group")
	}
	if !errors.Is(err, windows.ERROR_INVALID_OWNER) {
		t.Fatalf("err = %v, want the invalid owner refusal", err)
	}
}

// A pipe nobody serves is the ordinary dial error, not a refusal.
func TestDialNoBroker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Dial(ctx, fmt.Sprintf(`\\.\pipe\kivali-broker-test-absent-%d`, os.Getpid()))
	if err == nil || errors.Is(err, ErrNotBroker) {
		t.Fatalf("err = %v", err)
	}
}
