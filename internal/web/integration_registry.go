// Package web (integration_registry.go) — the integrations' running
// side, as core sees it.
//
// One entry per integration that should run: an approved version,
// switched on, and an uploaded bundle, not a built-in. Each has a pod (integrationpod) whose
// shim dials integration.sock with the per-pod token core minted for
// it. The registry keeps the token, the connected shim (the link: its
// events stream), the requests core is waiting on, the last tools/list
// the server answered, each agent's tools/list (AgentTools, kept on the
// link), and its health. Core talks MCP to the server through the link:
// Call sends a JSON-RPC request as an rpc event and waits for the
// response the shim posts back.
//
// Every pod operation runs on one goroutine (Run): a reconcile after
// boot and after each change, and the health check every 30 seconds,
// which pings each running server and replaces a pod whose server has
// missed three pings in a row. Pods outlive core: after a restart core
// takes on a pod built as it would build it, with the token in its
// spec (IntegrationProvisioner.Token), and the shim reconnects.
//
// Timers use the injected clock.

package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
)

// Integration runtime limits.
const (
	// integrationHealthEvery is how often core pings each running
	// server.
	integrationHealthEvery = 30 * time.Second
	// integrationHealthMisses pings missed in a row replace the pod.
	integrationHealthMisses = 3
	// integrationCallTimeout bounds a tools/call; integrationShortTimeout
	// every other request (ping, tools/list).
	integrationCallTimeout  = 60 * time.Second
	integrationShortTimeout = 20 * time.Second
	// integrationHeartbeat is the events stream's comment interval.
	integrationHeartbeat = 15 * time.Second
	// integrationLinkBuffer is how many events may wait for a pod; one
	// that lets more pile up is not reading and is dropped.
	integrationLinkBuffer = 64
	// integrationOpTimeout bounds one pod operation of a reconcile.
	integrationOpTimeout = 2 * time.Minute
	// integrationMaxBody bounds one message the pod posts: the shim's
	// own line limit and room for the envelope.
	integrationMaxBody = integrationpod.MaxLineBytes + 64<<10
	// integrationSecretPlaceholder replaces a connection's secret value
	// in a log line.
	integrationSecretPlaceholder = "[secret]"
)

var (
	// errIntegrationNotRunning: no shim is connected for the
	// integration, or it went before it answered.
	errIntegrationNotRunning = errors.New("the integration is not running")
	// errIntegrationTimeout: the server did not answer in time.
	errIntegrationTimeout = errors.New("the integration did not answer in time")
	// errIntegrationRefused refuses a connection with a token or a
	// version core does not expect.
	errIntegrationRefused = errors.New("no integration is waiting for this connection")
)

// IntegrationProvisioner starts and stops integration pods.
// integrationpod.Provisioner implements it; tests inject a fake.
type IntegrationProvisioner interface {
	Enabled() bool
	// Start makes sure name's pod runs version, its bundle's files of
	// digest (store.IntegrationVersion.BundleDigest), with token;
	// software says the version's manifest declares packages, which
	// the pod installs as root when it starts.
	Start(ctx context.Context, name, version, digest, token string, software bool) error
	Stop(ctx context.Context, name string) error
	List(ctx context.Context) ([]string, error)
	// Token is the token of a live pod built as Start would build it
	// for version, digest and software, empty when there is none.
	Token(ctx context.Context, name, version, digest string, software bool) (string, error)
	// Unschedulable is the scheduler's message while the pod cannot be
	// placed on a node, empty otherwise.
	Unschedulable(ctx context.Context, name string) (string, error)
}

// integrationEntry is one integration that should run, or has a pod
// or a link still.
type integrationEntry struct {
	name string
	// version is the approved version that should run.
	version string
	// token is the pod's, minted (or taken on) for tokenVersion.
	token        string
	tokenVersion string
	link         *integrationLink
	// tools is the last tools/list result.
	tools json.RawMessage
	// misses counts pings missed in a row; idle counts health checks
	// with no link.
	misses int
	idle   int
	// replace asks the next reconcile for a new pod with a new token.
	replace bool
	// scrub replaces the connection's secret values in log lines;
	// built on first use and dropped when the connection changes.
	scrub *strings.Replacer
	// unschedulable is the scheduler's message last logged.
	unschedulable string
}

// integrationLink is a connected shim: the events core queues for it,
// which the stream handler writes, and closed, which ends the stream.
type integrationLink struct {
	name      string
	version   string
	proxyHash string
	events    chan integrationpod.Event
	closed    chan struct{}
	once      sync.Once

	// agentTools is each agent's tools/list answer through this link,
	// by slug, with the hash of the access it was asked with; a new
	// link (a new pod, a new version, a reconnect) starts with none.
	// toolsGen counts the times it was emptied, so the answer to a
	// request sent before is not kept. Both guarded by the registry's
	// mu.
	agentTools map[string]agentToolsAnswer
	toolsGen   int
}

// agentToolsAnswer is one agent's tools/list answer and the hash of
// the access it was asked with.
type agentToolsAnswer struct {
	accessHash string
	tools      json.RawMessage
}

func newIntegrationLink(name, version, proxyHash string) *integrationLink {
	return &integrationLink{
		name: name, version: version, proxyHash: proxyHash,
		events:     make(chan integrationpod.Event, integrationLinkBuffer),
		closed:     make(chan struct{}),
		agentTools: map[string]agentToolsAnswer{},
	}
}

func (l *integrationLink) close() { l.once.Do(func() { close(l.closed) }) }

// send queues ev for the pod. A link whose queue is full is closed: a
// pod that does not read is dropped, and reconnects.
func (l *integrationLink) send(ev integrationpod.Event) bool {
	select {
	case <-l.closed:
		return false
	default:
	}
	select {
	case l.events <- ev:
		return true
	default:
		l.close()
		return false
	}
}

// integrationCall is one request core waits on.
type integrationCall struct {
	name string
	ch   chan integrationpod.Message
}

// integrationRegistry holds the integrations' entries and runs their
// pods.
type integrationRegistry struct {
	mu      sync.Mutex
	entries map[string]*integrationEntry
	// connGen counts, by integration, the changes of its connection
	// (ConnectionChanged), for what is worked out from the connection
	// and kept a while (Server.requirementsCache).
	connGen map[string]uint64
	pending map[int64]*integrationCall
	nextID  int64

	store   *store.FSStore
	secrets func() secrets.Store
	prov    IntegrationProvisioner
	clk     clock.Clock
	logf    func(format string, args ...any)
	kickCh  chan struct{}
	// checks counts the pings and tools/list refreshes in flight (tests
	// wait on it).
	checks sync.WaitGroup
	// onConnect, when set, runs once a shim's link is in place. A test
	// seam.
	onConnect func(name string)
	// progress are the progress notifications core waits for, by
	// integration and progress token (watchProgress); nextProgress
	// numbers the tokens. linked is closed, and made again, each time a
	// shim connects (awaitRunning). Guarded by mu.
	progress     map[progressKey]func(integrationpod.ProgressParams)
	nextProgress int64
	linked       chan struct{}
	// signIn is a sign-in connection's current access token, refreshed
	// first when it is about to expire (Server.signInAccessToken), given
	// its manifest, its record and its Secret's data. NewServer sets
	// it.
	signIn func(ctx context.Context, name string, m *integration.Manifest, conn store.IntegrationConnection, data map[string]string) (string, error)
}

func newIntegrationRegistry(st *store.FSStore, sec func() secrets.Store, prov IntegrationProvisioner, clk clock.Clock, logf func(string, ...any)) *integrationRegistry {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &integrationRegistry{
		entries: map[string]*integrationEntry{},
		pending: map[int64]*integrationCall{},
		store:   st,
		secrets: sec,
		prov:    prov,
		clk:     clk,
		logf:    logf,
		kickCh:  make(chan struct{}, 1),
	}
}

func (r *integrationRegistry) provEnabled() bool { return r.prov != nil && r.prov.Enabled() }

// StartIntegrations runs the integrations' pods until the returned
// stop function is called: a reconcile at once, then one after each
// change and a health check every 30 seconds. Stopping closes every
// link without stopping a pod: the pods outlive this process and their
// shims reconnect to the next. A no-op without a registry.
func (s *Server) StartIntegrations() func() {
	if s.Integrations == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Integrations.Run(ctx)
	}()
	return func() {
		cancel()
		<-done
		s.Integrations.closeLinks()
	}
}

// Run reconciles at once, taking on the pods left from before this
// start and stopping those of integrations that are gone, then again
// on every Changed and checks health every integrationHealthEvery,
// until ctx ends.
func (r *integrationRegistry) Run(ctx context.Context) {
	if err := r.reconcile(ctx, true); err != nil {
		r.logf("integrations: boot reconcile: %v", err)
	}
	tick := r.clk.NewTicker(integrationHealthEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.kickCh:
			if err := r.reconcile(ctx, false); err != nil {
				r.logf("integrations: reconcile: %v", err)
			}
		case <-tick.C():
			r.health(ctx)
		}
	}
}

// Changed asks Run for a reconcile: after an approval, a rejection, a
// roll back, the switch, a removal or a connection change.
func (r *integrationRegistry) Changed() {
	if r == nil {
		return
	}
	select {
	case r.kickCh <- struct{}{}:
	default:
	}
}

// closeLinks ends every stream, as at shutdown.
func (r *integrationRegistry) closeLinks() {
	r.mu.Lock()
	var links []*integrationLink
	for _, e := range r.entries {
		if e.link != nil {
			links = append(links, e.link)
			e.link = nil
		}
		r.failPendingLocked(e.name)
	}
	r.mu.Unlock()
	for _, l := range links {
		l.close()
	}
}

// Reconcile brings every integration's pod to what the store says:
// running for an approved, switched-on bundle, and stopped for every
// other: a built-in runs no pod, so one left from a Kivali that shipped
// the integration as a bundle is stopped.
func (r *integrationRegistry) Reconcile(ctx context.Context) error {
	return r.reconcile(ctx, false)
}

// reconcile is Reconcile; at boot it also stops the pods and drops the
// proxy credentials of integrations no longer installed.
func (r *integrationRegistry) reconcile(ctx context.Context, boot bool) error {
	its, err := r.store.ListIntegrations()
	if err != nil {
		return err
	}
	want := map[string]string{}
	for _, it := range its {
		if it.Enabled && it.CurrentVersion != "" && !it.Builtin {
			want[it.Name] = it.CurrentVersion
		}
	}
	// stop names what may have a pod or a credential to take down.
	stop := map[string]bool{}
	r.mu.Lock()
	for name := range r.entries {
		stop[name] = true
	}
	r.mu.Unlock()
	if boot {
		if r.provEnabled() {
			pods, err := r.prov.List(ctx)
			if err != nil {
				r.logf("integrations: list pods: %v", err)
			}
			for _, name := range pods {
				stop[name] = true
			}
		}
		for user := range r.store.EgressScopedCredentials() {
			// A built-in's connection's user (builtinintegrations.ProxyUser)
			// is no integration name after the prefix: it has a dot. Its
			// credential follows the connection, not a pod.
			if name, ok := strings.CutPrefix(user, integrationpod.ProxyUser("")); ok && integration.ValidName(name) {
				stop[name] = true
			}
		}
	}
	names := make([]string, 0, len(want)+len(stop))
	for name := range want {
		names = append(names, name)
	}
	for name := range stop {
		if _, ok := want[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		opCtx, cancel := context.WithTimeout(ctx, integrationOpTimeout)
		if version := want[name]; version != "" {
			err = r.ensureRunning(opCtx, name, version)
		} else {
			err = r.ensureStopped(opCtx, name)
		}
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// ensureRunning gives name a pod for version: the one it has when it
// matches, one taken on from before a restart, or a new one with a new
// token. A pod's spec carries the digest of the files it runs, so one
// built from other files than the ones stored now is never taken on,
// and Start replaces it.
func (r *integrationRegistry) ensureRunning(ctx context.Context, name, version string) error {
	digest, err := r.store.IntegrationBundleDigest(name, version)
	if err != nil {
		return fmt.Errorf("the bundle's digest: %w", err)
	}
	r.mu.Lock()
	e := r.entries[name]
	if e == nil {
		e = &integrationEntry{name: name}
		r.entries[name] = e
	}
	e.version = version
	adopt := e.token == ""
	r.mu.Unlock()

	// The pod starts as root only when the version installs software.
	_, m, err := r.store.GetIntegrationVersion(name, version)
	if err != nil {
		return fmt.Errorf("read version %s: %w", version, err)
	}
	software := len(m.Kivali.Packages) > 0

	adopted := ""
	if adopt && r.provEnabled() {
		tok, err := r.prov.Token(ctx, name, version, digest, software)
		if err != nil {
			r.logf("integration %s: read its pod: %v", name, err)
		}
		adopted = tok
	}

	r.mu.Lock()
	var old *integrationLink
	switch {
	case adopted != "" && e.token == "":
		e.token, e.tokenVersion = adopted, version
	case e.token == "" || e.tokenVersion != version || e.replace:
		e.token, e.tokenVersion, e.replace = newIntegrationToken(), version, false
		old, e.link = e.link, nil
		r.failPendingLocked(name)
	}
	token := e.token
	r.mu.Unlock()
	if old != nil {
		old.close()
	}
	if !r.provEnabled() {
		return nil
	}
	// A new token is a new spec: Start replaces the pod with the old
	// one.
	return r.prov.Start(ctx, name, version, digest, token, software)
}

// ensureStopped takes name down: its link, its pod and its proxy
// credential.
func (r *integrationRegistry) ensureStopped(ctx context.Context, name string) error {
	r.mu.Lock()
	var link *integrationLink
	if e := r.entries[name]; e != nil {
		link = e.link
		delete(r.entries, name)
	}
	r.failPendingLocked(name)
	r.mu.Unlock()
	if link != nil {
		link.send(integrationpod.Event{Type: integrationpod.EventShutdown, Data: []byte("{}")})
		link.close()
	}
	var errs []error
	if r.provEnabled() {
		if err := r.prov.Stop(ctx, name); err != nil {
			errs = append(errs, err)
		}
	}
	if err := r.store.DropEgressScopedCredential(integrationpod.ProxyUser(name)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// health pings every connected server, and asks for a reconcile when an
// integration that should run has had no link for three checks: its
// pod may be gone, and Start makes it again.
func (r *integrationRegistry) health(ctx context.Context) {
	r.mu.Lock()
	var ping, idle []string
	for name, e := range r.entries {
		switch {
		case e.link != nil:
			e.idle = 0
			ping = append(ping, name)
		default:
			e.idle++
			if e.idle >= integrationHealthMisses {
				e.idle = 0
				idle = append(idle, name)
			}
		}
	}
	r.mu.Unlock()
	for _, name := range ping {
		r.checks.Add(1)
		go func() {
			defer r.checks.Done()
			r.ping(ctx, name)
		}()
	}
	if len(idle) > 0 {
		for _, name := range idle {
			r.noteUnschedulable(ctx, name)
		}
		r.Changed()
	}
}

// ping sends the server MCP's ping. Any answer counts, an error
// included; a ping not answered in time is a miss, and the third miss
// in a row asks for a new pod.
func (r *integrationRegistry) ping(ctx context.Context, name string) {
	_, err := r.Call(ctx, name, "ping", map[string]any{})
	var rpcErr *integrationpod.RPCError
	if err != nil && !errors.Is(err, errIntegrationTimeout) && !errors.As(err, &rpcErr) {
		return
	}
	r.mu.Lock()
	e := r.entries[name]
	if e == nil {
		r.mu.Unlock()
		return
	}
	if !errors.Is(err, errIntegrationTimeout) {
		e.misses = 0
		r.mu.Unlock()
		return
	}
	e.misses++
	replace := e.misses >= integrationHealthMisses
	if replace {
		e.misses, e.replace = 0, true
	}
	r.mu.Unlock()
	if replace {
		r.logf("integration %s: missed %d health checks; replacing its pod", name, integrationHealthMisses)
		r.appendLog(name, "error", "The integration did not answer three checks in a row, so Kivali restarted it.")
		r.Changed()
	}
}

// noteUnschedulable logs, once per message, that the integration's pod
// cannot be placed for want of memory.
func (r *integrationRegistry) noteUnschedulable(ctx context.Context, name string) {
	if !r.provEnabled() {
		return
	}
	msg, err := r.prov.Unschedulable(ctx, name)
	if err != nil || msg == "" {
		return
	}
	r.mu.Lock()
	e := r.entries[name]
	seen := e == nil || e.unschedulable == msg
	if e != nil {
		e.unschedulable = msg
	}
	r.mu.Unlock()
	if seen {
		return
	}
	text := "The integration cannot start: the team's computer has no room for it (" + msg + ")."
	if strings.Contains(msg, "Insufficient memory") {
		text = "The integration cannot start: the team does not have enough memory for it."
	}
	r.appendLog(name, "error", text)
}

// connect takes on a shim's connection: its token must be the pod's
// and its version the approved one. A second connection for the same
// integration replaces the first. The shim's proxy credential is
// registered with the hosts the approved version names, and the link's
// first event is start.
func (r *integrationRegistry) connect(name, token, version, proxyHash string) (*integrationLink, error) {
	if !r.tokenMatches(name, token, version) {
		return nil, errIntegrationRefused
	}
	if proxyHash != "" {
		_, m, err := r.store.CurrentIntegrationVersion(name)
		if err != nil {
			return nil, err
		}
		if err := r.store.SetEgressScopedCredential(integrationpod.ProxyUser(name), proxyHash, m.Kivali.Hosts); err != nil {
			return nil, err
		}
	}
	link := newIntegrationLink(name, version, proxyHash)
	r.mu.Lock()
	e := r.entries[name]
	if e == nil || subtle.ConstantTimeCompare([]byte(e.token), []byte(token)) != 1 || e.version != version {
		r.mu.Unlock()
		return nil, errIntegrationRefused
	}
	old := e.link
	e.link, e.misses, e.idle = link, 0, 0
	r.failPendingLocked(name)
	if r.linked != nil {
		close(r.linked)
		r.linked = nil
	}
	r.mu.Unlock()
	if old != nil {
		old.close()
	}
	link.send(integrationpod.Event{Type: integrationpod.EventStart, Data: []byte("{}")})
	r.checks.Add(1)
	go func() {
		defer r.checks.Done()
		r.refreshTools(name)
	}()
	if r.onConnect != nil {
		r.onConnect(name)
	}
	return link, nil
}

// tokenMatches reports whether token is name's pod token and version
// the version it should run.
func (r *integrationRegistry) tokenMatches(name, token, version string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[name]
	return e != nil && e.token != "" && subtle.ConstantTimeCompare([]byte(e.token), []byte(token)) == 1 &&
		version != "" && e.version == version && e.tokenVersion == version
}

// authorized reports whether token is name's pod token: what every
// post from the pod must carry.
func (r *integrationRegistry) authorized(name, token string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[name]
	return e != nil && e.token != "" && subtle.ConstantTimeCompare([]byte(e.token), []byte(token)) == 1
}

// disconnect is the end of link's stream: the integration is not
// running until its shim connects again.
func (r *integrationRegistry) disconnect(link *integrationLink) {
	r.mu.Lock()
	if e := r.entries[link.name]; e != nil && e.link == link {
		e.link = nil
		r.failPendingLocked(link.name)
	}
	r.mu.Unlock()
	link.close()
}

// failPendingLocked ends every request waiting on name: its server is
// gone. r.mu is held.
func (r *integrationRegistry) failPendingLocked(name string) {
	for id, c := range r.pending {
		if c.name == name {
			delete(r.pending, id)
			close(c.ch)
		}
	}
}

// refreshTools asks the server for its tools and keeps the answer.
func (r *integrationRegistry) refreshTools(name string) {
	res, err := r.Call(context.Background(), name, "tools/list", map[string]any{})
	if err != nil {
		if !errors.Is(err, errIntegrationNotRunning) {
			r.logf("integration %s: tools/list: %v", name, err)
		}
		return
	}
	r.mu.Lock()
	if e := r.entries[name]; e != nil {
		e.tools = res
	}
	r.mu.Unlock()
}

// Tools is the last tools/list result of name's server, nil before one.
func (r *integrationRegistry) Tools(name string) json.RawMessage {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[name]; e != nil {
		return e.tools
	}
	return nil
}

// kivaliMeta is the _meta core puts on what it sends a server for an
// agent: the agent's slug and its access, the filled access form.
// Core's own requests (Test connection, the options a field lists,
// ping, the tools/list at connect) carry none.
func kivaliMeta(slug string, access json.RawMessage) map[string]any {
	return map[string]any{"kivali": map[string]any{"agent": slug, "access": access}}
}

// AgentTools is name's tools/list answer for one agent, asked with the
// agent's _meta, and kept for the link it came through until the
// agent's access or the connection changes, so a turn asks again only
// after a change. errIntegrationNotRunning without a shim.
func (r *integrationRegistry) AgentTools(ctx context.Context, name, slug string, access json.RawMessage) (json.RawMessage, error) {
	if r == nil {
		return nil, errIntegrationNotRunning
	}
	sum := sha256.Sum256(access)
	hash := hex.EncodeToString(sum[:])
	r.mu.Lock()
	e := r.entries[name]
	if e == nil || e.link == nil {
		r.mu.Unlock()
		return nil, errIntegrationNotRunning
	}
	link := e.link
	if a, ok := link.agentTools[slug]; ok && a.accessHash == hash {
		r.mu.Unlock()
		return a.tools, nil
	}
	gen := link.toolsGen
	r.mu.Unlock()
	res, err := r.Call(ctx, name, "tools/list", map[string]any{"_meta": kivaliMeta(slug, access)})
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	// Kept only on the link it was asked through, unemptied since.
	if e := r.entries[name]; e != nil && e.link == link && link.toolsGen == gen {
		link.agentTools[slug] = agentToolsAnswer{accessHash: hash, tools: res}
	}
	r.mu.Unlock()
	return res, nil
}

// forgetAgentToolsLocked empties the tools/list answers kept on name's
// link. r.mu is held.
func (r *integrationRegistry) forgetAgentToolsLocked(name string) {
	if e := r.entries[name]; e != nil && e.link != nil {
		e.link.agentTools = map[string]agentToolsAnswer{}
		e.link.toolsGen++
	}
}

// Running reports whether name's shim is connected.
func (r *integrationRegistry) Running(name string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[name]
	return e != nil && e.link != nil
}

// awaitRunning waits until name's shim is connected, and reports
// whether it is: false once ctx ends first.
func (r *integrationRegistry) awaitRunning(ctx context.Context, name string) bool {
	if r == nil {
		return false
	}
	for {
		r.mu.Lock()
		if e := r.entries[name]; e != nil && e.link != nil {
			r.mu.Unlock()
			return true
		}
		if r.linked == nil {
			r.linked = make(chan struct{})
		}
		ch := r.linked
		r.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// progressKey names the progress notifications of one request: the
// integration whose pod sends them and the request's token.
type progressKey struct{ name, token string }

// watchProgress hands each progress notification name's pod sends with
// the token it returns to fn, until stop is called. The caller puts the
// token in the request's _meta.progressToken.
func (r *integrationRegistry) watchProgress(name string, fn func(integrationpod.ProgressParams)) (token string, stop func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextProgress++
	token = "kivali-progress-" + strconv.FormatInt(r.nextProgress, 10)
	if r.progress == nil {
		r.progress = map[progressKey]func(integrationpod.ProgressParams){}
	}
	key := progressKey{name, token}
	r.progress[key] = fn
	return token, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.progress, key)
	}
}

// onProgress hands a progress notification of name's pod to the
// request waiting for it; one no request waits for is dropped.
func (r *integrationRegistry) onProgress(name string, params json.RawMessage) {
	var p integrationpod.ProgressParams
	if json.Unmarshal(params, &p) != nil {
		return
	}
	var token string
	if json.Unmarshal(p.ProgressToken, &token) != nil {
		return
	}
	r.mu.Lock()
	fn := r.progress[progressKey{name, token}]
	r.mu.Unlock()
	if fn != nil {
		fn(p)
	}
}

// timeoutFor is how long a request waits for its response: a call of a
// setup step as long as the setup runner waits for one, an action or an
// uninstall a minute, anything else (ping, tools/list, a bundle's
// requirements) the short wait.
func timeoutFor(method string) time.Duration {
	switch method {
	case integrationpod.MethodSetup:
		return setupCallTimeout
	case "tools/call", integrationpod.MethodUninstall:
		return integrationCallTimeout
	}
	return integrationShortTimeout
}

// Call sends name's server a JSON-RPC request and returns its result:
// errIntegrationNotRunning when no shim is connected or it goes before
// answering, errIntegrationTimeout past the method's deadline, and the
// server's *integrationpod.RPCError when it answers with one.
func (r *integrationRegistry) Call(ctx context.Context, name, method string, params any) (json.RawMessage, error) {
	if r == nil {
		return nil, errIntegrationNotRunning
	}
	p, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	e := r.entries[name]
	if e == nil || e.link == nil {
		r.mu.Unlock()
		return nil, errIntegrationNotRunning
	}
	link := e.link
	r.nextID++
	id := r.nextID
	call := &integrationCall{name: name, ch: make(chan integrationpod.Message, 1)}
	r.pending[id] = call
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
	}()
	msg, err := json.Marshal(integrationpod.Message{JSONRPC: "2.0", ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: p})
	if err != nil {
		return nil, err
	}
	if !link.send(integrationpod.Event{Type: integrationpod.EventRPC, Data: msg}) {
		return nil, errIntegrationNotRunning
	}
	t := r.clk.NewTimer(timeoutFor(method))
	defer t.Stop()
	select {
	case resp, ok := <-call.ch:
		if !ok {
			return nil, errIntegrationNotRunning
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-t.C():
		return nil, errIntegrationTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Notify sends name's server a JSON-RPC notification. False when no
// shim is connected.
func (r *integrationRegistry) Notify(name, method string, params any) bool {
	p, err := json.Marshal(params)
	if err != nil {
		return false
	}
	msg, err := json.Marshal(integrationpod.Message{JSONRPC: "2.0", Method: method, Params: p})
	if err != nil {
		return false
	}
	return r.sendEvent(name, integrationpod.Event{Type: integrationpod.EventRPC, Data: msg})
}

func (r *integrationRegistry) sendEvent(name string, ev integrationpod.Event) bool {
	r.mu.Lock()
	e := r.entries[name]
	var link *integrationLink
	if e != nil {
		link = e.link
	}
	r.mu.Unlock()
	return link != nil && link.send(ev)
}

// ConnectionChanged is a write or removal of name's connection: the
// log scrubber is built again, the agents' tools/list answers are
// forgotten, and the shim restarts the server with the new values.
func (r *integrationRegistry) ConnectionChanged(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if e := r.entries[name]; e != nil {
		e.scrub = nil
	}
	if r.connGen == nil {
		r.connGen = map[string]uint64{}
	}
	r.connGen[name]++
	r.forgetAgentToolsLocked(name)
	r.mu.Unlock()
	r.sendEvent(name, integrationpod.Event{Type: integrationpod.EventRestart, Data: []byte("{}")})
}

// connectionGen is how many times name's connection changed
// (ConnectionChanged) since the registry started; 0 with no registry.
func (r *integrationRegistry) connectionGen(name string) uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.connGen[name]
}

// onMessage handles one message the pod posted: a response is matched
// to the request waiting on it; a progress notification goes to the
// request whose token it names; a request for the connection's values
// or a log line is answered; any other request is a method that does
// not exist. It returns the JSON-RPC response for a request and nil
// for anything else.
func (r *integrationRegistry) onMessage(ctx context.Context, name string, msg integrationpod.Message) []byte {
	switch {
	case msg.IsResponse():
		r.deliver(name, msg)
		return nil
	case msg.Method == integrationpod.MethodLog:
		var p integrationpod.LogParams
		_ = json.Unmarshal(msg.Params, &p)
		r.appendLogCtx(ctx, name, p.Level, p.Message)
		if msg.IsNotification() {
			return nil
		}
		out, _ := integrationpod.Response(msg.ID, map[string]any{}, nil)
		return out
	case msg.Method == integrationpod.MethodProgress && msg.IsNotification():
		r.onProgress(name, msg.Params)
		return nil
	case msg.IsNotification():
		return nil
	case msg.Method == integrationpod.MethodCredential:
		cred, rpcErr := r.credential(ctx, name)
		out, _ := integrationpod.Response(msg.ID, cred, rpcErr)
		return out
	}
	out, _ := integrationpod.Response(msg.ID, nil, &integrationpod.RPCError{Code: integrationpod.CodeMethodNotFound, Message: "method not found: " + msg.Method})
	return out
}

// deliver hands a response to the request of name's that waits on it.
func (r *integrationRegistry) deliver(name string, msg integrationpod.Message) {
	id, err := strconv.ParseInt(string(msg.ID), 10, 64)
	if err != nil {
		return
	}
	r.mu.Lock()
	c := r.pending[id]
	if c == nil || c.name != name {
		r.mu.Unlock()
		return
	}
	delete(r.pending, id)
	r.mu.Unlock()
	c.ch <- msg
}

// connectionValues are name's connection's values by user_config key,
// with which of them are secret, from the store and the Secret. A
// missing connection is store.ErrNotFound. An integration that runs in
// a pod takes one connection, so this is the oldest: the one its
// server starts with.
func (r *integrationRegistry) connectionValues(ctx context.Context, name string) (*integration.Manifest, map[string]string, []string, error) {
	m, _, values, secretValues, _, err := r.readConnection(ctx, name, "")
	return m, values, secretValues, err
}

// readConnection is connectionValues for the connection connID ("" for
// the oldest), with the connection's record, which a built-in
// integration's call needs for its id, and the secret of the
// connection's scoped proxy credential (a built-in's, kept in its
// Secret under builtinintegrations.ProxySecretKey; "" for none), which
// is never one of the values.
//
// For a connection made by signing in, the values hold a current
// access token under the name the manifest's credential_keys gives it,
// refreshed first when it is about to expire (signIn), and never the
// client secret; the error is errSignInNeeded when the owner must sign
// in (again), errVendorUnavailable when the token has expired and the
// vendor did not answer its refresh. A refresh may write the
// connection, so no caller holds lockIntegrations.
func (r *integrationRegistry) readConnection(ctx context.Context, name, connID string) (*integration.Manifest, store.IntegrationConnection, map[string]string, []string, string, error) {
	m, conn, values, secretValues, proxySecret, data, err := r.readStoredConnection(ctx, name, connID)
	if err != nil || !m.Kivali.Connection.OAuth() {
		return m, conn, values, secretValues, proxySecret, err
	}
	token := data[connect.SecretKeyAccessToken]
	if r.signIn != nil {
		token, err = r.signIn(ctx, name, m, conn, data)
	} else if token == "" || conn.NeedsSignIn(true) {
		err = errSignInNeeded
	}
	if err != nil {
		return m, conn, nil, nil, "", err
	}
	if key := m.Kivali.Connection.CredentialKey(); key != "" {
		values[key] = token
	}
	secretValues = append(secretValues, token)
	return m, conn, values, secretValues, proxySecret, nil
}

// readStoredConnection is readConnection as the connection is stored,
// with no sign-in's refresh, and the Secret's data: for a caller that
// holds lockIntegrations, and for the log scrubber, which must not
// reach a vendor for every line. The secret values include a sign-in's
// stored tokens and a registered client's secrets; the values hold
// none of them, nor the client secret.
func (r *integrationRegistry) readStoredConnection(ctx context.Context, name, connID string) (*integration.Manifest, store.IntegrationConnection, map[string]string, []string, string, map[string]string, error) {
	_, m, err := r.store.CurrentIntegrationVersion(name)
	if err != nil {
		return nil, store.IntegrationConnection{}, nil, nil, "", nil, err
	}
	var conn store.IntegrationConnection
	if connID == "" {
		conn, err = r.store.GetIntegrationConnection(name)
	} else {
		conn, err = r.store.GetIntegrationConnectionByID(name, connID)
	}
	if err != nil {
		return m, conn, nil, nil, "", nil, err
	}
	values := map[string]string{}
	for k, v := range conn.Fields {
		values[k] = v
	}
	var secretValues []string
	proxySecret := ""
	data := map[string]string{}
	if conn.SecretName != "" {
		got, err := r.secrets().Get(ctx, conn.SecretName)
		switch {
		case errors.Is(err, secrets.ErrNotFound):
		case err != nil:
			return m, conn, nil, nil, "", nil, err
		default:
			data = got
		}
		for k, v := range data {
			if !integration.ConnectionKeySecret(m, k) || v == "" {
				continue
			}
			values[k] = v
			// A kept value declared not secret (an app's public name) is
			// a value like any other, in an answer or the log.
			if integration.ConnectionKeyScrubbed(m, k) {
				secretValues = append(secretValues, v)
			}
		}
		for _, k := range connect.SecretValueKeys {
			if v := data[k]; v != "" {
				secretValues = append(secretValues, v)
			}
		}
		proxySecret = data[builtinintegrations.ProxySecretKey]
	}
	// The client secret is core's alone: an integration is never given
	// it.
	delete(values, integration.FieldClientSecret)
	return m, conn, values, secretValues, proxySecret, data, nil
}

// credential is the kivali/credential answer: the server's env with
// the connection's values put in, and the values themselves. For a
// connection made by signing in both carry the current access token
// under its credential_keys name.
func (r *integrationRegistry) credential(ctx context.Context, name string) (*integrationpod.Credential, *integrationpod.RPCError) {
	m, values, secretValues, err := r.connectionValues(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, &integrationpod.RPCError{Code: integrationpod.CodeNoConnection, Message: "no connection"}
	case errors.Is(err, errSignInNeeded):
		return nil, &integrationpod.RPCError{Code: integrationpod.CodeSignInNeeded, Message: errSignInNeeded.Error()}
	case errors.Is(err, errVendorUnavailable):
		return nil, &integrationpod.RPCError{Code: integrationpod.CodeVendorUnavailable, Message: errVendorUnavailable.Error()}
	case err != nil:
		r.logf("integration %s: the connection's values: %v", name, err)
		return nil, &integrationpod.RPCError{Code: -32603, Message: "the connection could not be read"}
	}
	r.setScrub(name, secretValues)
	fields := map[string]string{}
	for _, k := range integration.ConnectionKeys(m) {
		if v, ok := values[k]; ok {
			fields[k] = v
		}
	}
	env := integration.ServerEnv(m, values)
	if key := m.Kivali.Connection.CredentialKey(); key != "" && m.Kivali.Connection.OAuth() {
		fields[key] = values[key]
		env[key] = values[key]
	}
	return &integrationpod.Credential{Env: env, Fields: fields}, nil
}

// scrubMinLen is the shortest secret value the scrubber replaces. A
// value shorter than this would be replaced wherever its letters happen
// to occur and mangle every line; no token or key is that short.
const scrubMinLen = 6

// newScrubber replaces each secret value of scrubMinLen or more
// characters, longest first.
func newScrubber(values []string) *strings.Replacer {
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if len(v) >= scrubMinLen {
			kept = append(kept, v)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return len(kept[i]) > len(kept[j]) })
	pairs := make([]string, 0, 2*len(kept))
	for _, v := range kept {
		pairs = append(pairs, v, integrationSecretPlaceholder)
	}
	return strings.NewReplacer(pairs...)
}

func (r *integrationRegistry) setScrub(name string, values []string) {
	scrub := newScrubber(values)
	r.mu.Lock()
	if e := r.entries[name]; e != nil {
		e.scrub = scrub
	}
	r.mu.Unlock()
}

// scrubber is name's log scrubber, built from the connection's secret
// values when there is none yet.
func (r *integrationRegistry) scrubber(ctx context.Context, name string) *strings.Replacer {
	r.mu.Lock()
	var scrub *strings.Replacer
	if e := r.entries[name]; e != nil {
		scrub = e.scrub
	}
	r.mu.Unlock()
	if scrub != nil {
		return scrub
	}
	// As stored: a log line never sends a vendor a refresh. The stored
	// tokens are among the values; one refreshed since is added when
	// the credential is next answered (setScrub).
	_, _, _, values, _, _, err := r.readStoredConnection(ctx, name, "")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		// Not knowing the secrets is no reason to keep the line: it
		// may quote one.
		return nil
	}
	r.setScrub(name, values)
	return newScrubber(values)
}

// logLevels are the levels a log line may have; any other reads as
// info.
var logLevels = map[string]bool{"info": true, "warning": true, "error": true, "stderr": true, "debug": true}

func (r *integrationRegistry) appendLog(name, level, message string) {
	r.appendLogCtx(context.Background(), name, level, message)
}

// appendLogCtx writes one line to name's log with the connection's
// secret values replaced. A line whose secrets cannot be known is
// replaced whole.
func (r *integrationRegistry) appendLogCtx(ctx context.Context, name, level, message string) {
	if !logLevels[level] {
		level = "info"
	}
	if scrub := r.scrubber(ctx, name); scrub != nil {
		message = scrub.Replace(message)
	} else {
		message = "(a line Kivali could not check for secrets was left out)"
	}
	if err := r.store.AppendIntegrationLog(name, store.IntegrationLogRow{At: r.clk.Now(), Level: level, Message: message}); err != nil && !errors.Is(err, store.ErrNotFound) {
		r.logf("integration %s: log: %v", name, err)
	}
}

func newIntegrationToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
