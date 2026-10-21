package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// An agent's shell, presenting its shell credential, reaches the
// team's list and that agent's own hosts, and nothing else; another
// agent's shell and an anonymous request still get only the team's
// list. Without the slug check one agent's additions would open for
// every shell; without the shell mode they would open for none. This
// fails with the wrong status for any row, or a refusal whose wording
// run_shell's notice no longer matches.
func TestShellCredentialReachesTheAgentsOwnHosts(t *testing.T) {
	e := newEchoServer(t)
	up := newHeaderServer(t)
	h := newHarness(t, harnessConfig{
		allow:      []string{"listed.test"},
		secrets:    map[string]string{"alice": "open-a", "bob": "open-b"},
		shells:     map[string]string{"alice": "shell-a", "bob": "shell-b"},
		agentHosts: map[string][]string{"alice": {"own.test"}},
	})
	for _, name := range []string{"listed.test", "own.test", "api.own.test", "unlisted.test"} {
		h.upstream(name, e.Addr())
	}
	alice, bob := basicAuth("alice", "shell-a"), basicAuth("bob", "shell-b")

	for _, c := range []struct {
		name   string
		target string
		auth   string
		status int
	}{
		{"alice's shell, her own host", "own.test:443", alice, 200},
		{"alice's shell, under her own host", "api.own.test:443", alice, 200},
		{"alice's shell, the team's host", "listed.test:443", alice, 200},
		{"alice's shell, unlisted", "unlisted.test:443", alice, 403},
		{"bob's shell, alice's host", "own.test:443", bob, 403},
		{"bob's shell, the team's host", "listed.test:443", bob, 200},
		{"anonymous, alice's host", "own.test:443", "", 403},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, conn, br := h.connect(t, c.target, c.auth)
			if resp.StatusCode != c.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.status)
			}
			if c.status == 200 {
				echo(t, conn, br, []byte("hi\n"))
			}
		})
	}

	// Plain HTTP: the same rule, and the refusal names the host and the
	// team's list in the wording run_shell matches.
	h.dialer.route(publicIP, up.addr())
	if r, body := h.get(t, "http://own.test/ping", alice); r.StatusCode != 200 || body != "hello /ping" {
		t.Fatalf("alice's shell over plain HTTP: %d %q", r.StatusCode, body)
	}
	r, body := h.get(t, "http://own.test/ping", bob)
	if want := "own.test is not on the team's allow list.\n"; r.StatusCode != 403 || body != want {
		t.Fatalf("bob's shell over plain HTTP: %d %q, want 403 %q", r.StatusCode, body, want)
	}
	if got := h.logs.matching("DENY agent=bob mode=shell host=own.test:80 reason=allowlist"); len(got) != 1 {
		t.Fatalf("refusal log = %q", h.logs.all())
	}
	h.logs.next(t, "CONN agent=alice mode=shell host=own.test:80 ")
	// Shell traffic is not the agent's own: nothing is counted.
	if ten, day := h.p.budget.Usage("alice"); ten != 0 || day != 0 {
		t.Fatalf("shell traffic counted: %d, %d", ten, day)
	}
}

// A shell's host checks come before any lookup, as for an anonymous
// request: a refused name never reaches the nameserver, which would
// carry it out. This fails if the shell mode resolves before deciding.
func TestShellModeChecksBeforeResolving(t *testing.T) {
	h := newHarness(t, harnessConfig{
		shells:     map[string]string{"alice": "shell-a"},
		agentHosts: map[string][]string{"alice": {"own.test"}},
	})
	if resp, _, _ := h.connect(t, "c2VjcmV0.attacker.test:443", basicAuth("alice", "shell-a")); resp.StatusCode != 403 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if asked := h.res.asked(); len(asked) != 0 {
		t.Fatalf("resolved %v", asked)
	}
}

// A secret that matches neither of the agent's hashes, or another
// agent's shell secret under this agent's slug, is refused with 407 as
// any bad credential is. This fails if a shell credential is checked
// against the wrong slug or a wrong one falls back to allowlist mode.
func TestWrongShellCredentialIs407(t *testing.T) {
	h := newHarness(t, harnessConfig{
		allow:  []string{"listed.test"},
		shells: map[string]string{"alice": "shell-a", "bob": "shell-b"},
	})
	for _, auth := range []string{basicAuth("alice", "nope"), basicAuth("alice", "shell-b"), basicAuth("carol", "shell-a")} {
		resp, _, _ := h.connect(t, "listed.test:443", auth)
		if resp.StatusCode != http.StatusProxyAuthRequired {
			t.Errorf("%s: status %d, want 407", auth, resp.StatusCode)
		}
	}
}

// An exempt agent reaches any public host with either credential, and
// the proxy counts nothing, enforced budget or not. This fails if the
// exemption leaves the shell on a list, or if its bytes or host names
// reach the budget (a Usage above zero, an OVER or COUNT line, or a
// refusal once the tiny limits are crossed).
func TestExemptAgentIsNeitherLimitedNorCounted(t *testing.T) {
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{
		secrets:    map[string]string{"alice": "open-a"},
		shells:     map[string]string{"alice": "shell-a"},
		exempt:     []string{"alice"},
		modelHosts: []string{"api.anthropic.com"},
		limits:     budgetLimits{Conn: 4, TenMin: 4, Day: 4},
		enforce:    true,
	})
	h.upstream("anywhere.test", e.Addr())
	h.upstream("api.anthropic.com", e.Addr())
	payload := []byte(strings.Repeat("x", 1000))

	for _, c := range []struct{ name, target, auth string }{
		{"shell credential", "anywhere.test:443", basicAuth("alice", "shell-a")},
		{"own credential", "anywhere.test:443", basicAuth("alice", "open-a")},
		{"own credential, model host, no ClientHello", "api.anthropic.com:443", basicAuth("alice", "open-a")},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, conn, br := h.connect(t, c.target, c.auth)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			echo(t, conn, br, payload)
			_ = conn.Close()
			if line := h.logs.next(t, "CONN"); !strings.HasPrefix(line, "CONN agent=alice mode=exempt host="+c.target+" up=1000 down=1000") {
				t.Fatalf("close line = %q", line)
			}
		})
	}
	// A second request after all of that is still let through.
	if r, _, _ := h.connect(t, "anywhere.test:443", basicAuth("alice", "open-a")); r.StatusCode != 200 {
		t.Fatalf("after 3000 bytes against a 4-byte budget: %d", r.StatusCode)
	}
	if ten, day := h.p.budget.Usage("alice"); ten != 0 || day != 0 {
		t.Fatalf("exempt agent counted: %d, %d", ten, day)
	}
	for _, prefix := range []string{"OVER", "COUNT", "DENY"} {
		if got := h.logs.matching(prefix); len(got) != 0 {
			t.Fatalf("%s lines for an exempt agent: %q", prefix, got)
		}
	}
}

// The exemption lifts the lists and the budget, not the address guard:
// an exempt agent still never reaches the cluster, loopback or a
// private address, except a host the owner listed (for the team, or
// for this agent), which keeps the allowlist's address rules. This
// fails if exempt traffic skips the guard, or loses an intranet host
// the agent's shell could reach before the switch.
func TestExemptAgentKeepsTheAddressGuard(t *testing.T) {
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{
		shells:     map[string]string{"alice": "shell-a"},
		agentHosts: map[string][]string{"alice": {"intranet.test"}},
		exempt:     []string{"alice"},
	})
	h.res.set("private.test", "10.0.0.6")
	h.res.set("intranet.test", "10.0.0.5")
	h.res.set("loop.test", "127.0.0.1")
	for _, ip := range []string{"10.0.0.5", "10.0.0.6", "127.0.0.1"} {
		h.dialer.route(ip, e.Addr())
	}
	auth := basicAuth("alice", "shell-a")
	for target, want := range map[string]int{
		"private.test:443":  403,
		"loop.test:443":     403,
		"127.0.0.1:443":     403,
		"intranet.test:443": 200,
	} {
		if resp, _, _ := h.connect(t, target, auth); resp.StatusCode != want {
			t.Errorf("CONNECT %s: %d, want %d", target, resp.StatusCode, want)
		}
	}
	for _, d := range h.dialer.calls() {
		if d != "10.0.0.5:443" {
			t.Errorf("dialled refused address %s", d)
		}
	}
}

// A host the owner adds for an agent, or the exemption, applies to the
// shell's very next request: a refusal re-reads a changed agents file
// before it is sent, as a credential miss does. Without it the agent
// would see a refusal for up to a watcher poll after the owner said
// yes. This fails with a 403 on the request after the file changed.
func TestNewAgentHostAppliesOnTheNextRequest(t *testing.T) {
	e := newEchoServer(t)
	shells := map[string]string{"alice": "shell-a"}
	h := newHarness(t, harnessConfig{shells: shells})
	h.upstream("own.test", e.Addr())
	h.upstream("other.test", e.Addr())
	auth := basicAuth("alice", "shell-a")

	if resp, _, _ := h.connect(t, "own.test:443", auth); resp.StatusCode != 403 {
		t.Fatalf("before the add: %d", resp.StatusCode)
	}
	writeAgentsFile(t, h.agentsPath, agentsFile{shells: shells, agentHosts: map[string][]string{"alice": {"own.test"}}})
	if resp, _, _ := h.connect(t, "own.test:443", auth); resp.StatusCode != 200 {
		t.Fatalf("after the add: %d", resp.StatusCode)
	}

	if resp, _, _ := h.connect(t, "other.test:443", auth); resp.StatusCode != 403 {
		t.Fatalf("before the exemption: %d", resp.StatusCode)
	}
	writeAgentsFile(t, h.agentsPath, agentsFile{shells: shells, exempt: []string{"alice"}})
	resp, conn, br := h.connect(t, "other.test:443", auth)
	if resp.StatusCode != 200 {
		t.Fatalf("after the exemption: %d", resp.StatusCode)
	}
	echo(t, conn, br, []byte("hi"))
	_ = conn.Close()
	if line := h.logs.next(t, "CONN agent=alice mode=exempt"); !strings.HasPrefix(line, "CONN agent=alice mode=exempt host=other.test:443") {
		t.Fatalf("close line = %q", line)
	}
	if resp, _, _ = h.connect(t, "own.test:443", auth); resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("own host dropped from the file but agent exempt: %d %q", resp.StatusCode, body)
	}
}
