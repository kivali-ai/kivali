package agentpod

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/egresscred"
)

// shellCredentialFile reads the file the runtime hands the shell and
// returns the URL and the secret in it.
func shellCredentialFile(t *testing.T, path string) (string, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the shell's credential file: %v", err)
	}
	raw := strings.TrimSuffix(string(b), "\n")
	if strings.Contains(raw, "\n") {
		t.Fatalf("the file holds more than one line: %q", b)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := u.User.Password()
	return raw, secret
}

// With an agent-shell socket the runtime draws a second secret for the
// shell, registers both hashes in one request, and writes the shell's
// credentialed URL beside the socket, private to the pod's user. The
// shell's secret is scrubbed from what the runtime relays like its
// own. This fails if core never hears the shell's hash, if the file is
// missing, readable by others or carries the runtime's own secret, or
// if a relay quoting the shell's URL reaches core unscrubbed.
func TestSetUpEgressCredentialHandsTheShellItsOwnCredential(t *testing.T) {
	core := &egressCore{}
	c := NewClient(core.serve(t), "alice")
	sockDir := t.TempDir()
	cred, err := SetUpEgressCredential(context.Background(), c, "http://10.43.0.12:3128", filepath.Join(sockDir, "sock"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sockDir, "proxy-credential")
	shellURL, shellSecret := shellCredentialFile(t, path)
	u, _ := url.Parse(shellURL)
	openURL, _ := url.Parse(cred.URL)
	openSecret, _ := openURL.User.Password()
	if u.User.Username() != "alice" || u.Host != "10.43.0.12:3128" || shellSecret == "" || shellSecret == openSecret {
		t.Fatalf("shell URL %q (runtime's %q)", shellURL, cred.URL)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v, %v", fi, err)
	}
	if len(core.hashes) != 1 || core.hashes[0] != cred.Hash || core.shellHashes[0] != egresscred.Hash(shellSecret) {
		t.Fatalf("core got hashes %v shells %v", core.hashes, core.shellHashes)
	}
	if entries, _ := os.ReadDir(sockDir); len(entries) != 1 {
		t.Fatalf("socket dir holds %v, want the credential file alone", entries)
	}

	if err := c.PostTurnEvent(context.Background(), "t1", TurnEvent{
		Kind: TurnEventFailed,
		Text: "curl via " + shellURL + " and " + shellSecret,
	}); err != nil {
		t.Fatal(err)
	}
	if b := core.bodies[0]; strings.Contains(b, shellSecret) || !strings.Contains(b, egresscred.Placeholder) {
		t.Fatalf("core received %s", b)
	}
	if out := cred.Scrubber.String(shellURL); out != egresscred.Placeholder {
		t.Fatalf("the returned scrubber leaves the shell's URL as %q", out)
	}
}

// A credential file an earlier runtime in this pod left behind names a
// secret the new registration replaces, so it is removed before
// registering, and a registration core refuses writes no new one.
// Without the removal the shell would present a dead secret and get
// 407 for every host. This fails with the old file still in place.
func TestSetUpEgressCredentialRemovesAnEarlierShellCredential(t *testing.T) {
	core := &egressCore{status: http.StatusBadRequest}
	c := NewClient(core.serve(t), "alice").WithClock(clock.NewFake())
	sockDir := t.TempDir()
	path := filepath.Join(sockDir, "proxy-credential")
	if err := os.WriteFile(path, []byte("http://alice:old@10.43.0.12:3128\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SetUpEgressCredential(context.Background(), c, "http://10.43.0.12:3128", filepath.Join(sockDir, "sock")); !errors.Is(err, ErrEgressCredentialRefused) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the earlier credential file is still there: %v", err)
	}
}

// Every reconnect sends the shell's hash with the runtime's own, so a
// core that lost the registration learns both again. This fails if a
// reconnect registers only the first, which would leave the shell on
// 407 after a core restart.
func TestEventsReconnectRegistersTheShellCredentialAgain(t *testing.T) {
	core := &egressCore{}
	c := NewClient(core.serve(t), "alice").WithClock(clock.NewFake())
	sockDir := t.TempDir()
	if _, err := SetUpEgressCredential(context.Background(), c, "http://10.43.0.12:3128", filepath.Join(sockDir, "sock")); err != nil {
		t.Fatal(err)
	}
	_, shellSecret := shellCredentialFile(t, filepath.Join(sockDir, "proxy-credential"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pings := 0
	err := c.RunEvents(ctx, func(Event) error {
		if pings++; pings == 2 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunEvents = %v", err)
	}
	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.shellHashes) != 2 {
		t.Fatalf("shell hashes registered = %v, want the startup one and one reconnect", core.shellHashes)
	}
	for _, h := range core.shellHashes {
		if h != egresscred.Hash(shellSecret) {
			t.Fatalf("registered shell hash %s, want %s", h, egresscred.Hash(shellSecret))
		}
	}
}
