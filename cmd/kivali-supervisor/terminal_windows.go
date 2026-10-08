package main

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// consoleSize is the console's size. Windows keeps the size on the
// screen buffer, which stdout's handle reaches and stdin's does not.
func consoleSize(int) (cols, rows int, err error) { return term.GetSize(int(os.Stdout.Fd())) }

// prepareOutput turns on virtual terminal processing for stdout, so the
// console interprets the guest's escape sequences (colours, cursor
// moves, the alternate screen) instead of printing them, and returns
// the undo. term.MakeRaw already turns on VT input for stdin.
func prepareOutput() (restore func()) {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return func() {}
	}
	if err := windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return func() {}
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }
}

// resizePoll is how often the console's size is read: Windows has no
// SIGWINCH, and a quarter second is below what a person notices while
// dragging a window edge.
const resizePoll = 250 * time.Millisecond

// watchResize polls the console's size and calls fn when it changes,
// until stop.
func watchResize(fd int, fn func(cols, rows int)) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(resizePoll)
		defer t.Stop()
		lastW, lastH, _ := consoleSize(fd)
		for {
			select {
			case <-t.C:
				w, h, err := consoleSize(fd)
				if err != nil || (w == lastW && h == lastH) {
					continue
				}
				lastW, lastH = w, h
				fn(w, h)
			case <-done:
				return
			}
		}
	}()
	return func() { close(done) }
}
