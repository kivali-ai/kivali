package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/clock"
)

// runBrowser is the `kivali browser` subcommand: the process in a
// browser pod. It dials core over core.sock, runs Chromium for one
// web session, and exits when core has the saved profile or closes
// the session. It exits 0 after a clean save or close and 1 otherwise.
func runBrowser(args []string) {
	fs := flag.NewFlagSet("browser", flag.ExitOnError)
	slug := fs.String("slug", os.Getenv(browserpod.EnvSlug), "agent slug the browser is for")
	session := fs.String("session", os.Getenv(browserpod.EnvSession), "web session id")
	token := fs.String("token", os.Getenv(browserpod.EnvToken), "per-pod token core expects on connect")
	coreUDS := fs.String("core-uds", os.Getenv(browserpod.EnvCoreUDS), "absolute path to core.sock")
	proxy := fs.String("proxy", os.Getenv(browserpod.EnvProxy), "proxy Chromium uses (--proxy-server), when set")
	chromium := fs.String("chromium", "chromium", "Chromium binary, looked up on PATH")
	dir := fs.String("dir", "/tmp/kivali-browser", "working directory for the profile, downloads and task files")
	_ = fs.Parse(args)

	logger := log.New(os.Stderr, "browser: ", log.LstdFlags|log.Lmicroseconds)
	for name, v := range map[string]string{"--slug": *slug, "--session": *session, "--token": *token, "--core-uds": *coreUDS} {
		if v == "" {
			logger.Fatalf("%s is required", name)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Printf("starting: slug=%s session=%s uds=%s dir=%s", *slug, *session, *coreUDS, *dir)
	rt := &browserpod.Runtime{
		Slug:     *slug,
		Session:  *session,
		Token:    *token,
		CoreUDS:  *coreUDS,
		Proxy:    *proxy,
		Chromium: *chromium,
		Dir:      *dir,
		Logger:   logger,
		Clock:    clock.New(),
	}
	if err := rt.Run(ctx); err != nil {
		logger.Printf("stopped: %v", err)
		cancel()
		os.Exit(1)
	}
	logger.Printf("done")
}
