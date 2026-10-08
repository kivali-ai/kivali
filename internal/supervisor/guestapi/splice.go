package guestapi

import (
	"io"
	"net"
)

// Splice copies between a and b until both directions end, then closes
// both. ar, if not nil, reads a's side (a buffered reader over a). When
// one direction ends, the other side's write half is closed if it can
// be, else the whole connection, which ends the other direction too.
func Splice(a net.Conn, ar io.Reader, b net.Conn) {
	if ar == nil {
		ar = a
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(b, ar); closeWrite(b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(a, b); closeWrite(a); done <- struct{}{} }()
	<-done
	<-done
	_ = a.Close()
	_ = b.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		if cw.CloseWrite() == nil {
			return
		}
	}
	_ = c.Close()
}
