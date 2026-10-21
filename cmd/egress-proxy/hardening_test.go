package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A mapped prefix covers the IPv4 addresses it maps, since the guard
// judges a mapped address as IPv4.
func TestDenyCIDRMappedPrefix(t *testing.T) {
	var deny cidrFlag
	if err := deny.Set("::ffff:10.0.0.0/104,::ffff:192.0.2.1/128"); err != nil {
		t.Fatal(err)
	}
	if got, want := deny.String(), "10.0.0.0/8,192.0.2.1/32"; got != want {
		t.Fatalf("deny = %q, want %q", got, want)
	}
	g := &guard{deny: deny}
	for addr, refused := range map[string]bool{
		"10.1.2.3":          true,
		"::ffff:10.1.2.3":   true,
		"192.0.2.1":         true,
		"11.0.0.1":          false,
		"::ffff:192.0.2.2":  false,
		"2001:4860::8888":   false,
		"::ffff:11.0.0.1":   false,
		"::ffff:10.255.0.0": true,
	} {
		if got := g.refusal(netip.MustParseAddr(addr), false) != ""; got != refused {
			t.Errorf("allowlist mode %s refused=%v, want %v", addr, got, refused)
		}
	}
}

// Many misses at once after core registers an agent read the file once
// between them, and every one of them sees the new agent.
func TestAgentsBurstOfMissesReadsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	writeAgents(t, path, map[string]string{"alice": "a"})
	a := &Agents{Path: path}
	if err := a.Load(path); err != nil {
		t.Fatal(err)
	}
	writeAgents(t, path, map[string]string{"alice": "a", "bob": "b"})
	before := a.loads.Load()

	var wg sync.WaitGroup
	failed := make(chan struct{}, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !verified(a, "bob", "b") {
				failed <- struct{}{}
			}
		}()
	}
	wg.Wait()
	if len(failed) != 0 {
		t.Fatalf("%d calls refused the new agent", len(failed))
	}
	if got := a.loads.Load() - before; got != 1 {
		t.Fatalf("burst read the file %d times, want 1", got)
	}
}

// A broken file is read once; misses do not read it again until it
// changes, and the last good entries stay in force meanwhile.
func TestAgentsBrokenFileReadOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	writeAgents(t, path, map[string]string{"alice": "a"})
	a := &Agents{Path: path}
	if err := a.Load(path); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	before := a.loads.Load()
	for i := 0; i < 100; i++ {
		if verified(a, "mallory", "x") {
			t.Fatal("bad credentials accepted")
		}
	}
	if got := a.loads.Load() - before; got != 1 {
		t.Fatalf("broken file read %d times, want 1", got)
	}
	if !verified(a, "alice", "a") {
		t.Fatal("last good entries dropped")
	}
	writeAgents(t, path, map[string]string{"alice": "a", "bob": "b"})
	if !verified(a, "bob", "b") {
		t.Fatal("fixed file not read")
	}
	if got := a.loads.Load() - before; got != 2 {
		t.Fatalf("reads = %d, want 2", got)
	}
}

// A plain HTTP request runs under the request deadline; a tunnel does
// not. A deadline of one nanosecond has passed before the request
// starts, so the outcome does not depend on timing.
func TestPlainHTTPRequestDeadline(t *testing.T) {
	up := newHeaderServer(t)
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{allow: []string{"site.test", "tunnel.test"}})
	h.p.httpDeadline = time.Nanosecond
	h.upstream("site.test", up.addr())
	h.res.set("tunnel.test", "203.0.113.9")
	h.dialer.route("203.0.113.9", e.Addr())

	if r, body := h.get(t, "http://site.test/", ""); r.StatusCode != 502 || body != "upstream request failed\n" {
		t.Fatalf("expired request: %d %q", r.StatusCode, body)
	}
	if got := h.logs.matching("FAIL agent=- mode=allowlist host=site.test:80 reason=request err=\"context deadline exceeded\""); len(got) != 1 {
		t.Fatalf("log = %q", h.logs.all())
	}
	r, conn, br := h.connect(t, "tunnel.test:443", "")
	if r.StatusCode != 200 {
		t.Fatalf("tunnel status %d", r.StatusCode)
	}
	echo(t, conn, br, []byte("still open"))
}
