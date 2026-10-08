//go:build !windows

package main

import (
	"fmt"
	"os"
)

// broker is Windows-only: the Hyper-V broker, and its install,
// uninstall, status and setup, exist only there.
func (c *cli) broker([]string) int {
	fmt.Fprintln(os.Stderr, "kivali-supervisor broker: the Hyper-V broker (--console, install, uninstall, status, setup) is only available on Windows")
	return 2
}
