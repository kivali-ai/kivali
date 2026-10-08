package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// terminal attaches a shell in the server container through serve. It
// is a byte pipe: nothing typed is read (Claude's sign-in runs here), and
// of what is shown only the BROWSER helper's own marker is taken out, to
// open Claude's sign-in page in this computer's browser (browser.go).
// With a terminal on stdin it switches it to raw mode and forwards
// window size changes (terminal_unix.go, terminal_windows.go); with a
// pipe it sends the input and then ^D.
func (c *cli) terminal(args []string) int {
	fs := flag.NewFlagSet("terminal", flag.ExitOnError)
	shell := fs.Bool("shell", false, "a bash shell in the server container instead of Claude Code")
	_ = fs.Parse(args)
	ctx := context.Background()
	if !c.client.Ping(ctx) {
		return fail(errors.New("serve is not running; run `kivali-supervisor up`"))
	}
	fd := int(os.Stdin.Fd())
	isTTY := term.IsTerminal(fd)
	var rows, cols uint16 = 24, 80
	if isTTY {
		if w, h, err := consoleSize(fd); err == nil {
			cols, rows = uint16(w), uint16(h)
		}
	}
	conn, err := c.client.Terminal(ctx, rows, cols, *shell)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = conn.Close() }()
	if isTTY {
		old, err := term.MakeRaw(fd)
		if err != nil {
			return fail(err)
		}
		defer func() { _ = term.Restore(fd, old) }()
		defer prepareOutput()()
	}
	fw := guestapi.NewFrameWriter(conn)
	if isTTY {
		defer watchResize(fd, func(w, h int) {
			_ = fw.JSON(guestapi.FrameResize, guestapi.Resize{Rows: uint16(h), Cols: uint16(w)})
		})()
	}
	go func() {
		_, _ = io.Copy(fw.Writer(guestapi.FrameStdin), os.Stdin)
		if !isTTY {
			_ = fw.Frame(guestapi.FrameStdinClose, nil)
		}
	}()
	out := &openFilter{open: openInBrowser()}
	if *shell {
		out.open = func(string) {}
	}
	for {
		typ, p, err := guestapi.ReadFrame(conn)
		if err != nil {
			if isTTY {
				fmt.Fprint(os.Stderr, "\r\n")
			}
			return fail(fmt.Errorf("terminal: %w", err))
		}
		switch typ {
		case guestapi.FrameStdout:
			_, _ = os.Stdout.Write(out.Write(p))
		case guestapi.FrameExit:
			var ex guestapi.Exit
			_ = json.Unmarshal(p, &ex)
			if ex.Error != "" {
				fmt.Fprintf(os.Stderr, "\r\nterminal: %s\r\n", ex.Error)
			}
			if ex.Code < 0 {
				return 1
			}
			return ex.Code
		}
	}
}
