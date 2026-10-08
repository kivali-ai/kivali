//go:build !linux

// Command kivali-guest is the agent inside the Kivali Desktop VM; it
// only runs on Linux (see main.go).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kivali-guest runs inside the Kivali Desktop VM (linux) only")
	os.Exit(2)
}
