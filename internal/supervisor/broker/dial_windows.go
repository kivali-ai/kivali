//go:build windows

package broker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync/atomic"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// Dial connects to the broker's pipe (PipeName, or the dev run's
// override) for one request, and checks it is the broker's before
// anything is written.
//
// The connection is opened at impersonation level, because the broker
// checks every path as the caller by impersonating the pipe's client,
// and an anonymous-level client (go-winio's DialPipeContext) gives it a
// token that can open nothing. A client at that level must also never
// talk to a server it has not identified: any local account can create
// \\.\pipe\kivali-broker before the service does, and a server that
// reads what the client writes can then impersonate it. Impersonation
// needs the server to have read from the pipe first, so the check comes
// before the first write.
//
// The check is the pipe's owner, read through this connection's own
// handle: it must be the broker service's own account, LocalSystem or the Administrators group
// (brokerOwned), which the broker names when it creates the pipe
// (Listen) and an ordinary account cannot. No process is opened, so an
// ordinary account can make the check against the service,
// and there is no pid to be reused. It holds because no one but the
// broker can add an instance to the broker's pipe (Listen's DACL grants
// Authenticated Users no FILE_CREATE_PIPE_INSTANCE): every instance of
// a pipe shares the security descriptor of its first, so a squatter's
// instance of the real pipe would otherwise show the broker's owner. An
// owner that cannot be read is a refusal too. A refusal closes the
// connection unused and is ErrNotBroker, logged once per process.
func Dial(ctx context.Context, pipe string) (net.Conn, error) {
	c, err := dialPipe(ctx, pipe)
	if err != nil {
		return nil, err
	}
	if err := verifyBroker(c); err != nil {
		_ = c.Close()
		err = fmt.Errorf("%s: %w", pipe, err)
		if !refusalLogged.Swap(true) {
			dialLogf("kivali-supervisor: %v", err)
		}
		return nil, err
	}
	return c, nil
}

// clientAccess is what Dial opens the pipe for: reading (which includes
// READ_CONTROL, for the owner check) and writing data, and not
// GENERIC_WRITE, which would also ask for FILE_CREATE_PIPE_INSTANCE, the
// right Listen's DACL withholds from Authenticated Users.
const clientAccess = windows.FILE_GENERIC_READ | windows.FILE_WRITE_DATA

// dialPipe opens the pipe as Dial's client, at impersonation level,
// without checking it.
func dialPipe(ctx context.Context, pipe string) (net.Conn, error) {
	return winio.DialPipeAccessImpLevel(ctx, pipe, clientAccess, winio.PipeImpLevelImpersonation)
}

// refusalLogged makes Dial log a refused server once: every request
// dials anew (and the backend polls), so a squatter would otherwise
// fill the log with one line per request. dialLogf is where it goes
// (the tests capture it).
var (
	refusalLogged atomic.Bool
	dialLogf      = log.Printf
)

// verifyBroker applies brokerOwned to the owner of the pipe c is
// connected to. It only queries the client's own handle: nothing is
// written to or read from the pipe.
func verifyBroker(c net.Conn) error {
	owner, err := pipeOwner(c)
	if err != nil {
		return fmt.Errorf("%w (its owner cannot be read: %v)", ErrNotBroker, err)
	}
	if !brokerOwned(owner) {
		return fmt.Errorf("%w (the pipe is owned by %s)", ErrNotBroker, owner)
	}
	return nil
}

// pipeOwner is the owner SID of the pipe a client connection is open
// on, from the connection's handle (READ_CONTROL, which clientAccess
// asks for).
func pipeOwner(c net.Conn) (string, error) {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return "", errors.New("the pipe connection has no handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return "", err
	}
	if owner == nil {
		return "", errors.New("the pipe has no owner")
	}
	return owner.String(), nil
}
