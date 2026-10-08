package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/supervisor"
)

func TestOpenFilterRemovesMarkersAcrossFrames(t *testing.T) {
	var opened []string
	f := &openFilter{open: func(u string) { opened = append(opened, u) }}
	marker := supervisor.OpenURLMarker + "https://claude.com/x?a=1\a"
	stream := "hello " + marker + "world"
	var shown strings.Builder
	// Every split point, one byte at a time and in two halves.
	for i := 0; i < len(stream); i++ {
		shown.Write(f.Write([]byte(stream[i : i+1])))
	}
	if got := shown.String(); got != "hello world" {
		t.Fatalf("shown %q", got)
	}
	if len(opened) != 1 || opened[0] != "https://claude.com/x?a=1" {
		t.Fatalf("opened %q", opened)
	}
	shown.Reset()
	for cut := 1; cut < len(stream); cut++ {
		opened = nil
		f = &openFilter{open: func(u string) { opened = append(opened, u) }}
		shown.Reset()
		shown.Write(f.Write([]byte(stream[:cut])))
		shown.Write(f.Write([]byte(stream[cut:])))
		if shown.String() != "hello world" || len(opened) != 1 {
			t.Fatalf("cut %d: shown %q opened %q", cut, shown.String(), opened)
		}
	}
}

func TestOpenFilterPassesOtherOutput(t *testing.T) {
	f := &openFilter{open: func(string) { t.Fatal("opened") }}
	// Other OSC sequences (Claude's own hyperlinks) pass untouched.
	in := "\x1b]8;id=1;https://claude.com/a\x1b\\link\x1b]8;;\x1b\\ \x1b]133;A\a"
	if got := string(f.Write([]byte(in))); got != in {
		t.Fatalf("got %q", got)
	}
	// A marker that never ends is shown once it outgrows a URL.
	long := supervisor.OpenURLMarker + strings.Repeat("x", maxMarkedURL+1)
	if got := string(f.Write([]byte(long))); got != long {
		t.Fatal("an unended marker was swallowed")
	}
}

func TestSigninURL(t *testing.T) {
	raw := "https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c&response_type=code" +
		"&redirect_uri=http%3A%2F%2Flocalhost%3A34437%2Fcallback&scope=user%3Ainference&code_challenge=C&code_challenge_method=S256&state=S"
	got, ok := signinURL(raw)
	if !ok {
		t.Fatal("refused")
	}
	u, _ := url.Parse(got)
	q := u.Query()
	if q.Get("redirect_uri") != manualCallback || q.Get("state") != "S" || q.Get("code_challenge") != "C" || q.Get("client_id") != "9d1c" {
		t.Fatalf("got %s", got)
	}
	// Already the manual link: kept.
	if got, _ := signinURL("https://claude.ai/oauth/authorize?redirect_uri=" + url.QueryEscape(manualCallback)); !strings.Contains(got, url.QueryEscape(manualCallback)) {
		t.Fatalf("got %s", got)
	}
	for _, bad := range []string{
		"http://claude.com/oauth", "https://evil.example/oauth", "https://claude.com.evil.example/x",
		"https://user@claude.com/x", "file:///etc/passwd", "javascript:alert(1)", "not a url",
	} {
		if _, ok := signinURL(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}
