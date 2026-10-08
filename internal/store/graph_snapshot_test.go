package store

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/graph"
)

// Every version the pass records is readable afterwards by the SHA it
// recorded, whatever the file has since become: the pin a certificate
// carries names bytes, and those bytes are what the reader gets.
func TestReadGraphSnapshotServesEveryRecordedVersion(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "vp", "rule.md", "---\nid: rule\nkind: requirement\nabout: cs\nstatus: provisional\ncondition: until v4 ships\n---\nRule text, first cut.\n")
	refresh(t, s)
	writePublic(t, s, "vp", "rule.md", "---\nid: rule\nkind: requirement\nabout: cs\nstatus: current\n---\nRule text, tightened.\n")
	ix := refresh(t, s)
	n, ok := ix.Get("vp/rule")
	if !ok || len(n.Versions) != 2 {
		t.Fatalf("vp/rule = %+v, %v; want two versions", n, ok)
	}
	first, err := s.ReadGraphSnapshot(n, n.Versions[0])
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	if !strings.Contains(first.Text, "status: provisional") || !strings.HasSuffix(first.Text, "Rule text, first cut.\n") || first.Binary {
		t.Errorf("v1 = %+v; want the first cut, front matter and body", first)
	}
	if first.Size != int64(len(first.Text)) {
		t.Errorf("v1 size %d; text is %d bytes", first.Size, len(first.Text))
	}
	second, err := s.ReadGraphSnapshot(n, n.Versions[1])
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if !strings.HasSuffix(second.Text, "Rule text, tightened.\n") || strings.Contains(second.Text, "first cut") {
		t.Errorf("v2 = %+v; want the tightened text only", second)
	}

	// A snapshot the store no longer holds (lost between a backup and
	// a restore) is reported as gone, not as an empty file.
	if err := os.RemoveAll(s.path("attachments", n.Versions[0].SHA)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadGraphSnapshot(n, n.Versions[0]); !errors.Is(err, ErrGraphSnapshotGone) {
		t.Errorf("v1 after losing its snapshot: err = %v; want ErrGraphSnapshotGone", err)
	}
}

// A bare binary in a public directory is versioned like anything
// else; its snapshot is reported as binary with its size, never
// rendered as text.
func TestReadGraphSnapshotReportsBinaryVersions(t *testing.T) {
	s := graphStore(t)
	writePublic(t, s, "cs", "logo.png", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	ix := refresh(t, s)
	n, ok := ix.Get("cs/logo.png")
	if !ok || len(n.Versions) != 1 {
		t.Fatalf("cs/logo.png = %+v, %v; want one version", n, ok)
	}
	snap, err := s.ReadGraphSnapshot(n, n.Versions[0])
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Binary || snap.Text != "" || snap.Size != 16 {
		t.Errorf("binary snapshot = %+v; want Binary with size 16 and no text", snap)
	}
}

// Project files version by upload; each version's bytes are the
// upload's, read as file_view would read it. Deleting an upload keeps
// the version on record (pins stay pins) and loses only its bytes.
func TestReadGraphSnapshotServesProjectFileVersions(t *testing.T) {
	s := graphStore(t)
	v1, err := s.AddProjectFile("Plan.md", strings.NewReader("the plan\n"))
	if err != nil {
		t.Fatal(err)
	}
	refresh(t, s)
	if _, err := s.AddProjectFile("Plan.md", strings.NewReader("the plan, revised\n")); err != nil {
		t.Fatal(err)
	}
	ix := refresh(t, s)
	n, ok := ix.Get("ceo/plan")
	if !ok || len(n.Versions) != 2 || n.Owner != graph.CEOSlug {
		t.Fatalf("ceo/plan = %+v, %v; want two versions owned by the CEO", n, ok)
	}
	first, err := s.ReadGraphSnapshot(n, n.Versions[0])
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	if first.Text != "the plan\n" || first.Binary {
		t.Errorf("v1 = %+v; want the first upload's text", first)
	}
	second, err := s.ReadGraphSnapshot(n, n.Versions[1])
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if second.Text != "the plan, revised\n" {
		t.Errorf("v2 = %+v; want the second upload's text", second)
	}
	if err := s.RemoveProjectFile(v1.SHA); err != nil {
		t.Fatal(err)
	}
	ix = refresh(t, s)
	n, _ = ix.Get("ceo/plan")
	if len(n.Versions) != 2 {
		t.Fatalf("deleting the old upload renumbered: %+v", n.Versions)
	}
	if _, err := s.ReadGraphSnapshot(n, n.Versions[0]); !errors.Is(err, ErrGraphSnapshotGone) {
		t.Errorf("v1 after deleting the upload: err = %v; want ErrGraphSnapshotGone", err)
	}
}

func TestLooksLikeText(t *testing.T) {
	long := strings.Repeat("é", 5000) // 10 000 bytes; the 8 KiB sample cuts a rune
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"", true},
		{"plain ascii\n", true},
		{"front matter\n---\nüñí©ödé\n", true},
		{long, true},
		{"has a \x00 in it", false},
		{"\xff\xfe not utf-8", false},
	} {
		if got := looksLikeText([]byte(tc.in)); got != tc.want {
			t.Errorf("looksLikeText(%.20q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}
