package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Software is extra software installed into an agent's shell: Debian
// package names, installed with apt when the agent's pod starts. Two
// lists feed an agent's shell: the org-wide list (software.yaml), which
// every agent gets, and the agent's own (Agent.Software in agent.yaml).
// EffectiveSoftware is their union, and it is what the pod installs.
type Software struct {
	Packages []string `yaml:"packages"`
}

// softwareFilename is the org-wide list under DataDir.
const softwareFilename = "software.yaml"

// softwareNameRE is Debian Policy §5.6.1's package name: lowercase
// letters, digits, '+', '-' and '.', at least two characters, starting
// with a letter or digit. Nothing else reaches apt-get: no option (a
// leading '-'), no removal (apt-get install reads a trailing '-' as
// "remove this package", so a name may not end with one), no version
// pin ('='), no release ('/'), no path.
var softwareNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*[a-z0-9+.]$`)

// ErrInvalidSoftware is wrapped by NormalizeSoftware for a name that is
// not a Debian package name.
var ErrInvalidSoftware = errors.New("not a Debian package name")

// ValidSoftwareName reports whether name is a Debian package name.
func ValidSoftwareName(name string) bool {
	return len(name) <= 128 && softwareNameRE.MatchString(name)
}

// NormalizeSoftware trims, lowercases, de-duplicates and sorts names,
// dropping blanks. Any name that is not a Debian package name fails the
// whole list, naming the first offender.
func NormalizeSoftware(names []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || seen[n] {
			continue
		}
		if !ValidSoftwareName(n) {
			return nil, fmt.Errorf("%q: %w", n, ErrInvalidSoftware)
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// ReadOrgSoftware returns the org-wide list. A missing file is an
// empty list.
func (s *FSStore) ReadOrgSoftware() ([]string, error) {
	b, err := os.ReadFile(s.path(softwareFilename))
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var sw Software
	if err := yaml.Unmarshal(b, &sw); err != nil {
		return nil, fmt.Errorf("%s: %w", softwareFilename, err)
	}
	return NormalizeSoftware(sw.Packages)
}

// ErrSoftwareNotListed is returned by RemoveOrgSoftware and
// RemoveAgentSoftware for a name that is not on the list.
var ErrSoftwareNotListed = errors.New("not on the software list")

// WriteOrgSoftware replaces the org-wide list.
func (s *FSStore) WriteOrgSoftware(names []string) error {
	clean, err := NormalizeSoftware(names)
	if err != nil {
		return err
	}
	s.orgSoftwareMu.Lock()
	defer s.orgSoftwareMu.Unlock()
	return s.writeOrgSoftwareLocked(clean)
}

// writeOrgSoftwareLocked writes a normalized list; orgSoftwareMu is held.
func (s *FSStore) writeOrgSoftwareLocked(clean []string) error {
	b, err := yaml.Marshal(Software{Packages: clean})
	if err != nil {
		return err
	}
	path := s.path(softwareFilename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, b, 0o644)
}

// updateOrgSoftware is the read-modify-write of the org-wide list under
// orgSoftwareMu. It returns the list as written.
func (s *FSStore) updateOrgSoftware(change func([]string) ([]string, error)) ([]string, error) {
	s.orgSoftwareMu.Lock()
	defer s.orgSoftwareMu.Unlock()
	cur, err := s.ReadOrgSoftware()
	if err != nil {
		return nil, err
	}
	next, err := change(cur)
	if err != nil {
		return nil, err
	}
	clean, err := NormalizeSoftware(next)
	if err != nil {
		return nil, err
	}
	if err := s.writeOrgSoftwareLocked(clean); err != nil {
		return nil, err
	}
	return clean, nil
}

// AddOrgSoftware adds names to the org-wide list, keeping what is
// there, and returns the list as it now stands.
func (s *FSStore) AddOrgSoftware(names []string) ([]string, error) {
	if _, err := NormalizeSoftware(names); err != nil {
		return nil, err
	}
	return s.updateOrgSoftware(func(cur []string) ([]string, error) {
		return append(cur, names...), nil
	})
}

// RemoveOrgSoftware removes name from the org-wide list and returns the
// list as it now stands. A name not on it is ErrSoftwareNotListed.
func (s *FSStore) RemoveOrgSoftware(name string) ([]string, error) {
	return s.updateOrgSoftware(func(cur []string) ([]string, error) {
		return removeSoftware(cur, name)
	})
}

// removeSoftware is list without name, or ErrSoftwareNotListed.
func removeSoftware(list []string, name string) ([]string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	out := make([]string, 0, len(list))
	found := false
	for _, n := range list {
		if n == name {
			found = true
			continue
		}
		out = append(out, n)
	}
	if !found {
		return nil, fmt.Errorf("%q: %w", name, ErrSoftwareNotListed)
	}
	return out, nil
}

// setAgentSoftwareList stores a normalized list on a, nil when empty.
func setAgentSoftwareList(a *Agent, names []string) error {
	clean, err := NormalizeSoftware(names)
	if err != nil {
		return err
	}
	a.Software = clean
	if len(clean) == 0 {
		a.Software = nil
	}
	return nil
}

// SetAgentSoftware replaces an active agent's own list. Returns
// ErrNotFound if the agent isn't active.
func (s *FSStore) SetAgentSoftware(slug string, names []string) error {
	if _, err := NormalizeSoftware(names); err != nil {
		return err
	}
	return s.updateAgent(slug, func(a *Agent) error {
		return setAgentSoftwareList(a, names)
	})
}

// AddAgentSoftware adds names to an active agent's own list, keeping
// what is there. Returns ErrNotFound if the agent isn't active.
func (s *FSStore) AddAgentSoftware(slug string, names []string) error {
	if _, err := NormalizeSoftware(names); err != nil {
		return err
	}
	return s.updateAgent(slug, func(a *Agent) error {
		return setAgentSoftwareList(a, append(append([]string{}, a.Software...), names...))
	})
}

// RemoveAgentSoftware removes name from an active agent's own list. A
// name not on it is ErrSoftwareNotListed; an agent that isn't active
// is ErrNotFound.
func (s *FSStore) RemoveAgentSoftware(slug, name string) error {
	return s.updateAgent(slug, func(a *Agent) error {
		cur, err := NormalizeSoftware(a.Software)
		if err != nil {
			return err
		}
		next, err := removeSoftware(cur, name)
		if err != nil {
			return err
		}
		return setAgentSoftwareList(a, next)
	})
}

// EffectiveSoftware is what the agent's shell installs: the org-wide
// list and the agent's own, merged and sorted.
func (s *FSStore) EffectiveSoftware(slug string) ([]string, error) {
	org, err := s.ReadOrgSoftware()
	if err != nil {
		return nil, err
	}
	a, err := s.GetAgent(slug)
	if err != nil {
		return nil, err
	}
	return NormalizeSoftware(append(org, a.Software...))
}
