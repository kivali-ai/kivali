package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/claudeagent"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/provider"
)

// runAgent is the `kivali agent` subcommand. Long-lived process that
// runs inside an agent Pod and talks to Kivali central via a single
// outbound HTTP-over-UDS connection.
//
// Lifecycle:
//
//   - Boot: dial core's events SSE stream, register via slug.
//   - Idle: hold the stream open; reconnect with backoff on drop.
//   - Chat-turn event arrives: drive the turn against an embedded
//     claudeagent.Client (CLI subprocess + kivali mcp). Translate
//     stream events to TurnEvents and POST back to core.
//   - SIGTERM/SIGINT: cancel the root ctx; the supervised CLI
//     subprocess gets cleanly drained; process exits within the
//     pod's terminationGracePeriodSeconds.
//
// The runtime owns no authoritative state. Crash → recreate →
// rebuild any cache from core on the next chat turn. See
// docs/developers/architecture.md.
func runAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	slug := fs.String("slug", "", "agent slug this runtime is bound to (required)")
	socket := fs.String("uds", "", "absolute path to core.sock (required)")
	scratch := fs.String("scratch", "/scratch", "per-agent scratch volume (session files, read cache, subagent run dirs)")
	skipClaude := fs.Bool("skip-claude", false, "skip wiring the claude CLI client (dev/test: agent runtime accepts events but reports failed for chat-turn)")
	_ = fs.Parse(args)

	logger := log.New(os.Stderr, "agent: ", log.LstdFlags|log.Lmicroseconds)

	if *slug == "" {
		logger.Fatal("--slug is required")
	}
	if *socket == "" {
		logger.Fatal("--uds is required")
	}

	// SIGTERM/SIGINT cancel the root ctx → Runtime.Run returns →
	// process exits cleanly within terminationGracePeriodSeconds.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logger.Printf("starting: slug=%s uds=%s scratch=%s", *slug, *socket, *scratch)

	udsClient := agentpod.NewClient(*socket, *slug)

	// Cache lives under <scratch>/cache/. Per docs/developers/architecture.md
	// §"Cache strategy": content-addressed entries (attachments,
	// project files) are immutable per SHA and cached forever;
	// path-addressed entries (role.md, agent_memory.md, skill files)
	// ride on ETag/If-None-Match. Truth lives on core; the cache is
	// purely an optimization and is safe to wipe at any moment.
	cacheDir := filepath.Join(*scratch, "cache")
	cache, cacheErr := agentpod.NewCache(cacheDir)
	if cacheErr != nil {
		logger.Printf("cache disabled: %v (continuing — every read goes direct to core)", cacheErr)
	} else {
		udsClient.WithCache(cache)
		logger.Printf("cache enabled: %s", cacheDir)
	}

	exe, err := os.Executable()
	if err != nil {
		logger.Fatalf("os.Executable: %v", err)
	}

	// The egress proxy credentials: two secrets drawn here and held in
	// memory, their hashes registered with core before any CLI starts
	// and again each time the events stream reconnects. The CLIs get
	// the first in their proxy URL (driver Options.Env) and reach the
	// public internet. The second is the shell's: it is written beside
	// the agent-shell socket (AGENT_SHELL_SOCKET) for the daemon to put
	// in each command's proxy URL, and grants only the team's list and
	// this agent's own hosts. Scrubbed of both: the body and query of
	// every request udsClient sends core, and each CLI's stderr and
	// stdin mirror as the driver writes them (to disk and to this
	// process's stderr). The `kivali mcp` servers the CLIs start
	// inherit the first and scrub their own requests (mcp_cmd.go). No
	// HTTPS_PROXY in this container means no proxy: nothing to
	// register. Why open egress is safe for this container's processes:
	// see SetUpEgressCredential.
	cred, err := agentpod.SetUpEgressCredential(ctx, udsClient, os.Getenv("HTTPS_PROXY"), os.Getenv("AGENT_SHELL_SOCKET"))
	if err != nil {
		if ctx.Err() != nil {
			logger.Print("shutdown before the egress credential was registered")
			return
		}
		logger.Fatalf("egress credential: %v", err)
	}
	var cliEnv []string
	var scrubber *egresscred.Scrubber
	if cred != nil {
		cliEnv, scrubber = cred.Env(), cred.Scrubber
		logger.Print("egress credential registered")
	}

	// The driver is chosen here and nowhere else: the runtime below talks
	// to whatever provider.Client it is handed, for parent turns and
	// subagent runs alike, asks the same value (as provider.Provider)
	// for its defaults, and never names a provider binary itself. Under
	// --skip-claude (tests / dev) the runtime gets no Client, but still
	// the provider.
	sessionDir := filepath.Join(*scratch, ".kivali-agent-sessions")
	// The agent container mounts no store (of the Kivali PVC, only
	// /data/claude-home, the CLI's HOME) and no /files — every
	// authoritative read/write has to round-trip to core over UDS, and
	// file_* / run_shell go to the agent-shell sidecar. Setting CoreUDS
	// routes the spawned `kivali mcp` subprocess through agentpod.Client
	// instead of opening a local FSStore (which would fail at startup).
	driver := claudeagent.New(claudeagent.Options{
		ClaudeBinary: "claude",
		KivaliBinary: exe,
		CoreUDS:      *socket,
		SessionDir:   sessionDir,
		Sessions:     udsClient.SessionStore(),
		HomeDir:      os.Getenv("HOME"),
		Env:          cliEnv,
		Scrubber:     scrubber,
		ShutdownCtx:  ctx,
	})

	var claudeClient provider.Client
	if !*skipClaude {
		if err := os.MkdirAll(sessionDir, 0o755); err != nil {
			logger.Fatalf("mkdir session dir: %v", err)
		}
		claudeClient = driver
	}

	rt := &agentpod.Runtime{
		Client:   udsClient,
		Claude:   claudeClient,
		Provider: driver,
		Logger:   logger,

		// Subagent configuration: the MCP server a subagent run loads.
		// Under --skip-claude Claude is nil and the runtime reports
		// failed for any Kind=subagent it receives.
		KivaliBinary: exe,
		CoreUDS:      *socket,
		ScratchRoot:  *scratch,
	}
	if err := rt.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Printf("Run exited: %v", err)
	}
	logger.Print("shutdown")
}
