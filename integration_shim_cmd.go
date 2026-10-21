package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

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
func runIntegrationShim(_ []string) {
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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger.Printf("starting: integration=%s version=%s uds=%s", shim.Name, shim.Version, shim.CoreUDS)
	code := shim.Run(ctx)
	cancel()
	os.Exit(code)
}
