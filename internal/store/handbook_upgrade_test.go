package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/graph"
)

const (
	oldHandbook   = "# Kivali Handbook\n\n## What this is\n\nRules.\n\n## The company\n\nWe sell rope.\n\n## How the org works\n\nCharts.\n"
	shapedDefault = "# Handbook\n\n## About\n\nA.\n\n## Values\n\nV.\n\n## Communicating\n\nC.\n\n## Working together\n\nW.\n"
)

var upgradeTime = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// A handbook written before the four sections existed is set aside
// whole and the default takes its place, and the Chief of Staff is
// told where the old one is: as an attachment in its chat, which is
// what makes the file readable under its /files/attachments/. A
// second run changes nothing, so a boot that runs this every time
// does not pile notes into the chat.
func TestUpgradeHandbookSetsTheOldOneAsideForTheChiefOfStaff(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: graph.ChiefOfStaffSlug, Role: "Chief of Staff"}, "# Chief of Staff"); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteHandbook(oldHandbook); err != nil {
		t.Fatal(err)
	}
	up, err := s.UpgradeHandbook(shapedDefault, upgradeTime)
	if err != nil {
		t.Fatal(err)
	}
	if !up.Replaced || !up.ToldChiefOfStaff || up.AttachmentSHA == "" {
		t.Fatalf("upgrade = %+v", up)
	}
	if got, _ := s.ReadHandbook(); got != shapedDefault {
		t.Errorf("handbook after upgrade = %q", got)
	}
	if got, err := s.ReadAttachmentText(up.AttachmentSHA); err != nil || got != oldHandbook {
		t.Errorf("kept text = %q, %v", got, err)
	}
	hist, err := s.ReadChatHistory(graph.ChiefOfStaffSlug)
	if err != nil || len(hist) != 1 {
		t.Fatalf("chat = %d rows, %v", len(hist), err)
	}
	note := hist[0]
	if note.Role != RoleReceived || note.Kind != KindWakeUpdate || !note.Quiet || !note.TS.Equal(upgradeTime) {
		t.Errorf("note = %+v", note)
	}
	if len(note.Attachments) != 1 || note.Attachments[0].SHA != up.AttachmentSHA || note.Attachments[0].Name != PreviousHandbookName {
		t.Errorf("note attachments = %+v", note.Attachments)
	}
	for _, want := range []string{PreviousHandbookName, "/files/attachments/", "propose_handbook_update", "About, Values, Communicating and Working together", "Kivali OS"} {
		if !strings.Contains(note.Content, want) {
			t.Errorf("note does not say %q", want)
		}
	}
	// The file is where the note says it is.
	names, err := s.AttachmentLinkNames(graph.ChiefOfStaffSlug)
	if err != nil || names[up.AttachmentSHA] != PreviousHandbookName {
		t.Errorf("link names = %v, %v", names, err)
	}
	link := filepath.Join(files.StorageRoot(filepath.Join(s.Root(), "agents", graph.ChiefOfStaffSlug)), "attachments", PreviousHandbookName)
	if b, err := os.ReadFile(link); err != nil || string(b) != oldHandbook {
		t.Errorf("/files/attachments/%s = %q, %v", PreviousHandbookName, b, err)
	}

	again, err := s.UpgradeHandbook(shapedDefault, upgradeTime.Add(time.Hour))
	if err != nil || again.Replaced || again.ToldChiefOfStaff {
		t.Errorf("second run = %+v, %v; want nothing done", again, err)
	}
	if hist, _ := s.ReadChatHistory(graph.ChiefOfStaffSlug); len(hist) != 1 {
		t.Errorf("second run left %d rows, want the one note", len(hist))
	}
}

// A handbook that has the shape is the team's own text and is not
// touched, and a deployment with none (setup not run yet) gets none:
// the upgrade replaces only what predates the shape.
func TestUpgradeHandbookLeavesAShapedOrMissingHandbookAlone(t *testing.T) {
	s := mustStore(t)
	up, err := s.UpgradeHandbook(shapedDefault, upgradeTime)
	if err != nil || up.Replaced {
		t.Errorf("with no handbook: %+v, %v", up, err)
	}
	if _, err := s.ReadHandbook(); err == nil {
		t.Error("the upgrade wrote a handbook where there was none")
	}
	own := strings.Replace(shapedDefault, "A.", "We make rope, carefully.", 1)
	if err := s.WriteHandbook(own); err != nil {
		t.Fatal(err)
	}
	up, err = s.UpgradeHandbook(shapedDefault, upgradeTime)
	if err != nil || up.Replaced {
		t.Errorf("with a shaped handbook: %+v, %v", up, err)
	}
	if got, _ := s.ReadHandbook(); got != own {
		t.Errorf("the team's handbook was changed to %q", got)
	}
}

// Without a Chief of Staff (a hire that failed between writing the
// handbook and the agent) the old text is still kept and the default
// still written; there is nobody to tell.
func TestUpgradeHandbookWithoutAChiefOfStaff(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteHandbook(oldHandbook); err != nil {
		t.Fatal(err)
	}
	up, err := s.UpgradeHandbook(shapedDefault, upgradeTime)
	if err != nil || !up.Replaced || up.ToldChiefOfStaff {
		t.Fatalf("upgrade = %+v, %v", up, err)
	}
	if got, err := s.ReadAttachmentText(up.AttachmentSHA); err != nil || got != oldHandbook {
		t.Errorf("kept text = %q, %v", got, err)
	}
	if got, _ := s.ReadHandbook(); got != shapedDefault {
		t.Errorf("handbook after upgrade = %q", got)
	}
}

// A replacement without the shape is refused before anything is
// touched: writing it would leave the team with a handbook every door
// then refuses to save.
func TestUpgradeHandbookRefusesAShapelessReplacement(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteHandbook(oldHandbook); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpgradeHandbook("# Handbook\n\n## About\n\nOnly.\n", upgradeTime); err == nil {
		t.Fatal("a replacement without the four sections was accepted")
	}
	if got, _ := s.ReadHandbook(); got != oldHandbook {
		t.Errorf("handbook = %q, want the old one untouched", got)
	}
}
