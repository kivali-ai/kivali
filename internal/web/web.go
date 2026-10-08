// Package web is the HTTP surface of Kivali.
//
// Structure:
//   - NewServer wires auth middleware, the web app, the JSON API, the
//     SSE streams and the agent-pod surface into a single
//     http.Handler (see wireRoutes).
//   - The web app is the Vite build embedded by internal/web/ui and
//     served at the site root; it reads the JSON API under /api/v1/
//     (api.go) and the SSE streams.
package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/clockcheck"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/tracker"
	"github.com/kivali-ai/kivali/internal/web/ui"
)

// AgentPodLifecycle is the narrow Provisioner subset the hire and
// offboard approvals use. Lives behind an interface so tests can
// inject a fake; production wiring passes *agentpod.Provisioner which
// satisfies it implicitly. Provision spins up the per-agent Pod +
// PVC; Destroy removes the Pod and intentionally retains the PVC so
// a departed agent's /scratch survives for forensics.
type AgentPodLifecycle interface {
	Provision(ctx context.Context, slug string) error
	Destroy(ctx context.Context, slug string) error
}

// Server wires all HTTP handlers. Get the ready-to-serve handler with
// Server.Handler().
type Server struct {
	Store  *store.FSStore
	Claude provider.Client // nil → chat endpoints return 503
	// Provider answers what the server needs to know about models and
	// credentials: labels, prices, efforts, defaults, sign-in status. In
	// production it is the same driver as Claude; tests pass
	// provider.MockProvider. Required: NewServer refuses a nil one.
	Provider  provider.Provider
	Runtime   *agent.Runtime       // per-agent execution; chat path drives Claude through it
	Messenger *messaging.Messenger // routing + delivery primitives
	// AgentPod is the per-agent Pod + PVC lifecycle manager. Both calls
	// hang off a CEO approval: ApplyHire fires Provision, and an
	// approved offboard fires Destroy (see respondAsCEO). Wired in
	// main.go via agentpod.Provisioner — interface here so tests inject
	// a fake. nil leaves both calls no-ops (tests that don't exercise
	// hire/offboard).
	AgentPod AgentPodLifecycle
	AuthMW   *auth.Middleware
	OAuth    *auth.GoogleOAuth
	Handoff  *auth.Handoff // GET /auth/handoff (docs/developers/auth.md); nil leaves it unregistered
	// DevUser, when non-empty, replaces the auth middleware with an
	// unconditional bypass attributing every request to this email.
	// Set only by main.go under DEV_MODE; ignored when AuthMW is set,
	// so a misconfiguration can't silently disable a real allowlist.
	DevUser string
	// Env is the deployment environment tag ("dev" | "prod"), reported
	// by /admin/version.
	Env        string
	AgentModel string // model to use for direct chat; NewServer fills the provider's default when empty
	// SummaryModel runs the summaries Kivali makes on its own behalf
	// (project files, the inbox line, episodes); SUMMARY_MODEL.
	// NewServer fills the provider's default when empty.
	SummaryModel string
	MaxTokens    int // per-request max (defaults to 4096)
	VersionName  string

	// SubagentService runs subagent batches in this process. Wired in
	// main.go alongside the claude client. Nil disables the subagent
	// tool — main.go always wires it today, but tests omit it.
	SubagentService *SubagentService

	// Tracker is the assignment tracker bound to this server's Store,
	// Messenger and delivery hooks. Built on first use by tracker()
	// because Messenger is set after NewServer in every test fixture;
	// nil until then and nil for good when there is no Messenger.
	Tracker     *tracker.Service
	trackerOnce sync.Once

	// AgentpodHub fans agentpod events (chat-turn wake-ups,
	// cache-invalidations, etc.) out to per-agent runtime SSE
	// subscribers. nil ⇒ /v1/agent/{slug}/events returns 503; safe
	// for tests that don't exercise the agentpod path. Production
	// always wires this via web.NewAgentpodHub.
	AgentpodHub *agentpodHub

	// Clock is the time source for every timer and ticker this package
	// arms: SSE heartbeats, the staged-publish sweep, the subagent
	// watch poll, the cancel grace. Defaulted in NewServer; tests
	// inject a clock.Fake so those intervals are driven rather than
	// waited out.
	Clock clock.Clock

	// SeedTimeout bounds one Chief of Staff hire, measured on Clock; a
	// hire still running when it passes ends as failed. Zero means
	// defaultSeedTimeout.
	SeedTimeout time.Duration

	// ClockCheck reports the latest drift between the host clock and a
	// public time reference. Surfaced on /healthz (the watcher logs a
	// large drift itself). nil ⇒ no clock check is running (tests, dev
	// without the watcher); /healthz reports drift_seconds=0 with a
	// "no check yet" age.
	ClockCheck *clockcheck.Watcher

	mux *http.ServeMux
	// protectedMux is the session-gated half of the route table (see
	// wireRoutes), kept so routes_test.go can ask which pattern a
	// request resolves to behind the middleware.
	protectedMux *http.ServeMux

	// chatHubs tracks per-agent in-flight direct-chat streams, keyed
	// by agent slug. When the user refreshes (or a second tab opens
	// the same chat), the new SSE connection subscribes to the
	// existing hub — replays buffered events, then streams live
	// ones — instead of starting a second parallel Claude call. The
	// originator goroutine runs detached so client disconnect
	// doesn't kill the in-flight work.
	//
	// Release-all is fire-and-forget — it just calls
	// Messenger.ReleaseAll which drains the pending queue, bumps
	// the counter, and wakes every active agent (re-using the
	// chatHubs path). No dedicated release-side hub.
	streamMu sync.Mutex
	chatHubs map[string]*chatHub

	// orgHub is the singleton broadcaster for org liveness:
	// per-agent working state, stuck flag, and context-fill bucket,
	// plus aggregate inbox + release counts. Subscribed via /org/stream
	// (SSE). Updated via Server.NotifyOrgState() from every code path
	// that mutates one of those facts (chat-hub open/close, release
	// engine agent start/stop, inbox/release-state writes), so clients
	// see state changes within milliseconds, without polling.
	orgHub *orgHub
	// notifyCh debounces NotifyOrgState calls. A buffered-1 channel
	// coalesces bursts (Release-all spawns N×2 markRunning notifies
	// in quick succession); a background goroutine drains, sleeps
	// for orgNotifyDebounce, then rebuilds + publishes once. Without
	// this, each notify rebuilt the snapshot independently — wire
	// dedup catches the duplicates but CPU still walks every agent's
	// chat history N times.
	notifyCh chan struct{}

	// autoReleaseCh wakes runAutoRelease: every queue write and every
	// slider move. Buffered-1 so a burst coalesces into one pass over
	// the queue. Made in NewServer; NotifyAutoRelease is a no-op on a
	// Server literal that never had one.
	autoReleaseCh chan struct{}

	// episodeCh feeds runEpisodeWriter one archived generation at a
	// time. finalizeRotation sends; a full buffer drops and the
	// writer's retry sweep rescans for gaps. nil when Claude is
	// unconfigured (NotifyEpisode is then a no-op).
	episodeCh chan episodeRequest

	// pendingDeliveries buffers chat-received entries that arrive
	// while a loop is running for that agent (chat hub active OR
	// runtime currently running the agent). The buffer is
	// flushed atomically when the active loop completes, so file
	// order, UI order, and the next inference's req.Messages all
	// see the same strictly-appended sequence:
	//   prior task → tool flow → prior reply → buffered delivery.
	//
	// Without buffering, deliveries would either (a) interleave in
	// chat.jsonl and the model would read its prior reply as
	// already-addressing them, or (b) require a post-hoc chat.jsonl
	// rewrite which creates subtle UI/file mismatches. Buffering
	// keeps chat.jsonl strictly append-only, which is the cleanest
	// invariant.
	//
	// See docs/developers/context-serialization.md §3 (message entry paths)
	// and §4 (mid-flight delivery).
	pendingDeliveries map[string][]bufferedDelivery

	// pendingTombstones holds the pending messages the CEO deleted, by
	// slug then id, for pendingRestoreWindow so a restore can put one
	// back where it was. Guarded by streamMu with pendingDeliveries;
	// made on first use by tombstonesLocked, so the zero value is fine.
	// See chat_pending.go.
	pendingTombstones map[string]map[string]pendingTombstone

	// restoreBG counts what a restore leaves running once it answers:
	// provisioning the restored agents' pods, then resuming the ones a
	// backup caught mid-turn. Tests wait on it.
	restoreBG sync.WaitGroup

	// latestRotations stashes the data finalizeRotation needs
	// (prior-memory snapshot) between consumeRotationIfReady and the
	// actual archive. The pending-rotation marker itself lives on
	// disk (agents/<slug>/pending_rotation.json) so it survives a
	// Kivali crash mid-rotation; this in-memory entry is just a
	// short-lived hand-off between the consume call and the archive
	// call within the same goroutine.
	latestRotations map[string]latestRotation

	// agentpodTurns tracks per-slug active chat-turn state for the
	// agent-pod path: assembled assistant text, pendingTools, etc.
	// Populated by InitAgentpodTurnState (called from the chat-spawn
	// flip in workstream 4-3) and torn down on the done/failed
	// TurnEvent. Guarded by streamMu — same lock as chatHubs so the
	// per-slug invariants stay symmetric across paths.
	//
	// Subagent runs (Kind=subagent) keyed by turn-id instead of slug
	// — multiple subagent turns can be in flight per slug from one
	// parent batch, so per-slug single-flight is the wrong invariant.
	// Lookup checks both: the per-slug map first (chat turns), then
	// agentpodSubagentTurns (subagent turns) keyed by turn-id.
	agentpodTurns         map[string]*agentpodTurnState
	agentpodSubagentTurns map[string]*agentpodTurnState // turnID → state

	// unknownModelLogged tracks which model ids have already triggered
	// a "cost=0 fallback (unknown model)" warning so we log once per
	// id, not once per call. Set values are struct{}; presence is the
	// signal. Used by recordAgentpodUsage to surface a misconfigured /
	// new-rollout model id that would otherwise silently land $0 rows
	// in usage.jsonl. Lazy sync.Map so first-write doesn't need an
	// init step in NewServer.
	unknownModelLogged sync.Map

	// assignmentsVersion counts tracker writes in this process; the org
	// snapshot publishes it as assignments_version so the web app knows when
	// to refetch the work views. assignmentCache holds the assignment set read
	// under a given version. See org_goals.go.
	assignmentsVersion atomic.Int64

	// snapshotCache keeps the org snapshot's per-agent chat facts and
	// the CEO's open inbox items between rebuilds.
	snapshotCache   snapshotCache
	assignmentCache assignmentSetCache

	// usageVersion counts usage.jsonl appends; spendCache holds the
	// priced usage window read under a given version, for the
	// snapshot's spend readouts. See org_goals.go.
	usageVersion atomic.Int64
	spendCache   spendCache

	// usageRollupRows holds the priced usage rows of the last 31 days
	// read under a given usageVersion, for the Org page's usage section
	// and the settings dashboard. Zero value is an empty cache. See
	// usage_rollup.go.
	usageRollupRows usageRollupCache

	// seedTrack holds the latest Chief of Staff hire, form or API, so
	// progress can be read and a second hire refused while one runs.
	// Zero value is idle. See api_setup.go.
	seedTrack seedTracker
}

// NewServer constructs a Server and wires routes.
func NewServer(s *Server) (*Server, error) {
	if s.Provider == nil {
		return nil, errors.New("web: Server.Provider is required")
	}
	if s.AgentModel == "" {
		s.AgentModel = s.Provider.Defaults().Agent
	}
	if s.SummaryModel == "" {
		s.SummaryModel = s.Provider.Defaults().Summary
	}
	// Default the time source so no handler has to nil-check it.
	if s.Clock == nil {
		s.Clock = clock.New()
	}
	if s.SubagentService != nil && s.SubagentService.Clock == nil {
		s.SubagentService.Clock = s.Clock
	}
	if s.SubagentService != nil && s.SubagentService.Provider == nil {
		s.SubagentService.Provider = s.Provider
	}
	s.orgHub = newOrgHub()
	s.notifyCh = make(chan struct{}, 1)
	s.autoReleaseCh = make(chan struct{}, 1)
	s.episodeCh = make(chan episodeRequest, episodeQueueDepth)
	go s.runOrgNotifier()
	if s.Claude != nil {
		// Only spin up the episode writer when there's a backend to
		// call. Tests and local boots without a backend skip it, and
		// episodes/ stays unpopulated (NotifyEpisode checks for a
		// backend at call time, so nothing queues either).
		go s.runEpisodeWriter()
	}
	if s.SubagentService != nil && s.SubagentService.EmitToParent == nil {
		// Late-bind the hub emitter — SubagentService can't be
		// constructed knowing the Server's chatHubs map (chicken-and-egg
		// since Server holds SubagentService). Wiring here keeps the
		// initialization order one-way.
		s.SubagentService.EmitToParent = s.emitToHub
	}
	if s.SubagentService != nil && s.SubagentService.Driver == nil {
		// Same late-bind for the SubagentDriver so main.go doesn't
		// need to know the Server's agent-pod plumbing.
		s.SubagentService.Driver = s
	}
	if s.SubagentService != nil && s.SubagentService.DeliverToParent == nil {
		// A finished background job reaches its parent through the
		// SAME door as a released inbox message and a direct chat
		// post. That is what gets a result in front of a mid-turn
		// parent promptly — folded into the turn already running,
		// falling back to winding it up — without a second delivery
		// path to keep in sync.
		s.SubagentService.DeliverToParent = s.deliverToAgent
	}
	if s.SubagentService != nil && s.SubagentService.NotifyWorkingChanged == nil {
		s.SubagentService.NotifyWorkingChanged = s.NotifyOrgState
	}
	if s.SubagentService != nil && s.SubagentService.WakeParent == nil {
		// "subagent" as the spawn source: it names what triggered this
		// loop, matching the label subagent TURNS already carry. It is
		// deliberately not "release" — that value marks a CEO-paced
		// messaging event, and a finished background task is not one.
		s.SubagentService.WakeParent = func(parent string) {
			s.spawnChatLoopIfIdle(parent, "subagent")
		}
	}
	// Install the post-write hooks on the Store so EVERY in-process
	// message queue write and assignment write — web handlers, engine
	// internals, the agents' tools, future callers — automatically
	// pushes a fresh snapshot. The hooks themselves are non-blocking
	// pings; the heavy lifting happens on the notified goroutines with
	// debouncing.
	s.Store.SetOnMessageQueueWrite(s.onMessageQueueWrite)
	s.Store.SetOnAssignmentWrite(s.onAssignmentWrite)
	s.Store.SetOnUsageAppend(s.onUsageAppend)
	// A pod that never dials in stops counting as starting when its
	// grace runs out; push a snapshot then so screens say so.
	if s.AgentpodHub != nil {
		s.AgentpodHub.SetGraceExpiry(s.clk(), s.NotifyOrgState)
	}
	s.wireRoutes()
	return s, nil
}

// onMessageQueueWrite is the post-write hook for the message queue.
// Fires for every mutation (enqueue, dequeue, release-all drain,
// bounce). Both notifiers are non-blocking — the heavy work happens on
// their respective goroutines.
func (s *Server) onMessageQueueWrite() {
	s.NotifyOrgState()
	s.NotifyAutoRelease()
}

// onAssignmentWrite is the post-write hook for assignment files: the
// snapshot republishes, since the agents' tool path has no handler of
// its own to notify from. The version bump comes first so the snapshot
// the notify triggers carries it.
func (s *Server) onAssignmentWrite() {
	s.bumpAssignmentsVersion()
	s.NotifyOrgState()
}

// Handler returns the root http.Handler, gzipping text responses.
func (s *Server) Handler() http.Handler { return compressResponses(s.mux) }

// wireRoutes builds the route table. Two muxes:
//
//   - the public mux: health, sign-in, and the web app's static build
//     (the sign-in page loads it before there is a session);
//   - the protected mux, behind the session middleware: the JSON API,
//     the SSE streams, raw messages and attachments, the few form-free
//     endpoints the app and the agents still call, and, last, the web
//     app itself as the catch-all "GET /{path...}".
//
// The catch-all is what makes every client-router path (/, /team,
// /agents/{slug}, /assignments/{id} ...) load the app. Go's mux prefers the
// most specific pattern, so each server-owned route registered here
// (/agents/{slug}/stream, /messages/{path...}, /api/v1/...) wins over
// it; routes_test.go pins that for each one.
func (s *Server) wireRoutes() {
	mux := http.NewServeMux()
	spa := ui.ServeSPA()

	// The web app's build files. Public: the sign-in page is served
	// without a session and loads its script, fonts and logos from
	// here. Nothing in the build is secret; the data is all behind
	// the API. Still, only these directories answer here (a file or
	// 404, never index.html); the rest of the build is the app, behind
	// the session below.
	public := ui.ServePublic()
	for _, dir := range ui.PublicDirs {
		mux.Handle("GET /"+dir+"/", public)
	}
	// The browser tab icon is the Kivali mark from the build. An
	// operator-uploaded logo stays reachable under /branding/.
	mux.Handle("GET /favicon.ico", ui.ServeIcon())
	// The operator's uploaded logo, derived into favicon sizes
	// (public, like the icon: the sign-in page shows the org's mark).
	// The store validates the name against a fixed allowlist, so
	// traversal and unknown names 404.
	mux.HandleFunc("GET /branding/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.handleFaviconAsset(w, r, r.PathValue("name"))
	})

	// Health / readiness (unauthenticated).
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Sign-in (unauthenticated). /login is the web app's sign-in
	// screen, so it serves the app like any other route; the screen
	// reads the public GET /api/v1/login (api_setup.go).
	mux.Handle("GET /login", spa)
	mux.HandleFunc("GET /api/v1/login", s.handleAPILogin)
	if s.OAuth != nil {
		mux.HandleFunc("GET /auth/login", s.OAuth.LoginHandler)
		mux.HandleFunc("GET /auth/callback", s.OAuth.CallbackHandler)
	}
	// The desktop app's first open of a team it just set up
	// (docs/developers/auth.md, Desktop handoff).
	if s.Handoff != nil {
		mux.Handle("GET "+auth.HandoffPath, s.Handoff)
	}
	mux.HandleFunc("GET /auth/logout", s.handleLogout)
	mux.HandleFunc("GET "+auth.NotInvitedPath, auth.NotInvitedHandler)

	// Protected routes.
	protected := http.NewServeMux()
	// /org/stream is the unified org-liveness SSE feed: snapshot-first,
	// emitted on every per-agent or aggregate state change.
	protected.HandleFunc("GET /org/stream", s.handleOrgStream)
	// An agent's live chat events, and one background task's.
	protected.HandleFunc("GET /agents/{slug}/stream", s.handleAgentStream)
	protected.HandleFunc("GET /agents/{slug}/subagents/{id}/stream", s.handleSubagentStream)
	// Posting to an agent and stopping its turn. The API's
	// POST /api/v1/agents/{slug}/messages and /stop wrap these. Both
	// take a plain form, so they refuse another site's the way the API
	// does: a page on another loopback port is the same site, and its
	// post would carry the session cookie.
	protected.Handle("POST /agents/{slug}/messages", requireSameOrigin(http.HandlerFunc(s.handleAgentMessagePost)))
	protected.Handle("POST /agents/{slug}/stop", requireSameOrigin(http.HandlerFunc(s.handleAgentStop)))
	// An uploaded attachment, by content hash.
	protected.HandleFunc("GET /attachments/{sha}", s.handleAttachmentDownload)
	// Messages are stored at data/messages/<bucket>/<file>.md. The
	// route takes a relative path (with the "messages/" prefix baked
	// into the match) and serves the raw markdown. A ?dl=1 query flips
	// Content-Disposition to attachment so the user gets a download
	// prompt rather than an in-browser view.
	protected.HandleFunc("GET /messages/{path...}", s.handleMessage)
	protected.HandleFunc("GET /admin/version", s.handleAdminVersion)
	s.wireAPIRoutes(protected)
	// The web app: every GET no route above claims.
	protected.Handle("GET /{path...}", spa)

	switch {
	case s.AuthMW != nil:
		s.AuthMW.LoginPath = "/login"
		mux.Handle("/", s.AuthMW.Wrap(protected))
	case s.DevUser != "":
		// DEV_MODE: no credential check, but handlers still see a user.
		mux.Handle("/", auth.DevBypass(s.DevUser)(protected))
	default:
		// Tests construct a Server with neither; requests arrive with
		// no user on the context, which every handler tolerates.
		mux.Handle("/", protected)
	}

	s.mux = mux
	s.protectedMux = protected
}

// clk is the server's time source, defaulting to the real clock.
//
// An accessor rather than a bare field read because tests construct
// Server literals directly, without going through NewServer. Reaching
// for s.Clock in a handler would then nil-panic inside an SSE stream,
// which surfaces as "the events channel closed" — a long way from the
// actual cause.
func (s *Server) clk() clock.Clock {
	if s.Clock == nil {
		return clock.New()
	}
	return s.Clock
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.OAuth != nil {
		s.OAuth.LogoutHandler(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.AuthMW.SessionCookieName(), Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusFound)
}

// ---- org status ----

// QueueStatus is a snapshot of the runtime's current state for the org
// snapshot. Running ⇒ a release is executing; otherwise PendingDocs
// tells the CEO how much work will fire on the next release.
type QueueStatus struct {
	Running      bool
	ActiveAgents int // number of agents currently running inside Execute
	// PendingDocs is the number of distinct MESSAGES queued for
	// release, not the number of queued pointers: a notice addressed
	// to three agents holds three pointers but is one message, one
	// row in Home's queue, and one decision for the CEO. Counting
	// pointers made the count disagree with the queue it summarizes.
	PendingDocs int

	// InboxUnactioned is the count of items sitting in the CEO's
	// inbox that still need an approve/deny/ack. Lives on QueueStatus
	// rather than a separate struct because both are part of the same
	// "what needs the CEO's attention" summary.
	InboxUnactioned int
}

// queueStatus is the org snapshot's queue counts. Cheap
// to compute: reads the runtime's running-agents map + the message queue
// on disk. Called on every org snapshot build.
func (s *Server) queueStatus() QueueStatus {
	ts := QueueStatus{}
	if s.Runtime != nil {
		ts.Running = s.Runtime.Running()
		ts.ActiveAgents = len(s.Runtime.RunningAgents())
	}
	// De-duplicated on purpose: pendingPathsOf is the same flattening
	// Home's queue, the /org/stream pending_paths list and the
	// inbox-summary cache key are built from, so the count can't
	// disagree with any of them.
	if state, err := s.Store.ReadMessageQueue(); err == nil {
		ts.PendingDocs = len(pendingPathsOf(state))
	}
	ts.InboxUnactioned = s.pendingCEOInbox()
	return ts
}

// workingAgents returns the set of agent slugs currently doing work —
// either via an actively-running direct-chat tool loop (non-completed
// entry in chatHubs) or inside a runtime worker goroutine (entry in
// Runtime.RunningAgents). Lingering hubs (loop done, awaiting linger
// eviction) are excluded — the agent isn't actually working.
// Lightweight read-only snapshot, cheap enough to call on every tree
// build + every status-poll request.
func (s *Server) workingAgents() map[string]bool {
	s.streamMu.Lock()
	out := make(map[string]bool, len(s.chatHubs))
	for slug, hub := range s.chatHubs {
		if !hub.isCompleted() {
			out[slug] = true
		}
	}
	s.streamMu.Unlock()
	if s.Runtime != nil {
		for _, slug := range s.Runtime.RunningAgents() {
			out[slug] = true
		}
	}
	return out
}

// ---- health ----

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// /healthz is unauthenticated — anything in here is public to
	// whoever can reach the pod (k8s probes, anything that gets past
	// the tailnet, anything inside the cluster network). We DO want
	// clock-drift here so an external poller can catch a skewed host
	// clock without shelling in, but we deliberately do NOT include
	// the build version: there's no operational reason a probe
	// needs it, and version disclosure helps an attacker
	// fingerprint the deployment for known-vulnerability targeting.
	// Operators who need the build can curl /admin/version (auth-
	// gated below) or check the deployment manifest's image tag.
	// k8s liveness probes ignore the body — they only care about the 200.
	cc := s.ClockCheck.Latest()
	driftSec := int64(cc.Drift / time.Second)
	checkAgeSec := int64(0)
	if !cc.CheckedAt.IsZero() {
		checkAgeSec = int64(time.Since(cc.CheckedAt) / time.Second)
	}
	clockErr := ""
	if cc.Err != nil {
		clockErr = cc.Err.Error()
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w,
		`{"status":"ok","clock_drift_seconds":%d,"clock_check_age_seconds":%d,"clock_check_ref":%q,"clock_check_err":%q}`,
		driftSec, checkAgeSec, cc.Ref, clockErr)
}

// handleAdminVersion exposes the build version + env behind the auth
// middleware; the public /healthz carries no version.
func (s *Server) handleAdminVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	_, _ = fmt.Fprintf(w, `{"version":%q,"env":%q}`, s.VersionName, s.Env)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// The .tmp- prefix keeps a probe racing a backup out of the archive.
	f, err := os.CreateTemp(s.Store.Root(), ".tmp-readyz-*")
	if err != nil {
		http.Error(w, "data dir not writable", http.StatusServiceUnavailable)
		return
	}
	_ = f.Close()
	_ = os.Remove(f.Name())
	w.WriteHeader(http.StatusOK)
}

// ---- login ----
