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
// the broker's VM directory, which stays while it holds the data disk
// or its snapshot (DeleteVHD deletes those); the directory alone when
// the VM does not exist.
func (c *Client) Remove(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/vm/"+url.PathEscape(name), nil, nil)
}

// IsAlreadyImported reports whether err is POST /v1/vhd/import's answer
// that the VM directory holds a data disk already.
func IsAlreadyImported(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusConflict && strings.HasPrefix(se.Msg, AlreadyImported)
}

// NewVHD is POST /v1/vhd: a new dynamic data disk for the team of
// configDir, in its VM directory, replacing one there only with
// replace.
func (c *Client) NewVHD(ctx context.Context, configDir string, size int64, replace bool) (VHDResult, error) {
	var res VHDResult
	err := c.do(ctx, http.MethodPost, "/v1/vhd", VHDRequest{ConfigDir: configDir, SizeBytes: size, Replace: replace}, &res)
	return res, err
}

// OptimizeVHD is POST /v1/vhd/optimize: the data disk of the team of
// configDir, its VM off, compacted.
func (c *Client) OptimizeVHD(ctx context.Context, configDir string) (OptimizeResult, error) {
	var res OptimizeResult
	err := c.do(ctx, http.MethodPost, "/v1/vhd/optimize", DiskRequest{ConfigDir: configDir}, &res)
	return res, err
}

// ImportVHD is POST /v1/vhd/import: data.vhdx in configDir, the data
// disk's place before the broker kept it, copied into the team's VM
// directory as its data disk (IsAlreadyImported when one is there).
func (c *Client) ImportVHD(ctx context.Context, configDir string) (ImportResult, error) {
	var res ImportResult
	err := c.do(ctx, http.MethodPost, "/v1/vhd/import", DiskRequest{ConfigDir: configDir}, &res)
	return res, err
}

// SnapshotVHD is POST /v1/vhd/snapshot: the data disk copied to its
// upgrade snapshot, its VM off.
func (c *Client) SnapshotVHD(ctx context.Context, configDir string) (SnapshotResult, error) {
	var res SnapshotResult
	err := c.do(ctx, http.MethodPost, "/v1/vhd/snapshot", DiskRequest{ConfigDir: configDir}, &res)
	return res, err
}

// RestoreVHD is POST /v1/vhd/restore: the upgrade snapshot put in the
// data disk's place, its VM off.
func (c *Client) RestoreVHD(ctx context.Context, configDir string) error {
	return c.do(ctx, http.MethodPost, "/v1/vhd/restore", DiskRequest{ConfigDir: configDir}, nil)
}

// DeleteSnapshot is DELETE /v1/vhd/snapshot: the upgrade snapshot
// deleted.
func (c *Client) DeleteSnapshot(ctx context.Context, configDir string) (DeleteResult, error) {
	var res DeleteResult
	err := c.do(ctx, http.MethodDelete, "/v1/vhd/snapshot", DiskRequest{ConfigDir: configDir}, &res)
	return res, err
}

// DeleteVHD is DELETE /v1/vhd: the data disk and its snapshot deleted,
// and the VM directory with them when the VM is gone.
func (c *Client) DeleteVHD(ctx context.Context, configDir string) (DeleteResult, error) {
	var res DeleteResult
	err := c.do(ctx, http.MethodDelete, "/v1/vhd", DiskRequest{ConfigDir: configDir}, &res)
	return res, err
}

// VHDProgress is POST /v1/vhd/progress: how far the long operation on
// the team's data disk has got.
func (c *Client) VHDProgress(ctx context.Context, configDir string) (VHDProgress, error) {
	var res VHDProgress
	err := c.do(ctx, http.MethodPost, "/v1/vhd/progress", DiskRequest{ConfigDir: configDir}, &res)
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
