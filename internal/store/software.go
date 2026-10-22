package store

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kivali-ai/kivali/internal/debpkg"
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

// ErrInvalidSoftware is wrapped by NormalizeSoftware for a name that is
// not a Debian package name.
var ErrInvalidSoftware = errors.New("not a Debian package name")

// ValidSoftwareName reports whether name is a Debian package name
// (debpkg.ValidName, Debian Policy §5.6.1). Nothing else reaches
// apt-get.
func ValidSoftwareName(name string) bool { return debpkg.ValidName(name) }

// NormalizeSoftware trims, lowercases, de-duplicates and sorts names,
// dropping blanks. Any name that is not a Debian package name fails the
// whole list, naming the first offender. It checks names on their way
// in; a list already saved is read with CleanSoftware.
func NormalizeSoftware(names []string) ([]string, error) {
	out := CleanSoftware(names)
	for _, n := range out {
		if !ValidSoftwareName(n) {
			return nil, fmt.Errorf("%q: %w", n, ErrInvalidSoftware)
		}
	}
	return out, nil
}

// CleanSoftware trims, lowercases, de-duplicates and sorts names,
// dropping blanks, and keeps names that are not Debian package names.
// A saved list can hold such a name (software.yaml or agent.yaml edited
// by hand): it stays listed, so the owner can see and remove it, and
// installs nothing (EffectiveSoftware leaves it out).
func CleanSoftware(names []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// InvalidSoftware returns the names in list that are not Debian package
// names, keeping list's order. Never nil.
func InvalidSoftware(list []string) []string {
	out := []string{}
	for _, n := range list {
		if !ValidSoftwareName(n) {
			out = append(out, n)
		}
	}
	return out
}

// ReadOrgSoftware returns the org-wide list, cleaned (CleanSoftware):
// a name in the file that is not a Debian package name is listed too.
// A missing file is an empty list.
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
	return CleanSoftware(sw.Packages), nil
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
// orgSoftwareMu. It returns the list as written. Names already on the
// list that are not Debian package names stay until they are removed;
// callers check the names they add.
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
	clean := CleanSoftware(next)
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
// list as it now stands. Any listed name can be removed, also one that
// is not a Debian package name. A name not on it is
// ErrSoftwareNotListed.
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

// setAgentSoftwareList stores a cleaned list on a, nil when empty.
// Callers check the names they add; a name already on the list that is
// not a Debian package name stays until it is removed.
func setAgentSoftwareList(a *Agent, names []string) {
	a.Software = CleanSoftware(names)
	if len(a.Software) == 0 {
		a.Software = nil
	}
}

// SetAgentSoftware replaces an active agent's own list. Returns
// ErrNotFound if the agent isn't active.
func (s *FSStore) SetAgentSoftware(slug string, names []string) error {
	if _, err := NormalizeSoftware(names); err != nil {
		return err
	}
	return s.updateAgent(slug, func(a *Agent) error {
		setAgentSoftwareList(a, names)
		return nil
	})
}

// AddAgentSoftware adds names to an active agent's own list, keeping
// what is there. Returns ErrNotFound if the agent isn't active.
func (s *FSStore) AddAgentSoftware(slug string, names []string) error {
	if _, err := NormalizeSoftware(names); err != nil {
		return err
	}
	return s.updateAgent(slug, func(a *Agent) error {
		setAgentSoftwareList(a, append(append([]string{}, a.Software...), names...))
		return nil
	})
}

// RemoveAgentSoftware removes name from an active agent's own list.
// Any listed name can be removed, also one that is not a Debian package
// name. A name not on it is ErrSoftwareNotListed; an agent that isn't
// active is ErrNotFound.
func (s *FSStore) RemoveAgentSoftware(slug, name string) error {
	return s.updateAgent(slug, func(a *Agent) error {
		next, err := removeSoftware(CleanSoftware(a.Software), name)
		if err != nil {
			return err
		}
		setAgentSoftwareList(a, next)
		return nil
	})
}

// EffectiveSoftware is what the agent's shell installs: the org-wide
// list and the agent's own, merged and sorted. A listed name that is not
// a Debian package name is left out and logged; the rest still install.
func (s *FSStore) EffectiveSoftware(slug string) ([]string, error) {
	org, err := s.ReadOrgSoftware()
	if err != nil {
		return nil, err
	}
	a, err := s.GetAgent(slug)
	if err != nil {
		return nil, err
	}
	all := CleanSoftware(append(org, a.Software...))
	if bad := InvalidSoftware(all); len(bad) > 0 {
		log.Printf("software %s: not installing %s: not Debian package names", slug, strings.Join(bad, ", "))
		all = SubtractSoftware(all, bad)
	}
	return all, nil
}
