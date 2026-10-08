package main

import (
	"bytes"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/supervisor"
)

// openMarker is what the BROWSER helper in the server container writes
// to its terminal for the sign-in page (supervisor.TerminalArgv): an OSC
// sequence no terminal acts on, ended by BEL.
var openMarker = []byte(supervisor.OpenURLMarker)

// maxMarkedURL bounds what is held while a marker is incomplete.
const maxMarkedURL = 8 << 10

// openFilter passes the terminal's output through unchanged except for
// the helper's markers, which it removes and hands to open. It reads
// nothing else: the person's input never passes through it, and what
// Claude shows is copied as is.
type openFilter struct {
	pending []byte
	open    func(string)
}

// Write returns what to show now; a marker split across frames is held
// until it is whole (or proves not to be one).
func (f *openFilter) Write(p []byte) []byte {
	buf := append(f.pending, p...)
	f.pending = nil
	var out []byte
	for {
		i := bytes.Index(buf, openMarker)
		if i < 0 {
			// Hold back a tail that could be the start of a marker.
			keep := partialPrefix(buf, openMarker)
			out = append(out, buf[:len(buf)-keep]...)
			f.pending = append(f.pending, buf[len(buf)-keep:]...)
			return out
		}
		out = append(out, buf[:i]...)
		rest := buf[i+len(openMarker):]
		end := bytes.IndexByte(rest, 0x07)
		if end < 0 {
			if len(rest) > maxMarkedURL {
				// Not ours after all: show it.
				out = append(out, buf[i:]...)
				return out
			}
			f.pending = append(f.pending, buf[i:]...)
			return out
		}
		f.open(string(rest[:end]))
		buf = rest[end+1:]
	}
}

// partialPrefix is the length of the longest suffix of b that is a
// proper prefix of m.
func partialPrefix(b, m []byte) int {
	for n := len(m) - 1; n > 0; n-- {
		if len(b) >= n && bytes.Equal(b[len(b)-n:], m[:n]) {
			return n
		}
	}
	return 0
}

// manualCallback is where Claude's sign-in shows the code to paste back
// into Terminal: the redirect its printed link uses.
const manualCallback = "https://platform.claude.com/oauth/code/callback"

// signinURL checks a URL the helper passed is Claude's own sign-in page
// and points it at the paste-the-code callback. Claude Code hands its
// browser a link whose callback is a port on localhost inside the
// container, which this computer's browser cannot reach; the link it
// prints instead differs only in that redirect.
func signinURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	ok := false
	for _, d := range []string{"claude.com", "claude.ai", "anthropic.com"} {
		if host == d || strings.HasSuffix(host, "."+d) {
			ok = true
		}
	}
	if !ok {
		return "", false
	}
	q := u.Query()
	if r, err := url.Parse(q.Get("redirect_uri")); err == nil && (r.Hostname() == "localhost" || r.Hostname() == "127.0.0.1") {
		q.Set("redirect_uri", manualCallback)
		u.RawQuery = q.Encode()
	}
	return u.String(), true
}

// openInBrowser opens a checked sign-in page in this computer's browser,
// at most once every few seconds.
func openInBrowser() func(string) {
	var last time.Time
	return func(raw string) {
		u, ok := signinURL(raw)
		if !ok || time.Since(last) < 3*time.Second {
			return
		}
		last = time.Now()
		var cmd *exec.Cmd
		switch runtime.GOOS {
		case "darwin":
			cmd = exec.Command("open", u)
		case "windows":
			cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
		default:
			cmd = exec.Command("xdg-open", u)
		}
		_ = cmd.Start()
		go func() { _ = cmd.Wait() }()
	}
}
