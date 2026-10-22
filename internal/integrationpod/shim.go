package integrationpod

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/integration"
)

// The shim's limits.
const (
	// MaxLineBytes bounds one line the server writes to its output: a
	// longer one ends the server.
	MaxLineBytes = 16 << 20
	// maxStderrLine bounds one line of the server's error output as
	// the log keeps it; the rest of a longer line is dropped.
	maxStderrLine = 8 << 10
	// handshakeTimeout bounds the MCP handshake after the server starts.
	handshakeTimeout = 20 * time.Second
	// stopTimeout is how long a server has to exit once its input is
	// closed before it is killed.
	stopTimeout = 5 * time.Second
	// minBackoff and maxBackoff bound the wait before the events stream
	// is opened again.
	minBackoff = 200 * time.Millisecond
	maxBackoff = 5 * time.Second
	// headerTimeout is how long a request waits for core's response
	// headers.
	headerTimeout = 10 * time.Second
	// shimIDPrefix starts the id of every request the shim itself
	// sends; a response with such an id is the shim's, not core's.
	shimIDPrefix = "kivali-shim-"
)

// Shim is `kivali integration-shim`, the process in an integration
// pod. It opens the events stream to core, starts the integration's
// server when core says start, and carries JSON-RPC both ways: core's
// requests to the server's input, one line each, and the server's
// output lines back to core. The server's requests for the
// connection's values and its log lines go to core too; any other
// request it makes (sampling, roots, elicitation) is answered here as
// a method that does not exist. The server's error output becomes log
// lines. The shim exits when the server does, with its status.
//
// Started as root, for a version whose manifest declares packages, the
// shim installs them and leaves root for good before it opens the
// events stream (prepare, Software); the server never runs as root.
//
// The server gets PATH, HOME=/tmp, LANG, the manifest's env with the
// connection's values put in, and the proxy variables carrying the
// scoped credential the shim draws at start, and nothing else of the
// shim's environment. Every byte the shim sends core has that
// credential scrubbed from it.
//
// Command, when set, replaces the manifest's mcp_config.command: a
// test hook (EnvCommand), so a test can run a server that is not
// Python under a manifest that passes the bundle check.
type Shim struct {
	Name    string
	Version string
	Token   string
	// CoreUDS is integration.sock's path.
	CoreUDS string
	// Bundle is the version's bundle, holding manifest.json.
	Bundle string
	// ProxyURL is the bare egress proxy URL, empty for none.
	ProxyURL string
	// Command replaces mcp_config.command when not empty.
	Command string
	// Path and Lang are the server's PATH and LANG.
	Path string
	Lang string
	// Software is set when the shim starts as root, which the pod does
	// only for a version that declares packages: before it dials core's
	// events stream the shim installs them and leaves root through it
	// (prepare). Nil for a shim that does not start as root.
	Software Software
	// Clock times the handshake, the stop and the reconnect backoff.
	Clock clock.Clock
	// Logf is the shim's own log, which goes to the container's log.
	Logf func(format string, args ...any)

	cred   *egresscred.Credential
	scrub  *egresscred.Scrubber
	stream *http.Client
	calls  *http.Client
	ids    atomic.Int64
}

// Run runs the shim until the server exits, core says shutdown, or ctx
// ends (SIGTERM), and returns the exit status: the server's when it
// exits on its own, 1 when the shim cannot start it or cannot install
// the integration's software.
func (s *Shim) Run(ctx context.Context) int {
	if s.Clock == nil {
		s.Clock = clock.New()
	}
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	if s.Bundle == "" {
		s.Bundle = DefaultBundle
	}
	m, err := s.readManifest()
	if err != nil {
		s.logf("%v", err)
		return 1
	}
	if s.ProxyURL != "" {
		cred, err := egresscred.New(s.ProxyURL, ProxyUser(s.Name))
		if err != nil {
			s.logf("proxy credential: %v", err)
			return 1
		}
		s.cred, s.scrub = cred, cred.Scrubber
	}
	s.clients()
	if !s.prepare(ctx, m) {
		if ctx.Err() != nil {
			return 0
		}
		return 1
	}

	evCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := make(chan Event, 64)
	go s.runEvents(evCtx, events)
	return s.loop(ctx, m, events)
}

// logf writes to the shim's own log with the proxy credential
// scrubbed.
func (s *Shim) logf(format string, args ...any) {
	s.Logf("%s", s.scrub.String(fmt.Sprintf(format, args...)))
}

func (s *Shim) readManifest() (*integration.Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(s.Bundle, integration.ManifestName))
	if err != nil {
		return nil, fmt.Errorf("read the bundle's manifest: %w", err)
	}
	m, err := integration.ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	if m.Name != s.Name || m.Version != s.Version {
		return nil, fmt.Errorf("the bundle is %s %s, not %s %s", m.Name, m.Version, s.Name, s.Version)
	}
	return m, nil
}

// clients builds the two HTTP clients over the Unix socket: one for
// the events stream and one for the posts. Both scrub the proxy
// credential from every request body and query.
func (s *Shim) clients() {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", s.CoreUDS)
	}
	tr := func() http.RoundTripper {
		return s.scrub.RoundTripper(&http.Transport{
			DialContext:           dial,
			IdleConnTimeout:       30 * time.Second,
			ResponseHeaderTimeout: headerTimeout,
		})
	}
	s.stream = &http.Client{Transport: tr()}
	s.calls = &http.Client{Transport: tr()}
}

func (s *Shim) url(route string) string {
	return "http://core/v1/integration/" + s.Name + "/" + route
}

// runEvents keeps the events stream open, delivering each event on
// out, and opens it again after a wait on the clock when it ends or is
// refused: 200 ms, doubling to 5 s, back to 200 ms after a stream that
// opened.
func (s *Shim) runEvents(ctx context.Context, out chan<- Event) {
	backoff := minBackoff
	for {
		opened, err := s.openEvents(ctx, out)
		if ctx.Err() != nil {
			return
		}
		if opened {
			backoff = minBackoff
		}
		if err != nil {
			s.logf("events: %v", err)
		}
		t := s.Clock.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		if !opened {
			backoff = min(backoff*2, maxBackoff)
		}
	}
}

// openEvents opens the stream and reads it until it ends. opened
// reports whether core accepted it.
func (s *Shim) openEvents(ctx context.Context, out chan<- Event) (opened bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url("events"), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(TokenHeader, s.Token)
	req.Header.Set(VersionHeader, s.Version)
	if s.cred != nil {
		req.Header.Set(ProxyHashHeader, s.cred.Hash)
	}
	resp, err := s.stream.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return false, fmt.Errorf("core refused the connection: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		return false, fmt.Errorf("events content-type %q", ct)
	}
	return true, ReadSSE(resp.Body, out, ctx.Done())
}

// ReadSSE reads SSE frames from r and delivers each on out, until r
// ends or done is closed. A frame is an event: line and data: lines;
// comment lines (heartbeats) are skipped.
func ReadSSE(r io.Reader, out chan<- Event, done <-chan struct{}) error {
	br := bufio.NewReader(r)
	var typ EventType
	var data bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "" && (typ != "" || data.Len() > 0):
			ev := Event{Type: typ, Data: append([]byte(nil), data.Bytes()...)}
			typ = ""
			data.Reset()
			select {
			case out <- ev:
			case <-done:
				return nil
			}
		case strings.HasPrefix(line, ":"):
		case line != "":
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				typ = EventType(value)
			case "data":
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(value)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// loop is the shim's life: core's events and the server's exit.
func (s *Shim) loop(ctx context.Context, m *integration.Manifest, events <-chan Event) int {
	var srv *server
	// queue holds core's messages for the server until it has started.
	var queue [][]byte
	start := func() bool {
		var err error
		srv, err = s.startServer(ctx, m)
		if err != nil {
			s.logf("%v", err)
			s.notifyLog(ctx, "error", "the server did not start: "+err.Error())
			return false
		}
		for _, msg := range queue {
			srv.write(msg)
		}
		queue = nil
		return true
	}
	for {
		var exited <-chan struct{}
		if srv != nil {
			exited = srv.done
		}
		select {
		case <-ctx.Done():
			if srv == nil {
				return 0
			}
			srv.stop(s.Clock, stopTimeout)
			return srv.code()
		case <-exited:
			s.logf("server exited: %s", srv.exitText())
			s.notifyLog(context.WithoutCancel(ctx), "info", "server exited: "+srv.exitText())
			return srv.code()
		case ev := <-events:
			switch ev.Type {
			case EventStart:
				if srv != nil {
					env, err := s.serverEnv(ctx, m)
					if err != nil || maps.Equal(env, srv.env) {
						continue
					}
					srv.stop(s.Clock, stopTimeout)
					srv = nil
				}
				if !start() {
					return 1
				}
			case EventRestart:
				if srv != nil {
					srv.stop(s.Clock, stopTimeout)
					srv = nil
				}
				if !start() {
					return 1
				}
			case EventRPC:
				if srv == nil {
					queue = append(queue, ev.Data)
					continue
				}
				srv.write(ev.Data)
			case EventShutdown:
				if srv != nil {
					srv.stop(s.Clock, stopTimeout)
				}
				return 0
			}
		}
	}
}

// serverEnv is the manifest's env with the connection's values put
// in, as core gives them; with no connection, one that needs the owner
// to sign in, or one whose vendor did not answer a due refresh, every
// value is empty, and the server asks kivali/credential at call time.
func (s *Shim) serverEnv(ctx context.Context, m *integration.Manifest) (map[string]string, error) {
	resp, err := s.request(ctx, MethodCredential, nil)
	if err != nil {
		return nil, fmt.Errorf("ask core for the connection: %w", err)
	}
	if resp.Error != nil {
		switch resp.Error.Code {
		case CodeNoConnection, CodeSignInNeeded, CodeVendorUnavailable:
			return integration.ServerEnv(m, nil), nil
		}
		return nil, fmt.Errorf("ask core for the connection: %s", resp.Error.Message)
	}
	var cred Credential
	if err := json.Unmarshal(resp.Result, &cred); err != nil {
		return nil, fmt.Errorf("core's answer for the connection: %w", err)
	}
	if cred.Env == nil {
		cred.Env = map[string]string{}
	}
	return cred.Env, nil
}

// environ is the server's whole environment.
func (s *Shim) environ(env map[string]string) []string {
	out := []string{"PATH=" + s.Path, "HOME=/tmp"}
	if s.Lang != "" {
		out = append(out, "LANG="+s.Lang)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	if s.cred != nil {
		out = append(out, s.cred.Env()...)
	}
	return out
}

// startServer fetches the connection's values, starts the server and
// runs the MCP handshake on it.
func (s *Shim) startServer(ctx context.Context, m *integration.Manifest) (*server, error) {
	env, err := s.serverEnv(ctx, m)
	if err != nil {
		return nil, err
	}
	command := m.Server.MCPConfig.Command
	if s.Command != "" {
		command = s.Command
	}
	cmd := exec.Command(command, m.Server.MCPConfig.Args...)
	cmd.Dir = s.Bundle
	cmd.Env = s.environ(env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the server: %w", err)
	}
	srv := &server{cmd: cmd, stdin: stdin, env: env, done: make(chan struct{}), own: map[string]chan Message{}}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		s.readOutput(ctx, srv, stdout)
	}()
	go func() {
		defer readers.Done()
		readLines(stderr, maxStderrLine, func(line string) {
			s.notifyLog(ctx, "stderr", line)
		})
	}()
	go func() {
		readers.Wait()
		srv.waitErr = cmd.Wait()
		close(srv.done)
	}()
	if err := s.handshake(ctx, srv); err != nil {
		srv.kill()
		<-srv.done
		return nil, err
	}
	s.logf("server started")
	s.notifyLog(ctx, "info", "server started")
	return srv, nil
}

// handshake is MCP's initialize and initialized, within
// handshakeTimeout.
func (s *Shim) handshake(ctx context.Context, srv *server) error {
	id := s.newID()
	ch := srv.expect(id)
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "kivali", "version": s.Version},
	}
	req, err := json.Marshal(Message{JSONRPC: "2.0", ID: id, Method: "initialize", Params: mustJSON(params)})
	if err != nil {
		return err
	}
	srv.write(req)
	t := s.Clock.NewTimer(handshakeTimeout)
	defer t.Stop()
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("the server refused initialize: %s", resp.Error.Message)
		}
	case <-t.C():
		return fmt.Errorf("the server did not answer initialize within %s", handshakeTimeout)
	case <-srv.done:
		return fmt.Errorf("the server exited during its handshake: %s", srv.exitText())
	case <-ctx.Done():
		return ctx.Err()
	}
	note, _ := json.Marshal(Message{JSONRPC: "2.0", Method: "notifications/initialized"})
	srv.write(note)
	return nil
}

// readOutput reads the server's output a line at a time. A response is
// core's, unless the shim sent the request; a request for the
// connection's values or a log line goes to core and core's answer to
// the server; any other request is answered as a method that does not
// exist; a log notification goes to core and any other notification
// is dropped. A line that is not JSON-RPC, or longer than
// MaxLineBytes, ends the server.
func (s *Shim) readOutput(ctx context.Context, srv *server, r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), MaxLineBytes)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil || (!msg.IsRequest() && !msg.IsNotification() && !msg.IsResponse()) {
			s.logf("the server wrote a line that is not JSON-RPC; stopping it")
			s.notifyLog(ctx, "error", "the server wrote a line that is not JSON-RPC; Kivali stopped it")
			srv.kill()
			_, _ = io.Copy(io.Discard, r)
			return
		}
		line = append([]byte(nil), line...)
		switch {
		case msg.IsResponse():
			if srv.deliver(msg) {
				continue
			}
			if _, err := s.post(ctx, line); err != nil {
				s.logf("relay a response to core: %v", err)
			}
		case msg.IsRequest() && (msg.Method == MethodCredential || msg.Method == MethodLog):
			body, err := s.post(ctx, line)
			if err != nil || len(body) == 0 {
				s.logf("relay %s to core: %v", msg.Method, err)
				reply, _ := Response(msg.ID, nil, &RPCError{Code: -32603, Message: "Kivali did not answer"})
				srv.write(reply)
				continue
			}
			srv.write(body)
		case msg.IsRequest():
			reply, _ := Response(msg.ID, nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + msg.Method})
			srv.write(reply)
		case msg.Method == MethodLog:
			if _, err := s.post(ctx, line); err != nil {
				s.logf("relay a log line to core: %v", err)
			}
		}
	}
	if err := sc.Err(); err != nil {
		s.logf("the server's output: %v; stopping it", err)
		s.notifyLog(ctx, "error", fmt.Sprintf("the server wrote a line over %d MiB; Kivali stopped it", MaxLineBytes>>20))
		srv.kill()
		_, _ = io.Copy(io.Discard, r)
	}
}

// readLines calls fn with each line of r, cut to max bytes.
func readLines(r io.Reader, max int, fn func(string)) {
	br := bufio.NewReaderSize(r, max)
	skipping := false
	for {
		b, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if !skipping {
				fn(string(b))
				skipping = true
			}
			continue
		}
		if line := strings.TrimRight(string(b), "\r\n"); line != "" && !skipping {
			fn(line)
		}
		skipping = false
		if err != nil {
			return
		}
	}
}

func (s *Shim) newID() json.RawMessage {
	return mustJSON(shimIDPrefix + strconv.FormatInt(s.ids.Add(1), 10))
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// post sends one JSON-RPC message to core and returns core's answer:
// the response to a request, empty for anything else.
func (s *Shim) post(ctx context.Context, msg []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url("rpc"), bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TokenHeader, s.Token)
	resp, err := s.calls.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxLineBytes))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return bytes.TrimSpace(body), nil
	case http.StatusNoContent:
		return nil, nil
	}
	return nil, fmt.Errorf("core answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// request sends core a request of the shim's own and returns core's
// response.
func (s *Shim) request(ctx context.Context, method string, params any) (Message, error) {
	msg := Message{JSONRPC: "2.0", ID: s.newID(), Method: method}
	if params != nil {
		msg.Params = mustJSON(params)
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return Message{}, err
	}
	body, err := s.post(ctx, b)
	if err != nil {
		return Message{}, err
	}
	var resp Message
	if err := json.Unmarshal(body, &resp); err != nil {
		return Message{}, fmt.Errorf("core's answer: %w", err)
	}
	return resp, nil
}

// notifyLog sends core one log line. One that cannot be sent goes to
// the shim's own log instead.
func (s *Shim) notifyLog(ctx context.Context, level, message string) {
	b, err := json.Marshal(Message{JSONRPC: "2.0", Method: MethodLog, Params: mustJSON(LogParams{Level: level, Message: message})})
	if err != nil {
		return
	}
	if _, err := s.post(ctx, b); err != nil {
		s.logf("log (%s): %s", level, message)
	}
}

// server is one run of the integration's server.
type server struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// env is the manifest's env as it was started with, so a start
	// event can tell whether the connection's values have changed.
	env map[string]string
	// done is closed once the server has exited and its output has
	// been read; waitErr is then its exit.
	done    chan struct{}
	waitErr error

	wmu sync.Mutex
	mu  sync.Mutex
	// own holds the shim's own requests waiting for a response, by id.
	own map[string]chan Message
}

// write sends one line to the server's input. A server that has gone
// takes nothing; its exit is seen on done.
func (srv *server) write(line []byte) {
	srv.wmu.Lock()
	defer srv.wmu.Unlock()
	_, _ = srv.stdin.Write(append(bytes.TrimSpace(line), '\n'))
}

// expect registers a request of the shim's own.
func (srv *server) expect(id json.RawMessage) <-chan Message {
	ch := make(chan Message, 1)
	srv.mu.Lock()
	srv.own[string(id)] = ch
	srv.mu.Unlock()
	return ch
}

// deliver hands a response to the shim's own request it answers, and
// reports whether there was one. A response with the shim's id prefix
// is the shim's even when nothing waits for it any more.
func (srv *server) deliver(msg Message) bool {
	srv.mu.Lock()
	ch, ok := srv.own[string(msg.ID)]
	delete(srv.own, string(msg.ID))
	srv.mu.Unlock()
	if ok {
		ch <- msg
		return true
	}
	var id string
	return json.Unmarshal(msg.ID, &id) == nil && strings.HasPrefix(id, shimIDPrefix)
}

func (srv *server) kill() {
	if srv.cmd.Process != nil {
		_ = srv.cmd.Process.Kill()
	}
}

// stop closes the server's input and gives it timeout to exit before
// it is killed, and returns once it has gone.
func (srv *server) stop(clk clock.Clock, timeout time.Duration) {
	srv.wmu.Lock()
	_ = srv.stdin.Close()
	srv.wmu.Unlock()
	t := clk.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-srv.done:
		return
	case <-t.C():
	}
	srv.kill()
	<-srv.done
}

// code is the server's exit status as the shim's: 1 for a server
// ended by a signal.
func (srv *server) code() int {
	st := srv.cmd.ProcessState
	if st == nil || st.ExitCode() < 0 {
		return 1
	}
	return st.ExitCode()
}

func (srv *server) exitText() string {
	if srv.waitErr == nil {
		return "exit status 0"
	}
	return srv.waitErr.Error()
}
