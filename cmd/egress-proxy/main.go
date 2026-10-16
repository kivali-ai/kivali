// Package main is the Kivali egress proxy.
//
// A small forward proxy that mediates HTTP and HTTPS (via CONNECT)
// traffic from per-agent agent pods to the internet. Traffic is
// allowed only if the requested host matches a pattern in the
// allowlist YAML file (hot-reloaded; see allowlist.go).
//
// Deployment model: run alongside the Kivali main container in the
// same Pod (shared emptyDir for the allowlist file). Agent pods set
// HTTP_PROXY / HTTPS_PROXY env vars pointing at the Service in front
// of this process.
//
// Enforcement is intentionally HTTP-layer only — an agent that
// chooses to bypass $HTTP_PROXY and dial a raw socket is unaffected.
// Hard enforcement (NetworkPolicy / DNS control) is a follow-up once
// we bring a policy-capable CNI into the cluster.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	defaultListen    = ":3128"
	defaultAllowlist = "/etc/egress/allowlist.yaml"
	// dialTimeout bounds the time spent reaching out to an allowed
	// upstream before giving up, so a slow destination doesn't pin a
	// proxy goroutine.
	dialTimeout = 10 * time.Second
	// copyDeadline bounds an individual allowed connection's total
	// life. Long-lived websockets aren't a use case here — agents are
	// running short-lived commands (apt/pip/curl/git).
	copyDeadline = 10 * time.Minute
)

func main() {
	listen := flag.String("listen", defaultListen, "address to listen on")
	allowlistPath := flag.String("allowlist", defaultAllowlist, "path to allowlist YAML (hot-reloaded)")
	flag.Parse()

	var allow Allowlist
	if err := allow.Load(*allowlistPath); err != nil {
		log.Fatalf("load allowlist: %v", err)
	}
	log.Printf("egress-proxy loaded %d patterns from %s", len(allow.Patterns()), *allowlistPath)
	watcher := &Watcher{Path: *allowlistPath, Interval: 5 * time.Second}
	go watcher.Run(&allow, log.Printf)

	h := &proxy{allow: &allow}
	srv := &http.Server{
		Addr:              *listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// ConnState used for observability in the future; leaving a
		// hook in place rather than chasing it down later.
		ConnState: nil,
	}
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	log.Printf("egress-proxy listening on %s", *listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}

type proxy struct {
	allow *Allowlist
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

// handleConnect services the HTTPS tunnel. We never decrypt: once the
// host passes the allowlist check we splice TCP bytes both ways.
func (p *proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host := r.Host // "example.com:443"
	if !p.allow.Allowed(host) {
		log.Printf("DENY CONNECT %s", host)
		http.Error(w, "host not in allowlist", http.StatusForbidden)
		return
	}
	log.Printf("ALLOW CONNECT %s", host)

	upstream, err := net.DialTimeout("tcp", host, dialTimeout)
	if err != nil {
		http.Error(w, fmt.Sprintf("upstream dial: %v", err), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		_ = upstream.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Tell the client the tunnel is up. After this we don't touch the
	// HTTP state machine — raw bytes each way.
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	_ = client.SetDeadline(time.Now().Add(copyDeadline))
	_ = upstream.SetDeadline(time.Now().Add(copyDeadline))
	pipe(client, upstream)
}

// handleHTTP proxies a plain HTTP request. We pull the Host from the
// URL (which is absolute in proxy requests) and check the allowlist
// before forwarding.
func (p *proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Host
	if !p.allow.Allowed(host) {
		log.Printf("DENY HTTP %s %s %s", r.Method, host, r.URL.Path)
		http.Error(w, "host not in allowlist", http.StatusForbidden)
		return
	}
	log.Printf("ALLOW HTTP %s %s %s", r.Method, host, r.URL.Path)
	// Build the outbound request. Strip hop-by-hop headers; keep the
	// absolute URL handling to net/http.
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	copyHeadersFiltered(outReq.Header, r.Header)
	outReq.Host = r.Host
	tr := &http.Transport{
		Proxy:             nil,
		DialContext:       (&net.Dialer{Timeout: dialTimeout}).DialContext,
		DisableKeepAlives: true, // simpler lifecycle in a short-lived proxy
	}
	resp, err := tr.RoundTrip(outReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeadersFiltered(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// hopByHop headers are not forwarded through a proxy per RFC 7230.
var hopByHop = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

func copyHeadersFiltered(dst, src http.Header) {
	for k, vv := range src {
		if _, skip := hopByHop[http.CanonicalHeaderKey(k)]; skip {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// pipe connects two byte streams in both directions and returns once
// either side closes. Both goroutines close their counterpart when
// their source is exhausted so the pair exits cleanly.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	// io.Copy errors on one half don't change the strategy — we still
	// signal the pair to unwind and close both sides. Discard the
	// errors explicitly so the linter stops flagging them.
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
	_ = a.Close()
	_ = b.Close()
}
