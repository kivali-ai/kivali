package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// Event is one line of an operation's NDJSON response: a log line, or
// the final line with Done set.
type Event struct {
	Log string `json:"log,omitempty"`
	// Stage is the operation's progress stage when the line was logged
	// (StageStarting, ...); "" for an operation without stages.
	Stage  string          `json:"stage,omitempty"`
	Done   bool            `json:"done,omitempty"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// DownRequest is the body of POST /v1/down.
type DownRequest struct {
	// Exit also ends `serve` once the VM is down.
	Exit bool `json:"exit,omitempty"`
}

// LoadImagesRequest is the body of POST /v1/load-images: the image
// archive to import, a file serve can read (the CLI writes stdin to
// one in the config directory).
type LoadImagesRequest struct {
	Path string `json:"path"`
}

// RestartRequest is the body of POST /v1/restart.
type RestartRequest struct{}

// ForwardRequest is the body of POST /v1/forward.
type ForwardRequest struct {
	Port int `json:"port,omitempty"`
}

// CheckRequest is the body of POST /v1/check.
type CheckRequest struct {
	Feed string `json:"feed,omitempty"`
}

// RPCServer serves the supervisor on its RPC endpoint (Host.Listen).
type RPCServer struct {
	sup      *Supervisor
	quit     chan struct{}
	quitOnce sync.Once
}

// NewRPCServer wraps sup.
func NewRPCServer(sup *Supervisor) *RPCServer {
	return &RPCServer{sup: sup, quit: make(chan struct{})}
}

// Quit is closed when a client asked serve to exit (down --exit,
// destroy --exit).
func (r *RPCServer) Quit() <-chan struct{} { return r.quit }

// Handler is the RPC's HTTP handler.
func (r *RPCServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.sup.Status(req.Context()))
	})
	// opCtx registers an operation. Operations run to completion even if
	// the client goes away (a half-done boot or upgrade helps nobody);
	// with withRequest they get the request's context, for the few that
	// may be abandoned (down's wait behind an upgrade, a backup).
	opCtx := func(name string, withRequest bool, body func() any, fn func(ctx context.Context, body any, logf Logf) (any, error)) {
		mux.HandleFunc("POST /v1/"+name, func(w http.ResponseWriter, req *http.Request) {
			b := body()
			if req.ContentLength != 0 {
				if err := json.NewDecoder(req.Body).Decode(b); err != nil && !errors.Is(err, io.EOF) {
					http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
					return
				}
			}
			ctx := context.Background()
			if withRequest {
				ctx = req.Context()
			}
			ctx, stage := withStage(ctx)
			stream(w, stage, func(logf Logf) (any, error) { return fn(ctx, b, logf) })
		})
	}
	op := func(name string, body func() any, fn func(ctx context.Context, body any, logf Logf) (any, error)) {
		opCtx(name, false, body, fn)
	}
	op("up", func() any { return &UpOptions{} }, func(ctx context.Context, b any, logf Logf) (any, error) {
		if err := r.sup.Up(ctx, *b.(*UpOptions), logf); err != nil {
			return nil, err
		}
		return r.sup.Status(ctx), nil
	})
	opCtx("down", true, func() any { return &DownRequest{} }, func(ctx context.Context, b any, logf Logf) (any, error) {
		err := r.sup.Down(ctx, logf)
		if err == nil && b.(*DownRequest).Exit {
			logf("serve is exiting")
			r.quitOnce.Do(func() { close(r.quit) })
		}
		return nil, err
	})
	op("install", func() any { return &InstallOptions{} }, func(ctx context.Context, b any, logf Logf) (any, error) {
		return nil, r.sup.Install(ctx, *b.(*InstallOptions), logf)
	})
	op("load-images", func() any { return &LoadImagesRequest{} }, func(ctx context.Context, b any, logf Logf) (any, error) {
		p := b.(*LoadImagesRequest).Path
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("load-images: %q is not an absolute path", p)
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("load-images: %w", err)
		}
		defer func() { _ = f.Close() }()
		return nil, r.sup.LoadImages(ctx, f, logf)
	})
	op("restart", func() any { return &RestartRequest{} }, func(ctx context.Context, _ any, logf Logf) (any, error) {
		return nil, r.sup.Restart(ctx, logf)
	})
	op("forward", func() any { return &ForwardRequest{} }, func(ctx context.Context, b any, logf Logf) (any, error) {
		return nil, r.sup.Forward(ctx, b.(*ForwardRequest).Port, logf)
	})
	op("check", func() any { return &CheckRequest{} }, func(ctx context.Context, b any, logf Logf) (any, error) {
		return r.sup.Check(ctx, b.(*CheckRequest).Feed, logf)
	})
	op("upgrade", func() any { return &UpgradeOptions{} }, func(ctx context.Context, b any, logf Logf) (any, error) {
		if err := r.sup.Upgrade(ctx, *b.(*UpgradeOptions), logf); err != nil {
			return nil, err
		}
		return r.sup.Status(ctx), nil
	})
	opCtx("backup", true, func() any { return &BackupRequest{} }, func(ctx context.Context, b any, logf Logf) (any, error) {
		return r.sup.BackupToFile(ctx, b.(*BackupRequest).Path, logf)
	})
	op("restore", func() any { return &RestoreRequest{} }, func(ctx context.Context, b any, logf Logf) (any, error) {
		if err := r.sup.RestoreFromFile(ctx, b.(*RestoreRequest).Path, logf); err != nil {
			return nil, err
		}
		return r.sup.Status(ctx), nil
	})
	op("address", func() any { return &AddressRequest{} }, func(ctx context.Context, b any, logf Logf) (any, error) {
		return nil, r.sup.SetAddress(ctx, *b.(*AddressRequest), logf)
	})
	// destroy, like down, may abandon only its wait for the lock.
	opCtx("destroy", true, func() any { return &DestroyRequest{} }, func(ctx context.Context, b any, logf Logf) (any, error) {
		res, err := r.sup.Destroy(ctx, logf)
		if err == nil && b.(*DestroyRequest).Exit {
			logf("serve is exiting")
			r.quitOnce.Do(func() { close(r.quit) })
		}
		return res, err
	})
	mux.HandleFunc("GET /v1/credential", func(w http.ResponseWriter, req *http.Request) {
		st, err := r.sup.Credential(req.Context())
		var nr *NotReadyError
		switch {
		case errors.As(err, &nr):
			http.Error(w, nr.Msg, http.StatusConflict)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	// The sign-in setup and the provider clear, like the credential,
	// answer plain JSON and never take the lock. A setup's values carry
	// secrets, which nothing here logs or echoes.
	answer := func(w http.ResponseWriter, res any, err error) {
		if err != nil {
			code, msg := errorStatus(err)
			http.Error(w, msg, code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(res)
	}
	mux.HandleFunc("GET /v1/credential/setup", func(w http.ResponseWriter, req *http.Request) {
		res, err := r.sup.SetupInfo(req.Context())
		answer(w, res, err)
	})
	mux.HandleFunc("POST /v1/credential/setup", func(w http.ResponseWriter, req *http.Request) {
		var body SetupRequest
		if err := json.NewDecoder(io.LimitReader(req.Body, 64<<10)).Decode(&body); err != nil {
			http.Error(w, "bad request: the body is not a sign-in setup", http.StatusBadRequest)
			return
		}
		res, err := r.sup.ApplySetup(req.Context(), body)
		answer(w, res, err)
	})
	mux.HandleFunc("POST /v1/credential/clear-provider", func(w http.ResponseWriter, req *http.Request) {
		res, err := r.sup.ClearProvider(req.Context())
		answer(w, res, err)
	})
	// handoff is not an operation: it answers plain JSON and never
	// takes the lock. The body is {} and carries nothing.
	mux.HandleFunc("POST /v1/handoff", func(w http.ResponseWriter, req *http.Request) {
		res, err := r.sup.Handoff(req.Context())
		var nr *NotReadyError
		switch {
		case errors.As(err, &nr):
			http.Error(w, nr.Msg, http.StatusConflict)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("GET /v1/terminal", func(w http.ResponseWriter, req *http.Request) {
		rows, _ := strconv.Atoi(req.URL.Query().Get("rows"))
		cols, _ := strconv.Atoi(req.URL.Query().Get("cols"))
		shell := req.URL.Query().Get("shell") == "1"
		relay(w, req, func() (net.Conn, error) {
			return r.sup.Terminal(context.Background(), uint16(rows), uint16(cols), shell)
		}, r.sup.terminalAttached)
	})
	mux.HandleFunc("GET /v1/exec", func(w http.ResponseWriter, req *http.Request) {
		var argv []string
		if err := json.Unmarshal([]byte(req.URL.Query().Get("argv")), &argv); err != nil || len(argv) == 0 {
			http.Error(w, "argv: want a non-empty JSON array", http.StatusBadRequest)
			return
		}
		relay(w, req, func() (net.Conn, error) { return r.sup.Exec(context.Background(), argv) }, nil)
	})
	return mux
}

// relay answers a streaming request by opening a guest stream and
// splicing the client to it: the client and the guest speak the same
// frames, and serve only relays them. attached, when set, is called as
// the client is attached, and what it returns once the splice ends.
func relay(w http.ResponseWriter, req *http.Request, open func() (net.Conn, error), attached func() (detached func())) {
	if req.Header.Get("Upgrade") != guestapi.UpgradeToken {
		http.Error(w, "needs Upgrade: "+guestapi.UpgradeToken, http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack", http.StatusInternalServerError)
		return
	}
	up, err := open()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	// Counted before the client hears it is attached.
	if attached != nil {
		detached := attached()
		defer detached()
	}
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + guestapi.UpgradeToken + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		_ = up.Close()
		return
	}
	guestapi.Splice(conn, rw.Reader, up)
}

// stream runs fn, sending its log lines, each with the operation's
// stage at the time, and its result as NDJSON.
func stream(w http.ResponseWriter, stage *stageHolder, fn func(logf Logf) (any, error)) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	send := func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(ev)
		if fl != nil {
			fl.Flush()
		}
	}
	res, err := fn(func(format string, args ...any) {
		send(Event{Log: fmt.Sprintf(format, args...), Stage: stage.get()})
	})
	final := Event{Done: true}
	if err != nil {
		final.Error = err.Error()
	} else if res != nil {
		final.Result, _ = json.Marshal(res)
	}
	send(final)
}

// Client talks to a running `serve` over the RPC endpoint of a config
// directory.
type Client struct {
	Host Host
	Dir  string
}

// Endpoint names the RPC endpoint, for messages.
func (c *Client) Endpoint() string { return c.Host.Endpoint(c.Dir) }

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	return c.Host.Dial(ctx, c.Dir)
}

func (c *Client) http() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return c.dial(ctx) },
	}}
}

// Ping reports whether serve answers.
func (c *Client) Ping(ctx context.Context) bool {
	conn, err := c.dial(ctx)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Status fetches the status report.
func (c *Client) Status(ctx context.Context) (Report, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://kivali/v1/status", nil)
	if err != nil {
		return Report{}, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return Report{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var r Report
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return Report{}, err
	}
	return r, nil
}

// Credential fetches the credential status. A 409 (the org is not
// running) comes back as the supervisor's sentence.
func (c *Client) Credential(ctx context.Context) (CredentialStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://kivali/v1/credential", nil)
	if err != nil {
		return CredentialStatus{}, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return CredentialStatus{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return CredentialStatus{}, errors.New(strings.TrimSpace(string(msg)))
	}
	var st CredentialStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return CredentialStatus{}, err
	}
	return st, nil
}

// call sends a plain-JSON request and decodes the answer into out; an
// answer other than 200 comes back as the supervisor's sentence.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://kivali"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.New(strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// SetupInfo fetches the model ids Kivali runs and the sign-in setup
// saved now, if any.
func (c *Client) SetupInfo(ctx context.Context) (SetupInfo, error) {
	var res SetupInfo
	return res, c.call(ctx, http.MethodGet, "/v1/credential/setup", nil, &res)
}

// ApplySetup signs the org's CLI in with a sign-in setup and checks
// each model.
func (c *Client) ApplySetup(ctx context.Context, r SetupRequest) (SetupResult, error) {
	var res SetupResult
	return res, c.call(ctx, http.MethodPost, "/v1/credential/setup", r, &res)
}

// ClearProvider removes every cloud provider's settings variables.
func (c *Client) ClearProvider(ctx context.Context) (ClearResult, error) {
	var res ClearResult
	return res, c.call(ctx, http.MethodPost, "/v1/credential/clear-provider", struct{}{}, &res)
}

// Handoff mints a one-time sign-in token for the org's owner. A 409
// (the org is not running) comes back as the supervisor's sentence.
func (c *Client) Handoff(ctx context.Context) (HandoffResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://kivali/v1/handoff", strings.NewReader("{}"))
	if err != nil {
		return HandoffResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return HandoffResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return HandoffResult{}, errors.New(strings.TrimSpace(string(msg)))
	}
	var res HandoffResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return HandoffResult{}, err
	}
	return res, nil
}

// Op runs an operation, calling onLog for every log line, and returns
// its result.
func (c *Client) Op(ctx context.Context, name string, body any, onLog func(string)) (json.RawMessage, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://kivali/v1/"+name, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: %s", resp.Status, msg)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var ev Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("bad event %q: %w", sc.Text(), err)
		}
		if ev.Done {
			if ev.Error != "" {
				return nil, errors.New(ev.Error)
			}
			return ev.Result, nil
		}
		if onLog != nil {
			onLog(ev.Log)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("serve closed the stream before the operation finished")
}

// Exec runs argv in the guest through serve and returns its exit code.
func (c *Client) Exec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return -1, err
	}
	b, err := json.Marshal(argv)
	if err != nil {
		return -1, err
	}
	s, err := guestapi.Upgrade(ctx, conn, "/v1/exec?"+url.Values{"argv": {string(b)}}.Encode())
	if err != nil {
		return -1, err
	}
	return guestapi.RunStream(ctx, s, stdin, stdout, stderr)
}

// Terminal attaches a terminal running Claude Code, or bash when shell
// is set; the stream speaks the guest agent's PTY frames.
func (c *Client) Terminal(ctx context.Context, rows, cols uint16, shell bool) (net.Conn, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"rows": {strconv.Itoa(int(rows))}, "cols": {strconv.Itoa(int(cols))}}
	if shell {
		q.Set("shell", "1")
	}
	return guestapi.Upgrade(ctx, conn, "/v1/terminal?"+q.Encode())
}
