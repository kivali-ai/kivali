package claudeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kivali-ai/kivali/internal/claudeauth"
)

// signInWatch tells the runner registry which sign-in the CLI would
// start with now, so a warm runner born under another one is respawned
// at its next turn (runnerRegistry.Acquire). A warm `claude -p` reads
// its credentials and its settings file's env block when it starts and
// keeps them, while every sign-in path writes under the shared HOME:
// `claude` (/login) in the server container, the supervisor's sign-in
// setup and provider clear, an operator editing settings.json by hand.
//
// The key is the sign-in's identity, not its token: what `claude auth
// status --json` reports (logged in, auth method, provider, account,
// organization, subscription) and the settings file's bytes. An OAuth
// refresh rewrites .credentials.json with a new token for the same
// account and leaves the key as it was, so it respawns nothing.
//
// The status costs a CLI run, so it is asked only when the files a
// sign-in writes (.credentials.json, settings.json) have changed size
// or modification time since the key was last worked out.
type signInWatch struct {
	// home is the CLI's HOME; empty stamps nothing, so the key is
	// worked out once.
	home string
	// status reports how the CLI is signed in (authStatus in
	// production).
	status func(ctx context.Context) (claudeauth.Status, error)

	mu       sync.Mutex
	computed bool
	stamp    string
	key      string
}

func newSignInWatch(binary, home string) *signInWatch {
	if binary == "" {
		binary = "claude"
	}
	return &signInWatch{
		home:   home,
		status: func(ctx context.Context) (claudeauth.Status, error) { return authStatus(ctx, binary) },
	}
}

// credentialsPath and settingsPath are the files under HOME a sign-in
// writes.
func (w *signInWatch) credentialsPath() string {
	return filepath.Join(w.home, ".claude", ".credentials.json")
}

func (w *signInWatch) settingsPath() string {
	return filepath.Join(w.home, ".claude", "settings.json")
}

// filesStamp is the size and modification time of each file a sign-in
// writes ("-" for a missing one).
func (w *signInWatch) filesStamp() string {
	if w.home == "" {
		return ""
	}
	var b strings.Builder
	for _, p := range []string{w.credentialsPath(), w.settingsPath()} {
		fi, err := os.Stat(p)
		if err != nil {
			b.WriteString("-;")
			continue
		}
		fmt.Fprintf(&b, "%d@%d;", fi.Size(), fi.ModTime().UnixNano())
	}
	return b.String()
}

// Key is the current sign-in's identity. When the status cannot be
// read it answers the last key it worked out (empty before the first),
// so a failed check respawns nothing, and the next call asks again.
func (w *signInWatch) Key(ctx context.Context) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	stamp := w.filesStamp()
	if w.computed && stamp == w.stamp {
		return w.key
	}
	st, err := w.status(ctx)
	if err != nil {
		log.Printf("claudeagent: sign-in check: %v", err)
		return w.key
	}
	var settings []byte
	if w.home != "" {
		settings, _ = os.ReadFile(w.settingsPath())
	}
	w.computed, w.stamp, w.key = true, stamp, signInKey(st, settings)
	return w.key
}

// signInKey hashes a sign-in's identity: the status's fields and the
// settings file's bytes.
func signInKey(st claudeauth.Status, settings []byte) string {
	id, _ := json.Marshal(st)
	h := sha256.New()
	h.Write(id)
	h.Write([]byte{0})
	h.Write(settings)
	return hex.EncodeToString(h.Sum(nil))[:16]
}
