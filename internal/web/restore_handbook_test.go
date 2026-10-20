package web

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/seed"
	"github.com/kivali-ai/kivali/internal/store"
)

// An archive from before the runtime's rules moved into Kivali OS
// carries a handbook without the four sections. Restoring it runs the
// same pass as boot: the old handbook is kept as an attachment the
// Chief of Staff can read, the default is written, and the Chief of
// Staff is told. Without this a restored team would run on a handbook
// every write door then refuses to save.
func TestRestoreSetsAnOldHandbookAsideForTheChiefOfStaff(t *testing.T) {
	src := newTestServer(t)
	const old = "# Kivali Handbook\n\n## The company\n\nWe sell rope.\n\n## How the org works\n\nCharts.\n"
	writeFile(t, filepath.Join(src.Store.Root(), "handbook.md"), old)
	if err := src.Store.CreateAgent(store.Agent{Slug: graph.ChiefOfStaffSlug, Role: "Chief of Staff"}, "# Chief of Staff"); err != nil {
		t.Fatal(err)
	}
	zipBytes := backupZip(t, src).Body.Bytes()

	dst := newTestServer(t)
	if rr := restoreZip(t, dst, "backup.zip", zipBytes); rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	if got, _ := dst.Store.ReadHandbook(); got != seed.Handbook {
		t.Errorf("restored handbook is not the default:\n%s", got)
	}
	hist, err := dst.Store.ReadChatHistory(graph.ChiefOfStaffSlug)
	if err != nil || len(hist) != 1 {
		t.Fatalf("Chief of Staff chat = %d rows, %v", len(hist), err)
	}
	note := hist[0]
	if note.Kind != store.KindWakeUpdate || !note.Quiet || len(note.Attachments) != 1 || note.Attachments[0].Name != store.PreviousHandbookName {
		t.Fatalf("note = %+v", note)
	}
	if !strings.Contains(note.Content, store.PreviousHandbookName) {
		t.Errorf("note does not name the attachment: %q", note.Content)
	}
	if got, err := dst.Store.ReadAttachmentText(note.Attachments[0].SHA); err != nil || got != old {
		t.Errorf("kept handbook = %q, %v", got, err)
	}
}
