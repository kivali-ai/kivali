//go:build unix

package browserpod

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/kivali-ai/kivali/internal/clock"
)

// fakeXvfbRun does to fd 3 what Debian's xvfb-run does: it opens fd 3
// for its own output and closes it for the command it runs.
const fakeXvfbRun = `#!/bin/sh
while [ $# -gt 0 ]; do
	case $1 in
	-a) shift ;;
	-s) shift 2 ;;
	*) break ;;
	esac
done
exec 3>/dev/null
"$@" 3>&-
`

// fakeChromium answers the first CDP call the way Chromium started
// with --remote-debugging-pipe does, reading fd 3 and writing fd 4,
// and exits as Chromium does when either is not open.
const fakeChromium = `#!/bin/sh
if ! { : <&3; } 2>/dev/null || ! { : >&4; } 2>/dev/null; then
	echo "Remote debugging pipe file descriptors are not open." >&2
	exit 13
fi
printf '{"id":1,"result":{"product":"Fake/1"}}\0' >&4
exec cat <&3 >/dev/null
`

// TestHeadfulLaunchKeepsThePipeUnderXvfbRun guards the headed launch,
// which every site session uses. xvfb-run closes fd 3 for the command
// it runs; when the pipe went through it on fds 3 and 4, Chromium
// exited at start with status 13 and no site session could start.
func TestHeadfulLaunchKeepsThePipeUnderXvfbRun(t *testing.T) {
	bin := t.TempDir()
	for name, script := range map[string]string{"xvfb-run": fakeXvfbRun, "chromium": fakeChromium} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), failsafe)
	defer cancel()
	p, err := launchChrome(ctx, chromeOptions{
		Binary:  filepath.Join(bin, "chromium"),
		Dir:     t.TempDir(),
		Headful: true,
		Logger:  log.New(io.Discard, "", 0),
		Clock:   clock.NewFake(),
	})
	if err != nil {
		t.Fatalf("headed launch under xvfb-run: %v", err)
	}
	p.Kill()
	<-p.Done()
}
