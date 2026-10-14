package store

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestValidSoftwareName(t *testing.T) {
	for _, ok := range []string{"git", "poppler-utils", "libc++-dev", "python3.11", "g++", "7zip", "a1"} {
		if !ValidSoftwareName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{
		"", "a", "-y", "--force-yes", "git=1.0", "git/stable", "../etc", "Git", "git status", "git;rm", "git\nfoo",
		".hidden", "+x", "naïve", strings.Repeat("a", 129),
		// apt-get install reads a trailing '-' as "remove this package".
		"git-", "libc++-", "a-",
	} {
		if ValidSoftwareName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
	if !ValidSoftwareName(strings.Repeat("a", 128)) {
		t.Error("128 characters should be valid")
	}
}

func TestNormalizeSoftware(t *testing.T) {
	got, err := NormalizeSoftware([]string{" Git ", "jq", "", "git", "  ", "ffmpeg"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ffmpeg", "git", "jq"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	got, err = NormalizeSoftware(nil)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("nil input = %#v, %v; want an empty, non-nil list", got, err)
	}
}

func TestNormalizeSoftwareNamesTheOffender(t *testing.T) {
	_, err := NormalizeSoftware([]string{"git", "--force-yes", "jq"})
	if !errors.Is(err, ErrInvalidSoftware) {
		t.Fatalf("err = %v, want ErrInvalidSoftware", err)
	}
	if !strings.Contains(err.Error(), "--force-yes") {
		t.Errorf("error %q does not name the offender", err)
	}
}

func newSoftwareStore(t *testing.T) *FSStore {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"alice", "bob"} {
		if err := s.CreateAgent(Agent{Slug: slug, Role: "r"}, "k"); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestOrgSoftwareRoundTrip(t *testing.T) {
	s := newSoftwareStore(t)
	got, err := s.ReadOrgSoftware()
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("missing file = %#v, %v; want an empty list", got, err)
	}
	if err := s.WriteOrgSoftware([]string{"Jq", "git", "jq"}); err != nil {
		t.Fatal(err)
	}
	got, err = s.ReadOrgSoftware()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"git", "jq"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if err := s.WriteOrgSoftware(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ReadOrgSoftware(); len(got) != 0 {
		t.Errorf("after clearing: %v", got)
	}
}

func TestWriteOrgSoftwareRefusesABadNameAndKeepsTheList(t *testing.T) {
	s := newSoftwareStore(t)
	if err := s.WriteOrgSoftware([]string{"git"}); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteOrgSoftware([]string{"jq", "-y"}); !errors.Is(err, ErrInvalidSoftware) {
		t.Fatalf("err = %v, want ErrInvalidSoftware", err)
	}
	if got, _ := s.ReadOrgSoftware(); !reflect.DeepEqual(got, []string{"git"}) {
		t.Errorf("list after refusal = %v", got)
	}
}

func TestAgentSoftwareSetAndAdd(t *testing.T) {
	s := newSoftwareStore(t)
	if err := s.SetAgentSoftware("alice", []string{"ffmpeg", "Poppler-Utils"}); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAgent("alice")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ffmpeg", "poppler-utils"}; !reflect.DeepEqual(a.Software, want) {
		t.Errorf("own = %v, want %v", a.Software, want)
	}
	if err := s.AddAgentSoftware("alice", []string{"git", "ffmpeg"}); err != nil {
		t.Fatal(err)
	}
	a, _ = s.GetAgent("alice")
	if want := []string{"ffmpeg", "git", "poppler-utils"}; !reflect.DeepEqual(a.Software, want) {
		t.Errorf("after add = %v, want %v", a.Software, want)
	}
	if b, _ := s.GetAgent("bob"); len(b.Software) != 0 {
		t.Errorf("another agent's list changed: %v", b.Software)
	}
	if err := s.SetAgentSoftware("alice", nil); err != nil {
		t.Fatal(err)
	}
	if a, _ = s.GetAgent("alice"); a.Software != nil {
		t.Errorf("after clearing = %#v, want nil", a.Software)
	}
}

func TestAgentSoftwareRefusals(t *testing.T) {
	s := newSoftwareStore(t)
	if err := s.SetAgentSoftware("alice", []string{"git"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentSoftware("alice", []string{"a b"}); !errors.Is(err, ErrInvalidSoftware) {
		t.Errorf("bad name: err = %v", err)
	}
	if err := s.AddAgentSoftware("alice", []string{"x/y"}); !errors.Is(err, ErrInvalidSoftware) {
		t.Errorf("bad name on add: err = %v", err)
	}
	if a, _ := s.GetAgent("alice"); !reflect.DeepEqual(a.Software, []string{"git"}) {
		t.Errorf("list after refusals = %v", a.Software)
	}
	if err := s.SetAgentSoftware("nobody", []string{"git"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown agent: err = %v", err)
	}
	if err := s.AddAgentSoftware("nobody", []string{"git"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown agent on add: err = %v", err)
	}
	if err := s.ArchiveAgent("bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentSoftware("bob", []string{"git"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("archived agent: err = %v", err)
	}
}

func TestEffectiveSoftwareIsTheSortedUnion(t *testing.T) {
	s := newSoftwareStore(t)
	if err := s.WriteOrgSoftware([]string{"jq", "git"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentSoftware("alice", []string{"git", "ffmpeg"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.EffectiveSoftware("alice")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ffmpeg", "git", "jq"}; !reflect.DeepEqual(got, want) {
		t.Errorf("alice = %v, want %v", got, want)
	}
	got, err = s.EffectiveSoftware("bob")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"git", "jq"}; !reflect.DeepEqual(got, want) {
		t.Errorf("bob = %v, want %v", got, want)
	}
	if _, err := s.EffectiveSoftware("nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown agent: err = %v", err)
	}
}

func TestOrgSoftwareAddAndRemove(t *testing.T) {
	s := newSoftwareStore(t)
	got, err := s.AddOrgSoftware([]string{"jq", "Git"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"git", "jq"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after add = %v, want %v", got, want)
	}
	if got, err = s.AddOrgSoftware([]string{"ffmpeg", "jq"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ffmpeg", "git", "jq"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after second add = %v, want %v", got, want)
	}
	if _, err := s.AddOrgSoftware([]string{"ok", "git-"}); !errors.Is(err, ErrInvalidSoftware) {
		t.Errorf("bad name: err = %v", err)
	}
	if got, err = s.RemoveOrgSoftware("git"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ffmpeg", "jq"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after remove = %v, want %v", got, want)
	}
	if _, err := s.RemoveOrgSoftware("git"); !errors.Is(err, ErrSoftwareNotListed) {
		t.Errorf("remove twice: err = %v", err)
	}
	if read, _ := s.ReadOrgSoftware(); !reflect.DeepEqual(read, []string{"ffmpeg", "jq"}) {
		t.Errorf("on disk = %v", read)
	}
}

func TestRemoveAgentSoftware(t *testing.T) {
	s := newSoftwareStore(t)
	if err := s.SetAgentSoftware("alice", []string{"ffmpeg", "git"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAgentSoftware("alice", "ffmpeg"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetAgent("alice"); !reflect.DeepEqual(a.Software, []string{"git"}) {
		t.Errorf("after remove = %v", a.Software)
	}
	if err := s.RemoveAgentSoftware("alice", "ffmpeg"); !errors.Is(err, ErrSoftwareNotListed) {
		t.Errorf("remove twice: err = %v", err)
	}
	if err := s.RemoveAgentSoftware("alice", "git"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetAgent("alice"); a.Software != nil {
		t.Errorf("after removing the last = %#v, want nil", a.Software)
	}
	if err := s.RemoveAgentSoftware("nobody", "git"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown agent: err = %v", err)
	}
}

// Every agent.yaml setter is a read-modify-write under one lock, and so
// is every software.yaml change: setters for different fields landing
// together all stick. Without the lock a setter reads before another's
// write and writes the old value back.
func TestAgentYAMLSettersDoNotLoseEachOther(t *testing.T) {
	s := newSoftwareStore(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, 4*n)
	for i := range n {
		pkg := fmt.Sprintf("pkg%02d", i)
		wg.Add(4)
		go func() { defer wg.Done(); errs <- s.AddAgentSoftware("alice", []string{pkg}) }()
		go func() { defer wg.Done(); errs <- s.SetAgentModel("alice", "model-x") }()
		go func() { defer wg.Done(); errs <- s.SetAgentEffort("alice", "high") }()
		go func() { defer wg.Done(); _, err := s.AddOrgSoftware([]string{pkg}); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.GetAgent("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Software) != n || a.Model != "model-x" || a.Effort != "high" {
		t.Errorf("agent.yaml lost a write: %d packages, model %q, effort %q", len(a.Software), a.Model, a.Effort)
	}
	if org, _ := s.ReadOrgSoftware(); len(org) != n {
		t.Errorf("software.yaml lost a write: %d packages, want %d", len(org), n)
	}
}

// A name saved by hand that is not a Debian package name breaks
// nothing: the lists still read and change, the name stays listed until
// it is removed, and the shell installs everything else.
func TestSavedInvalidSoftwareNameIsListedSkippedAndRemovable(t *testing.T) {
	s := newSoftwareStore(t)
	if err := os.WriteFile(s.path(softwareFilename), []byte("packages: [jq, \"Bad Name\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.updateAgent("alice", func(a *Agent) error {
		a.Software = []string{"ffmpeg", "x/y"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	org, err := s.ReadOrgSoftware()
	if err != nil || !reflect.DeepEqual(org, []string{"bad name", "jq"}) {
		t.Fatalf("ReadOrgSoftware = %v, %v; want both names listed", org, err)
	}
	eff, err := s.EffectiveSoftware("alice")
	if err != nil || !reflect.DeepEqual(eff, []string{"ffmpeg", "jq"}) {
		t.Fatalf("EffectiveSoftware = %v, %v; want the valid names only", eff, err)
	}
	if got := InvalidSoftware(CleanSoftware(append(org, "x/y"))); !reflect.DeepEqual(got, []string{"bad name", "x/y"}) {
		t.Errorf("InvalidSoftware = %v", got)
	}

	if org, err = s.AddOrgSoftware([]string{"git"}); err != nil || !reflect.DeepEqual(org, []string{"bad name", "git", "jq"}) {
		t.Errorf("AddOrgSoftware = %v, %v; want the add kept beside the listed names", org, err)
	}
	if org, err = s.RemoveOrgSoftware("Bad Name"); err != nil || !reflect.DeepEqual(org, []string{"git", "jq"}) {
		t.Errorf("RemoveOrgSoftware = %v, %v", org, err)
	}
	if err := s.AddAgentSoftware("alice", []string{"git"}); err != nil {
		t.Fatalf("AddAgentSoftware beside an invalid name: %v", err)
	}
	if err := s.RemoveAgentSoftware("alice", "x/y"); err != nil {
		t.Fatalf("RemoveAgentSoftware of the invalid name: %v", err)
	}
	if a, _ := s.GetAgent("alice"); !reflect.DeepEqual(a.Software, []string{"ffmpeg", "git"}) {
		t.Errorf("own list = %v, want [ffmpeg git]", a.Software)
	}
}
