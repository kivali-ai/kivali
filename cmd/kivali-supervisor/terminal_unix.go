//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// consoleSize is the terminal's size, read from stdin's terminal.
func consoleSize(fd int) (cols, rows int, err error) { return term.GetSize(fd) }

// prepareOutput has nothing to do: a Unix terminal interprets the
// guest's escape sequences already.
func prepareOutput() (restore func()) { return func() {} }

// watchResize calls fn with the new size on every SIGWINCH until stop.
func watchResize(fd int, fn func(cols, rows int)) (stop func()) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-winch:
				if w, h, err := term.GetSize(fd); err == nil {
					fn(w, h)
				}
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(winch)
		close(done)
	}
}
