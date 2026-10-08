// Package host is the supervisor's operating system: the config
// directory, the data disk snapshot, durability, backup exclusion, free
// space, the owner-only RPC endpoint, the serve lock, detaching serve
// and recognising a busy port. OS implements
// supervisor.Host on macOS (host_darwin.go), Linux (host_linux.go) and
// Windows (host_windows.go); host_unix.go holds what macOS and Linux
// share. It imports nothing from the supervisor, which depends on it.
package host

import "errors"

// OS is the host the binary is built for.
type OS struct{}

// Default returns the host of the running operating system.
func Default() OS { return OS{} }

// ErrServeRunning is LockServe's answer when another serve holds the
// lock.
var ErrServeRunning = errors.New("another kivali-supervisor serve owns this config directory")

// lockName is the serve lock's file in the config directory, held for
// the life of serve (the kernel drops it when the process exits,
// however it exits). serve takes it before touching the RPC endpoint,
// so two serves started at once cannot both probe the endpoint, find it
// dead, and remove each other's.
const lockName = "serve.lock"
