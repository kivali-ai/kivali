package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Client talks to the broker. Dial opens one connection to it (on
// Windows, the package's Dial: the broker's pipe, dialled so that the
// broker may impersonate the caller, and checked to be the broker's
// before anything is written); every call uses its own connection.
type Client struct {
	Dial func(ctx context.Context) (net.Conn, error)
}

// StatusError is the broker's answer other than 200: its status and
// its one sentence.
type StatusError struct {
	Status int
	Msg    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("broker: %s (%d)", e.Msg, e.Status)
}

// IsNotFound reports whether err is the broker's 404 (no such VM).
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://kivali-broker"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return c.Dial(ctx) },
		DisableKeepAlives: true,
	}}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return &StatusError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("broker: %s %s: %w", method, path, err)
	}
	return nil
}

// EnsureVM is POST /v1/vm: the VM exists with exactly this
// configuration.
func (c *Client) EnsureVM(ctx context.Context, req VMRequest) (VMState, error) {
	var st VMState
	err := c.do(ctx, http.MethodPost, "/v1/vm", req, &st)
	return st, err
}

// VM is GET /v1/vm/{name}.
func (c *Client) VM(ctx context.Context, name string) (VMState, error) {
	var st VMState
	err := c.do(ctx, http.MethodGet, "/v1/vm/"+url.PathEscape(name), nil, &st)
	return st, err
}

// Start is POST /v1/vm/{name}/start.
func (c *Client) Start(ctx context.Context, name string) (VMState, error) {
	var st VMState
	err := c.do(ctx, http.MethodPost, "/v1/vm/"+url.PathEscape(name)+"/start", struct{}{}, &st)
	return st, err
}

// Stop is POST /v1/vm/{name}/stop: graceful through the integration
// service, else turned off.
func (c *Client) Stop(ctx context.Context, name string, graceful bool) (VMState, error) {
	var st VMState
	err := c.do(ctx, http.MethodPost, "/v1/vm/"+url.PathEscape(name)+"/stop", StopRequest{Graceful: graceful}, &st)
	return st, err
}

// Remove is DELETE /v1/vm/{name}: the VM, its differencing disk and
// the broker's VM directory (never the data disk); the directory alone
// when the VM does not exist.
func (c *Client) Remove(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/vm/"+url.PathEscape(name), nil, nil)
}

// NewVHD is POST /v1/vhd: a new dynamic data disk at path, which the
// caller then has full control of.
func (c *Client) NewVHD(ctx context.Context, path string, size int64) (VHDResult, error) {
	var res VHDResult
	err := c.do(ctx, http.MethodPost, "/v1/vhd", VHDRequest{Path: path, SizeBytes: size}, &res)
	return res, err
}

// Setup is GET /v1/setup.
func (c *Client) Setup(ctx context.Context) (Setup, error) {
	var st Setup
	err := c.do(ctx, http.MethodGet, "/v1/setup", nil, &st)
	return st, err
}

// ApplySetup is POST /v1/setup.
func (c *Client) ApplySetup(ctx context.Context) (Setup, error) {
	var st Setup
	err := c.do(ctx, http.MethodPost, "/v1/setup", struct{}{}, &st)
	return st, err
}
