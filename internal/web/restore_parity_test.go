package web

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/builtinskills"
	"github.com/kivali-ai/kivali/internal/store"
)

// stagedRestores lists what a restore staged in the data directory and
// left there.
func stagedRestores(t *testing.T, srv *Server) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(srv.Store.Root(), ".tmp-restore-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The server's temp directory is a 1 GiB emptyDir in the chart, and an
// upload is allowed 2 GB, so the upload streams once onto the data
// volume rather than through the temp directory: a restore succeeds with a temp directory it cannot write to at all, and
// leaves nothing staged behind whether it succeeds or fails.
func TestRestoreStagesTheUploadOnTheDataVolumeNotInTemp(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	src := newTestServer(t)
	writeFile(t, filepath.Join(src.Store.Root(), "handbook.md"), "# the rules")
	zipBytes := backupZip(t, src).Body.Bytes()
	dst := newTestServer(t)
	bad := newTestServer(t)

	tmp := t.TempDir()
	if err := os.Chmod(tmp, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmp, 0o700) })
	t.Setenv("TMPDIR", tmp)

	if rr := restoreZip(t, dst, "backup.zip", zipBytes); rr.Code != http.StatusOK {
		t.Fatalf("restore with an unwritable temp dir: %d %s", rr.Code, rr.Body.String())
	}
	if got, _ := dst.Store.ReadHandbook(); got != "# the rules" {
		t.Errorf("restored handbook = %q", got)
	}
	if left := stagedRestores(t, dst); len(left) > 0 {
		t.Errorf("a successful restore left %v", left)
	}

	if rr := restoreZip(t, bad, "junk.zip", []byte("PK\x03\x04 not really")); rr.Code == http.StatusOK {
		t.Fatal("a junk archive restored")
	}
	if left := stagedRestores(t, bad); len(left) > 0 {
		t.Errorf("a failed restore left %v", left)
	}
}

// An in-app restore never restarts the process, so it has to do what
// main.go does to a data directory it did not write, or the restored
// org runs on this deployment's leftovers until the next restart:
//
//   - the restored agents get their pods (a turn for an agent with no
//     pod is dropped), and an agent the backup caught mid-turn gets the
//     disruption row and is resumed;
//   - the archive's built-in skills, written by another version, are
//     replaced by this binary's;
//   - the restored egress allowlist reaches the proxy's copy;
//   - the auto-release scheduler looks at the restored queue.
func TestRestoreDoesWhatABootDoes(t *testing.T) {
	src := e2eServer(t)
	if err := src.Store.CreateAgent(store.Agent{Slug: "alice", Role: "Analyst", ReportsTo: "chief-of-staff"}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	if err := src.Store.MarkActiveTurn("alice", store.ActiveTurnMarker{StartedAt: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), Source: "chat"}); err != nil {
		t.Fatal(err)
	}
	al, err := src.Store.ReadEgressAllowlist()
	if err != nil {
		t.Fatal(err)
	}
	al.Patterns = append(al.Patterns, "restored.example.org")
	if err := src.Store.WriteEgressAllowlist(al); err != nil {
		t.Fatal(err)
	}
	if err := src.Store.InstallBuiltinSkills(); err != nil {
		t.Fatal(err)
	}
	skill := builtinskills.Names()[0]
	skillFile := builtinskills.Files(skill)[0]
	skillPath := func(srv *Server) string {
		return filepath.Join(srv.Store.Root(), "skills", skill, filepath.FromSlash(skillFile.Path))
	}
	writeFile(t, skillPath(src), "another version of this skill")
	zipBytes := backupZip(t, src).Body.Bytes()

	dst := e2eServer(t)
	pods := &fakeAgentPodLifecycle{}
	dst.AgentPod = pods
	proxyCopy := filepath.Join(t.TempDir(), "allowed_egress.yaml")
	dst.Store.SetEgressSyncPath(proxyCopy)
	select { // the scheduler's wake channel holds one; empty it
	case <-dst.autoReleaseCh:
	default:
	}

	if rr := restoreZip(t, dst, "backup.zip", zipBytes); rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	dst.restoreBG.Wait()

	got := pods.provisionedSlugs()
	slices.Sort(got)
	if !slices.Equal(got, []string{"alice", "chief-of-staff"}) {
		t.Errorf("pods provisioned for %v, want alice and chief-of-staff", got)
	}

	hist, err := dst.Store.ReadChatHistory("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 || hist[len(hist)-1].Kind != store.KindRuntimeDisruption {
		t.Errorf("alice's chat after the restore = %+v, want a runtime-disruption row last", hist)
	}
	if m, _ := dst.Store.ReadActiveTurn("alice"); !m.StartedAt.IsZero() {
		t.Errorf("alice's active-turn marker survived the restore: %+v", m)
	}

	if b, _ := os.ReadFile(skillPath(dst)); string(b) != string(skillFile.Data) {
		t.Errorf("built-in skill %s/%s is the archive's, not this binary's: %q", skill, skillFile.Path, b)
	}

	if b, err := os.ReadFile(proxyCopy); err != nil || !strings.Contains(string(b), "restored.example.org") {
		t.Errorf("the proxy's allowlist = %q (%v), want the restored host", b, err)
	}

	select {
	case <-dst.autoReleaseCh:
	default:
		t.Error("the auto-release scheduler was not woken for the restored queue")
	}
}
