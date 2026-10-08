//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/kivali-ai/kivali/internal/supervisor/integrity"
)

// integrityMain is `kivali-guest integrity run|verify`, the data disk's
// crash-consistency oracle (internal/supervisor/integrity), which
// vm/boottest -integrity-cycles runs through the agent's exec. It is a
// test tool: it writes only under -dir and never runs on its own.
//
//	integrity run -dir D -from N [-writes K] [-corpus-mib M] [-workers W]
//	                [-check-every C] [-trim-every T]
//	integrity verify -dir D -acked N
//
// run prints "ACK <n>" per durable write; both exit 2 on corruption and
// 1 on any other failure.
func integrityMain(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: kivali-guest integrity run|verify [flags]")
		return 1
	}
	fl := flag.NewFlagSet("integrity "+args[0], flag.ContinueOnError)
	dir := fl.String("dir", "/var/lib/kivali/integrity", "the oracle's directory, on the data disk")
	from := fl.Uint64("from", 1, "run: the first write's number (the last acknowledged + 1)")
	writes := fl.Uint64("writes", 0, "run: how many writes (0: until stopped)")
	corpus := fl.Int("corpus-mib", 256, "run: the corpus's size in MiB")
	workers := fl.Int("workers", 4, "run: churn workers")
	checkEvery := fl.Uint64("check-every", 500, "run: writes between full verifications from the disk")
	trimEvery := fl.Uint64("trim-every", 2000, "run: writes between fstrims of the data disk (0: none)")
	acked := fl.Uint64("acked", 0, "verify: the highest acknowledged write")
	if err := fl.Parse(args[1:]); err != nil {
		return 1
	}
	exit := func(err error) int {
		switch {
		case err == nil:
			return 0
		case errors.Is(err, integrity.ErrCorrupt):
			fmt.Printf("CORRUPT: %v\n", err)
			return 2
		default:
			fmt.Fprintf(os.Stderr, "integrity: %v\n", err)
			return 1
		}
	}
	switch args[0] {
	case "verify":
		r, err := integrity.Verify(*dir, *acked)
		if err == nil {
			fmt.Printf("OK: %s\n", r)
		}
		return exit(err)
	case "run":
		stop := make(chan struct{})
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		go func() { <-sig; close(stop) }()
		return exit(integrity.Run(integrity.Config{
			Dir: *dir, From: *from, Writes: *writes, CorpusMiB: *corpus, Workers: *workers,
			CheckEvery: *checkEvery, TrimEvery: *trimEvery, Out: os.Stdout, Stop: stop,
			DropCaches: dropCaches,
			Trim: func() error {
				out, err := exec.Command("fstrim", "-v", "/var/lib/kivali").CombinedOutput()
				if err != nil {
					return fmt.Errorf("fstrim: %v: %s", err, out)
				}
				return nil
			},
		}))
	}
	fmt.Fprintf(os.Stderr, "integrity: unknown command %q (run or verify)\n", args[0])
	return 1
}

// dropCaches writes back dirty pages and drops the clean ones, so the
// next reads come from the disk, not the guest's memory.
func dropCaches() error {
	syscall.Sync()
	return os.WriteFile("/proc/sys/vm/drop_caches", []byte("3\n"), 0)
}
