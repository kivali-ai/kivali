package agentpod

import (
	"os"
	"path/filepath"
	"testing"
)

// A cache entry replaced by a link is a miss, whether the link leads
// out of the cache or to another entry in it: on a 304 the client
// serves a path entry's body unchecked, so it must be the file the
// cache wrote.
func TestCacheRefusesLinkedEntries(t *testing.T) {
	c, err := NewCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("credentials"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := c.StorePath("agent/alice/role", ETag([]byte("role")), []byte("role")); err != nil {
		t.Fatal(err)
	}
	body := filepath.Join(c.pathDir("agent/alice/role"), "body")
	if err := os.Remove(body); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, body); err != nil {
		t.Fatal(err)
	}
	if got, _, ok := c.LookupPath("agent/alice/role"); ok {
		t.Errorf("LookupPath through a link out of the cache = %q, want a miss", got)
	}

	if err := c.StoreSHA(shaHex("real"), "blob", []byte("real")); err != nil {
		t.Fatal(err)
	}
	if err := c.StoreSHA(shaHex("other"), "blob", []byte("other")); err != nil {
		t.Fatal(err)
	}
	p := c.shaPath(shaHex("real"), "blob")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(c.shaPath(shaHex("other"), "blob"), p); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.LookupSHA(shaHex("real"), "blob"); ok {
		t.Errorf("LookupSHA through a link inside the cache = %q, want a miss", got)
	}
	if got, ok := c.LookupSHA(shaHex("other"), "blob"); !ok || string(got) != "other" {
		t.Errorf("LookupSHA of a plain entry = %q, %v", got, ok)
	}
}
