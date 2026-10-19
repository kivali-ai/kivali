package main

import (
	"encoding/binary"
	"io"
	"strings"
)

const (
	tlsRecordHandshake  = 0x16
	tlsClientHello      = 0x01
	tlsExtServerName    = 0x0000
	tlsExtECH           = 0xfe0d
	tlsSNIHostName      = 0x00
	tlsRecordHeaderLen  = 5
	tlsMaxRecordPayload = 16384 + 2048 // TLSCiphertext's bound, the most a peer may send
)

// readFirstRecord reads the client's first TLS record from r: the
// header and the payload it announces. It stops after the first byte
// when that byte does not open a handshake record, and never reads past
// the record or past tlsMaxRecordPayload. It returns whatever it read,
// so the caller can replay it upstream whether or not the read
// completed.
func readFirstRecord(r io.Reader) ([]byte, error) {
	buf := make([]byte, 1, tlsRecordHeaderLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	if buf[0] != tlsRecordHandshake {
		return buf, nil
	}
	buf = buf[:tlsRecordHeaderLen]
	if n, err := io.ReadFull(r, buf[1:]); err != nil {
		return buf[:1+n], err
	}
	length := int(binary.BigEndian.Uint16(buf[3:5]))
	if length > tlsMaxRecordPayload {
		return buf, nil
	}
	rec := make([]byte, tlsRecordHeaderLen+length)
	copy(rec, buf)
	n, err := io.ReadFull(r, rec[tlsRecordHeaderLen:])
	return rec[:tlsRecordHeaderLen+n], err
}

// clientHelloSNI returns the server name a TLS record's ClientHello
// asks for. ok is false unless rec is one complete handshake record
// holding one complete ClientHello with exactly one host_name and no
// Encrypted Client Hello extension: with ECH the name in the clear is
// not the one the server acts on.
func clientHelloSNI(rec []byte) (name string, ok bool) {
	r := bytesReader(rec)
	typ, ok1 := r.u8()
	_, ok2 := r.u16() // legacy record version
	payload, ok3 := r.vec16()
	if !ok1 || !ok2 || !ok3 || typ != tlsRecordHandshake || !r.empty() {
		return "", false
	}
	hsType, ok1 := payload.u8()
	hello, ok2 := payload.vec24()
	// A ClientHello split across records, or followed by another
	// message in the same record, is not read.
	if !ok1 || !ok2 || hsType != tlsClientHello || !payload.empty() {
		return "", false
	}
	if !hello.skip(2 + 32) { // legacy_version, random
		return "", false
	}
	if _, ok := hello.vec8(); !ok { // legacy_session_id
		return "", false
	}
	if _, ok := hello.vec16(); !ok { // cipher_suites
		return "", false
	}
	if _, ok := hello.vec8(); !ok { // legacy_compression_methods
		return "", false
	}
	exts, ok := hello.vec16()
	if !ok || !hello.empty() {
		return "", false
	}
	found := false
	for !exts.empty() {
		extType, ok1 := exts.u16()
		data, ok2 := exts.vec16()
		if !ok1 || !ok2 {
			return "", false
		}
		switch extType {
		case tlsExtECH:
			return "", false
		case tlsExtServerName:
			if found {
				return "", false
			}
			n, ok := serverName(data)
			if !ok {
				return "", false
			}
			name, found = n, true
		}
	}
	return name, found
}

// serverName reads a server_name extension body holding exactly one
// host_name entry.
func serverName(data bytesReader) (string, bool) {
	list, ok := data.vec16()
	if !ok || !data.empty() {
		return "", false
	}
	nameType, ok1 := list.u8()
	host, ok2 := list.vec16()
	if !ok1 || !ok2 || nameType != tlsSNIHostName || !list.empty() || host.empty() {
		return "", false
	}
	return string(host), true
}

// sniMatches reports whether the ClientHello in rec names host, the
// CONNECT target's host, exactly (ignoring ASCII case and a trailing
// dot). A server name is ASCII (A-labels), so either name holding any
// other byte never matches: Unicode case folding would map characters
// such as U+212A KELVIN SIGN onto ASCII letters.
func sniMatches(rec []byte, host string) bool {
	name, ok := clientHelloSNI(rec)
	if !ok {
		return false
	}
	n, ok1 := asciiHostname(name)
	h, ok2 := asciiHostname(host)
	return ok1 && ok2 && n != "" && n == h
}

// asciiHostname lowercases ASCII letters in s and drops one trailing
// dot. ok is false when s holds a byte outside ASCII.
func asciiHostname(s string) (string, bool) {
	b := []byte(strings.TrimSuffix(s, "."))
	for i, c := range b {
		switch {
		case c >= 0x80:
			return "", false
		case 'A' <= c && c <= 'Z':
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b), true
}

// bytesReader reads big-endian TLS wire fields, reporting false instead
// of reading past its end.
type bytesReader []byte

func (b *bytesReader) empty() bool { return len(*b) == 0 }

func (b *bytesReader) skip(n int) bool {
	if len(*b) < n {
		return false
	}
	*b = (*b)[n:]
	return true
}

func (b *bytesReader) take(n int) (bytesReader, bool) {
	if n < 0 || len(*b) < n {
		return nil, false
	}
	out := (*b)[:n]
	*b = (*b)[n:]
	return out, true
}

func (b *bytesReader) u8() (int, bool) {
	v, ok := b.take(1)
	if !ok {
		return 0, false
	}
	return int(v[0]), true
}

func (b *bytesReader) u16() (int, bool) {
	v, ok := b.take(2)
	if !ok {
		return 0, false
	}
	return int(binary.BigEndian.Uint16(v)), true
}

func (b *bytesReader) u24() (int, bool) {
	v, ok := b.take(3)
	if !ok {
		return 0, false
	}
	return int(v[0])<<16 | int(v[1])<<8 | int(v[2]), true
}

func (b *bytesReader) vec8() (bytesReader, bool) {
	n, ok := b.u8()
	if !ok {
		return nil, false
	}
	return b.take(n)
}

func (b *bytesReader) vec16() (bytesReader, bool) {
	n, ok := b.u16()
	if !ok {
		return nil, false
	}
	return b.take(n)
}

func (b *bytesReader) vec24() (bytesReader, bool) {
	n, ok := b.u24()
	if !ok {
		return nil, false
	}
	return b.take(n)
}
