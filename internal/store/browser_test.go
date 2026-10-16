package store

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserSessionsRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	if list, err := s.ListBrowserSessions("alice"); err != nil || len(list) != 0 {
		t.Fatalf("fresh agent: %v %v", list, err)
	}
	bs, err := s.EnsureBrowserSession("alice", "open", "", t0)
	if err != nil || bs.Name != "open" || bs.Site != "" || !bs.Created.Equal(t0) {
		t.Fatalf("EnsureBrowserSession = %+v, %v", bs, err)
	}
	// Ensuring again keeps the record.
	later := t0.Add(time.Hour)
	if again, err := s.EnsureBrowserSession("alice", "open", "", later); err != nil || !again.Created.Equal(t0) {
		t.Errorf("second Ensure = %+v, %v; want the first record", again, err)
	}
	if _, err := s.EnsureBrowserSession("alice", "vendor.com", "vendor.com", later); err != nil {
		t.Fatal(err)
	}

	// No profile yet.
	if r, n, err := s.OpenBrowserProfile("alice", "open"); r != nil || n != 0 || err != nil {
		t.Errorf("OpenBrowserProfile before a save = %v %d %v", r, n, err)
	}
	payload := bytes.Repeat([]byte("profile"), 1000)
	n, err := s.WriteBrowserProfile("alice", "open", bytes.NewReader(payload), later)
	if err != nil || n != int64(len(payload)) {
		t.Fatalf("WriteBrowserProfile = %d, %v", n, err)
	}
	r, size, err := s.OpenBrowserProfile("alice", "open")
	if err != nil || size != int64(len(payload)) {
		t.Fatalf("OpenBrowserProfile = %d, %v", size, err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, payload) {
		t.Error("profile bytes differ")
	}
	bs, ok, err := s.ReadBrowserSession("alice", "open")
	if err != nil || !ok || bs.ProfileBytes != int64(len(payload)) || !bs.LastUsed.Equal(later) {
		t.Errorf("record after save = %+v %v %v", bs, ok, err)
	}
	// The profile lives outside the agent's memory tree.
	if _, err := os.Stat(filepath.Join(s.Root(), "agents", "alice", "browser", "open", "profile.tar.zst")); err != nil {
		t.Errorf("profile not at the expected path: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(s.Root(), "agents", "alice", "browser", "open")); len(entries) != 2 {
		t.Errorf("session dir holds %d entries, want session.json and the profile", len(entries))
	}

	list, err := s.ListBrowserSessions("alice")
	if err != nil || len(list) != 2 || list[0].Name != "open" || list[1].Name != "vendor.com" || list[1].Site != "vendor.com" {
		t.Errorf("ListBrowserSessions = %+v, %v", list, err)
	}
	if err := s.TouchBrowserSession("alice", "vendor.com", later.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if bs, _, _ := s.ReadBrowserSession("alice", "vendor.com"); !bs.LastUsed.Equal(later.Add(time.Minute)) {
		t.Errorf("Touch did not move LastUsed: %+v", bs)
	}

	if err := s.DeleteBrowserSession("alice", "open"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.ReadBrowserSession("alice", "open"); ok {
		t.Error("deleted session still reads")
	}
	if err := s.DeleteBrowserSession("alice", "open"); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
	if err := s.DeleteBrowserSessions("alice"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListBrowserSessions("alice"); len(list) != 0 {
		t.Errorf("sessions survive DeleteBrowserSessions: %+v", list)
	}
}

func TestBrowserSessionNamesArePlainLabels(t *testing.T) {
	for _, ok := range []string{"open", "vendor.com", "shop.example.co.uk", "a1-b2"} {
		if err := ValidBrowserSessionName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "Vendor.com", "-x", ".hidden", "a/b", "a b", "a_b", strings.Repeat("a", 254)} {
		if err := ValidBrowserSessionName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	s, _ := New(t.TempDir())
	if _, err := s.EnsureBrowserSession("alice", "../x", "", time.Now()); err == nil {
		t.Error("a traversal name created a session")
	}
	if _, _, err := s.OpenBrowserProfile("alice", "../x"); err == nil {
		t.Error("a traversal name opened a profile")
	}
}
