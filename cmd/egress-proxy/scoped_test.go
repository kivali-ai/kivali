package main

import (
	"net/http"
	"testing"
)

// An integration's server, presenting its scoped credential, reaches
// the hosts its version names and nothing else: not the team's list,
// not another integration's hosts, not any public host. Were the team's
// list admitted, an integration would reach whatever the owner opened
// for agents' shells; were it treated as open mode, the whole public
// web. The refusal names the host in the integration's own words. This
// fails with the wrong status for any row, a refusal in other words, or
// a refusal log line in another shape.
func TestScopedCredentialReachesOnlyItsHosts(t *testing.T) {
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{
		allow: []string{"listed.test"},
		scoped: map[string]scopedFixture{
			"integration-crm":  {secret: "crm-secret", hosts: []string{"api.crm.test", "*.cdn.crm.test"}},
			"integration-blog": {secret: "blog-secret", hosts: []string{"api.blog.test"}},
		},
	})
	for _, name := range []string{"api.crm.test", "eu.cdn.crm.test", "listed.test", "api.blog.test", "anywhere.test"} {
		h.upstream(name, e.Addr())
	}
	crm := basicAuth("integration-crm", "crm-secret")

	for _, c := range []struct {
		name   string
		target string
		auth   string
		status int
	}{
		{"its own host", "api.crm.test:443", crm, 200},
		{"under its own pattern", "eu.cdn.crm.test:443", crm, 200},
		{"the team's list", "listed.test:443", crm, 403},
		{"another integration's host", "api.blog.test:443", crm, 403},
		{"any other public host", "anywhere.test:443", crm, 403},
		{"its user with another's secret", "api.crm.test:443", basicAuth("integration-crm", "blog-secret"), 407},
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

	h.dialer.route(publicIP, newHeaderServer(t).addr())
	r, body := h.get(t, "http://anywhere.test/x", crm)
	if want := "anywhere.test is not a host this integration reaches.\n"; r.StatusCode != http.StatusForbidden || body != want {
		t.Fatalf("plain HTTP refusal: %d %q, want 403 %q", r.StatusCode, body, want)
	}
	if got := h.logs.matching("DENY agent=integration-crm mode=scoped host=anywhere.test:80 reason=hosts"); len(got) != 1 {
		t.Fatalf("refusal log = %q", h.logs.all())
	}
	// Nothing an integration sends is the agents' traffic: not counted.
	if ten, day := h.p.budget.Usage("integration-crm"); ten != 0 || day != 0 {
		t.Fatalf("scoped traffic counted: %d, %d", ten, day)
	}
}

// A scoped credential is never exempt, even when an agent of the same
// name has had its limits removed: the switch is the owner's for that
// agent, and must not open an integration to the whole web. This fails
// if the exemption is checked before the scoped kind.
func TestScopedCredentialIgnoresAnAgentsExemption(t *testing.T) {
	e := newEchoServer(t)
	h := newHarness(t, harnessConfig{
		exempt: []string{"integration-crm"},
		scoped: map[string]scopedFixture{"integration-crm": {secret: "crm-secret", hosts: []string{"api.crm.test"}}},
	})
	h.upstream("anywhere.test", e.Addr())
	resp, _, _ := h.connect(t, "anywhere.test:443", basicAuth("integration-crm", "crm-secret"))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

// A scoped host is checked before any lookup, as for every listed
// mode: a refused name never reaches the nameserver, which would carry
// it out. This fails if scoped mode resolves before deciding.
func TestScopedModeChecksBeforeResolving(t *testing.T) {
	h := newHarness(t, harnessConfig{
		scoped: map[string]scopedFixture{"integration-crm": {secret: "crm-secret", hosts: []string{"api.crm.test"}}},
	})
	if resp, _, _ := h.connect(t, "c2VjcmV0.attacker.test:443", basicAuth("integration-crm", "crm-secret")); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if asked := h.res.asked(); len(asked) != 0 {
		t.Fatalf("resolved %v", asked)
	}
}
