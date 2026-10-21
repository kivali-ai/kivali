// Package main is the Kivali egress proxy.
//
// A small forward proxy that mediates HTTP and HTTPS (via CONNECT)
// traffic from agent pods to the internet. It runs in the Kivali
// server pod, sharing an emptyDir with the server for its two files:
// the allowlist (allowlist.go) and the agents file (agents.go), both
// hot-reloaded. Agent pods set HTTP_PROXY / HTTPS_PROXY to the Service
// in front of this process.
//
// Each request is in one of five modes, decided by its
// Proxy-Authorization:
//
//   - Allowlist mode, for a request without Proxy-Authorization: the
//     host must match the allowlist.
//   - Shell mode, for a request carrying an agent's slug and its
//     shell's secret (a command in the agent's shell): the host must
//     match the allowlist or one of that agent's own patterns
//     (agents.go). Nothing is counted.
//   - Open mode, for a request carrying an agent's slug and its
//     runtime's secret (the agent's own fetches and model calls): any
//     public host, and the allowlist's hosts. What the agent sends out
//     is counted against a byte budget (budget.go), except through a
//     TLS tunnel to a model host that names that host in its
//     ClientHello (sni.go).
//   - Exempt mode, for a request carrying either credential of an
//     agent with no network limits: any public host, and the hosts
//     shell mode reaches. Nothing is counted.
//   - Scoped mode, for a request carrying an integration's user and
//     its server's secret: the host must match one of the hosts its
//     approved version names, never the allowlist. Nothing is counted.
//
// Present but invalid credentials get 407. In every mode the host is
// resolved once and dialled by address through the guard (guard.go),
// which refuses loopback, link-local and the cluster's own ranges, and
// for open and exempt mode hosts on no list every non-public range as
// well. Each source IP may hold a bounded number of connections
// (limit.go).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

const (
	defaultListen    = ":3128"
	defaultAllowlist = "/etc/egress/allowlist.yaml"
	defaultAgents    = "/etc/egress/agents.json"
	// dialTimeout bounds the time spent reaching out to an allowed
	// upstream before giving up, so a slow destination doesn't pin a
	// proxy goroutine. It also bounds the wait for a tunnel's first
	// TLS record.
	dialTimeout = 10 * time.Second
	// copyDeadline bounds an individual tunnel's total life. Long-lived
	// websockets aren't a use case here.
	copyDeadline = 10 * time.Minute
	// idleTimeout closes a client's keep-alive connection that has sat
	// unused this long.
	idleTimeout = 2 * time.Minute
)

func main() {
	listen := flag.String("listen", defaultListen, "address to listen on")
	allowlistPath := flag.String("allowlist", defaultAllowlist, "path to allowlist YAML (hot-reloaded)")
	agentsPath := flag.String("agents", defaultAgents, "path to agents JSON (hot-reloaded); missing means no agents")
	var deny cidrFlag
	flag.Var(&deny, "deny-cidr", "cluster range or address refused for every request (repeatable, comma-separated; empty ignored)")
	var openDeny cidrFlag
	flag.Var(&openDeny, "open-deny-cidr", "range or address refused only to open and exempt mode hosts on no list, on top of the non-public ranges (repeatable, comma-separated; empty ignored)")
	budgetConn := flag.Int64("budget-conn", 32768, "bytes an agent may send up one open-mode connection")
	budget10m := flag.Int64("budget-10m", 262144, "bytes an agent may send out in open mode per rolling 10 minutes")
	budgetDay := flag.Int64("budget-day", 4194304, "bytes an agent may send out in open mode per rolling 24 hours")
	enforce := flag.Bool("enforce-budget", false, "refuse over-budget agents; otherwise count and log only")
	maxPerSource := flag.Int("max-conns-per-source", 256, "connections one source IP may hold open at once")
	flag.Parse()

	clk := clock.New()

	var allow Allowlist
	if err := allow.Load(*allowlistPath); err != nil {
		log.Fatalf("load allowlist: %v", err)
	}
	log.Printf("egress-proxy loaded %d patterns from %s", len(allow.Patterns()), *allowlistPath)
	go (&Watcher{Path: *allowlistPath, Interval: 5 * time.Second}).Run(&allow, log.Printf)

	agents := &Agents{Path: *agentsPath, Logf: log.Printf}
	if err := agents.Load(*agentsPath); err != nil {
		// Start with no agents rather than not at all: a request
		// without a credential keeps the allowlist, and the watcher
		// picks up the next good file.
		log.Printf("load agents: %v", err)
	}
	go (&Watcher{Path: *agentsPath, Interval: 5 * time.Second}).Run(agents, log.Printf)

	h := &proxy{
		allow:  &allow,
		agents: agents,
		guard: &guard{
			resolver: net.DefaultResolver,
			dial:     (&net.Dialer{Timeout: dialTimeout}).DialContext,
			deny:     deny,
			openDeny: openDeny,
		},
		budget: newBudget(clk, budgetLimits{Conn: *budgetConn, TenMin: *budget10m, Day: *budgetDay}, *enforce, log.Printf),
		logf:   log.Printf,
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       idleTimeout,
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("egress-proxy listening on %s (deny-cidr=%s enforce-budget=%t max-conns-per-source=%d)",
		*listen, deny.String(), *enforce, *maxPerSource)
	if err := srv.Serve(newSourceLimitListener(ln, *maxPerSource, log.Printf)); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}

type proxy struct {
	allow  *Allowlist
	agents *Agents
	guard  *guard
	budget *budget
	logf   func(string, ...any)
	// httpDeadline bounds a plain HTTP request from dial to the last
	// byte of its response. Zero means copyDeadline.
	httpDeadline time.Duration
}

func (p *proxy) requestDeadline() time.Duration {
	if p.httpDeadline > 0 {
		return p.httpDeadline
	}
	return copyDeadline
}

// requestMode is how the proxy treats a request, decided by the
// credential it presents (admit).
type requestMode int

const (
	// modeAllowlist is a request with no credential: the host must be
	// on the allowlist.
	modeAllowlist requestMode = iota
	// modeOpen is a request with an agent runtime's own credential: any
	// public host, counted against the agent's budget.
	modeOpen
	// modeShell is a request with an agent shell's credential: the host
	// must be on the allowlist or among the agent's own patterns.
	modeShell
	// modeExempt is a request with either credential of an exempt
	// agent: any public host, not counted.
	modeExempt
	// modeScoped is a request with an integration server's credential:
	// the host must be among its own hosts, not counted.
	modeScoped
)

// String is the mode as the log lines name it.
func (m requestMode) String() string {
	switch m {
	case modeOpen:
		return "open"
	case modeShell:
		return "shell"
	case modeExempt:
		return "exempt"
	case modeScoped:
		return "scoped"
	default:
		return "allowlist"
	}
}

// session is one request's standing.
type session struct {
	mode requestMode
	// slug is the agent whose credential the request presented; empty
	// in allowlist mode.
	slug string
	// listed is set when the host matches the allowlist, or, for an
	// agent's shell, one of the agent's own patterns; the guard then
	// applies the allowlist mode's address rules in any mode.
	listed bool
	// modelHost is set for an open-mode tunnel to a model host, exempt
	// once its ClientHello names that host.
	modelHost bool
	counted   bool
}

func (s session) agent() string {
	if s.mode == modeAllowlist {
		return "-"
	}
	return s.slug
}

// strict reports whether the guard applies open mode's address rules:
// for a request that may reach any public host, to a host on no list.
func (s session) strict() bool {
	return (s.mode == modeOpen || s.mode == modeExempt) && !s.listed
}

// ServeHTTP dispatches CONNECT (HTTPS tunnel) and plain HTTP requests
// to their respective handlers.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		p.handleConnect(w, r)
	case r.URL != nil && r.URL.IsAbs():
		p.handleHTTP(w, r)
	default:
		http.Error(w, "proxy: expected absolute URI or CONNECT", http.StatusBadRequest)
	}
}

// allowlistRefusal follows the host name in the body of an allowlist
// or shell mode refusal, which only an agent's shell gets. It names the
// cause and nothing else: no list and no other way out, which planted
// instructions would look for. run_shell's notice matches it
// (internal/agent's ShellProxyRefusals) and says what to do.
const allowlistRefusal = " is not on the team's allow list."

// scopedRefusal follows the host name in the body of a scoped mode
// refusal, which an integration's server gets for a host its version
// does not name.
const scopedRefusal = " is not a host this integration reaches."

// admit decides a request's mode and whether it may go on to the
// dialer, writing the refusal itself when it may not. host is the
// "host:port" the client asked for. tunnel marks a CONNECT, whose
// model-host exemption waits on its ClientHello. Nothing here resolves
// the host.
func (p *proxy) admit(w http.ResponseWriter, r *http.Request, host string, tunnel bool) (session, bool) {
	var s session
	if v := r.Header.Get("Proxy-Authorization"); v != "" {
		slug, secret, ok := parseProxyAuth(v)
		var kind credKind
		if ok {
			kind, ok = p.agents.Verify(slug, secret)
		}
		if !ok {
			p.logf("DENY mode=auth host=%s reason=credentials", host)
			w.Header().Set("Proxy-Authenticate", `Basic realm="kivali"`)
			http.Error(w, "proxy credentials not accepted", http.StatusProxyAuthRequired)
			return s, false
		}
		s.slug = slug
		switch {
		case kind == credScoped:
			// Before the exemption: an agent's switch never reaches an
			// integration's credential, whatever its user is called.
			s.mode = modeScoped
		case p.agents.Exempt(slug):
			s.mode = modeExempt
		case kind == credShell:
			s.mode = modeShell
		default:
			s.mode = modeOpen
		}
	}
	if s.mode == modeScoped {
		// Its own hosts and nothing else: the team's list is for
		// agents' shells.
		if !p.agents.ScopedAllowed(s.slug, host) {
			p.logf("DENY agent=%s mode=scoped host=%s reason=hosts", s.slug, host)
			http.Error(w, hostOnly(host)+scopedRefusal, http.StatusForbidden)
			return s, false
		}
		s.listed = true
		return s, true
	}
	s.listed = p.allow.Allowed(host)

	switch s.mode {
	case modeAllowlist:
		if !s.listed {
			p.logf("DENY agent=- mode=allowlist host=%s reason=allowlist", host)
			http.Error(w, hostOnly(host)+allowlistRefusal, http.StatusForbidden)
			return s, false
		}
		return s, true
	case modeShell:
		if s.listed || p.agents.AgentAllowed(s.slug, host) {
			s.listed = true
			return s, true
		}
		// AgentAllowed re-reads a changed agents file, which may have
		// made the agent exempt since Verify.
		if !p.agents.Exempt(s.slug) {
			p.logf("DENY agent=%s mode=shell host=%s reason=allowlist", s.slug, host)
			http.Error(w, hostOnly(host)+allowlistRefusal, http.StatusForbidden)
			return s, false
		}
		s.mode = modeExempt
		return s, true
	case modeExempt:
		// Neither held to a list nor counted. A host on the allowlist or
		// among the agent's own patterns keeps the allowlist's address
		// rules, as it has for the agent's shell.
		s.listed = s.listed || matchAny(p.agents.AgentHosts(s.slug), host)
		return s, true
	}

	if tunnel && p.agents.IsModelHost(host) {
		s.modelHost = true
		return s, true
	}
	if !p.charge(&s, host) {
		http.Error(w, budgetRefusal, http.StatusForbidden)
		return s, false
	}
	return s, true
}

// charge starts counting s against its agent's budget, counting the
// hostname first: a lookup carries the name to whoever runs its
// nameserver. It reports false when the agent is held and the budget
// is enforced. A held agent's hostname is not counted again.
func (p *proxy) charge(s *session, host string) bool {
	s.counted = true
	held := p.budget.Held(s.slug)
	if !held {
		held = p.budget.Add(s.slug, host, int64(len(hostOnly(host))))
	}
	if held && p.budget.enforce {
		p.logf("DENY agent=%s mode=open host=%s reason=budget", s.slug, host)
		return false
	}
	return true
}

// dial opens the upstream connection for an admitted request, metered
// for the close log and, when counted, the budget.
func (p *proxy) dial(ctx context.Context, s session, host string) (*meteredConn, error) {
	conn, err := p.guard.Dial(ctx, host, s.strict())
	if err != nil {
		return nil, err
	}
	mc := &meteredConn{Conn: conn, slug: s.slug, host: host}
	if s.counted {
		mc.budget = p.budget
	}
	return mc, nil
}

// refuseDial answers a failed dial: 403 when the guard refused the
// destination, 502 otherwise. The detail goes to the log only; it can
// name the addresses the host resolved to.
func (p *proxy) refuseDial(w http.ResponseWriter, s session, host string, err error) {
	if errors.Is(err, errDestination) {
		p.logf("DENY agent=%s mode=%s host=%s reason=address", s.agent(), s.mode, host)
		http.Error(w, errDestination.Error(), http.StatusForbidden)
		return
	}
	p.logf("FAIL agent=%s mode=%s host=%s reason=dial err=%q", s.agent(), s.mode, host, err.Error())
	http.Error(w, "upstream dial failed", http.StatusBadGateway)
}

func (p *proxy) logConn(s session, host string, c *meteredConn) {
	p.logf("CONN agent=%s mode=%s host=%s up=%d down=%d", s.agent(), s.mode, host, c.up.Load(), c.down.Load())
}

// bufferedConn reads through the bufio.Reader the HTTP server used for
// the CONNECT request, so bytes the client sent right behind it are
// not lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// handleConnect services the HTTPS tunnel. We never decrypt: once the
// request is admitted and the guard has dialled an allowed address we
// splice TCP bytes both ways.
func (p *proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	// The authority must be on the request line. The server falls back
	// to the Host header when it is not; that form is refused.
	if r.URL == nil || r.URL.Host == "" {
		http.Error(w, "proxy: CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	host := r.URL.Host // "example.com:443"
	s, ok := p.admit(w, r, host, true)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	upstream, err := p.dial(ctx, s, host)
	cancel()
	if err != nil {
		p.refuseDial(w, s, host, err)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	raw, brw, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	client := &bufferedConn{Conn: raw, r: brw.Reader}
	// Tell the client the tunnel is up. After this we don't touch the
	// HTTP state machine — raw bytes each way.
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	if s.modelHost && !p.exemptTunnel(&s, client, upstream, host) {
		_ = client.Close()
		_ = upstream.Close()
		p.logConn(s, host, upstream)
		return
	}
	// Socket deadlines are wall-clock instants, so these use real time.
	_ = client.SetDeadline(time.Now().Add(copyDeadline))
	_ = upstream.SetDeadline(time.Now().Add(copyDeadline))
	pipe(client, upstream)
	p.logConn(s, host, upstream)
}

// exemptTunnel reads the first record a model-host tunnel's client
// sends and passes it upstream. The tunnel stays exempt only when that
// record is a ClientHello naming the CONNECT host: a front such as a
// CDN routes on that name, so any other name, none, or bytes that are
// not TLS reach somewhere else, and the tunnel is counted from its
// first byte like any open-mode connection. It reports false when the
// tunnel must close: the agent is held, or the record could not be
// passed on.
func (p *proxy) exemptTunnel(s *session, client net.Conn, upstream *meteredConn, host string) bool {
	_ = client.SetReadDeadline(time.Now().Add(dialTimeout))
	rec, err := readFirstRecord(client)
	_ = client.SetReadDeadline(time.Time{})
	if err == nil && sniMatches(rec, hostOnly(host)) {
		return writeAll(upstream, rec)
	}
	s.modelHost = false
	p.logf("COUNT agent=%s mode=open host=%s reason=sni", s.slug, host)
	if !p.charge(s, host) {
		return false
	}
	upstream.budget = p.budget
	if len(rec) > 0 && !writeAll(upstream, rec) {
		return false
	}
	// A read that failed for any reason but its deadline has nothing
	// left to tunnel.
	var ne net.Error
	return err == nil || errors.As(err, &ne) && ne.Timeout()
}

func writeAll(w io.Writer, b []byte) bool {
	_, err := w.Write(b)
	return err == nil
}

// httpDial remembers the one upstream connection, or the dial error,
// of a plain HTTP request, for the log and the response.
type httpDial struct {
	mu   sync.Mutex
	conn *meteredConn
	err  error
}

func (d *httpDial) get() (*meteredConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conn, d.err
}

// handleHTTP proxies a plain HTTP request. The host comes from the
// absolute URL; the path and query are never logged.
func (p *proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Host
	if r.URL.Port() == "" {
		port := "80"
		if strings.EqualFold(r.URL.Scheme, "https") {
			port = "443"
		}
		host = net.JoinHostPort(r.URL.Hostname(), port)
	}
	s, ok := p.admit(w, r, host, false)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.requestDeadline())
	defer cancel()
	outReq, err := http.NewRequestWithContext(ctx, r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	copyHeadersFiltered(outReq.Header, r.Header)
	outReq.Host = r.Host

	var d httpDial
	tr := &http.Transport{
		Proxy: nil,
		// Every dial goes to the request's own host through the guard,
		// whatever address the transport asks for.
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			conn, err := p.dial(ctx, s, host)
			d.mu.Lock()
			d.conn, d.err = conn, err
			d.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return conn, nil
		},
		DisableKeepAlives: true, // simpler lifecycle in a short-lived proxy
	}
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(outReq)
	upstream, dialErr := d.get()
	if err != nil {
		switch {
		case dialErr != nil:
			p.refuseDial(w, s, host, dialErr)
		case upstream != nil && upstream.closed.Load():
			p.logConn(s, host, upstream)
			http.Error(w, budgetRefusal, http.StatusForbidden)
		default:
			p.logf("FAIL agent=%s mode=%s host=%s reason=request err=%q", s.agent(), s.mode, host, requestErr(err))
			if upstream != nil {
				p.logConn(s, host, upstream)
			}
			http.Error(w, "upstream request failed", http.StatusBadGateway)
		}
		return
	}
	copyHeadersFiltered(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	_ = resp.Body.Close()
	if upstream != nil {
		p.logConn(s, host, upstream)
	}
}

// requestErr is err's text without a URL, whose path and query are
// never logged.
func requestErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// hopByHop headers are not forwarded through a proxy per RFC 7230.
// Proxy-Authorization is among them: an agent's credentials stop here.
var hopByHop = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// copyHeadersFiltered copies src into dst without the hop-by-hop
// headers or any header src's Connection header names.
func copyHeadersFiltered(dst, src http.Header) {
	named := map[string]struct{}{}
	for _, v := range src.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				named[http.CanonicalHeaderKey(tok)] = struct{}{}
			}
		}
	}
	for k, vv := range src {
		ck := http.CanonicalHeaderKey(k)
		if _, skip := hopByHop[ck]; skip {
			continue
		}
		if _, skip := named[ck]; skip {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// pipe connects two byte streams in both directions and returns once
// both copies have finished. The first to finish closes both sides so
// the other unwinds.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	// io.Copy errors on one half don't change the strategy — we still
	// signal the pair to unwind and close both sides.
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
