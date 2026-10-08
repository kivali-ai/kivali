package web

import (
	"errors"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
)

// TestStartStagedPublishJanitorNilStoreNoop guards the edge case
// where StartStagedPublishJanitor is called before Store wiring —
// it must return a no-op stop without crashing.
func TestStartStagedPublishJanitorNilStoreNoop(t *testing.T) {
	srv := &Server{}
	stop := srv.StartStagedPublishJanitor()
	if stop == nil {
		t.Fatalf("stop func is nil")
	}
	stop() // must not panic / hang.
}

// TestStartStagedPublishJanitorStopReturnsCleanly: the returned stop
// func must shut down the goroutine deterministically. Two-second
// timeout via t.Cleanup catches any hang.
func TestStartStagedPublishJanitorStopReturnsCleanly(t *testing.T) {
	srv := newTestServer(t)
	stop := srv.StartStagedPublishJanitor()
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("stop did not return within 2s — janitor goroutine leaked")
	}
}

// TestStagedPublishSweepRemovesOld is the integration check that the
// FSStore sweep actually drops on-disk records older than maxAge.
// The janitor goroutine isn't exercised directly (it ticks every
// 5 minutes; we'd need a clock injection to drive it in unit-test
// time). The sweep function is what matters; this test confirms the
// behavior end-to-end against a real Store.
func TestStagedPublishSweepRemovesOld(t *testing.T) {
	srv := newTestServer(t)
	now := time.Date(2026, 5, 11, 23, 16, 0, 0, time.UTC)
	old := store.StagedPublish{StageID: "expired", Tool: "publish_notice", From: "alice", CreatedAt: now.Add(-2 * stagedPublishMaxAge)}
	fresh := store.StagedPublish{StageID: "fresh", Tool: "publish_notice", From: "alice", CreatedAt: now.Add(-1 * time.Minute)}
	if err := srv.Store.WriteStagedPublish(old); err != nil {
		t.Fatalf("write old: %v", err)
	}
	if err := srv.Store.WriteStagedPublish(fresh); err != nil {
		t.Fatalf("write fresh: %v", err)
	}
	removed, err := srv.Store.SweepStagedPublishes(now, stagedPublishMaxAge)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only the expired record)", removed)
	}
	if _, err := srv.Store.ReadStagedPublish("expired"); !errors.Is(err, store.ErrStagedPublishNotFound) {
		t.Errorf("expired should be gone: err=%v", err)
	}
	if _, err := srv.Store.ReadStagedPublish("fresh"); err != nil {
		t.Errorf("fresh should still be there: err=%v", err)
	}
}
