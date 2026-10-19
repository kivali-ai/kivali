package agentpod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/egresscred"
)

// egressCore is a fake core.sock serving the egress registration and
// the turn-event route, recording what arrives.
type egressCore struct {
	mu sync.Mutex
	// fail answers this many registrations 503 before accepting.
	fail int
	// status, when set, answers every registration with it.
	status   int
	attempts int
	slugs    []string
	hashes   []string
	bodies   []string
}

func (f *egressCore) serve(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "eg-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s")
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+EgressCredentialPath, func(w http.ResponseWriter, r *http.Request) {
		var req EgressCredentialRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.attempts++
		switch {
		case f.status != 0:
			w.WriteHeader(f.status)
			return
		case f.attempts <= f.fail:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		f.slugs = append(f.slugs, SlugFromHeader(r.Header))
		f.hashes = append(f.hashes, req.SHA256)
		w.WriteHeader(http.StatusNoContent)
	})
	// Each events stream sends one ping and closes, so the client
	// reconnects at once.
	mux.HandleFunc("GET /v1/agent/{slug}/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: ping\ndata: {}\n\n")
	})
	mux.HandleFunc("POST /v1/agent/{slug}/chat-turn/{turn}/event", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.RemoveAll(dir)
	})
	return path
}

func TestSetUpEgressCredentialRegistersTheHashAndScrubsRelays(t *testing.T) {
	core := &egressCore{}
	c := NewClient(core.serve(t), "alice")
	cred, err := SetUpEgressCredential(context.Background(), c, "http://10.43.0.12:3128")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(cred.URL)
	secret, _ := u.User.Password()
	if u.User.Username() != "alice" || u.Host != "10.43.0.12:3128" || secret == "" {
		t.Fatalf("credentialed URL %q", cred.URL)
	}
	if len(core.hashes) != 1 || core.hashes[0] != egresscred.Hash(secret) || core.slugs[0] != "alice" {
		t.Fatalf("core got slugs %v hashes %v", core.slugs, core.hashes)
	}
	// Core is told the hash only.
	if strings.Contains(strings.Join(core.hashes, ""), secret) {
		t.Fatal("the secret reached core")
	}

	// Everything relayed afterwards is scrubbed.
	err = c.PostTurnEvent(context.Background(), "t1", TurnEvent{
		Kind: TurnEventFailed,
		Text: "WebFetch: proxy " + cred.URL + " said 502; secret " + secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(core.bodies) != 1 {
		t.Fatalf("bodies = %v", core.bodies)
	}
	if strings.Contains(core.bodies[0], secret) {
		t.Fatalf("core received the secret: %s", core.bodies[0])
	}
	if !strings.Contains(core.bodies[0], egresscred.Placeholder) {
		t.Fatalf("body has no placeholder: %s", core.bodies[0])
	}
}

// Every reconnect of the events stream registers the hash again, before
// any event of the new stream is handled; the first connection, right
// after the startup registration, does not.
func TestEventsReconnectRegistersTheCredentialAgain(t *testing.T) {
	core := &egressCore{}
	c := NewClient(core.serve(t), "alice").WithClock(clock.NewFake())
	cred, err := SetUpEgressCredential(context.Background(), c, "http://10.43.0.12:3128")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seen []int // registrations core had when each ping was handled
	err = c.RunEvents(ctx, func(ev Event) error {
		core.mu.Lock()
		seen = append(seen, len(core.hashes))
		core.mu.Unlock()
		if len(seen) == 3 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunEvents = %v", err)
	}
	if len(seen) != 3 || seen[0] != 1 || seen[1] != 2 || seen[2] != 3 {
		t.Fatalf("registrations at each ping = %v, want [1 2 3]", seen)
	}
	for _, h := range core.hashes {
		if h != cred.Hash {
			t.Fatalf("registered %s, want %s", h, cred.Hash)
		}
	}
}

func TestSetUpEgressCredentialWithoutAProxyDoesNothing(t *testing.T) {
	core := &egressCore{}
	c := NewClient(core.serve(t), "alice")
	cred, err := SetUpEgressCredential(context.Background(), c, "")
	if err != nil || cred != nil {
		t.Fatalf("cred %v err %v", cred, err)
	}
	if core.attempts != 0 {
		t.Fatalf("registered %d times with no proxy", core.attempts)
	}
}

// Core not ready yet: the runtime retries on its clock until accepted.
func TestRegisterEgressCredentialRetriesWithBackoff(t *testing.T) {
	core := &egressCore{fail: 2}
	clk := clock.NewFake()
	c := NewClient(core.serve(t), "alice").WithClock(clk)
	done := make(chan error, 1)
	go func() { done <- c.RegisterEgressCredential(context.Background(), egresscred.Hash("x")) }()

	clk.BlockUntil(1)
	clk.Advance(200 * time.Millisecond)
	clk.BlockUntil(1)
	clk.Advance(400 * time.Millisecond)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if core.attempts != 3 || len(core.hashes) != 1 {
		t.Fatalf("attempts %d, accepted %v", core.attempts, core.hashes)
	}
}

func TestRegisterEgressCredentialStopsOnARefusal(t *testing.T) {
	core := &egressCore{status: http.StatusBadRequest}
	c := NewClient(core.serve(t), "alice").WithClock(clock.NewFake())
	err := c.RegisterEgressCredential(context.Background(), "not-a-hash")
	if !errors.Is(err, ErrEgressCredentialRefused) {
		t.Fatalf("err = %v, want ErrEgressCredentialRefused", err)
	}
	if core.attempts != 1 {
		t.Fatalf("attempts = %d", core.attempts)
	}
}

func TestRegisterEgressCredentialEndsWithItsContext(t *testing.T) {
	core := &egressCore{status: http.StatusServiceUnavailable}
	clk := clock.NewFake()
	c := NewClient(core.serve(t), "alice").WithClock(clk)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.RegisterEgressCredential(ctx, egresscred.Hash("x")) }()
	clk.BlockUntil(1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
