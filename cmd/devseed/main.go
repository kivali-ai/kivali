// Command devseed builds a DATA_DIR with fixed, realistic content for
// the e2e suite and `make run`:
//
//	devseed -data <dir> [-scenario demo|empty|setup] [-now RFC3339] [-force]
//
// Scenarios:
//
//	demo   the Plainsong org: agents, goals, proposals, a queue, chats,
//	       usage, a knowledge graph and settings (see demo.go)
//	empty  setup done (org name, handbook, CEO, Chief of Staff) and
//	       nothing else, so Home shows its empty states
//	setup  an empty directory, so the setup wizard runs
//
// Every time is relative to -now (default: the current minute, UTC), so
// labels like "2h ago" read the same on every run; ids, order, text and
// numbers are fixed. The directory must be empty; -force wipes it first.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	dir := flag.String("data", "", "DATA_DIR to build (required)")
	scenario := flag.String("scenario", ScenarioDemo, "demo, empty or setup")
	nowFlag := flag.String("now", "", "the seed's present, RFC 3339 (default: now, truncated to the minute)")
	force := flag.Bool("force", false, "delete everything in -data before seeding")
	flag.Parse()

	if err := run(*dir, *scenario, *nowFlag, *force); err != nil {
		fmt.Fprintln(os.Stderr, "devseed:", err)
		os.Exit(1)
	}
}

func run(dir, scenario, nowFlag string, force bool) error {
	if dir == "" {
		return fmt.Errorf("-data is required")
	}
	now := time.Now().UTC().Truncate(time.Minute)
	if nowFlag != "" {
		t, err := time.Parse(time.RFC3339, nowFlag)
		if err != nil {
			return fmt.Errorf("-now: %w", err)
		}
		now = t.UTC()
	}
	if force {
		if err := wipeDir(dir); err != nil {
			return err
		}
	}
	if err := Seed(dir, scenario, now); err != nil {
		return err
	}
	fmt.Printf("devseed: seeded %s (%s) at %s\n", dir, scenario, now.Format(time.RFC3339))
	return nil
}
