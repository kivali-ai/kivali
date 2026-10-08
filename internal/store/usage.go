package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// UsageRecord is one Claude API call's cost/token accounting, persisted
// to usage.jsonl (append-only) so it survives process restart and can be
// aggregated externally.
type UsageRecord struct {
	TS                time.Time `json:"ts"`
	Agent             string    `json:"agent,omitempty"`
	Purpose           string    `json:"purpose,omitempty"` // "release" | "chat" | "summary" | "seed" | ...
	Model             string    `json:"model"`
	InputTokens       int       `json:"input_tokens"`
	OutputTokens      int       `json:"output_tokens"`
	CacheReadTokens   int       `json:"cache_read_tokens,omitempty"`
	CacheCreateTokens int       `json:"cache_create_tokens,omitempty"`

	// CostUSD is NOT this row's cost, and the column is NOT summable.
	// usage.jsonl is an operator-facing artifact that people grep and
	// script over, so read this before writing `awk '{s+=$cost}'`.
	//
	// Three different quantities land in this one field, depending on
	// which path wrote the row — the reader cannot tell them apart from
	// the row alone:
	//
	//  1. Parent chat/release turns. The Claude Code CLI's
	//     total_cost_usd, which is a SESSION-CUMULATIVE GAUGE: it counts
	//     everything the warm runner has spent since it spawned, and we
	//     snapshot it per turn. It climbs monotonically across a
	//     session's rows and drops back on runner respawn. Summing these
	//     multiplies real spend by roughly the number of turns.
	//  2. Subagent runs (purpose "subagent"). Also total_cost_usd, but
	//     the pod drives subagents as one-shot `claude -p` processes, so
	//     for these rows it IS that run's own total — not cumulative.
	//  3. Mixed-model turns, rows after the first, and any row where the
	//     CLI reported no cost: locally derived from this row's own
	//     token counts at the provider's rates (Price). An absolute
	//     per-row cost, like (2).
	//
	// So a delta between consecutive rows is only meaningful within a
	// run of case-(1) rows from one unbroken runner session, and rows of
	// the other two kinds are interleaved with them.
	//
	// The token columns above have none of this ambiguity: they are
	// per-call throughout, captured off each assistant message. Derive
	// spend from those (Provider.Price) rather than from
	// this field — which is what the settings dashboard does, and why
	// it does it. This field stays as a diagnostic.
	//
	// The JSON key is "cost_usd" despite the trap: no single name is
	// honest for all three cases above.
	CostUSD float64 `json:"cost_usd"`
}

// AppendUsage writes a usage record to usage.jsonl with O_APPEND + fsync.
func (s *FSStore) AppendUsage(r UsageRecord) error {
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := appendJSONL(s.path("usage.jsonl"), append(line, '\n')); err != nil {
		return err
	}
	if s.onUsageAppend != nil {
		s.onUsageAppend()
	}
	return nil
}

// usageReadChunkSize is the block size for ReadUsageSince's backward scan.
// A package var (not const) so tests can shrink it to exercise multi-chunk
// line stitching without writing megabytes of fixture.
var usageReadChunkSize = 64 * 1024

// ReadUsageSince returns all records with TS >= since, in ascending
// timestamp order. Pass time.Time{} to get the full log.
//
// usage.jsonl is append-only and records land in timestamp order, so the
// requested window is always a contiguous tail of the file. We read
// backwards from EOF in chunks and stop as soon as we cross `since`,
// making a windowed read O(window) instead of O(all-time). The file grows
// without bound, but callers (e.g. the settings dashboard) only ever want
// the last few days, so read cost stays flat as history accumulates.
//
// The tail-is-the-window property relies on timestamp monotonicity. Two
// concurrent appends can land sub-second out of order, so a record sitting
// exactly on the `since` boundary might be included or dropped — immaterial
// for usage accounting, and the only window where it matters is one tick
// wide.
func (s *FSStore) ReadUsageSince(since time.Time) ([]UsageRecord, error) {
	f, err := os.Open(s.path("usage.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// rev collects matching records newest-first as we walk backward;
	// carry holds the partial line whose start sits in a not-yet-read
	// (lower) chunk, to be stitched onto the front of the next chunk.
	// A cut append's fragment after the last newline is skipped (see
	// jsonl.go); a whole row that lost only its newline still counts.
	keep, tail, err := jsonlTail(f, fi.Size())
	if err != nil {
		return nil, err
	}
	var (
		rev    []UsageRecord
		carry  []byte
		offset = keep
		buf    = make([]byte, usageReadChunkSize)
	)
	if t := bytes.TrimSpace(tail); len(t) > 0 && json.Valid(t) {
		var r UsageRecord
		if err := json.Unmarshal(t, &r); err == nil && !r.TS.Before(since) {
			rev = append(rev, r)
		}
	}
	for offset > 0 {
		n := int64(len(buf))
		if offset < n {
			n = offset
		}
		offset -= n
		if _, err := f.ReadAt(buf[:n], offset); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		// File-order bytes for this region are the chunk we just read
		// followed by the carry fragment from the chunk above it.
		region := make([]byte, 0, int(n)+len(carry))
		region = append(region, buf[:n]...)
		region = append(region, carry...)

		// Everything up to the first newline may continue into the next
		// (lower) chunk; hold it as carry unless this is the file start,
		// where it's already a complete line.
		parts := bytes.Split(region, []byte{'\n'})
		var lines [][]byte
		if offset > 0 {
			carry = append(carry[:0], parts[0]...)
			lines = parts[1:]
		} else {
			carry = nil
			lines = parts
		}

		// Walk complete lines newest-first; stop at the first record
		// older than the window, since all further-back records are older.
		stop := false
		for i := len(lines) - 1; i >= 0; i-- {
			line := lines[i]
			if len(line) == 0 {
				continue
			}
			var r UsageRecord
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, fmt.Errorf("usage.jsonl: %w", err)
			}
			if r.TS.Before(since) {
				stop = true
				break
			}
			rev = append(rev, r)
		}
		if stop {
			break
		}
	}

	// Flip newest-first into ascending order to match append order.
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}
