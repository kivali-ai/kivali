package guestapi

import (
	"net"
)

// HostCID is the vsock context id of the host (VMADDR_CID_HOST).
const HostCID = 2

// FilterListener accepts only connections Allow admits; every other one
// is logged and closed before a byte is read. The guest agent uses it
// to accept only the host's vsock CID, so neither a guest process
// (through vsock loopback, CID 1) nor anything else can reach it.
type FilterListener struct {
	net.Listener
	Allow func(remote net.Addr) bool
	Logf  func(format string, args ...any)
}

// Accept returns the next admitted connection.
func (l *FilterListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.Allow(c.RemoteAddr()) {
			return c, nil
		}
		if l.Logf != nil {
			l.Logf("refused a connection from %v: not the host", c.RemoteAddr())
		}
		_ = c.Close()
	}
}
