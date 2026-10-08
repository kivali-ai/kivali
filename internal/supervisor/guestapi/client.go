package guestapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var timeZero time.Time

// Client talks to the guest agent. Dial opens one new connection to the
// agent (a vsock connection to Port in production); every call uses its
// own connection.
type Client struct {
	Dial func(ctx context.Context) (net.Conn, error)
}

func (c *Client) httpClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return c.Dial(ctx) },
		DisableKeepAlives: true,
	}}
}

func agentURL(p string, q url.Values) string {
	u := "http://kivali-guest" + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func errorFromResponse(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return fmt.Errorf("guest agent: %s: %s", resp.Status, strings.TrimSpace(string(b)))
}

// Status asks for the guest's status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, agentURL(PathStatus, nil), nil)
	if err != nil {
		return Status{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Status{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Status{}, errorFromResponse(resp)
	}
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return Status{}, fmt.Errorf("guest agent: status: %w", err)
	}
	return st, nil
}

// WriteFile writes r to rel, a path relative to the guest's data
// directory, atomically (temporary file and rename), with mode.
func (c *Client) WriteFile(ctx context.Context, rel string, mode os.FileMode, r io.Reader) error {
	q := url.Values{"path": {rel}, "mode": {strconv.FormatUint(uint64(mode.Perm()), 8)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, agentURL(PathFile, q), r)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return errorFromResponse(resp)
	}
	return nil
}

// ImportImages streams an uncompressed image tar into `k3s ctr images
// import`.
func (c *Client) ImportImages(ctx context.Context, r io.Reader) (ImportResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL(PathImport, nil), r)
	if err != nil {
		return ImportResult{}, err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return ImportResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ImportResult{}, errorFromResponse(resp)
	}
	var res ImportResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return ImportResult{}, fmt.Errorf("guest agent: import: %w", err)
	}
	return res, nil
}

// Shutdown asks the guest for its clean shutdown. The guest answers
// before it starts; the caller waits for the VM to stop.
func (c *Client) Shutdown(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL(PathShutdown, nil), nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		return errorFromResponse(resp)
	}
	return nil
}

// Upgrade opens a streaming request and returns the connection after
// the agent's 101. It is exported for the supervisor's own socket,
// which uses the same handshake.
func Upgrade(ctx context.Context, conn net.Conn, target string) (net.Conn, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// Cancelling ctx during the handshake abandons it.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	conn, err := upgradeHandshake(conn, target)
	if !stop() && err == nil {
		// ctx ended as the handshake finished and closed the conn.
		return nil, ctx.Err()
	}
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return conn, err
}

func upgradeHandshake(conn net.Conn, target string) (net.Conn, error) {
	req := "GET " + target + " HTTP/1.1\r\nHost: kivali\r\nConnection: Upgrade\r\nUpgrade: " + UpgradeToken + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		err := errorFromResponse(resp)
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(timeZero)
	return &bufferedConn{Conn: conn, r: br}, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (c *Client) upgrade(ctx context.Context, p string, q url.Values) (net.Conn, error) {
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, err
	}
	target := p
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	return Upgrade(ctx, conn, target)
}

// Proxy opens a raw byte stream to 127.0.0.1:port inside the guest.
func (c *Client) Proxy(ctx context.Context, port int) (net.Conn, error) {
	return c.upgrade(ctx, PathProxy, url.Values{"port": {strconv.Itoa(port)}})
}

// OpenExec starts argv in the guest and returns the frame stream (see
// the frame types); RunExec is the usual way to use it.
func (c *Client) OpenExec(ctx context.Context, spec ExecSpec) (net.Conn, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	return c.upgrade(ctx, PathExec, url.Values{"spec": {string(b)}})
}

// OpenPTY starts argv on a PTY in the guest and returns the frame
// stream: FrameStdin and FrameResize in, FrameStdout and FrameExit out.
func (c *Client) OpenPTY(ctx context.Context, spec ExecSpec) (net.Conn, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	return c.upgrade(ctx, PathPTY, url.Values{"spec": {string(b)}})
}

// Exec runs argv in the guest, feeding stdin (may be nil) and copying
// its output, and returns its exit code. Cancelling ctx closes the
// stream, which kills the process.
func (c *Client) Exec(ctx context.Context, spec ExecSpec, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	conn, err := c.OpenExec(ctx, spec)
	if err != nil {
		return -1, err
	}
	return RunStream(ctx, conn, stdin, stdout, stderr)
}

// RunStream drives an open exec stream to completion: it sends stdin
// (then FrameStdinClose), copies FrameStdout and FrameStderr, and
// returns the code from FrameExit. It closes conn.
func RunStream(ctx context.Context, conn net.Conn, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	fw := NewFrameWriter(conn)
	if stdin != nil {
		go func() {
			_, _ = io.Copy(fw.Writer(FrameStdin), stdin)
			_ = fw.Frame(FrameStdinClose, nil)
		}()
	} else if err := fw.Frame(FrameStdinClose, nil); err != nil {
		return -1, err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	for {
		typ, p, err := ReadFrame(conn)
		if err != nil {
			if ctx.Err() != nil {
				return -1, ctx.Err()
			}
			return -1, fmt.Errorf("guest agent: exec stream: %w", err)
		}
		switch typ {
		case FrameStdout:
			if _, err := stdout.Write(p); err != nil {
				return -1, err
			}
		case FrameStderr:
			if _, err := stderr.Write(p); err != nil {
				return -1, err
			}
		case FrameExit:
			var ex Exit
			if err := json.Unmarshal(p, &ex); err != nil {
				return -1, err
			}
			// stdin may still be blocked reading from the caller; it
			// is the caller's reader, so it is not waited for.
			if ex.Error != "" {
				return ex.Code, errors.New("guest agent: " + ex.Error)
			}
			return ex.Code, nil
		}
	}
}
