package main

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"io"
	"math/rand"
	"net"
	"strings"
	"testing"
)

// realClientHello is the first record crypto/tls sends for serverName
// (no SNI when serverName is empty).
func realClientHello(t testing.TB, serverName string) []byte {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		c := tls.Client(a, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}) //nolint:gosec // only the ClientHello is used
		_ = c.Handshake()
		_ = a.Close()
	}()
	rec, err := readFirstRecord(b)
	_ = b.Close()
	if err != nil {
		t.Fatalf("read ClientHello: %v", err)
	}
	return rec
}

func u16(n int) []byte { return binary.BigEndian.AppendUint16(nil, uint16(n)) }

func ext(typ int, data []byte) []byte {
	return append(append(u16(typ), u16(len(data))...), data...)
}

// sniExt is a server_name extension holding the given (type, name)
// entries.
func sniExt(entries ...any) []byte {
	var list []byte
	for i := 0; i < len(entries); i += 2 {
		list = append(list, byte(entries[i].(int)))
		name := entries[i+1].(string)
		list = append(append(list, u16(len(name))...), name...)
	}
	return ext(tlsExtServerName, append(u16(len(list)), list...))
}

// buildHello wraps extensions in a minimal ClientHello record.
func buildHello(exts ...[]byte) []byte {
	var body []byte
	body = append(body, 0x03, 0x03)
	body = append(body, make([]byte, 32)...)
	body = append(body, 0)                // session id
	body = append(body, 0, 2, 0x13, 0x01) // cipher suites
	body = append(body, 1, 0)             // compression
	all := bytes.Join(exts, nil)
	body = append(append(body, u16(len(all))...), all...)
	hs := append([]byte{tlsClientHello, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
	return append(append([]byte{tlsRecordHandshake, 0x03, 0x01}, u16(len(hs))...), hs...)
}

func TestClientHelloSNIFromCryptoTLS(t *testing.T) {
	rec := realClientHello(t, "api.anthropic.com")
	if name, ok := clientHelloSNI(rec); !ok || name != "api.anthropic.com" {
		t.Fatalf("SNI = %q, %v", name, ok)
	}
	if name, ok := clientHelloSNI(realClientHello(t, "")); ok {
		t.Fatalf("no SNI sent, parsed %q", name)
	}
	// Every truncation is a parse failure, never a panic.
	for i := 0; i < len(rec); i++ {
		if name, ok := clientHelloSNI(rec[:i]); ok {
			t.Fatalf("truncated to %d bytes, parsed %q", i, name)
		}
	}
	// Trailing bytes past the record are refused too.
	if _, ok := clientHelloSNI(append(append([]byte(nil), rec...), 0)); ok {
		t.Fatal("trailing byte accepted")
	}
}

func TestClientHelloSNIOddRecords(t *testing.T) {
	good := buildHello(sniExt(0, "api.anthropic.com"))
	if name, ok := clientHelloSNI(good); !ok || name != "api.anthropic.com" {
		t.Fatalf("built hello: %q, %v", name, ok)
	}
	withType := func(rec []byte, i int, v byte) []byte {
		out := append([]byte(nil), rec...)
		out[i] = v
		return out
	}
	cases := map[string][]byte{
		"empty":                {},
		"not a handshake":      withType(good, 0, 0x17),
		"not a ClientHello":    withType(good, 5, 0x02),
		"no extensions":        buildHello(),
		"two server_name exts": buildHello(sniExt(0, "api.anthropic.com"), sniExt(0, "api.anthropic.com")),
		"two names":            buildHello(sniExt(0, "api.anthropic.com", 0, "evil.example")),
		"non-host name type":   buildHello(sniExt(1, "api.anthropic.com")),
		"empty name":           buildHello(sniExt(0, "")),
		"ECH":                  buildHello(sniExt(0, "api.anthropic.com"), ext(tlsExtECH, []byte{0, 1, 2})),
		"ECH first":            buildHello(ext(tlsExtECH, nil), sniExt(0, "api.anthropic.com")),
		"record overstates":    append(append([]byte{0x16, 3, 1}, u16(len(good))...), good[5:]...),
		"handshake overstates": withType(good, 6, 0xff),
		"extension overstates": buildHello(append(u16(tlsExtServerName), 0xff, 0xff)),
	}
	// A record carrying the ClientHello and then a second message.
	two := append([]byte(nil), good[5:]...)
	two = append(two, good[5:]...)
	cases["two handshakes"] = append(append([]byte{0x16, 3, 1}, u16(len(two))...), two...)
	for name, rec := range cases {
		if got, ok := clientHelloSNI(rec); ok {
			t.Errorf("%s: parsed %q", name, got)
		}
	}
}

// Random corruption of real records never panics.
func TestClientHelloSNIMutations(t *testing.T) {
	seeds := [][]byte{realClientHello(t, "api.anthropic.com"), buildHello(sniExt(0, "a.test"))}
	rng := rand.New(rand.NewSource(1))
	for _, seed := range seeds {
		for i := 0; i < 5000; i++ {
			rec := append([]byte(nil), seed...)
			for j := 0; j < 1+rng.Intn(4); j++ {
				rec[rng.Intn(len(rec))] = byte(rng.Intn(256))
			}
			if rng.Intn(4) == 0 {
				rec = rec[:rng.Intn(len(rec))]
			}
			_, _ = clientHelloSNI(rec)
		}
	}
}

func FuzzClientHelloSNI(f *testing.F) {
	f.Add(realClientHello(f, "api.anthropic.com"))
	f.Add(buildHello(sniExt(0, "a.test")))
	f.Add([]byte{0x16, 3, 1, 0, 0})
	f.Fuzz(func(t *testing.T, rec []byte) {
		name, ok := clientHelloSNI(rec)
		if !ok && name != "" {
			t.Fatalf("name %q without ok", name)
		}
	})
}

func TestSNIMatches(t *testing.T) {
	cases := []struct {
		sni, host string
		want      bool
	}{
		{"api.anthropic.com", "api.anthropic.com", true},
		{"API.Anthropic.COM", "api.anthropic.com", true},
		{"api.anthropic.com.", "api.anthropic.com", true},
		{"api.anthropic.com", "API.anthropic.com.", true},
		{"attacker.example", "api.anthropic.com", false},
		{"x.api.anthropic.com", "api.anthropic.com", false},
		{"anthropic.com", "api.anthropic.com", false},
		{".", "", false},
		{"K.TEST", "k.test", true},
		{"K.test", "k.test", false},                       // KELVIN SIGN folds to k in Unicode
		{"k.test", "K.test", false},                       // on either side
		{"api.anthropic.com", "api.anthropıc.com", false}, // dotless i
	}
	for _, c := range cases {
		if got := sniMatches(buildHello(sniExt(0, c.sni)), c.host); got != c.want {
			t.Errorf("sniMatches(%q, %q) = %v", c.sni, c.host, got)
		}
	}
}

func TestReadFirstRecord(t *testing.T) {
	// Not TLS: one byte read, the rest left for the tunnel.
	r := strings.NewReader("GET / HTTP/1.1\r\n")
	got, err := readFirstRecord(r)
	if err != nil || string(got) != "G" {
		t.Fatalf("non-TLS read %q, %v", got, err)
	}
	if rest, _ := io.ReadAll(r); string(rest) != "ET / HTTP/1.1\r\n" {
		t.Fatalf("left %q", rest)
	}

	// One record and no more.
	rec := buildHello(sniExt(0, "a.test"))
	r = strings.NewReader(string(rec) + "next")
	got, err = readFirstRecord(r)
	if err != nil || !bytes.Equal(got, rec) {
		t.Fatalf("record read %d bytes, %v", len(got), err)
	}
	if rest, _ := io.ReadAll(r); string(rest) != "next" {
		t.Fatalf("left %q", rest)
	}

	// A length past the protocol's bound stops after the header.
	big := []byte{0x16, 3, 1, 0xff, 0xff, 1, 2, 3}
	r = strings.NewReader(string(big))
	got, err = readFirstRecord(r)
	if err != nil || len(got) != tlsRecordHeaderLen {
		t.Fatalf("oversized: %d bytes, %v", len(got), err)
	}

	// Cut short: what arrived is returned with the error, for replay.
	got, err = readFirstRecord(strings.NewReader(string(rec[:20])))
	if err == nil || !bytes.Equal(got, rec[:20]) {
		t.Fatalf("short: %d bytes, %v", len(got), err)
	}
	got, err = readFirstRecord(strings.NewReader("\x16\x03"))
	if err == nil || string(got) != "\x16\x03" {
		t.Fatalf("short header: %q, %v", got, err)
	}
}
