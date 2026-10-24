package store

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/gitintegration"
	"github.com/kivali-ai/kivali/internal/integration"
)

// echoWithConnection installs the echo built-in with connection a.
func echoWithConnection(t *testing.T, s *FSStore) {
	t.Helper()
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{echotest.New().Manifest()}, intNow); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIntegrationConnection(echotest.Name, IntegrationConnection{ID: "a", CreatedAt: intNow, SecretName: "kivali-connection-echo-a", SecretKeys: []string{"token"}}); err != nil {
		t.Fatal(err)
	}
}

func setAccessFor(t *testing.T, s *FSStore, name, slug string) {
	t.Helper()
	if err := s.SetIntegrationAccess(name, IntegrationAccess{Slug: slug, Access: map[string]any{}, SetAt: intNow}); err != nil {
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

// Why this test exists: a setup flow and a shell job hold the id of the
// connection they started with and ask by it whether the connection is
// still that one. An id read back as the integration's connection
// whatever it named would hand a job's result to a connection made
// after it, and a path-like id must never reach the file system.
func TestIntegrationConnectionByID(t *testing.T) {
	s := mustStore(t)
	echoWithConnection(t, s)
	a, err := s.GetIntegrationConnectionByID(echotest.Name, "a")
	if err != nil || a.ID != "a" || a.SecretName != "kivali-connection-echo-a" {
		t.Fatalf("a = %+v, %v", a, err)
	}
	for _, id := range []string{"b", "../a", ""} {
		if _, err := s.GetIntegrationConnectionByID(echotest.Name, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("id %q: %v", id, err)
		}
	}
}

// Why this test exists: an access row is the agent's access through the
// integration's one connection, whatever that connection's id. A row
// still carrying the connection_id it was written with must read, not
// fail the whole list, and a row is valid exactly while the
// integration has its connection.
func TestIntegrationAccessValidWithTheConnection(t *testing.T) {
	s := mustStore(t)
	mustAgent(t, s, "dana")
	mustAgent(t, s, "lee")
	echoWithConnection(t, s)
	setAccessFor(t, s, echotest.Name, "dana")
	writeLegacyYAML(t, s, echotest.Name, "access/lee.yaml", map[string]any{"slug": "lee", "connection_id": "a", "access": map[string]any{}, "set_at": intNow})
	if got := invalidBySlug(t, s, echotest.Name); got["dana"] != "" || got["lee"] != "" || len(got) != 2 {
		t.Errorf("with the connection: %+v", got)
	}
	raw, _ := os.ReadFile(filepath.Join(s.Root(), IntegrationsDirName, echotest.Name, "access", "dana.yaml"))
	if strings.Contains(string(raw), "connection_id") {
		t.Errorf("a row is written with a connection id:\n%s", raw)
	}
	if err := os.Remove(filepath.Join(s.Root(), IntegrationsDirName, echotest.Name, "connections", "a.yaml")); err != nil {
		t.Fatal(err)
	}
	if got := invalidBySlug(t, s, echotest.Name); got["dana"] != "it was set for a connection that is no longer there" {
		t.Errorf("without the connection: %+v", got)
	}
}

// Why this test exists: a setup flow that made the connection and
// failed forgets it by its id. The agents' access goes with it, as the
// connection's; and a stale id (the connection was replaced meanwhile)
// must take neither the connection there now nor its access.
func TestDeleteIntegrationConnectionByID(t *testing.T) {
	s := mustStore(t)
	mustAgent(t, s, "dana")
	mustAgent(t, s, "lee")
	echoWithConnection(t, s)
	setAccessFor(t, s, echotest.Name, "dana")
	setAccessFor(t, s, echotest.Name, "lee")

	if _, _, err := s.DeleteIntegrationConnectionByID(echotest.Name, "b"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting b: %v", err)
	}
	if rows, _ := s.ListIntegrationAccess(echotest.Name); len(rows) != 2 {
		t.Errorf("access after deleting another id = %+v", rows)
	}
	gone, cleared, err := s.DeleteIntegrationConnectionByID(echotest.Name, "a")
	if err != nil || gone.ID != "a" || gone.SecretName != "kivali-connection-echo-a" || !slices.Equal(cleared, []string{"dana", "lee"}) {
		t.Fatalf("deleted = %+v, cleared %v, %v", gone, cleared, err)
	}
	if _, err := s.GetIntegrationConnection(echotest.Name); !errors.Is(err, ErrNotFound) {
		t.Errorf("connection after delete: %v", err)
	}
	if rows, _ := s.ListIntegrationAccess(echotest.Name); len(rows) != 0 {
		t.Errorf("access after delete = %+v", rows)
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
	echoWithConnection(t, s)
	a, err := s.GetIntegrationConnectionByID(echotest.Name, "a")
	if err != nil || a.SignIn != nil || !a.NeedsSignIn(true) {
		t.Errorf("a connection never signed in: %+v, %v", a, err)
	}
	cest := time.FixedZone("CEST", 2*60*60)
	in := IntegrationConnection{ID: "a", CreatedAt: intNow, SignIn: &ConnectionSignIn{
		Method: "device", SignedInAt: intNow.In(cest), ExpiresAt: intNow.Add(8 * time.Hour).In(cest), Lifetime: 8 * time.Hour, Scope: "repo,read:org",
		Error: "the vendor refused it (bad_refresh_token)", ErrorAt: intNow.Add(9 * time.Hour),
	}}
	if err := s.PutIntegrationConnection(echotest.Name, in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIntegrationConnection(echotest.Name)
	if err != nil {
		t.Fatal(err)
	}
	si := got.SignIn
	if si == nil || si.Method != "device" || !si.SignedInAt.Equal(intNow) || !si.ExpiresAt.Equal(intNow.Add(8*time.Hour)) ||
		si.Scope != "repo,read:org" || si.Lifetime != 8*time.Hour || si.Error != in.SignIn.Error || !si.ErrorAt.Equal(intNow.Add(9*time.Hour)) {
		t.Errorf("sign-in read back as %+v", si)
	}
	if !got.NeedsSignIn(true) {
		t.Error("a connection whose refresh was refused does not need a sign-in")
	}
	got.SignIn.Error = ""
	if got.NeedsSignIn(true) || !got.NeedsSignIn(false) {
		t.Error("a working sign-in: needs one only without its access token")
	}
}

// writeLegacyYAML writes v at rel under the integration's directory, as
// a record from before one connection per installation is stored.
func writeLegacyYAML(t *testing.T, s *FSStore, name, rel string, v map[string]any) {
	t.Helper()
	if err := writeYAMLFile(filepath.Join(s.Root(), IntegrationsDirName, name, rel), v); err != nil {
		t.Fatal(err)
	}
}

// legacyGit is Git as a binary that took several connections left it:
// connections a (GitHub, labelled, oldest), b (no label, its host
// gitlab.example.com) and c (labelled Old), and the access rows rows
// names, slug to connection id.
func legacyGit(t *testing.T, s *FSStore, rows map[string]string) {
	t.Helper()
	if err := s.InstallBuiltinIntegrations([]*integration.Manifest{gitintegration.New().Manifest()}, intNow); err != nil {
		t.Fatal(err)
	}
	for i, c := range []map[string]any{
		{"id": "a", "label": "GitHub", "fields": map[string]any{"host": "github.com"}},
		{"id": "b", "fields": map[string]any{"host": "gitlab.example.com"}},
		{"id": "c", "label": "Old", "fields": map[string]any{"host": "git.old.example"}},
	} {
		id := c["id"].(string)
		c["created_at"] = intNow.Add(time.Duration(i) * time.Minute)
		c["secret_name"] = "kivali-connection-git-" + id
		c["secret_keys"] = []string{"token"}
		writeLegacyYAML(t, s, gitintegration.Name, "connections/"+id+".yaml", c)
	}
	for slug, id := range rows {
		mustAgent(t, s, slug)
		writeLegacyYAML(t, s, gitintegration.Name, "access/"+slug+".yaml", map[string]any{
			"slug": slug, "connection_id": id, "access": map[string]any{"pull_repositories": []any{"acme/*"}}, "set_at": intNow, "set_by": "ceo",
		})
	}
}

// gitFiles is every file under Git's directory but its versions, by
// path, with its bytes.
func gitFiles(t *testing.T, s *FSStore) map[string]string {
	t.Helper()
	root := filepath.Join(s.Root(), IntegrationsDirName, gitintegration.Name)
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, "versions") {
			return nil
		}
		b, err := os.ReadFile(path)
		out[rel] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Why this test exists: Git took several connections, and its agents'
// access each named one. With one connection per installation, boot
// keeps the one most agents use, and must lose nothing: a connection
// set aside stays on disk with its Secret named, a row set for it
// grants nothing and says which connection it was set for and to set
// it again, and running boot again changes nothing. Keeping the oldest
// regardless would cut off most agents; deleting the others would lose
// tokens the owner may still need.
func TestMigrateIntegrationConnectionsKeepsTheMostUsed(t *testing.T) {
	s := mustStore(t)
	legacyGit(t, s, map[string]string{"dana": "a", "lee": "b", "kim": "b", "ana": "c", "pat": "gone"})
	if err := s.MigrateIntegrationConnections(); err != nil {
		t.Fatal(err)
	}
	conns, err := s.ListIntegrationConnections(gitintegration.Name)
	if err != nil || len(conns) != 1 || conns[0].ID != "b" {
		t.Fatalf("connections = %+v, %v", conns, err)
	}
	retired, err := s.RetiredIntegrationConnections(gitintegration.Name)
	if err != nil || len(retired) != 2 || retired[0].ID != "a" || retired[0].SecretName != "kivali-connection-git-a" || retired[1].ID != "c" || retired[1].Fields["host"] != "git.old.example" {
		t.Errorf("retired = %+v, %v", retired, err)
	}
	got := invalidBySlug(t, s, gitintegration.Name)
	want := map[string]string{
		"lee":  "",
		"kim":  "",
		"dana": "set for the connection GitHub, which Git no longer has; set it again",
		"ana":  "set for the connection Old, which Git no longer has; set it again",
		"pat":  "it was set for a connection that is no longer there; set it again",
	}
	for slug, w := range want {
		if got[slug] != w {
			t.Errorf("%s: invalid %q, want %q", slug, got[slug], w)
		}
	}
	files := gitFiles(t, s)
	if strings.Contains(files["access/lee.yaml"], "connection_id") || !strings.Contains(files["access/dana.yaml"], "connection_id: a") {
		t.Errorf("rows as written:\nlee:\n%s\ndana:\n%s", files["access/lee.yaml"], files["access/dana.yaml"])
	}
	if !strings.Contains(files["connections-retired/a.yaml"], "label: GitHub") {
		t.Errorf("a set aside as:\n%s", files["connections-retired/a.yaml"])
	}

	// Again, as the next boot runs it: nothing changes.
	if err := s.MigrateIntegrationConnections(); err != nil {
		t.Fatal(err)
	}
	if again := gitFiles(t, s); !maps.Equal(again, files) {
		t.Errorf("a second run changed the files:\nbefore %v\nafter  %v", files, again)
	}

	// Setting the access again makes the row valid.
	if err := s.SetIntegrationAccess(gitintegration.Name, IntegrationAccess{Slug: "dana", Access: map[string]any{"pull_repositories": []any{"acme/*"}}, SetAt: intNow}); err != nil {
		t.Fatal(err)
	}
	if got := invalidBySlug(t, s, gitintegration.Name); got["dana"] != "" {
		t.Errorf("dana set again: %q", got["dana"])
	}
}

// Why this test exists: when two connections are named by as many
// rows, the oldest is kept, as the page showed it first. A label-less
// connection set aside is named by its host, the name the owner saw.
func TestMigrateIntegrationConnectionsTieKeepsTheOldest(t *testing.T) {
	s := mustStore(t)
	legacyGit(t, s, map[string]string{"dana": "a", "lee": "b"})
	if err := s.MigrateIntegrationConnections(); err != nil {
		t.Fatal(err)
	}
	if c, err := s.GetIntegrationConnection(gitintegration.Name); err != nil || c.ID != "a" {
		t.Fatalf("connection = %+v, %v", c, err)
	}
	got := invalidBySlug(t, s, gitintegration.Name)
	if got["dana"] != "" || got["lee"] != "set for the connection gitlab.example.com, which Git no longer has; set it again" {
		t.Errorf("rows: %+v", got)
	}
}

// Why this test exists: an integration that took one connection may
// still hold a row whose connection_id names a connection made before
// the one it has now. That row granted nothing; read without its id it
// would grant access nobody gave for this connection.
func TestMigrateIntegrationConnectionsOneConnection(t *testing.T) {
	s := mustStore(t)
	mustAgent(t, s, "dana")
	mustAgent(t, s, "lee")
	approvedWithConnection(t, s, "crm")
	writeLegacyYAML(t, s, "crm", "access/dana.yaml", map[string]any{"slug": "dana", "connection_id": "c1", "access": map[string]any{"blogs": []any{"M"}}, "set_at": intNow})
	writeLegacyYAML(t, s, "crm", "access/lee.yaml", map[string]any{"slug": "lee", "connection_id": "c0", "access": map[string]any{"blogs": []any{"M"}}, "set_at": intNow})
	if err := s.MigrateIntegrationConnections(); err != nil {
		t.Fatal(err)
	}
	if c, err := s.GetIntegrationConnection("crm"); err != nil || c.ID != "c1" {
		t.Fatalf("connection = %+v, %v", c, err)
	}
	got := invalidBySlug(t, s, "crm")
	if got["dana"] != "" || got["lee"] != "it was set for a connection that is no longer there; set it again" {
		t.Errorf("rows: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), IntegrationsDirName, "crm", retiredConnectionsDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a one-connection integration has %s: %v", retiredConnectionsDir, err)
	}
}
