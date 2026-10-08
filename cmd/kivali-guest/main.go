//go:build linux

// Command kivali-guest is the agent inside the Kivali Desktop VM. It
// serves the host's kivali-supervisor: status, exec, a PTY, write-file
// into the data directory, image import, a TCP proxy to the guest's
// loopback, and the clean shutdown. Protocol:
// internal/supervisor/guestapi and docs/developers/supervisor.md.
//
// busybox init starts it (respawn) after the boot script, and it listens
// on vsock port 1024, which only the host can reach, never the pod
// network: virtio-vsock on macOS, Hyper-V sockets on Windows, the same
// socket to the guest.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mdlayher/vsock"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "integrity" {
		os.Exit(integrityMain(os.Args[2:]))
	}
	flag.Parse()

	// Start-up lines go to the VM's console (stdin is /dev/null there,
	// vm/rootfs/usr/libexec/kivali/guest-agent: console-control is the
	// only reader of the console).
	console, consoleName := os.Stdout, "the console"
	say := func(format string, args ...any) {
		_, _ = fmt.Fprintf(console, "KIVALI-VM: guest: "+format+"\n", args...)
	}

	logPath := filepath.Join(guestapi.DataDir, "log", "guest.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		say("cannot open %s: %v; logging to %s", logPath, err, consoleName)
	} else {
		log.SetOutput(lf)
	}
	log.SetFlags(log.LstdFlags | log.LUTC)

	if _, err := os.Stat("/run/kivali/booted"); err != nil {
		// boot failed and is powering the VM off; nothing to serve.
		say("boot did not complete; agent not started")
		// Idle instead of exiting, or init would respawn it in a loop.
		for {
			time.Sleep(time.Hour)
		}
	}

	srv := guestapi.NewServer(version)
	srv.Logf = log.Printf
	l, err := vsock.Listen(guestapi.Port, nil)
	if err != nil {
		say("cannot listen on vsock port %d: %v", guestapi.Port, err)
		log.Fatalf("listen: %v", err)
	}
	say("agent %s listening on vsock port %d (host CID only)", version, guestapi.Port)
	log.Printf("agent %s listening on vsock port %d (host CID only)", version, guestapi.Port)
	log.Fatal(srv.Serve(&guestapi.FilterListener{
		Listener: l,
		Allow: func(a net.Addr) bool {
			va, ok := a.(*vsock.Addr)
			return ok && va.ContextID == guestapi.HostCID
		},
		Logf: log.Printf,
	}))
}
