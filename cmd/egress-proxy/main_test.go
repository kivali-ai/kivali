package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAllowlist(t *testing.T, dir string, lines []string) string {
	t.Helper()
	p := filepath.Join(dir, "allowlist.yaml")
	body := "patterns:\n"
	for _, l := range lines {
		// Quote: YAML treats `*` as anchor/alias, so `*.pypi.org` must
		// be a quoted scalar to round-trip intact.
		body += "  - \"" + l + "\"\n"
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAllowlistPatterns(t *testing.T) {
	dir := t.TempDir()
	path := writeAllowlist(t, dir, []string{"pypi.org", "*.pypi.org", "deb.debian.org"})
	var a Allowlist
	if err := a.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"pypi.org":           true,
		"files.pypi.org":     true,
		"a.b.pypi.org":       true,
		"deb.debian.org":     true,
		"deb.debian.org:80":  true,
		"pypi.org.evil.com":  false, // suffix-match must not leak
		"fakedeb.debian.org": false, // label boundary, not raw suffix
		"example.com":        false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// A bare domain is implicitly wildcarded downward: apex plus every
// subdomain, at any depth, without the operator writing "*.".
func TestAllowlistBareDomainCoversSubdomains(t *testing.T) {
	dir := t.TempDir()
	path := writeAllowlist(t, dir, []string{"example.com"})
	var a Allowlist
	if err := a.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"example.com":       true,
		"api.example.com":   true,
		"a.b.c.example.com": true,
		"notexample.com":    false,
		"example.com.evil":  false,
		"example.org":       false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// An explicit subdomain entry scopes downward from that subdomain —
// its own children are in, the parent apex stays out.
func TestAllowlistSubdomainEntryDoesNotCoverApex(t *testing.T) {
	dir := t.TempDir()
	path := writeAllowlist(t, dir, []string{"api.example.com"})
	var a Allowlist
	if err := a.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"api.example.com":    true,
		"v2.api.example.com": true,
		"example.com":        false,
		"www.example.com":    false,
		"notapi.example.com": false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// A "*." entry is taken as written and means exactly what the bare
// form means — operators with existing wildcards see no change.
func TestAllowlistWildcardEqualsBareForm(t *testing.T) {
	dir := t.TempDir()
	var wild, bare Allowlist
	if err := wild.Load(writeAllowlist(t, t.TempDir(), []string{"*.example.com"})); err != nil {
		t.Fatalf("Load wildcard: %v", err)
	}
	if err := bare.Load(writeAllowlist(t, dir, []string{"example.com"})); err != nil {
		t.Fatalf("Load bare: %v", err)
	}
	for _, host := range []string{"example.com", "api.example.com", "a.b.example.com", "notexample.com", "example.org"} {
		if w, b := wild.Allowed(host), bare.Allowed(host); w != b {
			t.Errorf("Allowed(%q): wildcard=%v bare=%v, want equal", host, w, b)
		}
	}
}

// A glob matches label by label: "*" stays inside its label, a class
// holds one character, and the names it matches bring their subdomains.
// The default Bedrock and Vertex entries reach every region's endpoint
// and nothing a stranger can name under the same parent.
func TestAllowlistGlobMatchesLabelByLabel(t *testing.T) {
	var a Allowlist
	if err := a.Load(writeAllowlist(t, t.TempDir(), []string{
		"bedrock-runtime.[a-z][a-z]-*-[0-9].amazonaws.com",
		"sts.[a-z][a-z]-*-[0-9].amazonaws.com",
		"*-aiplatform.googleapis.com",
		"aiplatform.*.rep.googleapis.com",
		"bad[.example.com",
	})); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"bedrock-runtime.us-east-1.amazonaws.com":      true,
		"bedrock-runtime.ap-southeast-2.amazonaws.com": true,
		"bedrock-runtime.us-gov-west-1.amazonaws.com":  true,
		"bedrock-runtime.eu-west-3.amazonaws.com:443":  true,
		"x.bedrock-runtime.us-east-1.amazonaws.com":    true,
		"sts.eu-central-1.amazonaws.com":               true,
		"us-east5-aiplatform.googleapis.com":           true,
		"europe-west1-aiplatform.googleapis.com":       true,
		"aiplatform.eu.rep.googleapis.com":             true,
		// S3 names a bucket can own under amazonaws.com.
		"bedrock-runtime.s3.amazonaws.com":             false,
		"bedrock-runtime.s3-us-west-2.amazonaws.com":   false,
		"bedrock-runtime.evil.s3.amazonaws.com":        false,
		"bedrock-runtime.us-east-1.amazonaws.com.evil": false,
		"bedrock-runtime.us-east-1.evil.amazonaws.com": false,
		"s3.us-east-1.amazonaws.com":                   false,
		"amazonaws.com":                                false,
		"storage.googleapis.com":                       false,
		"aiplatform.googleapis.com.evil.io":            false,
		"us-east5-aiplatform.googleapis.com.evil.io":   false,
		"aiplatform.eu.rep.googleapis.com.evil.io":     false,
		"aiplatform.a.b.rep.googleapis.com":            false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// The default Microsoft Foundry entries reach every resource's
// endpoint and Entra ID's token host, and stop at the label boundary:
// "*." covers everything under services.ai.azure.com and nothing that
// merely ends in it or carries it as a prefix.
func TestAllowlistFoundryDefaults(t *testing.T) {
	var a Allowlist
	if err := a.Load(writeAllowlist(t, t.TempDir(), []string{"*.services.ai.azure.com", "login.microsoftonline.com"})); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"my-resource.services.ai.azure.com":      true,
		"my-resource.services.ai.azure.com:443":  true,
		"login.microsoftonline.com":              true,
		"login.microsoftonline.com:443":          true,
		"services.ai.azure.com.evil.io":          false,
		"my-resource.services.ai.azure.com.evil": false,
		"evilservices.ai.azure.com":              false,
		"ai.azure.com":                           false,
		"my-resource.openai.azure.com":           false,
		"evil-login.microsoftonline.com.io":      false,
		"microsoftonline.com":                    false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// Nothing lives under an IP address, so an IP literal matches only
// itself — no phantom "subdomain" of an address.
func TestAllowlistIPLiteralExactOnly(t *testing.T) {
	dir := t.TempDir()
	path := writeAllowlist(t, dir, []string{"10.0.0.5"})
	var a Allowlist
	if err := a.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]bool{
		"10.0.0.5":      true,
		"10.0.0.5:443":  true,
		"host.10.0.0.5": false,
		"10.0.0.6":      false,
	}
	for host, want := range cases {
		if got := a.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestAllowlistEmptyFile(t *testing.T) {
	dir := t.TempDir()
	// No file at all → empty allowlist, deny-all.
	var a Allowlist
	if err := a.Load(filepath.Join(dir, "none.yaml")); err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if a.Allowed("pypi.org") {
		t.Error("deny-by-default expected")
	}
}

// countingLoader records each Load the Watcher asks for.
type countingLoader struct{ loads int }

func (c *countingLoader) Load(string) error { c.loads++; return nil }

// setMod gives path a fixed modification time, so a test controls
// what the watcher sees changing.
func setMod(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherReloadsOnChange(t *testing.T) {
	dir := t.TempDir()
	path := writeAllowlist(t, dir, []string{"one.example.com"})
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	setMod(t, path, base)
	var a Allowlist
	w := &Watcher{Path: path}
	if err := w.pollOnce(&a); err != nil {
		t.Fatal(err)
	}
	if !a.Allowed("one.example.com") || a.Allowed("two.example.com") {
		t.Fatalf("first poll: %v", a.Patterns())
	}

	_ = writeAllowlist(t, dir, []string{"two.example.com"})
	setMod(t, path, base.Add(time.Second))
	if err := w.pollOnce(&a); err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if !a.Allowed("two.example.com") {
		t.Errorf("did not pick up new allowlist: %v", a.Patterns())
	}
}

// An unchanged file is not re-read; a change of size alone is enough
// to reload, so two writes within one mtime tick are not missed; a
// file that disappears is loaded as missing, which empties it.
func TestWatcherReloadsOnlyOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agents.json")
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var l countingLoader
	w := &Watcher{Path: path}

	if err := w.pollOnce(&l); err != nil || l.loads != 0 {
		t.Fatalf("missing file from the start: loads=%d err=%v", l.loads, err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	setMod(t, path, at)
	_ = w.pollOnce(&l)
	_ = w.pollOnce(&l)
	if l.loads != 1 {
		t.Fatalf("unchanged file: loads=%d, want 1", l.loads)
	}
	if err := os.WriteFile(path, []byte(`{"agents":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	setMod(t, path, at)
	_ = w.pollOnce(&l)
	if l.loads != 2 {
		t.Fatalf("size change: loads=%d, want 2", l.loads)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_ = w.pollOnce(&l)
	_ = w.pollOnce(&l)
	if l.loads != 3 {
		t.Fatalf("removed file: loads=%d, want 3", l.loads)
	}
}
