package store

import (
	"testing"
	"time"
)

func TestUsageAppendAndRead(t *testing.T) {
	s := mustStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	records := []UsageRecord{
		{TS: now.Add(-time.Hour), Model: "claude-opus-4-7", InputTokens: 1000, OutputTokens: 200, CostUSD: 0.01},
		{TS: now.Add(-30 * time.Minute), Model: "claude-haiku-4-5", InputTokens: 500, OutputTokens: 50, CostUSD: 0.001},
		{TS: now, Model: "claude-opus-4-7", InputTokens: 2000, OutputTokens: 400, CostUSD: 0.02},
	}
	for _, r := range records {
		if err := s.AppendUsage(r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	all, err := s.ReadUsageSince(time.Time{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all = %d", len(all))
	}
	recent, err := s.ReadUsageSince(now.Add(-45 * time.Minute))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("recent = %d", len(recent))
	}
}

func TestUsageReadEmptyFile(t *testing.T) {
	s := mustStore(t)
	recs, err := s.ReadUsageSince(time.Time{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("len = %d", len(recs))
	}
}

func TestUsageDefaultTS(t *testing.T) {
	s := mustStore(t)
	before := time.Now().UTC()
	if err := s.AppendUsage(UsageRecord{Model: "x"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	after := time.Now().UTC()
	recs, _ := s.ReadUsageSince(time.Time{})
	if len(recs) != 1 {
		t.Fatalf("len = %d", len(recs))
	}
	if recs[0].TS.Before(before) || recs[0].TS.After(after) {
		t.Errorf("TS = %v, not in [%v, %v]", recs[0].TS, before, after)
	}
}
