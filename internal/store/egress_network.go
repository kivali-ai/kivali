package store

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// An agent's own network settings, kept in its agent.yaml: the hosts
// its shell commands may reach on top of the team's list
// (Agent.NetworkHosts), and the switch that lifts its network limits
// (Agent.NetworkExempt). Each change is mirrored to the egress proxy's
// agents file (EgressAgents.AgentHosts and Exempt), which the proxy
// hot-reloads, so it applies to the agent's next request with no pod
// restart.

// ErrInvalidEgressHost is returned for a host that is not one the
// egress proxy can match (ValidEgressHost).
var ErrInvalidEgressHost = errors.New("not a host name")

// ErrNetworkHostListed is returned by AddAgentNetworkHost for a host
// already on the agent's own list.
var ErrNetworkHostListed = errors.New("already on the agent's network list")

// ErrNetworkHostNotListed is returned by RemoveAgentNetworkHost for a
// host that is not on the agent's own list.
var ErrNetworkHostNotListed = errors.New("not on the agent's network list")

// NormalizeEgressHost is h trimmed and lowercased, the form every
// network list keeps.
func NormalizeEgressHost(h string) string { return strings.ToLower(strings.TrimSpace(h)) }

// ValidEgressHost reports whether h, normalized, is a host pattern the
// egress proxy can match: not empty, not a comment, and with no scheme,
// path, port, user info, query or whitespace. The proxy matches the
// host and every name under it, and reads "*" and "[a-z]" as a glob
// within one label.
func ValidEgressHost(h string) bool {
	return h != "" && !strings.HasPrefix(h, "#") && !strings.ContainsAny(h, "/: \t\r\n@?")
}

// CleanEgressHosts is list trimmed, lowercased, de-duplicated and
// sorted, without blanks and comment lines. Never nil.
func CleanEgressHosts(list []string) []string { return normalizePatterns(list) }

// setAgentNetworkHosts stores a cleaned list on a, nil when empty.
func setAgentNetworkHosts(a *Agent, hosts []string) {
	a.NetworkHosts = CleanEgressHosts(hosts)
	if len(a.NetworkHosts) == 0 {
		a.NetworkHosts = nil
	}
}

// AddAgentNetworkHost adds host to an active agent's own list and
// returns the list as it now stands. A host that is not valid
// (ValidEgressHost) is ErrInvalidEgressHost, one already on the
// agent's list ErrNetworkHostListed, and an agent that isn't active
// ErrNotFound. A host on the team's list may be added too.
func (s *FSStore) AddAgentNetworkHost(slug, host string) ([]string, error) {
	host = NormalizeEgressHost(host)
	if !ValidEgressHost(host) {
		return nil, fmt.Errorf("%q: %w", host, ErrInvalidEgressHost)
	}
	var out []string
	err := s.updateAgent(slug, func(a *Agent) error {
		cur := CleanEgressHosts(a.NetworkHosts)
		if slices.Contains(cur, host) {
			return fmt.Errorf("%q: %w", host, ErrNetworkHostListed)
		}
		setAgentNetworkHosts(a, append(cur, host))
		out = CleanEgressHosts(a.NetworkHosts)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, s.SyncEgressAgentsToProxy()
}

// RemoveAgentNetworkHost removes host from an active agent's own list
// and returns the list as it now stands. Any listed host can be
// removed, also one that is not valid. A host not on the list is
// ErrNetworkHostNotListed; an agent that isn't active is ErrNotFound.
func (s *FSStore) RemoveAgentNetworkHost(slug, host string) ([]string, error) {
	host = NormalizeEgressHost(host)
	var out []string
	err := s.updateAgent(slug, func(a *Agent) error {
		cur := CleanEgressHosts(a.NetworkHosts)
		i := slices.Index(cur, host)
		if i < 0 {
			return fmt.Errorf("%q: %w", host, ErrNetworkHostNotListed)
		}
		setAgentNetworkHosts(a, slices.Delete(cur, i, i+1))
		out = CleanEgressHosts(a.NetworkHosts)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, s.SyncEgressAgentsToProxy()
}

// SetAgentNetworkExempt turns an active agent's network limits off
// (exempt) or back on. Returns ErrNotFound if the agent isn't active.
func (s *FSStore) SetAgentNetworkExempt(slug string, exempt bool) error {
	err := s.updateAgent(slug, func(a *Agent) error {
		a.NetworkExempt = exempt
		return nil
	})
	if err != nil {
		return err
	}
	return s.SyncEgressAgentsToProxy()
}

// egressAgentNetworks reads every active agent's network settings for
// the proxy's agents file: each agent's own hosts (valid ones only,
// and only agents that have any) and the exempt agents' slugs, sorted.
// An agent.yaml that cannot be read is logged and left out, so it
// grants nothing, and the rest still reach the proxy.
//
// It reads agent.yaml without agentYAMLMu: updateAgent replaces the
// file by rename, so a read sees one whole version.
func (s *FSStore) egressAgentNetworks() (hosts map[string][]string, exempt []string) {
	hosts = map[string][]string{}
	exempt = []string{}
	dir := s.path("agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("store: egress: list agents: %v", err)
		}
		return hosts, exempt
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		a, err := s.readAgent(filepath.Join(dir, e.Name()))
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				log.Printf("store: egress: agent %s left out of the proxy's agents file: %v", e.Name(), err)
			}
			continue
		}
		var own []string
		for _, h := range CleanEgressHosts(a.NetworkHosts) {
			if ValidEgressHost(h) {
				own = append(own, h)
			}
		}
		if len(own) > 0 {
			hosts[a.Slug] = own
		}
		if a.NetworkExempt {
			exempt = append(exempt, a.Slug)
		}
	}
	sort.Strings(exempt)
	return hosts, exempt
}
