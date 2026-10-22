// Package web (integration_builtin_egress.go) — the scoped egress proxy
// credential of a built-in integration's connection.
//
// A built-in whose shell jobs reach the network (builtinintegrations.HostsOf:
// git's clone, fetch and push, run by the agent-shell daemon) gives each
// connection a proxy credential of its own: a 32-byte secret, kept in
// the connection's Secret under builtinintegrations.ProxySecretKey,
// whose SHA-256 is registered as the scoped proxy user
// builtinintegrations.ProxyUser(name, connection id) with the hosts the
// connection reaches. The proxy admits that user to those hosts and no
// others, and counts nothing; the team's allow list is not touched.
// Core puts the secret in each job; the agent never holds it between
// jobs.
//
// The lifecycle: saving the connection mints the secret when the Secret
// has none and registers it, again on every save, so a changed host is
// registered at once; deleting the connection, switching the
// integration off and removing it drop it; switching it on registers it
// again; and every boot and restore registers each switched-on
// built-in's connection again from its Secret, since egress_agents.json
// is kept out of backups and the Secret is not.

package web

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
)

// builtinHostsOf is bi as a built-in whose connections have a proxy
// credential, nil when it has none (or bi is nil: a bundle).
func builtinHostsOf(bi builtinintegrations.Integration) builtinintegrations.HostsOf {
	h, _ := bi.(builtinintegrations.HostsOf)
	return h
}

// registerBuiltinEgress registers secret as name's connection conn's
// scoped proxy credential, admitted to the hosts h names for it.
func (s *Server) registerBuiltinEgress(name string, h builtinintegrations.HostsOf, conn builtinintegrations.Connection, secret string) error {
	return s.Store.SetEgressScopedCredential(builtinintegrations.ProxyUser(name, conn.ID), egresscred.Hash(secret), h.Hosts(conn))
}

// dropBuiltinEgress drops the scoped proxy credential of each of name's
// connections conns.
func (s *Server) dropBuiltinEgress(name string, conns []store.IntegrationConnection) error {
	var errs []error
	for _, c := range conns {
		if err := s.Store.DropEgressScopedCredential(builtinintegrations.ProxyUser(name, c.ID)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// dropAllBuiltinEgress drops the scoped proxy credential of every
// connection name has.
func (s *Server) dropAllBuiltinEgress(name string) error {
	conns, err := s.Store.ListIntegrationConnections(name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return s.dropBuiltinEgress(name, conns)
}

// builtinProxySecret is the secret of the scoped proxy credential of
// name's connection connID: the one its Secret holds, or a new one,
// minted and kept there now. Either way, while the integration is
// switched on, it is registered. "" with no such connection, or for a
// built-in whose connections have none.
func (s *Server) builtinProxySecret(ctx context.Context, name string, bi builtinintegrations.Integration, connID string) (string, error) {
	defer s.lockIntegrations()()
	return s.builtinProxySecretLocked(ctx, name, bi, connID)
}

// builtinProxySecretLocked is builtinProxySecret for a caller that
// holds lockIntegrations: it may write the connection's Secret.
func (s *Server) builtinProxySecretLocked(ctx context.Context, name string, bi builtinintegrations.Integration, connID string) (string, error) {
	h := builtinHostsOf(bi)
	if h == nil {
		return "", nil
	}
	it, err := s.Store.GetIntegration(name)
	if err != nil {
		return "", err
	}
	conn, secret, _, err := s.builtinConnection(ctx, name, connID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", nil
	case err != nil:
		return "", err
	}
	if secret == "" {
		if secret, err = s.mintBuiltinProxySecret(ctx, name, conn.ID); err != nil {
			return "", err
		}
	}
	if it.Enabled {
		if err := s.registerBuiltinEgress(name, h, conn, secret); err != nil {
			return "", err
		}
	}
	return secret, nil
}

// registerAllBuiltinEgressLocked registers the scoped proxy credential
// of every connection name has, minting the ones missing, as
// builtinProxySecretLocked does for one. The caller holds
// lockIntegrations.
func (s *Server) registerAllBuiltinEgressLocked(ctx context.Context, name string, bi builtinintegrations.Integration) error {
	conns, err := s.Store.ListIntegrationConnections(name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var errs []error
	for _, c := range conns {
		if _, err := s.builtinProxySecretLocked(ctx, name, bi, c.ID); err != nil {
			errs = append(errs, fmt.Errorf("connection %s: %w", c.ID, err))
		}
	}
	return errors.Join(errs...)
}

// mintBuiltinProxySecret draws a secret for name's connection connID
// and adds it to the connection's Secret, which it names on the record
// when the record named none.
func (s *Server) mintBuiltinProxySecret(ctx context.Context, name, connID string) (string, error) {
	rec, err := s.Store.GetIntegrationConnectionByID(name, connID)
	if err != nil {
		return "", err
	}
	data := map[string]string{}
	if rec.SecretName != "" {
		got, err := s.Secrets.Get(ctx, rec.SecretName)
		switch {
		case err == nil:
			data = got
		case !errors.Is(err, secrets.ErrNotFound):
			return "", err
		}
	}
	secret, err := egresscred.NewSecret()
	if err != nil {
		return "", err
	}
	data[builtinintegrations.ProxySecretKey] = secret
	secretName := rec.SecretName
	if secretName == "" {
		secretName = secrets.ConnectionSecretName(name, rec.ID)
	}
	if err := s.Secrets.Put(ctx, secretName, data); err != nil {
		return "", err
	}
	if rec.SecretName == "" {
		rec.SecretName = secretName
		if err := s.Store.PutIntegrationConnection(name, rec); err != nil {
			return "", err
		}
	}
	return secret, nil
}

// RegisterBuiltinEgress registers the scoped proxy credential of every
// connection of every switched-on built-in integration again, from its
// Secret, and drops those of the ones switched off. Boot calls it
// (main.go), and so does a restore: the proxy's credentials file is
// not in a backup, and the Secrets are not touched by one. Failures are
// logged; the integration's calls then mint and register as they go.
func (s *Server) RegisterBuiltinEgress(ctx context.Context) {
	if s.Store == nil {
		return
	}
	for _, bi := range s.BuiltinIntegrations {
		if builtinHostsOf(bi) == nil {
			continue
		}
		name := bi.Manifest().Name
		it, err := s.Store.GetIntegration(name)
		if err != nil || s.builtinFor(it) != bi {
			continue
		}
		if !it.Enabled {
			if err := s.dropAllBuiltinEgress(name); err != nil {
				log.Printf("integration %s: drop the connection's proxy credential: %v", name, err)
			}
			continue
		}
		unlock := s.lockIntegrations()
		err = s.registerAllBuiltinEgressLocked(ctx, name, bi)
		unlock()
		if err != nil {
			log.Printf("integration %s: register the connection's proxy credential: %v", name, err)
		}
	}
}

// builtinEgressAfterSwitch follows the integration's switch: on, every
// connection's credential is registered (minted if need be); off, each
// is dropped. The caller holds lockIntegrations.
func (s *Server) builtinEgressAfterSwitch(ctx context.Context, name string, bi builtinintegrations.Integration, on bool) error {
	if builtinHostsOf(bi) == nil {
		return nil
	}
	if !on {
		return s.dropAllBuiltinEgress(name)
	}
	if err := s.registerAllBuiltinEgressLocked(ctx, name, bi); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	return nil
}
