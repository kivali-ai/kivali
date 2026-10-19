package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// Agents is the hot-reloadable credentials file core writes beside the
// allowlist:
//
//	{"agents": {"<slug>": "<lowercase hex sha256 of the secret>"},
//	 "model_hosts": ["api.anthropic.com"]}
//
// An agent that presents its slug and secret gets open mode. Model
// hosts are exact hostnames exempt from the byte budget; they grant
// nothing to a request without valid credentials.
type Agents struct {
	// Path is re-read on a credential miss when it has changed.
	Path string
	// Logf reports a failed re-read. Nil means silent.
	Logf func(string, ...any)

	data atomic.Pointer[agentsData]
	// loads counts reads of the file.
	loads atomic.Int64

	// mu serialises loads, so the watcher and a miss cannot finish out
	// of order and leave an older file current.
	mu sync.Mutex
	// failed is the file as it was when it last failed to load; it is
	// not read again until it changes. Guarded by mu.
	failed os.FileInfo
}

type agentsData struct {
	hashes     map[string][]byte
	modelHosts map[string]struct{}
	// info describes the file these entries were read from; nil when
	// it was missing.
	info os.FileInfo
}

// Load reads the agents file at path. A missing file means no agents
// and no model hosts. An entry whose hash is not 32 bytes of hex is
// dropped, so it can never match.
func (a *Agents) Load(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.loadLocked(path)
}

// loadLocked is Load with mu held.
func (a *Agents) loadLocked(path string) error {
	a.loads.Add(1)
	d, info, err := readAgents(path)
	if err != nil {
		a.failed = info
		return err
	}
	a.failed = nil
	a.data.Store(d)
	return nil
}

// readAgents parses the agents file at path. info describes the file
// read, also when parsing it failed; it is nil when the file is missing
// or could not be opened.
func readAgents(path string) (*agentsData, os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &agentsData{}, nil, nil
		}
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, info, err
	}
	var file struct {
		Agents     map[string]string `json:"agents"`
		ModelHosts []string          `json:"model_hosts"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		return nil, info, fmt.Errorf("agents json: %w", err)
	}
	d := &agentsData{
		hashes:     make(map[string][]byte, len(file.Agents)),
		modelHosts: make(map[string]struct{}, len(file.ModelHosts)),
		info:       info,
	}
	for slug, h := range file.Agents {
		sum, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(h)))
		if err != nil || len(sum) != sha256.Size || slug == "" {
			continue
		}
		d.hashes[slug] = sum
	}
	for _, h := range file.ModelHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			d.modelHosts[h] = struct{}{}
		}
	}
	return d, info, nil
}

func (a *Agents) current() *agentsData {
	if d := a.data.Load(); d != nil {
		return d
	}
	return &agentsData{}
}

// Verify reports whether secret is slug's. On a miss it re-reads the
// file if it has changed since it was loaded, and checks again, so a
// pod registered moments ago is not refused while the watcher waits
// for its next poll. A miss against an unchanged file costs one stat
// and reads nothing, however many arrive.
func (a *Agents) Verify(slug, secret string) bool {
	sum := sha256.Sum256([]byte(secret))
	if a.current().match(slug, sum[:]) {
		return true
	}
	// Check again even when this call did not read: another miss may
	// have loaded the new file while this one waited for it.
	a.reloadIfChanged()
	return a.current().match(slug, sum[:])
}

func (d *agentsData) match(slug string, sum []byte) bool {
	want, ok := d.hashes[slug]
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare(sum, want) == 1
}

// reloadIfChanged re-reads the file when it is neither the one last
// loaded nor one that last failed to load.
// The check is repeated under mu, so a burst of misses after a change
// reads the file once.
func (a *Agents) reloadIfChanged() {
	if a.unchanged() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.unchangedLocked() {
		return
	}
	if err := a.loadLocked(a.Path); err != nil && a.Logf != nil {
		a.Logf("agents reload error: %v", err)
	}
}

// unchanged reports, without waiting on a load in progress, whether
// the file is still the one last loaded. A stat error counts as
// unchanged: there is nothing new to read.
func (a *Agents) unchanged() bool {
	info, err := statAgents(a.Path)
	return err != nil || sameFile(a.current().info, info)
}

// unchangedLocked is unchanged with mu held, also counting the file
// that last failed to load as unchanged.
func (a *Agents) unchangedLocked() bool {
	info, err := statAgents(a.Path)
	if err != nil || sameFile(a.current().info, info) {
		return true
	}
	return info != nil && a.failed != nil && sameFile(a.failed, info)
}

// statAgents stats path; a missing file is a nil info and no error.
func statAgents(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return info, err
}

// sameFile reports whether two stats describe the same, unchanged
// file: same inode, modification time and size. Two missing files are
// the same. Core replaces the file by rename, which changes the inode.
func sameFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

// IsModelHost reports whether hostport's host is exactly one of the
// model hosts, ignoring case and port.
func (a *Agents) IsModelHost(hostport string) bool {
	_, ok := a.current().modelHosts[hostOnly(hostport)]
	return ok
}

// hostOnly is hostport's host, lowercased, without port or brackets.
func hostOnly(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]"))
}

// parseProxyAuth reads a Basic Proxy-Authorization value into slug and
// secret. ok is false for any other scheme or a malformed value.
func parseProxyAuth(v string) (slug, secret string, ok bool) {
	scheme, rest, found := strings.Cut(strings.TrimSpace(v), " ")
	if !found || !strings.EqualFold(scheme, "Basic") {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
	if err != nil {
		return "", "", false
	}
	slug, secret, found = strings.Cut(string(raw), ":")
	if !found || slug == "" || secret == "" {
		return "", "", false
	}
	return slug, secret, true
}
