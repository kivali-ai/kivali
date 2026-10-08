//go:build windows

package main

import (
	"context"
	"net"
	"os"

	"github.com/kivali-ai/kivali/internal/supervisor"
	"github.com/kivali-ai/kivali/internal/supervisor/broker"
	"github.com/kivali-ai/kivali/internal/supervisor/hyperv"
)

// vmImageHint names the VM image artifact serve looks for, for the
// "no VM image found" message.
const vmImageHint = "root.vhdx"

// newBackend is the Hyper-V backend, driven through the broker over its
// named pipe (brokerPipe). broker.Dial connects so the broker may
// impersonate this account for its checks, and, before writing to it,
// refuses a pipe whose owner is not LocalSystem or the Administrators
// group (which the broker names, and an ordinary account cannot); the
// broker, not this process, holds the Hyper-V privilege.
func newBackend() supervisor.Backend {
	pipe := brokerPipe()
	client := &broker.Client{Dial: func(ctx context.Context) (net.Conn, error) {
		return broker.Dial(ctx, pipe)
	}}
	return hyperv.Backend{Client: client}
}

// brokerPipe is the broker's named pipe: KIVALI_BROKER_PIPE for a dev
// run (the broker reads the same variable), else broker.PipeName.
func brokerPipe() string {
	if p := os.Getenv("KIVALI_BROKER_PIPE"); p != "" {
		return p
	}
	return broker.PipeName
}
