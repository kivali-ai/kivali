package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// signalingAgentPod reports each provisioned slug on a channel, so a
// test waits for the background reprovision without polling.
type signalingAgentPod struct{ got chan string }

func (f *signalingAgentPod) Provision(_ context.Context, slug string) error {
	f.got <- slug
	return nil
}

func (f *signalingAgentPod) Destroy(context.Context, string) error { return nil }

func softwareServer(t *testing.T) (*Server, *signalingAgentPod) {
	t.Helper()
	srv := newTestServer(t)
	for _, slug := range []string{"alice", "bob"} {
		if err := srv.Store.CreateAgent(store.Agent{Slug: slug, Role: slug, ReportsTo: "ceo"}, "role"); err != nil {
			t.Fatal(err)
		}
	}
	pod := &signalingAgentPod{got: make(chan string, 16)}
	srv.AgentPod = pod
	return srv, pod
}

// received collects n provisioned slugs, sorted.
func (f *signalingAgentPod) received(n int) []string {
	out := make([]string, 0, n)
	for range n {
		out = append(out, <-f.got)
	}
	slices.Sort(out)
	return out
}

func softwareDelete(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	return orgDo(t, srv, "", http.MethodDelete, path, "", nil)
}

func TestAPIOrgSoftware(t *testing.T) {
	srv, pod := softwareServer(t)
	sw := orgOK[apitypes.OrgSoftware](t, orgGet(t, srv, "/api/v1/org/software"))
	if sw.Packages == nil || len(sw.Packages) != 0 {
		t.Fatalf("empty org list = %#v", sw.Packages)
	}
	sw = orgOK[apitypes.OrgSoftware](t, orgJSON(t, srv, http.MethodPost, "/api/v1/org/software", apitypes.SoftwareAdd{Packages: []string{" JQ ", "git", "jq"}}))
	if want := []string{"git", "jq"}; !reflect.DeepEqual(sw.Packages, want) {
		t.Errorf("after add = %v, want %v", sw.Packages, want)
	}
	if got, _ := srv.Store.ReadOrgSoftware(); !reflect.DeepEqual(got, []string{"git", "jq"}) {
		t.Errorf("stored = %v", got)
	}
	// Every active agent is re-provisioned, and the owner has no pod.
	if got := pod.received(2); !slices.Contains(got, "alice") || !slices.Contains(got, "bob") {
		t.Errorf("provisioned %v, want alice and bob", got)
	}
	sw = orgOK[apitypes.OrgSoftware](t, softwareDelete(t, srv, "/api/v1/org/software/git"))
	if want := []string{"jq"}; !reflect.DeepEqual(sw.Packages, want) {
		t.Errorf("after remove = %v, want %v", sw.Packages, want)
	}
	if got := pod.received(2); !reflect.DeepEqual(got, []string{"alice", "bob"}) {
		t.Errorf("provisioned %v after remove, want alice and bob", got)
	}
	assertAPIError(t, softwareDelete(t, srv, "/api/v1/org/software/git"), http.StatusNotFound)
}

// Two pages open at once each add a name from what they last read: both
// names stick, because a write applies to the list as it stands.
func TestAPIOrgSoftwareWritesApplyToTheCurrentList(t *testing.T) {
	srv, pod := softwareServer(t)
	orgOK[apitypes.OrgSoftware](t, orgJSON(t, srv, http.MethodPost, "/api/v1/org/software", apitypes.SoftwareAdd{Packages: []string{"git"}}))
	pod.received(2)
	sw := orgOK[apitypes.OrgSoftware](t, orgJSON(t, srv, http.MethodPost, "/api/v1/org/software", apitypes.SoftwareAdd{Packages: []string{"jq"}}))
	pod.received(2)
	if want := []string{"git", "jq"}; !reflect.DeepEqual(sw.Packages, want) {
		t.Errorf("after two adds = %v, want %v", sw.Packages, want)
	}
}

func TestAPIOrgSoftwareRefusesABadName(t *testing.T) {
	srv, _ := softwareServer(t)
	rr := orgJSON(t, srv, http.MethodPost, "/api/v1/org/software", apitypes.SoftwareAdd{Packages: []string{"git", "rm -rf /"}})
	assertAPIError(t, rr, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "rm -rf /") {
		t.Errorf("message does not name the offender: %s", rr.Body.String())
	}
	if got, _ := srv.Store.ReadOrgSoftware(); len(got) != 0 {
		t.Errorf("a refused write saved %v", got)
	}
	assertAPIError(t, orgJSON(t, srv, http.MethodPost, "/api/v1/org/software", apitypes.SoftwareAdd{Packages: []string{}}), http.StatusBadRequest)
}

func TestAPIAgentSoftware(t *testing.T) {
	srv, pod := softwareServer(t)
	if err := srv.Store.WriteOrgSoftware([]string{"git"}); err != nil {
		t.Fatal(err)
	}
	sw := orgOK[apitypes.AgentSoftware](t, orgGet(t, srv, "/api/v1/agents/alice/software"))
	if !reflect.DeepEqual(sw.Org, []string{"git"}) || sw.Own == nil || len(sw.Own) != 0 {
		t.Fatalf("initial = %#v", sw)
	}
	sw = orgOK[apitypes.AgentSoftware](t, orgJSON(t, srv, http.MethodPost, "/api/v1/agents/alice/software", apitypes.SoftwareAdd{Packages: []string{"poppler-utils", "FFmpeg"}}))
	if !reflect.DeepEqual(sw.Own, []string{"ffmpeg", "poppler-utils"}) || !reflect.DeepEqual(sw.Org, []string{"git"}) {
		t.Errorf("after add = %#v", sw)
	}
	// Only alice's pod is re-provisioned.
	if got := pod.received(1); !reflect.DeepEqual(got, []string{"alice"}) {
		t.Errorf("provisioned %v, want [alice]", got)
	}
	if b := orgOK[apitypes.AgentSoftware](t, orgGet(t, srv, "/api/v1/agents/bob/software")); len(b.Own) != 0 {
		t.Errorf("bob's own list = %v", b.Own)
	}
	// A second add keeps the first.
	sw = orgOK[apitypes.AgentSoftware](t, orgJSON(t, srv, http.MethodPost, "/api/v1/agents/alice/software", apitypes.SoftwareAdd{Packages: []string{"jq"}}))
	if want := []string{"ffmpeg", "jq", "poppler-utils"}; !reflect.DeepEqual(sw.Own, want) {
		t.Errorf("after second add = %v, want %v", sw.Own, want)
	}
	pod.received(1)
	sw = orgOK[apitypes.AgentSoftware](t, softwareDelete(t, srv, "/api/v1/agents/alice/software/ffmpeg"))
	if want := []string{"jq", "poppler-utils"}; !reflect.DeepEqual(sw.Own, want) {
		t.Errorf("after remove = %v, want %v", sw.Own, want)
	}
	if got := pod.received(1); !reflect.DeepEqual(got, []string{"alice"}) {
		t.Errorf("provisioned %v after remove, want [alice]", got)
	}
}

// Names saved by hand that are not Debian package names are listed and
// marked, removable, and break neither reads nor writes.
func TestAPISoftwareListsAndRemovesSavedInvalidNames(t *testing.T) {
	srv, pod := softwareServer(t)
	if err := os.WriteFile(filepath.Join(srv.Store.Root(), "software.yaml"), []byte("packages: [git, \"bad name\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sw := orgOK[apitypes.OrgSoftware](t, orgGet(t, srv, "/api/v1/org/software"))
	if !reflect.DeepEqual(sw.Packages, []string{"bad name", "git"}) || !reflect.DeepEqual(sw.Invalid, []string{"bad name"}) {
		t.Fatalf("GET = %#v, want both names listed and the bad one marked", sw)
	}
	a := orgOK[apitypes.AgentSoftware](t, orgGet(t, srv, "/api/v1/agents/alice/software"))
	if !reflect.DeepEqual(a.Invalid, []string{"bad name"}) || !reflect.DeepEqual(a.Own, []string{}) {
		t.Errorf("agent GET = %#v", a)
	}
	sw = orgOK[apitypes.OrgSoftware](t, softwareDelete(t, srv, "/api/v1/org/software/bad%20name"))
	if !reflect.DeepEqual(sw.Packages, []string{"git"}) || sw.Invalid == nil || len(sw.Invalid) != 0 {
		t.Errorf("after removing the bad name = %#v", sw)
	}
	pod.received(2)
}

func TestAPIAgentSoftwareRefusals(t *testing.T) {
	srv, _ := softwareServer(t)
	add := func(slug string, pkgs ...string) *httptest.ResponseRecorder {
		return orgJSON(t, srv, http.MethodPost, "/api/v1/agents/"+slug+"/software", apitypes.SoftwareAdd{Packages: pkgs})
	}
	assertAPIError(t, add("alice", "bad name"), http.StatusBadRequest)
	assertAPIError(t, add("alice"), http.StatusBadRequest)
	assertAPIError(t, add("nobody", "git"), http.StatusNotFound)
	assertAPIError(t, softwareDelete(t, srv, "/api/v1/agents/alice/software/git"), http.StatusNotFound)
	assertAPIError(t, softwareDelete(t, srv, "/api/v1/agents/nobody/software/git"), http.StatusNotFound)
	assertAPIError(t, orgGet(t, srv, "/api/v1/agents/nobody/software"), http.StatusNotFound)
	assertAPIError(t, orgGet(t, srv, "/api/v1/agents/ceo/software"), http.StatusBadRequest)
	assertAPIError(t, add("ceo", "git"), http.StatusBadRequest)
	assertAPIError(t, softwareDelete(t, srv, "/api/v1/agents/ceo/software/git"), http.StatusBadRequest)
	if err := srv.Store.ArchiveAgent("bob"); err != nil {
		t.Fatal(err)
	}
	assertAPIError(t, orgGet(t, srv, "/api/v1/agents/bob/software"), http.StatusNotFound)
	assertAPIError(t, add("bob", "git"), http.StatusNotFound)
	assertAPIError(t, softwareDelete(t, srv, "/api/v1/agents/bob/software/git"), http.StatusNotFound)
}
