package agentpod

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}

// TestCacheLookupAndStoreSHA covers the content-addressed read path:
// store bytes under (sha, kind), look them back up. A miss returns
// (nil, false) and is OK to call on a nil cache.
func TestCacheLookupAndStoreSHA(t *testing.T) {
	c, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	hello := shaHex("hello")
	if _, ok := c.LookupSHA(hello, "blob"); ok {
		t.Errorf("LookupSHA on empty cache returned ok=true")
	}
	if err := c.StoreSHA(hello, "blob", []byte("hello")); err != nil {
		t.Fatalf("StoreSHA: %v", err)
	}
	got, ok := c.LookupSHA(hello, "blob")
	if !ok {
		t.Fatal("LookupSHA after Store returned ok=false")
	}
	if string(got) != "hello" {
		t.Errorf("body = %q, want hello", got)
	}
	// Different kind for the same SHA is independent.
	if _, ok := c.LookupSHA(hello, "canonical"); ok {
		t.Error("canonical lookup hit on a blob-only entry")
	}
	// A canonical is not the bytes of its SHA; it is served as stored.
	if err := c.StoreSHA(hello, "canonical", []byte("hello, as text")); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.LookupSHA(hello, "canonical"); !ok || string(got) != "hello, as text" {
		t.Errorf("canonical = %q, %v", got, ok)
	}
}

func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// An entry is served only when its bytes are what its key or ETag
// says: a blob whose bytes hash to another SHA, or a path entry whose
// body does not give its stored ETag, is a miss (and the caller
// refetches over it).
func TestCacheLookupsVerifyBytes(t *testing.T) {
	c, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sha := shaHex("real")
	if err := c.StoreSHA(sha, "blob", []byte("forged")); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.LookupSHA(sha, "blob"); ok {
		t.Errorf("a blob of other bytes = %q, want a miss", got)
	}
	if err := c.StorePath("agent/alice/role", ETag([]byte("# role")), []byte("# forged")); err != nil {
		t.Fatal(err)
	}
	if got, _, ok := c.LookupPath("agent/alice/role"); ok {
		t.Errorf("a path entry whose body does not match its etag = %q, want a miss", got)
	}
}

// TestCacheNilSafe locks in the no-cache shape: a nil receiver is
// safe to call against — every method degrades to a no-op so the
// Client doesn't have to nil-check at every call site.
func TestCacheNilSafe(t *testing.T) {
	var c *Cache
	if _, ok := c.LookupSHA("abc", "blob"); ok {
		t.Error("nil cache LookupSHA returned ok=true")
	}
	if _, _, ok := c.LookupPath("k"); ok {
		t.Error("nil cache LookupPath returned ok=true")
	}
	if err := c.StoreSHA("a", "b", []byte("x")); err != nil {
		t.Errorf("nil cache StoreSHA returned err: %v", err)
	}
	if err := c.StorePath("k", "etag", []byte("v")); err != nil {
		t.Errorf("nil cache StorePath returned err: %v", err)
	}
	c.InvalidatePath("k") // no-op, no panic
	c.InvalidateSHA("a")  // no-op, no panic
}

// TestCacheLookupAndStorePath covers the path-addressed shape:
// body + etag round-trip and stay independent across keys.
func TestCacheLookupAndStorePath(t *testing.T) {
	c, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	roleETag := ETag([]byte("# role"))
	if err := c.StorePath("agent/alice/role", roleETag, []byte("# role")); err != nil {
		t.Fatalf("StorePath: %v", err)
	}
	body, etag, ok := c.LookupPath("agent/alice/role")
	if !ok {
		t.Fatal("LookupPath returned ok=false after store")
	}
	if string(body) != "# role" {
		t.Errorf("body = %q, want '# role'", body)
	}
	if etag != roleETag {
		t.Errorf("etag = %q, want %q", etag, roleETag)
	}
	// Independent key.
	if _, _, ok := c.LookupPath("agent/bob/role"); ok {
		t.Error("Lookup for different key returned ok=true")
	}
}

// TestCacheInvalidatePath drops a stored entry; subsequent lookup
// is a miss. Re-storing repopulates.
func TestCacheInvalidatePath(t *testing.T) {
	c, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	_ = c.StorePath("k", ETag([]byte("v")), []byte("v"))
	c.InvalidatePath("k")
	if _, _, ok := c.LookupPath("k"); ok {
		t.Error("Lookup after InvalidatePath returned ok=true")
	}
	// Re-store works.
	_ = c.StorePath("k", ETag([]byte("v2")), []byte("v2"))
	body, etag, ok := c.LookupPath("k")
	if !ok || string(body) != "v2" || etag != ETag([]byte("v2")) {
		t.Errorf("re-store roundtrip failed: ok=%v body=%q etag=%q", ok, body, etag)
	}
}

// TestCacheConcurrentStorePathSerialized fires N concurrent stores
// against the same key and asserts the entry ends valid (body + etag
// from the same write — no half-old/half-new). The atomic-rename
// path + per-key mutex are what make this work.
func TestCacheConcurrentStorePathSerialized(t *testing.T) {
	c, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	const writers = 20
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := []byte(fmt.Sprintf("body-%d", i))
			if err := c.StorePath("k", ETag(body), body); err != nil {
				t.Errorf("StorePath: %v", err)
			}
		}(i)
	}
	wg.Wait()
	// LookupPath hits only when body and etag match, i.e. come from a
	// single write.
	if _, _, ok := c.LookupPath("k"); !ok {
		t.Fatal("LookupPath after concurrent stores missed: body and etag from different writes")
	}
}

// TestETagDeterministic locks the ETag format: same bytes → same
// string, different bytes → different string. Wire compatibility
// depends on this not drifting.
func TestETagDeterministic(t *testing.T) {
	a := ETag([]byte("hello"))
	b := ETag([]byte("hello"))
	if a != b {
		t.Errorf("ETag not deterministic: %q vs %q", a, b)
	}
	if ETag([]byte("hello")) == ETag([]byte("hellp")) {
		t.Error("ETag collision on different inputs")
	}
	// Format check: starts with `"sha256-` and ends with `"`.
	const prefix = `"sha256-`
	if a[:len(prefix)] != prefix || a[len(a)-1:] != `"` {
		t.Errorf("ETag format = %q, want quoted sha256-... shape", a)
	}
}

// TestClientCachedReadShortCircuitsServer drives the end-to-end
// behavior through a fake core: first ReadRole hits the server,
// second ReadRole sends If-None-Match and the server replies 304;
// the client returns the cached body without calling the server's
// body-build path.
//
// Asserts (1) the second request carries If-None-Match with the
// stored ETag and (2) the cached body is what the client returns
// after the 304.
func TestClientCachedReadShortCircuitsServer(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewCache(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	var (
		mu      sync.Mutex
		hits    int
		notMods int
		lastINM string
	)
	body := []byte(`{"body":"# my role"}`)
	bodyETag := ETag([]byte("# my role"))

	srv, sockPath := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		lastINM = r.Header.Get("If-None-Match")
		mu.Unlock()
		w.Header().Set("ETag", bodyETag)
		if r.Header.Get("If-None-Match") == bodyETag {
			mu.Lock()
			notMods++
			mu.Unlock()
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer func() { _ = srv.Close() }()

	c := NewClient(sockPath, "alice").WithCache(cache)

	// First read: cache miss, server returns 200 + bytes.
	got, err := c.ReadRole(context.Background())
	if err != nil {
		t.Fatalf("ReadRole #1: %v", err)
	}
	if got != "# my role" {
		t.Errorf("ReadRole #1 = %q, want '# my role'", got)
	}
	mu.Lock()
	if lastINM != "" {
		t.Errorf("first read sent If-None-Match=%q, want empty", lastINM)
	}
	mu.Unlock()

	// Second read: cache hit, server returns 304, client returns
	// cached body.
	got, err = c.ReadRole(context.Background())
	if err != nil {
		t.Fatalf("ReadRole #2: %v", err)
	}
	if got != "# my role" {
		t.Errorf("ReadRole #2 = %q, want '# my role'", got)
	}
	mu.Lock()
	if lastINM != bodyETag {
		t.Errorf("second read sent If-None-Match=%q, want %q", lastINM, bodyETag)
	}
	if hits != 2 {
		t.Errorf("server hits = %d, want 2", hits)
	}
	if notMods != 1 {
		t.Errorf("server 304 count = %d, want 1", notMods)
	}
	mu.Unlock()
}

// TestClientCachedReadAfterEtagChangeRefreshes: when role.md changes
// server-side, the next ReadRole sends the stale ETag, gets a 200
// with a fresh ETag, refreshes the cache, and returns the new body.
// Lock-in for the path-addressed-mutable invariant.
func TestClientCachedReadAfterEtagChangeRefreshes(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewCache(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	var current atomic.Value // string body
	current.Store("# v1")

	srv, sockPath := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := current.Load().(string)
		etag := ETag([]byte(body))
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		envelope := map[string]string{"body": body}
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	defer func() { _ = srv.Close() }()

	c := NewClient(sockPath, "alice").WithCache(cache)

	if got, err := c.ReadRole(context.Background()); err != nil || got != "# v1" {
		t.Fatalf("ReadRole #1 = (%q, %v), want ('# v1', nil)", got, err)
	}

	// Server-side mutation: next read sees fresh ETag → 200.
	current.Store("# v2")
	if got, err := c.ReadRole(context.Background()); err != nil || got != "# v2" {
		t.Fatalf("ReadRole #2 after mutation = (%q, %v), want ('# v2', nil)", got, err)
	}

	// Third read with the new ETag should 304 and still serve "# v2".
	if got, err := c.ReadRole(context.Background()); err != nil || got != "# v2" {
		t.Fatalf("ReadRole #3 = (%q, %v), want ('# v2', nil)", got, err)
	}
}

// TestClientContentAddressedSecondReadIsLocal: the second
// OpenAttachmentBlob with the same SHA should not hit the server at
// all (content-addressed cache is forever-cache).
func TestClientContentAddressedSecondReadIsLocal(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewCache(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	var hits atomic.Int32
	srv, sockPath := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("attachment-bytes"))
	}))
	defer func() { _ = srv.Close() }()

	c := NewClient(sockPath, "alice").WithCache(cache)

	rc, err := c.OpenAttachmentBlob(context.Background(), shaHex("attachment-bytes"))
	if err != nil {
		t.Fatalf("OpenAttachmentBlob #1: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "attachment-bytes" {
		t.Errorf("read #1 = %q, want attachment-bytes", got)
	}
	if hits.Load() != 1 {
		t.Errorf("server hits after #1 = %d, want 1", hits.Load())
	}

	// Second read: must NOT hit the server.
	rc, err = c.OpenAttachmentBlob(context.Background(), shaHex("attachment-bytes"))
	if err != nil {
		t.Fatalf("OpenAttachmentBlob #2: %v", err)
	}
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "attachment-bytes" {
		t.Errorf("read #2 = %q, want attachment-bytes", got)
	}
	if hits.Load() != 1 {
		t.Errorf("server hits after #2 = %d, want 1 (cache should serve)", hits.Load())
	}
}

// TestClientWriteRoleInvalidatesCache: WriteRole drops the cached
// entry so the next ReadRole refetches (server may have normalized
// the body we sent, so we don't pre-populate from the request).
func TestClientWriteRoleInvalidatesCache(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewCache(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}

	var (
		mu       sync.Mutex
		readHits int
		body     = "# v1"
	)
	srv, sockPath := startUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			readHits++
			etag := ETag([]byte(body))
			w.Header().Set("ETag", etag)
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"body": body})
		case http.MethodPost:
			var in struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			body = in.Body
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer func() { _ = srv.Close() }()

	c := NewClient(sockPath, "alice").WithCache(cache)

	// Prime the cache.
	_, _ = c.ReadRole(context.Background())
	if got := readHits; got != 1 {
		t.Fatalf("read hits after prime = %d, want 1", got)
	}

	// Write — should drop the cache.
	if err := c.WriteRole(context.Background(), "# v2"); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}
	if _, _, ok := cache.LookupPath(roleCacheKey("alice")); ok {
		t.Error("cache entry still present after WriteRole; expected invalidation")
	}

	// Next ReadRole should refetch with no If-None-Match (cold miss
	// after invalidation) and get the fresh body.
	got, err := c.ReadRole(context.Background())
	if err != nil || got != "# v2" {
		t.Fatalf("ReadRole after write = (%q, %v), want ('# v2', nil)", got, err)
	}
}

// TestRuntimeCacheInvalidateEvent: a cache-invalidate event over
// the SSE stream drops the named keys from the local cache. Locks
// the wire-side belt-and-suspenders path.
func TestRuntimeCacheInvalidateEvent(t *testing.T) {
	fc := startFakeCore(t)
	dir := t.TempDir()
	cache, err := NewCache(dir)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	// Pre-populate.
	_ = cache.StorePath("agent/alice/role", ETag([]byte("# role")), []byte("# role"))
	_ = cache.StoreSHA(shaHex("att-bytes"), "blob", []byte("att-bytes"))
	if _, _, ok := cache.LookupPath("agent/alice/role"); !ok {
		t.Fatal("fixture: role entry misses before the event")
	}
	if _, ok := cache.LookupSHA(shaHex("att-bytes"), "blob"); !ok {
		t.Fatal("fixture: blob entry misses before the event")
	}

	c := NewClient(fc.addr, "alice").WithCache(cache)
	rt := &Runtime{Client: c, Logger: discardLogger(t)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rt.Run(ctx) }()
	waitForSubscribe(t, fc, "alice")

	body, _ := json.Marshal(CacheInvalidateEvent{
		Paths: []string{"agent/alice/role"},
		SHAs:  []string{shaHex("att-bytes")},
	})
	fc.publish("alice", Event{Type: EventCacheInvalidate, Data: body})

	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _, pathOK := cache.LookupPath("agent/alice/role")
		_, shaOK := cache.LookupSHA(shaHex("att-bytes"), "blob")
		if !pathOK && !shaOK {
			return // both invalidated
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache entries not invalidated within 2s: pathOK=%v shaOK=%v", pathOK, shaOK)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startUnixServer is a tiny helper that boots an http.Server on a
// fresh Unix socket in a t.TempDir(). Mirrors the agentpod_publish
// test fixture but generalized for any handler.
func startUnixServer(t *testing.T, h http.Handler) (*http.Server, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wos-cache-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv, sockPath
}

func waitForSubscribe(t *testing.T, fc *fakeCore, slug string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fc.mu.Lock()
		_, ok := fc.subs[slug]
		fc.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime never subscribed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
