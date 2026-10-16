package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/browserpod"
	"github.com/kivali-ai/kivali/internal/files"
)

// TestBrowserRegistryTakesOnlyWhatCoreOpened: what the pod sends is
// checked against what core recorded. A download must name a group
// core opened on this browser, fit the size the pod announced and the
// group's bounds; a profile is taken only while core asked for one.
func TestBrowserRegistryTakesOnlyWhatCoreOpened(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	b, _ := f.running(ctx, "job1")

	for _, group := range []string{"job2", "../../job1", "", "JOB1", "job1/../job1"} {
		f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeDownload, Group: group, Tab: "t1", FileName: "x.pdf", Bytes: 3}, strings.NewReader("abc"))
	}
	if _, err := os.Stat(filepath.Join(subagentFilesRoot(f.st, "alice", "job2"), files.UntrustedDownloadsDir)); err == nil {
		t.Error("a download for a group core never opened was written")
	}
	dir := filepath.Join(f.root, files.UntrustedDownloadsDir)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("job1 got files it did not download: %v", entries)
	}

	// Over the single-download limit: refused before a byte is read.
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeDownload, Group: "job1", Tab: "t1", FileName: "huge.bin", Bytes: browserpod.MaxDownloadBytes + 1}, strings.NewReader("x"))
	// Up to the count, and no further.
	for i := 0; i <= maxDownloadsPerGroup; i++ {
		f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeDownload, Group: "job1", Tab: "t1", FileName: fmt.Sprintf("f%d.txt", i), Bytes: 1}, strings.NewReader("x"))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != maxDownloadsPerGroup {
		t.Errorf("job1 has %d downloads, want the cap of %d", len(entries), maxDownloadsPerGroup)
	}
	if _, downloads := f.reg.ReleaseGroup("alice", "open", "job1"); len(downloads) != maxDownloadsPerGroup {
		t.Errorf("ReleaseGroup lists %d downloads, want %d", len(downloads), maxDownloadsPerGroup)
	}

	// A profile while nobody asked for one.
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeProfile, Bytes: 4}, strings.NewReader("evil"))
	if r, n, err := f.st.OpenBrowserProfile("alice", "open"); err != nil || n != 0 || r != nil {
		t.Errorf("a profile outside a save was stored: %d bytes, %v", n, err)
	}
}

// TestBrowserRegistryIdlesABrowserNobodyWaitsFor: the command that
// started a browser was cancelled before the pod came up. The browser
// is idle from the moment it runs and is reaped like any other.
func TestBrowserRegistryIdlesABrowserNobodyWaitsFor(t *testing.T) {
	f := newBrowserFixture(t)
	cctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := f.reg.Command(cctx, "alice", "open", "job1", browserpod.Message{Name: browserpod.CommandSnapshot})
		errs <- err
	}()
	var token string
	select {
	case token = <-f.prov.startedC:
	case <-time.After(10 * time.Second):
		t.Fatal("the provisioner was not asked to start a pod")
	}
	cancel()
	if err := <-errs; err == nil {
		t.Fatal("the cancelled command returned no error")
	}
	link := newFakeLink()
	b := f.reg.claim("alice", "open", token, link)
	if b == nil {
		t.Fatal("claim found no waiting browser")
	}
	if err := f.reg.attach(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	f.reg.onMessage(b, browserpod.Message{Type: browserpod.TypeState, State: browserpod.StateRunning}, nil)
	if states := f.reg.states(); states["alice/open"] != string(browserIdle) {
		t.Fatalf("state with no group = %v, want idle", states)
	}
	f.clk.Advance(browserIdleAfter)
	f.reg.sweep(f.clk.Now())
	link.next(t, browserpod.TypeSave)
	if states := f.reg.states(); states["alice/open"] != string(browserSaving) {
		t.Errorf("state after the idle limit = %v, want saving", states)
	}
}

// TestBrowserRegistryRefusesACommandForAnEndedTask: a command for a
// group that was released, because its task ended, is refused rather
// than reopening the group; a call still in flight at the end of a
// task cannot keep the browser busy.
func TestBrowserRegistryRefusesACommandForAnEndedTask(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	_, link := f.running(ctx, "job1")
	f.reg.ReleaseGroup("alice", "open", "job1")
	link.next(t, browserpod.TypeCloseGroup)
	if _, err := f.reg.Command(ctx, "alice", "open", "job1", browserpod.Message{Name: browserpod.CommandSnapshot}); err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Errorf("command after release: %v", err)
	}
	if states := f.reg.states(); states["alice/open"] != string(browserIdle) {
		t.Errorf("state = %v, want idle", states)
	}
}

// TestBrowserRegistryKeepsAStoppingBrowserUntilItsPodIsGone: the
// record of a browser whose pod is being deleted stays, and counts
// toward the cap, until the delete returns; a start under the same
// name waits rather than racing the delete of a pod with its name.
func TestBrowserRegistryKeepsAStoppingBrowserUntilItsPodIsGone(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	gate := make(chan struct{})
	f.prov.mu.Lock()
	f.prov.stopGate = gate
	f.prov.mu.Unlock()
	b, _ := f.running(ctx, "job1")
	f.reg.onLost(b)
	if stopped := f.prov.awaitStop(t); stopped != "alice/open" {
		t.Fatalf("stop = %q", stopped)
	}
	if states := f.reg.states(); states["alice/open"] != string(browserStopped) {
		t.Fatalf("state while the delete runs = %v, want stopped", states)
	}
	if _, err := f.reg.Command(ctx, "alice", "open", "job2", browserpod.Message{Name: browserpod.CommandSnapshot}); err == nil || !strings.Contains(err.Error(), "still stopping") {
		t.Errorf("start during the delete: %v", err)
	}
	f.reg.cap = 1
	if _, err := f.reg.Command(ctx, "alice", "vendor.com", "job2", browserpod.Message{Name: browserpod.CommandSnapshot}); err == nil || !strings.Contains(err.Error(), "all 1 browsers are in use") {
		t.Errorf("start at the cap during the delete: %v", err)
	}
	close(gate)
	f.reg.awaitStops()
	if states := f.reg.states(); len(states) != 0 {
		t.Errorf("record kept after the delete: %v", states)
	}
}

// TestBrowserRegistryEvictsTheOldestIdleBrowserAtTheCap: an idle
// browser does not hold a slot another agent needs for its whole idle
// time; a start at the cap begins its save at once.
func TestBrowserRegistryEvictsTheOldestIdleBrowserAtTheCap(t *testing.T) {
	f := newBrowserFixture(t)
	ctx := context.Background()
	f.reg.cap = 1
	_, link := f.running(ctx, "job1")
	f.reg.ReleaseGroup("alice", "open", "job1")
	link.next(t, browserpod.TypeCloseGroup)
	if _, err := f.reg.Command(ctx, "alice", "vendor.com", "job2", browserpod.Message{Name: browserpod.CommandSnapshot}); err == nil || !strings.Contains(err.Error(), "being saved") {
		t.Errorf("start at the cap with an idle browser: %v", err)
	}
	link.next(t, browserpod.TypeSave)
	if states := f.reg.states(); states["alice/open"] != string(browserSaving) {
		t.Errorf("state = %v, want saving", states)
	}
}

// TestBrowserRegistryOffboardEndsBrowsersWithoutSaving: an offboarded
// agent's browsers are deleted, not saved.
func TestBrowserRegistryOffboardEndsBrowsersWithoutSaving(t *testing.T) {
	f := newBrowserFixture(t)
	_, link := f.running(context.Background(), "job1")
	f.reg.Offboard("alice")
	if stopped := f.stopped(t); stopped != "alice/open" {
		t.Errorf("stop = %q", stopped)
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	if !link.closed {
		t.Error("link not closed")
	}
	for _, fr := range link.frames {
		if fr.msg.Type == browserpod.TypeSave {
			t.Error("offboard asked for a save")
		}
	}
	if r, n, err := f.st.OpenBrowserProfile("alice", "open"); err != nil || n != 0 || r != nil {
		t.Errorf("a profile was stored: %d bytes, %v", n, err)
	}
}
