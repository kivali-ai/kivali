package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kivali-ai/kivali/internal/clock"
)

// Allowlist is a hot-reloadable set of host patterns the proxy will
// let through. Patterns:
//
//	example.com        → the name itself plus every subdomain under it
//	                     (api.example.com, a.b.example.com). Every
//	                     entry is implicitly wildcarded downward, so
//	                     an explicit subdomain entry like
//	                     api.example.com covers v2.api.example.com but
//	                     NOT the example.com apex.
//	*.example.com      → accepted as written; means the same thing
//	                     as the bare form above.
//	10.0.0.5           → IP literal, exact match only (an IP has no
//	                     subdomains to descend into)
//	*-aiplatform.googleapis.com,
//	bedrock.[a-z][a-z]-*-[0-9].amazonaws.com
//	                   → a glob: past a leading "*.", a pattern holding
//	                     "*" or "[" matches label by label with
//	                     path.Match ("*" any run of characters, "[a-z]"
//	                     one from a class), so "*" never crosses a dot.
//	                     It covers the names it matches and their
//	                     subdomains, like the bare form. Region-shaped
//	                     classes keep a glob from matching a name a
//	                     stranger can register under the same parent
//	                     (an S3 bucket's bedrock.s3.amazonaws.com).
//
// Matching is case-insensitive and always lands on a label boundary,
// so example.com never matches notexample.com or example.com.evil.io.
// The "default deny" policy is enforced at the callsite — if Allowed
// returns false, the caller refuses the connection.
type Allowlist struct {
	patterns atomic.Pointer[[]string]
}

// Load reads the allowlist from path (YAML: `patterns: [list]`). A
// missing file is treated as an empty allowlist — proxy starts in a
// useful state, the UI can populate it.
func (a *Allowlist) Load(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			empty := []string{}
			a.patterns.Store(&empty)
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	var message struct {
		Patterns []string `yaml:"patterns"`
	}
	if err := yaml.Unmarshal(b, &message); err != nil {
		return fmt.Errorf("allowlist yaml: %w", err)
	}
	// Normalize: lowercase, trim, drop any :port the user included
	// (allowlisting is port-agnostic), skip empties.
	cleaned := make([]string, 0, len(message.Patterns))
	for _, p := range message.Patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		if i := strings.LastIndex(p, ":"); i >= 0 && !strings.HasPrefix(p, "[") {
			p = p[:i]
		}
		cleaned = append(cleaned, p)
	}
	a.patterns.Store(&cleaned)
	return nil
}

// Patterns returns the current pattern set (safe to read concurrently).
func (a *Allowlist) Patterns() []string {
	p := a.patterns.Load()
	if p == nil {
		return nil
	}
	return *p
}

// Allowed reports whether host matches any pattern. Host may include
// a port (":443") which is stripped before matching.
func (a *Allowlist) Allowed(host string) bool {
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, pat := range a.Patterns() {
		if matchPattern(pat, host) {
			return true
		}
	}
	return false
}

// matchPattern implements the pattern language described on
// Allowlist. Every entry is treated as "this name and everything
// below it": a bare domain matches its own apex plus all subdomains,
// and an explicit subdomain entry matches that subdomain plus its
// own children. A leading "*." is stripped first, so the wildcard
// form an operator may already have written stays equivalent rather
// than becoming a second, subtly different rule.
//
// IP literals are the exception — nothing lives "under" an address,
// so they match exactly.
func matchPattern(pat, host string) bool {
	pat = strings.TrimPrefix(pat, "*.")
	if pat == "" {
		return false
	}
	if pat == host {
		return true
	}
	if isIPLiteral(pat) {
		return false
	}
	if strings.ContainsAny(pat, "*[") {
		return matchGlob(pat, host)
	}
	// Anchor on the label boundary so "example.com" cannot match
	// "notexample.com".
	return strings.HasSuffix(host, "."+pat)
}

// matchGlob matches host's last labels against pat's labels one for
// one, each with path.Match: a label holds no "/", so no wildcard
// reaches past its own label. Extra leading labels on host are its
// subdomains. A malformed pattern matches nothing.
func matchGlob(pat, host string) bool {
	pl := strings.Split(pat, ".")
	hl := strings.Split(host, ".")
	if len(hl) < len(pl) {
		return false
	}
	hl = hl[len(hl)-len(pl):]
	for i, p := range pl {
		if ok, err := path.Match(p, hl[i]); err != nil || !ok || hl[i] == "" {
			return false
		}
	}
	return true
}

// isIPLiteral reports whether pat is a bare IP address (v4, or v6
// with or without the surrounding brackets a host:port form uses).
func isIPLiteral(pat string) bool {
	p := strings.TrimSuffix(strings.TrimPrefix(pat, "["), "]")
	return net.ParseIP(p) != nil
}

// Watcher periodically re-reads the allowlist from disk so edits made
// by the Kivali pod (via the shared volume) take effect without a
// proxy restart. Interval is short enough for snappy UI feedback,
// long enough not to thrash the file.
type Watcher struct {
	Path     string
	Interval time.Duration

	// Clock drives the poll interval. Nil means the real clock.
	Clock clock.Clock

	mu      sync.Mutex
	lastMod time.Time
}

// clk is the watcher's time source, defaulting to the real clock.
func (w *Watcher) clk() clock.Clock {
	if w.Clock == nil {
		return clock.New()
	}
	return w.Clock
}

func (w *Watcher) pollOnce(a *Allowlist) error {
	info, err := os.Stat(w.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !info.ModTime().After(w.lastMod) {
		return nil
	}
	if err := a.Load(w.Path); err != nil {
		return err
	}
	w.lastMod = info.ModTime()
	return nil
}

// Run loops until ctx is done, polling pollOnce.
func (w *Watcher) Run(a *Allowlist, log func(string, ...any)) {
	if w.Interval == 0 {
		w.Interval = 5 * time.Second
	}
	t := w.clk().NewTicker(w.Interval)
	defer t.Stop()
	for range t.C() {
		if err := w.pollOnce(a); err != nil {
			log("allowlist reload error: %v", err)
		}
	}
}
