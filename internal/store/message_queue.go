package store

import (
	"encoding/json"
	"errors"
	"os"
)

// AgentQueue is the per-agent persisted state for the message
// queue: the inbox of paths awaiting delivery + a status flag
// (paused/ready/waiting) the operator can toggle.
type AgentQueue struct {
	Status string   `json:"status"` // "waiting" | "ready" | "paused"
	Inbox  []string `json:"inbox"`  // message paths (relative to root) pending delivery
}

// MessageQueue is the persisted routing ledger: every agent's
// inbox of pending message paths. Written atomically on each change.
//
// Inbox paths are relative to the store root (e.g.
// "messages/2026-04-18/20260418T120000.000000000Z-notice-a--to--b.md").
//
// CommittedStageIDs is a bounded, FIFO ring of staged-publish IDs
// that have already been committed (see internal/store/staged_publish.go).
// The two-phase publish flow's commit phase checks this set inside the
// queue lock and skips the inbox append when the id is present, so
// retried commits don't double-route. Cap is CommittedStageIDsCap;
// older ids are evicted from the front on overflow. Eviction is safe
// because retries happen within seconds and the cap is orders of
// magnitude larger than any realistic in-flight publish concurrency.
type MessageQueue struct {
	Agents            map[string]AgentQueue `json:"agents"`
	CommittedStageIDs []string              `json:"committed_stage_ids,omitempty"`
}

// CommittedStageIDsCap bounds CommittedStageIDs. 1024 covers far more
// concurrent publishes than the system ever generates in flight; old
// entries fall off the front once full.
const CommittedStageIDsCap = 1024

// HasCommittedStageID reports whether stageID was already committed.
// Caller MUST hold LockMessageQueue — the set is part of the queue's
// read-modify-write critical section.
func (q *MessageQueue) HasCommittedStageID(stageID string) bool {
	for _, id := range q.CommittedStageIDs {
		if id == stageID {
			return true
		}
	}
	return false
}

// RecordCommittedStageID appends stageID to CommittedStageIDs,
// evicting the oldest entry when the cap is reached. No-op if the id
// is already present (defense in depth — callers should HasCommittedStageID
// first so they can skip the inbox mutation too). Caller MUST hold
// LockMessageQueue.
func (q *MessageQueue) RecordCommittedStageID(stageID string) {
	if q.HasCommittedStageID(stageID) {
		return
	}
	if len(q.CommittedStageIDs) >= CommittedStageIDsCap {
		// Drop oldest. Allocate a fresh slice so we don't pin the
		// backing array's earliest entries indefinitely.
		drop := len(q.CommittedStageIDs) - CommittedStageIDsCap + 1
		trimmed := make([]string, 0, CommittedStageIDsCap)
		trimmed = append(trimmed, q.CommittedStageIDs[drop:]...)
		q.CommittedStageIDs = trimmed
	}
	q.CommittedStageIDs = append(q.CommittedStageIDs, stageID)
}

const messageQueueFilename = "message_queue.json"

// ReadMessageQueue returns the current message queue. If the file
// doesn't exist, returns a zero-value queue with a non-nil Agents map.
func (s *FSStore) ReadMessageQueue() (MessageQueue, error) {
	b, err := os.ReadFile(s.path(messageQueueFilename))
	if errors.Is(err, os.ErrNotExist) {
		return MessageQueue{Agents: map[string]AgentQueue{}}, nil
	}
	if err != nil {
		return MessageQueue{}, err
	}
	var q MessageQueue
	if err := json.Unmarshal(b, &q); err != nil {
		return MessageQueue{}, err
	}
	if q.Agents == nil {
		q.Agents = map[string]AgentQueue{}
	}
	return q, nil
}

// WriteMessageQueue persists the queue atomically and fires
// onMessageQueueWrite (if set) so any in-process subscriber (today:
// the web layer's org-state SSE) learns about the change without each
// caller having to remember to notify. See FSStore.SetOnMessageQueueWrite.
func (s *FSStore) WriteMessageQueue(q MessageQueue) error {
	b, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path(messageQueueFilename), b, 0o644); err != nil {
		return err
	}
	if s.onMessageQueueWrite != nil {
		s.onMessageQueueWrite()
	}
	return nil
}
