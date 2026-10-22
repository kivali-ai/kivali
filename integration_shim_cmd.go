package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/kivali-ai/kivali/internal/agentshell"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/integrationpod"
)

// runIntegrationShim is `kivali integration-shim`, the process in an
// integration pod: it dials core over integration.sock, starts the
// integration's server from the bundle when core says start, and
// relays JSON-RPC between the two. Configured by the pod's env alone
// (internal/integrationpod). It exits with the server's status, and 1
// when it cannot start it.
//
// Started as root, which the pod does only for a version whose
// manifest declares packages, it first installs them in a root child
// (`kivali integration-shim install-software <pkgs...>`, the agent
// shell's installer) and leaves root for good; the server never runs
// as root.
func runIntegrationShim(args []string) {
	if len(args) > 0 && args[0] == agentshell.InstallSubcommand {
		os.Exit(integrationInstallSoftware(args[1:]))
	}
	logger := log.New(os.Stderr, "integration-shim: ", log.LstdFlags|log.Lmicroseconds)
	shim := &integrationpod.Shim{
		Name:    os.Getenv(integrationpod.EnvName),
		Version: os.Getenv(integrationpod.EnvVersion),
		Token:   os.Getenv(integrationpod.EnvToken),
		CoreUDS: os.Getenv(integrationpod.EnvCoreUDS),
		Bundle:  os.Getenv(integrationpod.EnvBundle),
		// The pod's proxy variables are bare; the shim draws the
		// credential its server's requests carry.
		ProxyURL: egresscred.BareProxyURL(os.Getenv("HTTPS_PROXY")),
		Command:  os.Getenv(integrationpod.EnvCommand),
		Path:     os.Getenv("PATH"),
		Lang:     os.Getenv("LANG"),
		Clock:    clock.New(),
		Logf:     logger.Printf,
	}
	for name, v := range map[string]string{
		integrationpod.EnvName:    shim.Name,
		integrationpod.EnvVersion: shim.Version,
		integrationpod.EnvToken:   shim.Token,
		integrationpod.EnvCoreUDS: shim.CoreUDS,
	} {
		if v == "" {
			logger.Fatalf("%s is required", name)
		}
	}
	creds := agentshell.HostCreds{}
	ids, err := creds.IDs()
	if err != nil {
		logger.Fatalf("read credentials: %v", err)
	}
	if ids.Privileged() {
		// The install child keeps the pod's bare proxy variables: apt-get
		// goes through the egress proxy with no credential.
		shim.Software = integrationpod.RootSoftware{
			Exe:   "/proc/self/exe",
			Env:   agentshell.InstallEnv(os.Getenv),
			Clock: shim.Clock,
			Creds: creds,
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger.Printf("starting: integration=%s version=%s uds=%s as %s", shim.Name, shim.Version, shim.CoreUDS, ids)
	code := shim.Run(ctx)
	cancel()
	os.Exit(code)
}

// integrationInstallSoftware is `kivali integration-shim
// install-software <pkgs...>`, the shim's root child: apt-get for pkgs,
// as the agent shell's install-software subcommand runs it, its output
// on stdout and stderr and its result written as JSON to
// agentshell.InstallResultFD.
func integrationInstallSoftware(pkgs []string) int {
	result, err := agentshell.OpenInstallResult()
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration-shim install-software: %v\n", err)
		return 2
	}
	in := agentshell.Installer{Env: os.Environ(), Log: os.Stderr}
	if err := agentshell.RunInstallSubcommand(in, pkgs, result); err != nil {
		fmt.Fprintf(os.Stderr, "integration-shim install-software: write result: %v\n", err)
		return 1
	}
	return 0
}
