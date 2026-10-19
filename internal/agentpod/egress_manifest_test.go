package agentpod

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/kube"
)

func fixedResolver(addrs []string, err error, calls *int) func(context.Context, string) ([]string, error) {
	return func(_ context.Context, host string) ([]string, error) {
		if calls != nil {
			*calls++
		}
		return addrs, err
	}
}

func TestProxyByIP(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		addrs     []string
		want      string
		resolves  bool
	}{
		{"service name", "http://kivali-egress.kivali.svc.cluster.local:3128", []string{"10.43.0.12"}, "http://10.43.0.12:3128", true},
		{"ipv4 wins over ipv6", "http://kivali-egress:3128", []string{"fd00::12", "10.43.0.12"}, "http://10.43.0.12:3128", true},
		{"ipv6 only", "http://kivali-egress:3128", []string{"fd00::12"}, "http://[fd00::12]:3128", true},
		{"lowest ipv4", "http://kivali-egress:3128", []string{"10.43.0.20", "fd00::1", "10.43.0.3", "10.43.0.12"}, "http://10.43.0.3:3128", true},
		{"lowest ipv6", "http://kivali-egress:3128", []string{"fd00::20", "fd00::3"}, "http://[fd00::3]:3128", true},
		{"mapped ipv4", "http://kivali-egress:3128", []string{"::ffff:10.43.0.12"}, "http://10.43.0.12:3128", true},
		{"ip kept", "http://10.43.0.9:3128", nil, "http://10.43.0.9:3128", false},
		{"no port", "http://kivali-egress", []string{"10.43.0.12"}, "http://10.43.0.12", true},
		{"no proxy", "", nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got, err := proxyByIP(context.Background(), tc.raw, fixedResolver(tc.addrs, nil, &calls))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("proxyByIP(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			if (calls > 0) != tc.resolves {
				t.Errorf("resolver called %d times", calls)
			}
		})
	}
	for name, tc := range map[string]struct {
		raw   string
		addrs []string
		err   error
	}{
		"lookup fails":    {"http://kivali-egress:3128", nil, errors.New("no such host")},
		"no address":      {"http://kivali-egress:3128", nil, nil},
		"credentials":     {"http://u:p@kivali-egress:3128", []string{"10.43.0.12"}, nil},
		"not a proxy URL": {"kivali-egress:3128", []string{"10.43.0.12"}, nil},
	} {
		if _, err := proxyByIP(context.Background(), tc.raw, fixedResolver(tc.addrs, tc.err, nil)); err == nil {
			t.Errorf("%s: proxyByIP(%q) accepted", name, tc.raw)
		}
	}
}

// The same records in any order give the same pod spec hash.
func TestManifestForIsStableAcrossLookupOrder(t *testing.T) {
	hash := func(addrs []string) string {
		cfg := testConfig()
		cfg.ProxyURL = "http://kivali-egress:3128"
		p, err := NewProvisioner(cfg, WithResolver(fixedResolver(addrs, nil, nil)))
		if err != nil {
			t.Fatal(err)
		}
		pod, err := p.manifestFor(context.Background(), "alice")
		if err != nil {
			t.Fatal(err)
		}
		return pod["metadata"].(map[string]any)["labels"].(map[string]string)[kube.SpecHashLabel]
	}
	a := hash([]string{"10.43.0.12", "10.43.0.3", "fd00::1"})
	b := hash([]string{"fd00::1", "10.43.0.3", "10.43.0.12"})
	if a == "" || a != b {
		t.Fatalf("spec hashes %q and %q differ with lookup order", a, b)
	}
}

func containerEnv(t *testing.T, pod map[string]any) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, x := range pod["spec"].(map[string]any)["containers"].([]any) {
		m := x.(map[string]any)
		env := map[string]string{}
		for _, e := range m["env"].([]map[string]any) {
			v, _ := e["value"].(string)
			env[e["name"].(string)] = v
		}
		out[m["name"].(string)] = env
	}
	return out
}

// Provision writes the proxy by IP, with no credentials, into both
// containers: the agent-shell never gets more than the bare URL.
func TestManifestForPutsTheBareProxyIPInBothContainers(t *testing.T) {
	cfg := testConfig()
	cfg.ProxyURL = "http://kivali-egress.kivali.svc.cluster.local:3128"
	cfg.FilesSubPath = func(slug string) string { return "agents/" + slug + "/memory" }
	p, err := NewProvisioner(cfg, WithResolver(fixedResolver([]string{"10.43.0.12"}, nil, nil)))
	if err != nil {
		t.Fatal(err)
	}
	pod, err := p.manifestFor(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	envs := containerEnv(t, pod)
	for _, name := range []string{"agent", "agent-shell"} {
		env, ok := envs[name]
		if !ok {
			t.Fatalf("no %s container", name)
		}
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
			if env[k] != "http://10.43.0.12:3128" {
				t.Errorf("%s %s = %q, want the bare proxy IP", name, k, env[k])
			}
		}
		for k, v := range env {
			if strings.Contains(v, "@") {
				t.Errorf("%s %s = %q carries user info", name, k, v)
			}
		}
	}
}

func TestManifestForFailsWhenTheProxyDoesNotResolve(t *testing.T) {
	cfg := testConfig()
	cfg.ProxyURL = "http://kivali-egress:3128"
	p, err := NewProvisioner(cfg, WithResolver(fixedResolver(nil, errors.New("no such host"), nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.manifestFor(context.Background(), "alice"); err == nil {
		t.Fatal("a manifest with an unresolved proxy")
	}
}

// A local shell run by a process that inherited a credentialed proxy
// URL hands bash the bare URL.
func TestLocalShellGetsTheBareProxyURL(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://alice:s3cret@10.43.0.12:3128")
	t.Setenv("https_proxy", "http://alice:s3cret@10.43.0.12:3128")
	sh := &LocalShell{ScratchRoot: t.TempDir(), Slug: "alice"}
	res, err := sh.Exec(context.Background(), "alice", agent.ShellRequest{
		Command:        `printf '%s|%s' "$HTTPS_PROXY" "$https_proxy"`,
		TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "http://10.43.0.12:3128|http://10.43.0.12:3128" {
		t.Fatalf("bash saw %q", res.Stdout)
	}
}
