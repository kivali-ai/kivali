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
// runtime's egress proxy credential, as the SHA-256 of its secret: the
// runtime registers it over core.sock when it starts. A backup leaves
// it out (files.BackupExcludePrefixes): a hash belongs to the runtime
// that drew its secret, and a restored deployment's pods register
// their own.
const egressAgentsFilename = "egress_agents.json"

// EgressAgents is what the egress proxy reads from the agents sync
// path: the credential hash of each agent's runtime, and the hosts the
// model provider's sign-in calls. A request that presents a registered
// credential reaches the public internet; the proxy counts what each
// agent sends there, and leaves ModelHosts out of the count.
type EgressAgents struct {
	// Agents maps an agent's slug to the lowercase hex SHA-256 of its
	// runtime's secret.
	Agents map[string]string `json:"agents"`
	// ModelHosts are exact hostnames, sorted.
	ModelHosts []string `json:"model_hosts"`
}

// egressAgentsFile is the DataDir file's shape: the hashes only. The
// model hosts are worked out from the sign-in at boot and whenever it
// changes, so they are held in memory and written to the sync path
// alone.
type egressAgentsFile struct {
	Agents map[string]string `json:"agents"`
}

// SetEgressAgentsSyncPath names the file, outside DataDir, that every
// change to the agent credentials or the model hosts is mirrored to:
// the egress proxy's shared volume. Empty disables the mirror.
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
	return s.readEgressAgentsLocked()
}

// SetEgressAgentCredential records sha256Hex (egresscred.Hash of the
// runtime's secret) as slug's credential, replacing any earlier one,
// and mirrors the result to the proxy.
func (s *FSStore) SetEgressAgentCredential(slug, sha256Hex string) error {
	if strings.TrimSpace(slug) == "" || strings.ContainsAny(slug, `/\`) {
		return fmt.Errorf("store: egress credential: bad slug %q", slug)
	}
	if !egresscred.ValidHash(sha256Hex) {
		return errors.New("store: egress credential: not a lowercase hex SHA-256")
	}
	s.egressAgentsMu.Lock()
	defer s.egressAgentsMu.Unlock()
	agents, err := s.readEgressAgentsLocked()
	if err != nil {
		return err
	}
	agents[slug] = sha256Hex
	return s.writeEgressAgentsLocked(agents)
}

// DropEgressAgentCredential forgets slug's credential, so the proxy
// refuses it from then on. Nothing to drop is not an error.
func (s *FSStore) DropEgressAgentCredential(slug string) error {
	s.egressAgentsMu.Lock()
	defer s.egressAgentsMu.Unlock()
	agents, err := s.readEgressAgentsLocked()
	if err != nil {
		return err
	}
	if _, ok := agents[slug]; !ok {
		return nil
	}
	delete(agents, slug)
	return s.writeEgressAgentsLocked(agents)
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

// SyncEgressAgentsToProxy writes the agent credentials and the model
// hosts to the sync path. Boot calls it so a fresh proxy volume knows
// every running agent before their traffic starts, and so does a
// restore.
func (s *FSStore) SyncEgressAgentsToProxy() error {
	s.egressAgentsMu.Lock()
	defer s.egressAgentsMu.Unlock()
	return s.syncEgressAgentsLocked()
}

// readEgressAgentsLocked returns the recorded hashes. A file that cannot
// be read or decoded reads as empty, and is logged: the next
// registration writes a good one over it, and every runtime registers
// again when its events stream reconnects, so the hashes come back
// without a person.
func (s *FSStore) readEgressAgentsLocked() (map[string]string, error) {
	b, err := os.ReadFile(s.path(egressAgentsFilename))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		log.Printf("store: %s unreadable, treated as empty: %v", egressAgentsFilename, err)
		return map[string]string{}, nil
	}
	var f egressAgentsFile
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("store: %s corrupt, treated as empty: %v", egressAgentsFilename, err)
		return map[string]string{}, nil
	}
	if f.Agents == nil {
		f.Agents = map[string]string{}
	}
	return f.Agents, nil
}

func (s *FSStore) writeEgressAgentsLocked(agents map[string]string) error {
	b, err := json.MarshalIndent(egressAgentsFile{Agents: agents}, "", "  ")
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
	agents, err := s.readEgressAgentsLocked()
	if err != nil {
		return err
	}
	hosts := s.egressModelHosts
	if hosts == nil {
		hosts = []string{}
	}
	b, err := json.MarshalIndent(EgressAgents{Agents: agents, ModelHosts: hosts}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.egressAgentsSyncPath), 0o755); err != nil {
		return err
	}
	return writeAtomic(s.egressAgentsSyncPath, append(b, '\n'), 0o644)
}
