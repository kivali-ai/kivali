// Command spike is a throwaway harness for the agent-browsers spike. It is not
// part of the product. See ../../README.md.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "window":
		err = cmdWindow(args)
	case "check":
		err = cmdCheck(args)
	case "memory":
		err = cmdMemory(args)
	case "attack":
		err = cmdAttack(args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: spike <command> [flags]

  window  --session <name> --mode headless|xvfb [--url <start url>]
  check   --session <name> --url <account url> --mode headless|xvfb [--restarts 3]
  memory  [--browsers 3] [--tabs 4] [--urls a,b,c] [--mode headless|xvfb] [--wait 60s]
  attack  [--mode headless|xvfb]

Run from spike/agent-browsers/. Each command takes -h for its flags.`)
	os.Exit(2)
}
