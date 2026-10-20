package web

import (
	"testing"

	"github.com/kivali-ai/kivali/internal/store"
)

// TestHandbookUpdateProposalNormalizesCRLF: when the current
// handbook on disk has CRLF line endings (a hand-edit from a
// Windows browser) and the proposed body has LF (the typical model
// output), the proposal's before and after must both come out LF, so
// the web app's diff treats unchanged lines as unchanged. Before the
// fix every line differed by a trailing \r and the whole file read as
// replaced. The diff itself is drawn client-side.
func TestHandbookUpdateProposalNormalizesCRLF(t *testing.T) {
	srv, _ := newTurnServer(t)
	proposalTeam(t, srv)
	if err := srv.Store.WriteHandbook("# C\r\n\r\nLine A.\r\nLine B.\r\nLine C.\r\n"); err != nil {
		t.Fatal(err)
	}
	path := seedProposal(t, srv, store.Message{
		Title: "Handbook: tweak Line B", Body: "small tweak",
		HandbookUpdate: &store.HandbookUpdate{Body: "# C\n\nLine A.\nLine B (revised).\nLine C.\n"},
	})

	p := getProposal(t, srv, path)
	if len(p.Docs) != 1 {
		t.Fatalf("docs = %+v", p.Docs)
	}
	d := p.Docs[0]
	if d.Before == nil || *d.Before != "# C\n\nLine A.\nLine B.\nLine C.\n" {
		t.Errorf("before = %q, want the current text with LF endings", derefString(d.Before))
	}
	if d.After != "# C\n\nLine A.\nLine B (revised).\nLine C.\n" {
		t.Errorf("after = %q", d.After)
	}
}

func derefString(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
