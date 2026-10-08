package main

import (
	"flag"
	"log"
	"os"

	"github.com/kivali-ai/kivali/internal/agentpod"
)

// runPrepareScratch is `kivali prepare-scratch`, the agent pod's init
// container: it lays out the per-agent volume so the agent and
// dev-shell containers each mount their own half of it, and neither
// can write what the other reads. See agentpod.PrepareScratch.
func runPrepareScratch(args []string) {
	fs := flag.NewFlagSet("prepare-scratch", flag.ExitOnError)
	root := fs.String("root", agentpod.ScratchVolumeMountPath, "where the init container mounts the whole per-agent volume")
	_ = fs.Parse(args)
	logger := log.New(os.Stderr, "prepare-scratch: ", log.LstdFlags)
	if err := agentpod.PrepareScratch(*root, logger.Printf); err != nil {
		logger.Fatalf("%s: %v", *root, err)
	}
}
