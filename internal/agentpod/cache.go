package agentpod

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Cache is the agent-pod's local read cache backed by /scratch/cache/.
// Two flavors live side-by-side under one rooted directory:
//
//   - Content-addressed (sha/<first-2>/<sha>/<kind>) — immutable.
//     Once a (sha, kind) pair lands, it never changes; we hand back
//     the cached bytes forever. Used for attachment + project-file
//     blob/canonical reads.
//   - Path-addressed (path/<key-hash>/{body,etag}) — validated per
//     read against the server's ETag. The Client sends If-None-Match
//     on the wire and on a 304 returns the cached body; on 200 the
//     cache is refreshed. Used for role.md, agent_memory.md, and
//     skill files.
//
// Truth lives on core. Cache eviction at any moment is correct — the
// next read fetches fresh. No LRU yet (per "skip pre-emptive caps
// without measurements"); add bounded eviction when we have a
// production read-pattern signal that motivates a specific cap.
//
// Thread-safety:
//
//   - Reads are independent across keys; concurrent reads are fine.
//   - Writes use temp-file + rename so a partial write can never be
//     observed by a reader.
//   - A per-key mutex serializes refresh-write races for the same
//     key (otherwise two concurrent ReadRole misses could both fetch
//     and clobber each other's bytes — harmless but wasteful).
//
// Disk layout (relative to root):
//
//	sha/<first-2>/<sha>/blob
//	sha/<first-2>/<sha>/canonical
//	path/<key-hash>/body
//	path/<key-hash>/etag
//
// Key-hash for path entries is sha256(key) — so cache keys can include
// arbitrary characters (slashes, query strings) without filesystem
// concerns.
type Cache struct {
	root string

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewCache returns a Cache rooted at dir. The directory is created
// (with intermediates) if it doesn't exist; a permissions failure
// here is fatal because the cache is on the pod's local PVC and
// not having it means the runtime's hot path round-trips to core
// for every system-prompt assembly.
func NewCache(dir string) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("agentpod: cache dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("agentpod: mkdir cache dir %q: %w", dir, err)
	}
	return &Cache{
		root:  dir,
		locks: map[string]*sync.Mutex{},
	}, nil
}

// Root reports the directory the cache writes to. Mostly for tests
// + diagnostics; callers shouldn't need this on the hot path.
func (c *Cache) Root() string {
	if c == nil {
		return ""
	}
	return c.root
}

// LookupSHA returns the cached bytes for a (sha, kind) pair. Returns
// (nil, false) on a miss. kind is one of "blob" / "canonical".
//
// Errors during read (corrupted entry, IO failure) are treated as
// misses: the caller will refetch + overwrite, which heals the
// entry. We don't log here — this is a hot path and successful
// re-fetch is the right semantic.
//
// A "blob" entry is the bytes whose SHA-256 is sha, so one whose bytes
// hash to anything else is a miss too. A canonical is derived from the
// blob and carries no hash of its own to check.
func (c *Cache) LookupSHA(sha, kind string) ([]byte, bool) {
	if c == nil || sha == "" || kind == "" {
		return nil, false
	}
	body, err := c.readEntry(c.shaPath(sha, kind))
	if err != nil {
		return nil, false
	}
	if kind == "blob" {
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != sha {
			return nil, false
		}
	}
	return body, true
}

// StoreSHA writes data under the (sha, kind) entry. Atomic: writes
// to a temp file in the destination directory and renames into
// place. Returns nil even when the write loses a race with a
// concurrent StoreSHA for the same key (the bytes are immutable, so
// any winner is correct).
func (c *Cache) StoreSHA(sha, kind string, data []byte) error {
	if c == nil {
		return nil
	}
	if sha == "" || kind == "" {
		return fmt.Errorf("agentpod: StoreSHA requires sha + kind")
	}
	dst := c.shaPath(sha, kind)
	if err := writeFileAtomic(dst, data); err != nil {
		return fmt.Errorf("agentpod: StoreSHA %s/%s: %w", sha, kind, err)
	}
	return nil
}

// LookupPath returns the cached body + etag for a path-addressed
// key. Returns ok=false on miss; body+etag are zero in that case.
//
// The etag is the value the server emitted on the 200 that filled
// the cache; the caller forwards it as If-None-Match on the next
// fetch so the server can short-circuit with 304. Every server ETag
// is ETag(body), so an entry whose body does not give its etag is a
// miss: on a 304 the body is served unchecked.
func (c *Cache) LookupPath(key string) (body []byte, etag string, ok bool) {
	if c == nil || key == "" {
		return nil, "", false
	}
	dir := c.pathDir(key)
	body, err := c.readEntry(filepath.Join(dir, "body"))
	if err != nil {
		return nil, "", false
	}
	etagBytes, err := c.readEntry(filepath.Join(dir, "etag"))
	if err != nil {
		// Bare body without an etag — treat as miss; the next 200
		// will write both fields and heal.
		return nil, "", false
	}
	etag = strings.TrimRight(string(etagBytes), "\n")
	if ETag(body) != etag {
		return nil, "", false
	}
	return body, etag, true
}

// StorePath writes body + etag for a path-addressed key. Atomic per
// file: a concurrent reader sees either both old or both new; never
// half-old/half-new (writeFileAtomic uses rename).
func (c *Cache) StorePath(key, etag string, body []byte) error {
	if c == nil {
		return nil
	}
	if key == "" {
		return fmt.Errorf("agentpod: StorePath requires key")
	}
	c.lockKey(key).Lock()
	defer c.lockKey(key).Unlock()
	dir := c.pathDir(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("agentpod: StorePath mkdir %q: %w", dir, err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "body"), body); err != nil {
		return fmt.Errorf("agentpod: StorePath body %q: %w", key, err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "etag"), []byte(etag)); err != nil {
		return fmt.Errorf("agentpod: StorePath etag %q: %w", key, err)
	}
	return nil
}

// InvalidatePath removes the cached entry for key. Idempotent — a
// missing entry is not an error. Used by the cache-invalidate SSE
// event handler to drop stale path-addressed entries on demand.
func (c *Cache) InvalidatePath(key string) {
	if c == nil || key == "" {
		return
	}
	c.lockKey(key).Lock()
	defer c.lockKey(key).Unlock()
	dir := c.pathDir(key)
	_ = os.RemoveAll(dir)
}

// InvalidateSHA removes the cached entries for one content-addressed
// hash (both blob and canonical). Idempotent; mostly here for
// completeness — content-addressed entries are immutable in
// principle, so the only legitimate caller is a content-addressed
// store wipe (e.g. attachment GC) that we don't have today.
func (c *Cache) InvalidateSHA(sha string) {
	if c == nil || sha == "" {
		return
	}
	dir := c.shaDir(sha)
	_ = os.RemoveAll(dir)
}

// readEntry reads the cache file at p (a path under c.root) through an
// os.Root on the cache root, so no link along the way can take the
// read outside it, and takes only a regular file. The cache sits in
// the agent container's own half of /scratch, which nothing else
// writes (see scratch.go); this, and the lookups' checks of each
// entry's bytes against its SHA or ETag, hold even if that ever
// changed.
func (c *Cache) readEntry(p string) ([]byte, error) {
	rel, err := filepath.Rel(c.root, p)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(c.root)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	// The entry itself is not followed: a link (even one inside the
	// cache) or a FIFO, on which an open would block, is refused
	// before the open; the fstat below checks what was opened.
	if li, err := root.Lstat(rel); err != nil {
		return nil, err
	} else if !li.Mode().IsRegular() {
		return nil, fmt.Errorf("agentpod: cache entry %s is not a regular file", rel)
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("agentpod: cache entry %s is not a regular file", rel)
	}
	return io.ReadAll(f)
}

func (c *Cache) shaDir(sha string) string {
	if len(sha) < 2 {
		// Defensive: a malformed sha shouldn't escape the cache root.
		return filepath.Join(c.root, "sha", "_short", sha)
	}
	return filepath.Join(c.root, "sha", sha[:2], sha)
}

func (c *Cache) shaPath(sha, kind string) string {
	return filepath.Join(c.shaDir(sha), kind)
}

func (c *Cache) pathDir(key string) string {
	h := sha256.Sum256([]byte(key))
	hh := hex.EncodeToString(h[:])
	return filepath.Join(c.root, "path", hh[:2], hh)
}

// lockKey returns the mutex guarding writes for one cache key. New
// keys get a fresh mutex on first access; the locks map grows
// unbounded today (one entry per distinct cache key per pod
// lifetime), which is fine — the per-pod key cardinality is low
// (role.md, agent_memory.md, ~10 skill manifests, ~50 skill files).
func (c *Cache) lockKey(key string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m, ok := c.locks[key]; ok {
		return m
	}
	m := &sync.Mutex{}
	c.locks[key] = m
	return m
}

// writeFileAtomic writes data to dst via a temp file + rename. The
// temp file lives in the same directory as dst so the rename is on
// the same filesystem and therefore atomic on Linux. Mode 0644 —
// the cache is readable by whoever can read /scratch (just the
// agent-pod UID).
func writeFileAtomic(dst string, data []byte) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cache-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// ETag computes the canonical ETag value for body bytes — sha256
// hex prefixed with `"sha256-"` and quoted. Stable across binaries
// and platforms (no clock, no UID involvement).
//
// Lives on the agentpod package so server-side endpoint handlers
// and client-side cache code agree on the exact format.
func ETag(body []byte) string {
	h := sha256.Sum256(body)
	return `"sha256-` + hex.EncodeToString(h[:]) + `"`
}

// readAllAndClose drains rc into memory and closes it. Tiny helper
// used by the client cache wrappers when they need to materialize a
// streaming response into a cacheable []byte.
func readAllAndClose(rc io.ReadCloser) ([]byte, error) {
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}
