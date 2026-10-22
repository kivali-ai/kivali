package store

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/integration"
)

// multipleEcho installs the echo built-in taking several connections,
// with connections a (older) and b, labelled.
func multipleEcho(t *testing.T, s *FSStore) {
	t.Helper()
	echo := echotest.New()
	echo.Multiple = true
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echo.Manifest()}, intNow); err != nil {
		t.Fatal(err)
	}
	for i, c := range []IntegrationConnection{
		{ID: "a", Label: "GitHub", CreatedAt: intNow, SecretName: "kivali-connection-echo-a", SecretKeys: []string{"token"}},
		{ID: "b", Label: "Company GitLab", CreatedAt: intNow.Add(time.Minute), SecretName: "kivali-connection-echo-b", SecretKeys: []string{"token"}},
	} {
		if err := s.PutIntegrationConnection(echotest.Name, c); err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
	}
}

func setAccessFor(t *testing.T, s *FSStore, name, slug, connID string) {
	t.Helper()
	if err := s.SetIntegrationAccess(name, IntegrationAccess{Slug: slug, ConnectionID: connID, Access: map[string]any{}, SetAt: intNow}); err != nil {
		t.Fatal(err)
	}
}

func invalidBySlug(t *testing.T, s *FSStore, name string) map[string]string {
	t.Helper()
	rows, err := s.ListIntegrationAccess(name)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Slug] = r.Invalid
	}
	return out
}

// Why this test exists: a connection's label is what the owner and the
// access rows know it by. A label lost on the way to disk would show
// every Git connection unnamed after a restart, and a connection read
// by id that came back as the oldest would hand an agent's call the
// wrong host's token.
func TestIntegrationConnectionLabelAndByID(t *testing.T) {
	s := mustStore(t)
	multipleEcho(t, s)
	b, err := s.GetIntegrationConnectionByID(echotest.Name, "b")
	if err != nil || b.ID != "b" || b.Label != "Company GitLab" || b.SecretName != "kivali-connection-echo-b" {
		t.Fatalf("b = %+v, %v", b, err)
	}
	all, err := s.ListIntegrationConnections(echotest.Name)
	if err != nil || len(all) != 2 || all[0].Label != "GitHub" || all[1].Label != "Company GitLab" {
		t.Fatalf("all = %+v, %v", all, err)
	}
	for _, id := range []string{"c", "../a", ""} {
		if _, err := s.GetIntegrationConnectionByID(echotest.Name, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("id %q: %v", id, err)
		}
	}
}

// Why this test exists: for an integration that takes several
// connections, an access row is valid for whichever connection it
// names, not only the oldest; if the single-connection rule applied,
// every agent on the second connection would read "set for a
// connection that is no longer there" and lose its access. For a
// one-connection integration the rule stays: a row naming any other id
// than the one connection is invalid.
func TestIntegrationAccessValidAcrossConnections(t *testing.T) {
	s := mustStore(t)
	mustAgent(t, s, "dana")
	mustAgent(t, s, "lee")
	mustAgent(t, s, "kim")
	multipleEcho(t, s)
	setAccessFor(t, s, echotest.Name, "dana", "a")
	setAccessFor(t, s, echotest.Name, "lee", "b")
	setAccessFor(t, s, echotest.Name, "kim", "gone")
	got := invalidBySlug(t, s, echotest.Name)
	if got["dana"] != "" || got["lee"] != "" || got["kim"] != "it was set for a connection that is no longer there" {
		t.Errorf("several connections: %+v", got)
	}

	approvedWithConnection(t, s, "crm")
	if err := s.PutIntegrationConnection("crm", IntegrationConnection{ID: "c2", CreatedAt: intNow.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntegrationAccess("crm", IntegrationAccess{Slug: "dana", ConnectionID: "c1", Access: map[string]any{"blogs": []any{"M"}}, SetAt: intNow}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetIntegrationAccess("crm", IntegrationAccess{Slug: "lee", ConnectionID: "c2", Access: map[string]any{"blogs": []any{"M"}}, SetAt: intNow}); err != nil {
		t.Fatal(err)
	}
	got = invalidBySlug(t, s, "crm")
	if got["dana"] != "" || got["lee"] != "it was set for a connection that is no longer there" {
		t.Errorf("one connection: %+v", got)
	}
}

// Why this test exists: deleting one of several connections must take
// only the access set for it. Clearing every row, as deleting the one
// connection does, would cut off the agents on the other host; leaving
// the deleted connection's rows would keep them pointing at nothing.
// The slugs come back so the page can say whose access went.
func TestDeleteIntegrationConnectionByID(t *testing.T) {
	s := mustStore(t)
	mustAgent(t, s, "dana")
	mustAgent(t, s, "lee")
	mustAgent(t, s, "kim")
	multipleEcho(t, s)
	setAccessFor(t, s, echotest.Name, "dana", "a")
	setAccessFor(t, s, echotest.Name, "lee", "b")
	setAccessFor(t, s, echotest.Name, "kim", "b")

	gone, cleared, err := s.DeleteIntegrationConnectionByID(echotest.Name, "b")
	if err != nil || gone.ID != "b" || gone.SecretName != "kivali-connection-echo-b" || !slices.Equal(cleared, []string{"kim", "lee"}) {
		t.Fatalf("deleted = %+v, cleared %v, %v", gone, cleared, err)
	}
	if _, err := s.GetIntegrationConnectionByID(echotest.Name, "b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("b after delete: %v", err)
	}
	if a, err := s.GetIntegrationConnectionByID(echotest.Name, "a"); err != nil || a.Label != "GitHub" {
		t.Errorf("a after deleting b = %+v, %v", a, err)
	}
	rows, _ := s.ListIntegrationAccess(echotest.Name)
	if len(rows) != 1 || rows[0].Slug != "dana" || rows[0].Invalid != "" {
		t.Errorf("access after deleting b = %+v", rows)
	}
	if _, _, err := s.DeleteIntegrationConnectionByID(echotest.Name, "b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting b again: %v", err)
	}

	// Clearing by connection alone leaves the connection and the other
	// rows.
	setAccessFor(t, s, echotest.Name, "lee", "a")
	cleared, err = s.clearIntegrationAccessForConnectionLocked(echotest.Name, "a")
	if err != nil || !slices.Equal(cleared, []string{"dana", "lee"}) {
		t.Errorf("cleared = %v, %v", cleared, err)
	}
	if _, err := s.GetIntegrationConnectionByID(echotest.Name, "a"); err != nil {
		t.Errorf("a after clearing its access: %v", err)
	}
}

// Why this test exists: a sign-in connection's state (how and when the
// owner signed in, when the access token expires, why it needs a new
// sign-in) is what the credential path decides on and what the page
// and Needs you show. Lost on the way to disk, a restart would refresh
// a token too late or forget that the owner must sign in again; and a
// token connection must keep no sign-in state at all.
func TestIntegrationConnectionSignInRoundTrip(t *testing.T) {
	s := mustStore(t)
	multipleEcho(t, s)
	cest := time.FixedZone("CEST", 2*60*60)
	in := IntegrationConnection{ID: "c", Label: "GitHub app", CreatedAt: intNow, SignIn: &ConnectionSignIn{
		Method: "device", SignedInAt: intNow.In(cest), ExpiresAt: intNow.Add(8 * time.Hour).In(cest), Scope: "repo,read:org",
		Error: "the vendor refused it (bad_refresh_token)", ErrorAt: intNow.Add(9 * time.Hour),
	}}
	if err := s.PutIntegrationConnection(echotest.Name, in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIntegrationConnectionByID(echotest.Name, "c")
	if err != nil {
		t.Fatal(err)
	}
	si := got.SignIn
	if si == nil || si.Method != "device" || !si.SignedInAt.Equal(intNow) || !si.ExpiresAt.Equal(intNow.Add(8*time.Hour)) ||
		si.Scope != "repo,read:org" || si.Error != in.SignIn.Error || !si.ErrorAt.Equal(intNow.Add(9*time.Hour)) {
		t.Errorf("sign-in read back as %+v", si)
	}
	if !got.NeedsSignIn(true) {
		t.Error("a connection whose refresh was refused does not need a sign-in")
	}
	got.SignIn.Error = ""
	if got.NeedsSignIn(true) || !got.NeedsSignIn(false) {
		t.Error("a working sign-in: needs one only without its access token")
	}
	a, err := s.GetIntegrationConnectionByID(echotest.Name, "a")
	if err != nil || a.SignIn != nil || !a.NeedsSignIn(true) {
		t.Errorf("a connection never signed in: %+v, %v", a, err)
	}
}
