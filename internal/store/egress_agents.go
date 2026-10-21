package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/kivali-ai/kivali/internal/egresscred"
)

// egressAgentsFilename is the file under DataDir that holds each agent
// runtime's two egress proxy credentials, as the SHA-256 of each
// secret: the runtime registers them over core.sock when it starts. A
// backup leaves it out (files.BackupExcludePrefixes): a hash belongs to
// the runtime that drew its secret, and a restored deployment's pods
// register their own.
const egressAgentsFilename = "egress_agents.json"

// EgressAgents is what the egress proxy reads from the agents sync
// path: each agent runtime's two credential hashes, each agent's own
// network settings, and the hosts the model provider's sign-in calls.
// A request that presents an agent's credential (Agents) reaches the
// public internet; the proxy counts what each agent sends there, and
// leaves ModelHosts out of the count. A request that presents an
// agent's shell credential (Shells) reaches the team's list and that
// agent's own hosts (AgentHosts). An exempt agent's requests, with
// either credential, reach any public host and are not counted.
type EgressAgents struct {
	// Agents maps an agent's slug to the lowercase hex SHA-256 of its
	// runtime's secret.
	Agents map[string]string `json:"agents"`
	// Shells maps an agent's slug to the lowercase hex SHA-256 of the
	// secret its runtime hands the agent's shell.
	Shells map[string]string `json:"shells"`
	// AgentHosts maps an agent's slug to the hosts its shell may reach
	// on top of the team's list (Agent.NetworkHosts), sorted. Only
	// agents with any are listed.
	AgentHosts map[string][]string `json:"agent_hosts"`
	// Exempt are the slugs of the agents with no network limits
	// (Agent.NetworkExempt), sorted.
	Exempt []string `json:"exempt"`
	// ModelHosts are exact hostnames, sorted.
	ModelHosts []string `json:"model_hosts"`
}

// egressAgentsFile is the DataDir file's shape: the hashes only. The
// model hosts are worked out from the sign-in at boot and whenever it
// changes, so they are held in memory, and the agents' network settings
// live in their agent.yaml; both are written to the sync path alone.
type egressAgentsFile struct {
	Agents map[string]string `json:"agents"`
	Shells map[string]string `json:"shells,omitempty"`
}

// SetEgressAgentsSyncPath names the file, outside DataDir, that every
// change to the agent credentials, the agents' network settings or the
// model hosts is mirrored to: the egress proxy's shared volume. Empty
// disables the mirror.
func (s *FSStore) SetEgressAgentsSyncPath(p string) {
	s.egressAgentsMu.Lock()
	defer s.egressAgentsMu.Unlock()
	s.egressAgentsSyncPath = p
}

// EgressAgentCredentials returns each agent's registered credential
// hash. Empty (not an error) before any registration.
func (s *FSStore) EgressAgentCredentials() (map[string]string, error) {
	s.egressAgentsMu.Lock()
	defer s.egressAgentsMu.Unlock()
	return s.readEgressAgentsLocked().Agents, nil
}

// EgressShellCredentials returns each agent's registered shell
// credential hash. Empty (not an error) before any registration.
func (s *FSStore) EgressShellCredentials() (map[string]string, error) {
	s.egressAgentsMu.Lock()
	defer s.egressAgentsMu.Unlock()
	return s.readEgressAgentsLocked().Shells, nil
}

// checkEgressSlug refuses a slug that could not name an agent.
func checkEgressSlug(slug string) error {
	if strings.TrimSpace(slug) == "" || strings.ContainsAny(slug, `/\`) {
		return fmt.Errorf("store: egress credential: bad slug %q", slug)
	}
	return nil
}

// SetEgressAgentCredential records sha256Hex (egresscred.Hash of the
// runtime's secret) as slug's credential, replacing any earlier one,
// and mirrors the result to the proxy.
func (s *FSStore) SetEgressAgentCredential(slug, sha256Hex string) error {
	if err := checkEgressSlug(slug); err != nil {
		return err
	}
	if !egresscred.ValidHash(sha256Hex) {
		return errors.New("store: egress credential: not a lowercase hex SHA-256")
	}
	s.egressAgentsMu.Lock()
	defer s.egressAgentsMu.Unlock()
	f := s.readEgressAgentsLocked()
	f.Agents[slug] = sha256Hex
	return s.writeEgressAgentsLocked(f)
}

// SetEgressShellCredential records sha256Hex (egresscred.Hash of the
// secret the runtime hands the agent's shell) as slug's shell
// credential, replacing any earlier one, and mirrors the result to the
// proxy. An empty sha256Hex drops slug's shell credential: a runtime
// that registers none has no shell to hand one to.
func (s *FSStore) SetEgressShellCredential(slug, sha256Hex string) error {
	if err := checkEgressSlug(slug); err != nil {
		return err
	}
	if sha256Hex != "" && !egresscred.ValidHash(sha256Hex) {
		return errors.New("store: egress shell credential: not a lowercase hex SHA-256")
	}
	s.egressAgentsMu.Lock()
	defer s.egressAgentsMu.Unlock()
	f := s.readEgressAgentsLocked()
	if sha256Hex == "" {
		if _, ok := f.Shells[slug]; !ok {
			return nil
		}
		delete(f.Shells, slug)
	} else {
		f.Shells[slug] = sha256Hex
	}
	return s.writeEgressAgentsLocked(f)
}

// DropEgressAgentCredential forgets both of slug's credentials, so the
// proxy refuses them from then on, and mirrors the result to the proxy.
// The mirror is written also when there is nothing to drop: ArchiveAgent
// calls this once the agent has left agents/, so its network settings
// leave the proxy's file too. Nothing to drop is not an error.
func (s *FSStore) DropEgressAgentCredential(slug string) error {
	s.egressAgentsMu.Lock()
	defer s.egressAgentsMu.Unlock()
	f := s.readEgressAgentsLocked()
	_, open := f.Agents[slug]
	_, shell := f.Shells[slug]
	if !open && !shell {
		return s.syncEgressAgentsLocked()
	}
	delete(f.Agents, slug)
	delete(f.Shells, slug)
	return s.writeEgressAgentsLocked(f)
}

// SetEgressModelHosts replaces the model provider's hosts (trimmed,
// lowercased, deduplicated, sorted) and mirrors the result to the
// proxy.
func (s *FSStore) SetEgressModelHosts(hosts []string) error {
	s.egressAgentsMu.Lock()
	defer s.egressAgentsMu.Unlock()
	s.egressModelHosts = normalizePatterns(hosts)
	return s.syncEgressAgentsLocked()
}

// SyncEgressAgentsToProxy writes the agent credentials, the agents'
// network settings and the model hosts to the sync path. Boot calls it
// so a fresh proxy volume knows every running agent before their
// traffic starts, and so do a restore and every change to an agent's
// network settings (egress_network.go).
//
// It holds egressAgentsMu while it reads the agents' agent.yaml files,
// which takes no lock (egressAgentNetworks). Nothing takes
// egressAgentsMu with agentYAMLMu held: the network setters and
// ArchiveAgent finish their agent.yaml write first.
func (s *FSStore) SyncEgressAgentsToProxy() error {
	s.egressAgentsMu.Lock()
	defer s.egressAgentsMu.Unlock()
	return s.syncEgressAgentsLocked()
}

// readEgressAgentsLocked returns the recorded hashes, both maps
// non-nil. A file that cannot be read or decoded reads as empty, and is
// logged: the next registration writes a good one over it, and every
// runtime registers again when its events stream reconnects, so the
// hashes come back without a person.
func (s *FSStore) readEgressAgentsLocked() egressAgentsFile {
	empty := egressAgentsFile{Agents: map[string]string{}, Shells: map[string]string{}}
	b, err := os.ReadFile(s.path(egressAgentsFilename))
	if errors.Is(err, fs.ErrNotExist) {
		return empty
	}
	if err != nil {
		log.Printf("store: %s unreadable, treated as empty: %v", egressAgentsFilename, err)
		return empty
	}
	var f egressAgentsFile
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("store: %s corrupt, treated as empty: %v", egressAgentsFilename, err)
		return empty
	}
	if f.Agents == nil {
		f.Agents = map[string]string{}
	}
	if f.Shells == nil {
		f.Shells = map[string]string{}
	}
	return f
}

func (s *FSStore) writeEgressAgentsLocked(f egressAgentsFile) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the hashes are not secrets, but nothing but this server
	// reads them.
	if err := writeAtomic(s.path(egressAgentsFilename), append(b, '\n'), 0o600); err != nil {
		return err
	}
	return s.syncEgressAgentsLocked()
}

func (s *FSStore) syncEgressAgentsLocked() error {
	if s.egressAgentsSyncPath == "" {
		return nil
	}
	f := s.readEgressAgentsLocked()
	hosts := s.egressModelHosts
	if hosts == nil {
		hosts = []string{}
	}
	agentHosts, exempt := s.egressAgentNetworks()
	b, err := json.MarshalIndent(EgressAgents{
		Agents:     f.Agents,
		Shells:     f.Shells,
		AgentHosts: agentHosts,
		Exempt:     exempt,
		ModelHosts: hosts,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.egressAgentsSyncPath), 0o755); err != nil {
		return err
	}
	return writeAtomic(s.egressAgentsSyncPath, append(b, '\n'), 0o644)
}
