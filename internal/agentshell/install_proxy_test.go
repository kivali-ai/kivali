package agentshell

import (
	"bytes"
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

func TestProxyHostPort(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"none", []string{"PATH=/bin", "HOME=/root"}, ""},
		{"bare ip", []string{"https_proxy=http://10.43.0.12:3128"}, "10.43.0.12:3128"},
		{"https first", []string{"http_proxy=http://a:1", "https_proxy=http://b:2"}, "b:2"},
		{"upper case", []string{"HTTP_PROXY=http://proxy:3128"}, "proxy:3128"},
		{"scheme's port", []string{"http_proxy=http://proxy"}, "proxy:80"},
		{"https scheme's port", []string{"http_proxy=https://proxy"}, "proxy:443"},
		{"ipv6", []string{"http_proxy=http://[fd00::12]:3128"}, "[fd00::12]:3128"},
		{"credentials dropped", []string{"https_proxy=http://alice:s3cret@10.43.0.12:3128"}, "10.43.0.12:3128"},
		{"empty value", []string{"https_proxy=", "http_proxy=http://proxy:3128"}, "proxy:3128"},
		{"not a URL", []string{"https_proxy=proxy:3128"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := proxyHostPort(c.env); got != c.want {
				t.Errorf("proxyHostPort(%q) = %q, want %q", c.env, got, c.want)
			}
		})
	}
}

// A dialer that refuses the first n connections, as kube-proxy does
// while the proxy's Service has no endpoint, then accepts.
func dialRefusing(n int32, got *atomic.Int32, addrs chan<- string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		addrs <- addr
		if got.Add(1) <= n {
			return nil, errors.New("connect: connection refused")
		}
		return dialAnswering(ctx, network, addr)
	}
}

func TestAwaitProxyRetriesUntilTheProxyAnswers(t *testing.T) {
	clk := clock.NewFake()
	var dials atomic.Int32
	addrs := make(chan string, 8)
	in := Installer{
		Env:   []string{"PATH=/bin", "https_proxy=http://10.43.0.12:3128"},
		Dial:  dialRefusing(2, &dials, addrs),
		Clock: clk,
	}
	done := make(chan string, 1)
	go func() { done <- in.awaitProxy(context.Background()) }()
	for i := 0; i < 2; i++ {
		clk.BlockUntil(1)
		clk.Advance(proxyRetryInterval)
	}
	if reason := <-done; reason != "" {
		t.Fatalf("awaitProxy = %q, want it to return nothing once the proxy answers", reason)
	}
	if n := dials.Load(); n != 3 {
		t.Errorf("dials = %d, want 3", n)
	}
	close(addrs)
	for a := range addrs {
		if a != "10.43.0.12:3128" {
			t.Errorf("dialled %q, want the proxy's host:port", a)
		}
	}
}

func TestAwaitProxyGivesUpAfterItsBound(t *testing.T) {
	clk := clock.NewFake()
	var dials atomic.Int32
	addrs := make(chan string, 16)
	var log bytes.Buffer
	in := Installer{
		Env:       []string{"https_proxy=http://10.43.0.12:3128"},
		Dial:      dialRefusing(100, &dials, addrs),
		Clock:     clk,
		ProxyWait: 3 * time.Second,
		Log:       &log,
	}
	done := make(chan string, 1)
	go func() { done <- in.awaitProxy(context.Background()) }()
	// Attempts at 0s, 1s, 2s and 3s; the last is past the bound.
	for i := 0; i < 3; i++ {
		clk.BlockUntil(1)
		clk.Advance(proxyRetryInterval)
	}
	reason := <-done
	want := "the egress proxy at 10.43.0.12:3128 accepted no connection in 3s: connect: connection refused"
	if reason != want {
		t.Errorf("awaitProxy = %q, want %q", reason, want)
	}
	if n := dials.Load(); n != 4 {
		t.Errorf("dials = %d, want 4", n)
	}
	if !strings.Contains(log.String(), want) {
		t.Errorf("log = %q, want the reason in it", log.String())
	}
}

func TestAwaitProxyStopsWhenTheInstallEnds(t *testing.T) {
	clk := clock.NewFake()
	var dials atomic.Int32
	addrs := make(chan string, 16)
	in := Installer{
		Env:   []string{"https_proxy=http://10.43.0.12:3128"},
		Dial:  dialRefusing(100, &dials, addrs),
		Clock: clk,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() { done <- in.awaitProxy(ctx) }()
	clk.BlockUntil(1)
	cancel()
	if reason := <-done; !strings.HasPrefix(reason, "the egress proxy at 10.43.0.12:3128 accepted no connection") {
		t.Errorf("awaitProxy = %q, want the reason", reason)
	}
	if n := dials.Load(); n != 1 {
		t.Errorf("dials = %d, want 1", n)
	}
}

func TestAwaitProxyNeedsAProxy(t *testing.T) {
	in := Installer{
		Env: []string{"PATH=/bin"},
		Dial: func(context.Context, string, string) (net.Conn, error) {
			t.Error("dialled with no proxy in the environment")
			return nil, errors.New("no")
		},
	}
	if reason := in.awaitProxy(context.Background()); reason != "" {
		t.Errorf("awaitProxy = %q, want nothing", reason)
	}
}

// The install waits for the proxy before apt-get update, and names a
// wait that ran out ahead of the update's own failure.
func TestInstallerWaitsForTheProxyBeforeUpdate(t *testing.T) {
	apt := newFakeApt(t)
	clk := clock.NewFake()
	var dials atomic.Int32
	addrs := make(chan string, 8)
	in := apt.installer("https_proxy=http://10.43.0.12:3128")
	in.Dial = dialRefusing(1, &dials, addrs)
	in.Clock = clk
	done := make(chan InstallResult, 1)
	go func() { done <- in.Install(context.Background(), []string{"jq"}) }()
	clk.BlockUntil(1)
	if got := apt.runs(t); got != nil {
		t.Errorf("apt-get ran %q before the proxy answered", got)
	}
	clk.Advance(proxyRetryInterval)
	res := <-done
	if want := []string{"jq"}; !reflect.DeepEqual(res.Installed, want) || len(res.Failed) != 0 {
		t.Errorf("result = %+v, want jq installed", res)
	}
	if got := apt.runs(t); len(got) == 0 || !reflect.DeepEqual(got[0], []string{"update"}) {
		t.Errorf("apt-get runs = %q, want update first", got)
	}

	apt = newFakeApt(t)
	in = apt.installer("https_proxy=http://10.43.0.12:3128", "FAKE_APT_UPDATE_FAILS=1", "FAKE_APT_UNKNOWN=jq")
	in.Dial = dialRefusing(100, &dials, make(chan string, 64))
	in.ProxyWait = time.Second
	in.Clock = clk
	go func() { done <- in.Install(context.Background(), []string{"jq"}) }()
	clk.BlockUntil(1)
	clk.Advance(proxyRetryInterval)
	res = <-done
	want := "no package has this name; the package lists could not be updated: " +
		"the egress proxy at 10.43.0.12:3128 accepted no connection in 1s: connect: connection refused; " +
		"Some index files failed to download. They have been ignored, or old ones used instead."
	if len(res.Failed) != 1 || res.Failed[0].Error != want {
		t.Errorf("Failed = %+v, want jq with %q", res.Failed, want)
	}
}
