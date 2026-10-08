package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/claudeagent"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/clockcheck"
	"github.com/kivali-ai/kivali/internal/config"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web"
)

// version is stamped into the binary at build time via
// `-ldflags="-X main.version=$(VERSION)"`. Plain `go run` / `go build`
// without ldflags leaves it as "dev" — the expected indicator that
// you're not looking at a tagged release. Exposed at /healthz and in
// the boot log so operators can tell at a glance which build is
// running.
var version = "dev"

func main() {
	// Subcommand dispatch. `kivali mcp ...` → runMCP; otherwise the
	// main server. Backup and restore are the app's (Org, Backup and
	// restore; kivali-supervisor backup/restore call the same endpoints).
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		runMCP(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		runAgent(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "prepare-scratch" {
		runPrepareScratch(os.Args[2:])
		return
	}

	addrFlag := flag.String("addr", "", "address to listen on (overrides ADDR env)")
	flag.Parse()

	// Top-level shutdown ctx. Canceled by the SIGINT/SIGTERM handler
	// below; subsystems that hold long-lived state (e.g. claudeagent
	// pool) listen on this so SIGTERM cleanly drains warm CLIs.
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *addrFlag != "" {
		cfg.Addr = *addrFlag
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}
	// Agent pods share the CLI's sign-in under HOME, not this
	// environment, so a server environment that would sign the CLI in
	// is refused: one deployment bills one way.
	if err := claudeagent.CheckEnvironment(os.Getenv); err != nil {
		log.Fatalf("config: %v", err)
	}
	// DEV_MODE turns off authentication entirely. The two guards below
	// are what make that safe to ship: it cannot be combined with a
	// prod tag, and it cannot listen anywhere but loopback unless the
	// operator explicitly opts out. Both are fatal rather than a
	// warning — a silently-unauthenticated server is the exact failure
	// this mode invites.
	if cfg.DevMode {
		if cfg.Env == "prod" {
			log.Fatalf("config: DEV_MODE=true with KIVALI_ENV=prod — refusing to run an unauthenticated production server")
		}
		if !isLoopbackAddr(cfg.Addr) && !cfg.DevAllowNonLoopback {
			log.Fatalf("config: DEV_MODE=true but ADDR=%q is not loopback.\n"+
				"  DEV_MODE disables authentication, so it may only bind 127.0.0.1 / ::1 / localhost.\n"+
				"  Set ADDR=127.0.0.1:8080, or set DEV_MODE_ALLOW_NONLOOPBACK=true if you really\n"+
				"  intend to expose an unauthenticated server on every interface.", cfg.Addr)
		}
		if len(cfg.SessionKey) == 0 {
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				log.Fatalf("config: DEV_MODE ephemeral session key: %v", err)
			}
			cfg.SessionKey = key
		}
		log.Printf("kivali boot: DEV_MODE — AUTHENTICATION DISABLED, every request is %s", cfg.DevUser)
		if cfg.DevAllowNonLoopback {
			log.Printf("kivali boot: DEV_MODE_ALLOW_NONLOOPBACK — unauthenticated server bound to %s", cfg.Addr)
		}
	}

	s, err := store.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	// Usage recorder: the CLI reports token counts and a per-call
	// cost on its result event, and the recorder persists whatever
	// arrived.
	recorder := func(ev provider.UsageEvent) error {
		return s.AppendUsage(store.UsageRecord{
			TS:                ev.TS,
			Agent:             ev.Agent,
			Purpose:           ev.Purpose,
			Model:             ev.Model,
			InputTokens:       ev.InputTokens,
			OutputTokens:      ev.OutputTokens,
			CacheReadTokens:   ev.CacheReadTokens,
			CacheCreateTokens: ev.CacheCreateTokens,
			CostUSD:           ev.CostUSD,
		})
	}

	// The model driver: the one value that is both the Client every
	// model call goes through and the Provider every question about
	// models and credentials goes to. The Claude Code CLI authenticates
	// itself with whatever its sign-in set up under $HOME; agent pods
	// mount the same HOME, so one deployment bills one way everywhere.
	driver := claudeagent.New(claudeagent.Options{
		Recorder:    recorder,
		DataDir:     cfg.DataDir,
		Sessions:    s,
		ShutdownCtx: shutdownCtx,
	})
	creds := driver.Credentials()
	if err := cfg.ResolveModels(driver); err != nil {
		log.Fatalf("%v", err)
	}
	// Materialise the skills that ship with the binary before anything
	// reads the skill library. Fatal rather than logged: a deployment
	// that cannot write its own built-in skills has a broken data
	// volume, and finding that out now beats finding it out when an
	// agent reaches for a skill that should be there.
	if err := s.InstallBuiltinSkills(); err != nil {
		log.Fatalf("install builtin skills: %v", err)
	}
	// Model pins follow their lineage. An agent pinned to a model the
	// catalog has since retired moves to the newest model of the same
	// family — Opus 4.8 to Opus 5.5, never Opus to Fable. Every boot,
	// not one-shot: a pin is only stale relative to the catalog this
	// binary ships, so the pass re-runs with each release and is a
	// no-op when nothing has been retired. The restore handler runs
	// the same pass on uploaded data. See Provider.Current.
	moved, err := s.UpgradeModelPins(driver.Current)
	for _, m := range moved {
		log.Printf("model pin: %s %s → %s", m.Slug, m.From, m.To)
	}
	if err != nil {
		log.Printf("upgrade model pins: %v", err)
	}
	// Make sure HOME exists for the claude CLI. The image sets HOME
	// to a PVC-backed path so the CLI's sign-in persists, but
	// the directory itself is created on first boot (the PVC starts
	// empty).
	if home := os.Getenv("HOME"); home != "" {
		if err := os.MkdirAll(home, 0o700); err != nil {
			log.Printf("mkdir HOME %s: %v", home, err)
		}
	}
	if cfg.EgressAllowlistSyncPath != "" {
		s.SetEgressSyncPath(cfg.EgressAllowlistSyncPath)
		if err := s.SyncEgressAllowlistToProxy(); err != nil {
			log.Printf("egress: initial sync: %v", err)
		} else {
			log.Printf("egress: synced allowlist to %s", cfg.EgressAllowlistSyncPath)
		}
	}

	codec, err := auth.NewCodec(cfg.SessionKey)
	if err != nil {
		log.Fatalf("session codec: %v", err)
	}
	allowlist := auth.NewAllowlist(cfg.OwnerEmails...)

	// Under DEV_MODE the middleware is left nil and web.Server.DevUser
	// takes over; see internal/auth.DevBypass.
	var mw *auth.Middleware
	var handoff *auth.Handoff
	if !cfg.DevMode {
		mw = &auth.Middleware{
			Codec:        codec,
			Allowlist:    allowlist,
			LoginPath:    "/login",
			CookieSuffix: cfg.CookieSuffix,
		}
		// The desktop app opens a team it just set up signed in
		// (docs/developers/auth.md, Desktop handoff).
		handoff = &auth.Handoff{Codec: codec, Allowlist: allowlist, CookieSuffix: cfg.CookieSuffix}
	}

	// Sign-in: Google, through either this deployment's own OAuth
	// client (id + secret + registered redirect) or the public client
	// Kivali ships with, whose secret lives in the relay. Validate has
	// already refused the half-configured shapes.
	var oauth *auth.GoogleOAuth
	if cfg.GoogleClientID != "" && (cfg.GoogleClientSec != "" || cfg.OAuthRelayURL != "") {
		oauth = &auth.GoogleOAuth{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSec,
			RedirectURL:  cfg.OAuthRedirectURL,
			RelayURL:     cfg.OAuthRelayURL,
			ExternalURL:  cfg.ExternalURL,
			AuthURL:      cfg.OAuthAuthURL,
			TokenURL:     cfg.OAuthTokenURL,
			UserInfoURL:  cfg.OAuthUserInfoURL,
			JWKSURL:      cfg.OAuthJWKSURL,
			Issuer:       cfg.OAuthIssuer,
			Codec:        codec,
			Allowlist:    allowlist,
			CookieSuffix: cfg.CookieSuffix,
		}
		if cfg.GoogleClientSec == "" {
			log.Printf("sign-in: Google, public client %s through the relay at %s", cfg.GoogleClientID, cfg.OAuthRelayURL)
		} else {
			log.Printf("sign-in: Google, this deployment's own client %s", cfg.GoogleClientID)
		}
	}

	// Environment + billing banner. Printed at WARN volume so it's
	// obvious in the pod logs which cluster this is and what every
	// model call bills — the same rollout command against prod vs dev
	// should never be silent about which it hit.
	credential := "not signed in"
	if st, err := creds.Status(shutdownCtx); err != nil {
		credential = "unknown"
		log.Printf("credential status: %v", err)
	} else if st.Present {
		credential = st.Billing
		if credential == "" {
			credential = "signed in"
		}
	}
	log.Printf("kivali boot: version=%s env=%s provider=%s credential=%q", version, cfg.Env, driver.Name(), credential)
	if cfg.Env == "prod" {
		log.Printf("kivali boot: PRODUCTION MODE")
	}

	log.Printf("claude driver: cli (one warm CLI per active agent; empty-slug requests take the per-call path)")

	// Per-agent execution coordinator. Holds the running-agents tracker
	// and the per-call defaults (model + token caps). Tool dispatch
	// runs inside the agent pod; Kivali web only builds the chat-turn
	// request and tracks who's currently mid-turn.
	rt := agent.NewRuntime(s, agent.RuntimeDefaults{
		AgentModel:   cfg.AgentModel,
		SummaryModel: cfg.SummaryModel,
		// Per-response output cap. Sized near the model ceiling
		// so agents can one-shot large artifacts (agent roles,
		// full spec drafts, long status updates) without chunking.
		MaxTokens:     32768,
		MaxTurnTokens: 500000,
	})

	// Per-agent Pod + PVC provisioner. ApplyHire fires Provision so
	// every fresh hire gets its agent pod in the same flow that
	// creates its on-disk record; an approved offboard fires the
	// matching Destroy. The provisioner is a no-op out-of-cluster
	// (developer laptop / dev cluster without ServiceAccount).
	apCfg := agentpod.DefaultsForNamespace(cfg.AgentpodNamespace)
	apCfg.Image = cfg.AgentpodImage
	apCfg.ProxyURL = cfg.EgressProxyURL
	// The provider names the home directory the agent pod mounts: the
	// CLI's HOME, with the sign-in the CLI wrote there.
	apCfg.UseCredentials(creds)
	if cfg.AgentpodUDSDir != "" {
		apCfg.UDSDir = cfg.AgentpodUDSDir
	}
	// Per-slug subPath for the unified /files/ mount.
	apCfg.FilesSubPath = func(slug string) string {
		return path.Join("agents", slug, files.StoragePrefix)
	}
	aprov, err := agentpod.NewProvisioner(apCfg, agentpod.WithProvisionerLogger(log.Printf))
	if err != nil {
		log.Fatalf("agentpod provisioner: %v", err)
	}
	if aprov.Enabled() {
		log.Printf("agentpod: enabled (namespace=%s image=%s uds_dir=%s)", apCfg.Namespace, apCfg.Image, apCfg.UDSDir)
	} else {
		log.Printf("agentpod: disabled (not running in-cluster)")
	}
	if rt != nil {
		rt.AgentPod = aprov
	}

	// Routing + delivery primitives. Holds the MessageQueue mutex
	// shared with chat-path Route calls and the release-all batch.
	var msgr *messaging.Messenger
	if rt != nil {
		msgr = messaging.New(s, rt)
	}

	// Clock-drift watcher: probes a public time reference at boot
	// and every 6h, surfacing drift on /healthz and (when |drift| ≥ 1h)
	// as a yellow UI banner. The pod's UTC clock determines every
	// message's date bucket (messages/<YYYY-MM-DD>/), so a skewed
	// clock silently corrupts the audit trail. Logging-only by design:
	// transient network failure shouldn't lock pod startup. Uses
	// context.Background() — the goroutine lives the process lifetime
	// and exits with the process; no separate cancellation needed.
	clockWatcher := clockcheck.NewWatcher(clockcheck.Config{
		Logf: log.Printf,
	})
	go clockWatcher.Run(context.Background())

	// Subagent execution drives `claude -p` inside the parent's agent
	// pod via the agent-pod chat-turn event. Driver is late-bound to
	// the Server inside web.NewServer (it implements SubagentDriver),
	// so we just wire the store + the eventual EmitToParent here.
	subagentService := &web.SubagentService{
		Store: s,
	}

	// Agent-pod events hub. Lightweight (an in-memory map of slug →
	// subscribers); always wired so the events SSE endpoint replies
	// 503 only on a literal misconfiguration rather than on a
	// "feature not enabled" condition. The dispatch flip in
	// spawnChatLoopIfIdle gates on SubscriberCount, so a hub with no
	// connected runtimes harmlessly falls through to the in-process
	// path.
	apHub := web.NewAgentpodHub()

	srv, err := web.NewServer(&web.Server{
		Store:           s,
		Claude:          driver,
		Provider:        driver,
		Runtime:         rt,
		Messenger:       msgr,
		AgentPod:        aprov,
		AgentpodHub:     apHub,
		AuthMW:          mw,
		DevUser:         devUser(cfg),
		OAuth:           oauth,
		Handoff:         handoff,
		Env:             cfg.Env,
		AgentModel:      cfg.AgentModel,
		SummaryModel:    cfg.SummaryModel,
		VersionName:     version,
		ClockCheck:      clockWatcher,
		SubagentService: subagentService,
	})
	if err != nil {
		log.Fatalf("web: %v", err)
	}
	// The name and kind the desktop app's setup already asked for. A
	// name refused here (too long) is left for setup to ask again.
	if err := srv.SeedBranding(cfg.SeedOrgName, cfg.TeamKind); err != nil {
		log.Printf("branding: %v", err)
	}
	// What the person asked agents to call them (KIVALI_OWNER_NAME).
	if err := srv.SeedOwnerName(cfg.OwnerName); err != nil {
		log.Printf("owner name: %v", err)
	}

	// Sidebar liveness: every time an agent enters or leaves a run
	// loop, push a fresh snapshot to /org/stream subscribers so the
	// working dot in the sidebar lights up the instant Release-all
	// kicks off. NotifyOrgState is cheap (no-ops when the resulting
	// bytes are unchanged).
	if rt != nil {
		rt.SetOnRunningChange(srv.NotifyOrgState)
	}

	// Crash recovery: scan for agents whose previous chat turn was
	// killed mid-flight (Kivali OOM / pod restart / kubelet eviction
	// — any case where the post-loop defer didn't get to clear the
	// active-turn marker). For each, write a kind:runtime-disruption
	// chat entry. Auto-spawn is deferred until AFTER bulk agent-pod
	// provisioning (below) so a recovered agent's first chat-turn
	// event lands on a subscribed runtime, not a still-coming-up pod.
	recovered := srv.RecoverInterruptedTurns()

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Control socket: separate Unix-socket listener for inbound calls
	// from the MCP-subprocess fleet (publish_* tool calls land here
	// instead of being routed in the subprocess, so all message queue
	// writes happen in this process — no cross-process race).
	controlPath := web.ControlSocketPath(cfg.DataDir)
	stopControl, err := srv.StartControlSocket(controlPath)
	if err != nil {
		log.Fatalf("control socket: %v", err)
	}
	log.Printf("control socket listening on %s", controlPath)

	// Agent-pod hostPath socket: the per-agent runtime dials this to
	// reach core. Distinct file from the PVC control socket because
	// the hostPath dir is the only path visible across pod boundaries
	// on the same node — agent pods can't reach the Kivali web pod's
	// PVC. Both sockets serve identical handlers; splitting them
	// reflects the topological reality, not a security boundary.
	stopAgentpodSocket := func() {}
	if cfg.AgentpodUDSDir != "" {
		if err := os.MkdirAll(cfg.AgentpodUDSDir, 0o700); err != nil {
			log.Fatalf("agentpod uds dir: %v", err)
		}
		agentpodSocketPath := filepath.Join(cfg.AgentpodUDSDir, agentpod.SocketName)
		stopAgentpodSocket, err = srv.StartAgentpodSocket(agentpodSocketPath)
		if err != nil {
			log.Fatalf("agentpod socket: %v", err)
		}
		log.Printf("agentpod socket listening on %s", agentpodSocketPath)
	}

	// Janitor for the two-phase publish flow's staged-publish records.
	// Records older than the configured max-age (1h) get reaped on
	// each sweep tick; this is the safety net for stage requests
	// that never see a follow-up commit (bridge crash, process exit).
	stopStagedPublishJanitor := srv.StartStagedPublishJanitor()

	// Auto-release: the inbox slider's scheduler. Releases queued
	// messages whose delay has run out; a no-op while the slider is
	// off. Started after the socket so a release can wake a pod.
	stopAutoRelease := srv.StartAutoRelease()

	idle := make(chan struct{})
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		// Order matters: drain HTTP first so in-flight handlers
		// (which hold open Streams from warm CLIs) get to finish.
		// THEN cancel the shutdown ctx so the claudeagent registry
		// closes its warm runners. Doing it the other way around
		// races: we'd kill the runners while their Streams are
		// still being drained by handlers, cutting off responses
		// mid-message.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
		shutdownCancel()
		stopControl()
		stopAgentpodSocket()
		stopStagedPublishJanitor()
		stopAutoRelease()
		if err := s.Graph().Flush(); err != nil {
			log.Printf("shutdown: graph index: %v", err)
		}
		close(idle)
	}()

	// Diagnostic: log memory stats periodically so we can catch
	// runaway growth. ReadMemStats does a STW pause; the Kivali pod
	// stays alive for weeks at a time, so a 5s tick (the prior
	// interval) was 17,000 STWs/day for purely diagnostic value.
	// 60s keeps the visibility without the constant pause cost.
	// Shutdown-aware so the ticker is Stop()'d cleanly on SIGTERM
	// — leaks a goroutine + timer for the process lifetime
	// otherwise. Override the interval via KIVALI_MEMSTATS_INTERVAL
	// (Go duration string) for debugging hot growth.
	memstatsInterval := 60 * time.Second
	if v := os.Getenv("KIVALI_MEMSTATS_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			memstatsInterval = d
		}
	}
	go func() {
		var m runtime.MemStats
		t := time.NewTicker(memstatsInterval)
		defer t.Stop()
		for {
			select {
			case <-shutdownCtx.Done():
				return
			case <-t.C:
				runtime.ReadMemStats(&m)
				log.Printf("memstats alloc=%dMB sys=%dMB heap_obj=%d goroutines=%d",
					m.Alloc>>20, m.Sys>>20, m.HeapObjects, runtime.NumGoroutine())
			}
		}
	}()

	// Hydrate every active agent's /files/ view so the file_* tools
	// have project/ and past-chats/ populated on first use. Before the
	// pods below: they mount subPaths of these trees, and kubelet makes
	// a missing one as root (BulkProvisionAgentPods syncs each agent
	// again right before its pod, for any this pass skipped on an
	// error).
	if err := s.SyncAllFilesystems(); err != nil {
		log.Printf("files: initial sync: %v", err)
	}

	// The knowledge-graph maintainer: it loads the index on disk now,
	// so the graph tools and wake notes answer from it at once, and
	// runs its first scan in the background, then one every
	// store.GraphScanInterval. When the snapshot store is empty that
	// first scan gives every published file and project file a node and
	// a first version. Off the boot path,
	// because on a large install it copies every published byte into
	// the snapshot store once, and the liveness probe would kill a
	// process that had not started listening yet. Before the pods and
	// the recovered agents' wakes below, so a wake's graph read finds
	// the maintainer loaded and never scans on its own turn.
	s.Graph().Start(shutdownCtx, clock.System{})

	// Bulk-provision agent pods for already-active agents, then
	// re-spawn any agents that crashed mid-turn last time (recovered
	// slugs returned by RecoverInterruptedTurns above). Sequenced so
	// a recovered agent's first chat-turn event lands on a subscribed
	// runtime, not a still-coming-up pod.
	go func() {
		if aprov.Enabled() {
			// Bulk provision runs goroutines in parallel; the timeout
			// bounds the WHOLE wave. With drift-recreates dominated by
			// terminationGracePeriodSeconds (~10s) plus image-pull /
			// scheduler hesitation (~30-60s on a cold node), 5 min
			// covers a realistic full-fleet recreate with headroom.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			srv.BulkProvisionAgentPods(ctx)
		}
		// Recovered agents wake here. SpawnRecovered honors the
		// spawn gate and the circuit breaker; quarantined agents
		// stay quiet.
		srv.SpawnRecovered(recovered)
	}()

	// Assignment tracker: name a corrupt assignment file now rather than on the
	// first tool call, and route any wake a crash left unrouted. Cheap
	// enough to stay on the boot path: a few hundred small files.
	if err := srv.ReconcileAssignments(context.Background()); err != nil {
		log.Printf("assignments: boot reconcile: %v", err)
	}

	log.Printf("kivali listening on %s (data=%s env=%s)", cfg.Addr, cfg.DataDir, cfg.Env)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
	<-idle
}

// devUser returns the email every request is attributed to under
// DEV_MODE, or "" when dev mode is off. Kept as a function so the
// zero value can never leak into a configured deployment: web.Server
// only consults DevUser when AuthMW is nil.
func devUser(cfg config.Config) string {
	if !cfg.DevMode {
		return ""
	}
	return cfg.DevUser
}

// isLoopbackAddr reports whether a listen address binds only the
// loopback interface. A bare port (":8080") or an empty host means
// "every interface", which is explicitly NOT loopback — that's the
// case the DEV_MODE guard exists to catch. Hostnames other than
// "localhost" are treated as non-loopback: resolving them here would
// make the guard depend on DNS, and the conservative answer is the
// safe one.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Not host:port — could be a bare port or something odd.
		// Either way we can't prove it's loopback.
		return false
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
