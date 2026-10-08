package store

import (
	"testing"
	"time"
)

// TestUsageReadSinceBackscan exercises the backward chunked reader against
// a brute-force reference, with a tiny chunk size so many records straddle
// chunk boundaries (the line-stitching path). It checks ordering, the
// `since` window, and that long lines spanning multiple chunks reassemble.
func TestUsageReadSinceBackscan(t *testing.T) {
	orig := usageReadChunkSize
	usageReadChunkSize = 48 // smaller than a single record line → forces stitching
	defer func() { usageReadChunkSize = orig }()

	s := mustStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 500
	for i := 0; i < total; i++ {
		// Purpose varies in length so line sizes differ and boundaries
		// land mid-record at assorted offsets.
		purpose := "chat"
		if i%3 == 0 {
			purpose = "release-with-a-longer-purpose-string"
		}
		if err := s.AppendUsage(UsageRecord{
			TS:          base.Add(time.Duration(i) * time.Minute),
			Agent:       "cos",
			Purpose:     purpose,
			Model:       "claude-opus-4-8",
			InputTokens: i,
			CostUSD:     float64(i),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	check := func(name string, since time.Time, wantFrom int) {
		t.Helper()
		got, err := s.ReadUsageSince(since)
		if err != nil {
			t.Fatalf("%s: read: %v", name, err)
		}
		wantN := total - wantFrom
		if len(got) != wantN {
			t.Fatalf("%s: got %d records, want %d", name, len(got), wantN)
		}
		for i, r := range got {
			wantIdx := wantFrom + i
			if r.InputTokens != wantIdx {
				t.Fatalf("%s: record %d has InputTokens=%d, want %d (out of order or corrupt)", name, i, r.InputTokens, wantIdx)
			}
			if i > 0 && r.TS.Before(got[i-1].TS) {
				t.Fatalf("%s: record %d not in ascending TS order", name, i)
			}
		}
	}

	check("all", time.Time{}, 0)
	check("from-100", base.Add(100*time.Minute), 100)
	check("from-499", base.Add(499*time.Minute), 499)
	check("after-end", base.Add(time.Duration(total)*time.Minute), total)
}
