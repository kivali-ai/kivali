package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// Scenario names.
const (
	ScenarioDemo  = "demo"
	ScenarioEmpty = "empty"
	ScenarioSetup = "setup"
)

// Seed builds a DATA_DIR at dir for scenario, with every time relative
// to now. dir must be missing or empty. Everything but the times (and
// the few store-stamped fields noted in the report: project-file
// upload times, graph version times) is fixed, so two runs with the
// same now write the same assignment and message files byte for byte.
func Seed(dir, scenario string, now time.Time) error {
	switch scenario {
	case ScenarioDemo, ScenarioEmpty, ScenarioSetup:
	default:
		return fmt.Errorf("unknown scenario %q (want demo, empty or setup)", scenario)
	}
	empty, err := dirEmpty(dir)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("%s is not empty; pass -force to wipe it first", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if scenario == ScenarioSetup {
		return nil
	}
	st, err := store.New(dir)
	if err != nil {
		return err
	}
	s := &seeder{st: st, now: now.UTC()}
	if scenario == ScenarioEmpty {
		return s.empty()
	}
	return s.demo()
}

// dirEmpty reports whether dir is missing or has no entries.
func dirEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// wipeDir removes everything inside dir, leaving dir itself.
func wipeDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// seeder writes one scenario through the store.
type seeder struct {
	st  *store.FSStore
	now time.Time
}

// ago is now minus d.
func (s *seeder) ago(d time.Duration) time.Time { return s.now.Add(-d) }

// rel is a store path relative to the root, slash-separated, as the
// queue and chat rows name messages.
func (s *seeder) rel(abs string) (string, error) {
	r, err := filepath.Rel(s.st.Root(), abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(r), nil
}

// agentSpec is one agent with everything the agent page shows.
type agentSpec struct {
	store.Agent
	RoleMD string
	Memory string
	Habits string
}

func (s *seeder) createAgent(a agentSpec) error {
	if err := s.st.CreateAgent(a.Agent, a.RoleMD); err != nil {
		return fmt.Errorf("create %s: %w", a.Slug, err)
	}
	if a.Memory != "" {
		if err := s.st.WriteAgentMemory(a.Slug, a.Memory); err != nil {
			return fmt.Errorf("memory %s: %w", a.Slug, err)
		}
	}
	if a.Habits != "" {
		if err := s.st.WriteAgentHabits(a.Slug, a.Habits); err != nil {
			return fmt.Errorf("habits %s: %w", a.Slug, err)
		}
	}
	return nil
}

// chat appends rows to slug's chat. Every row must carry its time;
// AppendChatMessage would otherwise stamp the wall clock.
func (s *seeder) chat(slug string, rows ...store.ChatMessage) error {
	for _, m := range rows {
		if m.TS.IsZero() {
			return fmt.Errorf("chat row for %s has no time: %q", slug, m.Content)
		}
		if err := s.st.AppendChatMessage(slug, m); err != nil {
			return fmt.Errorf("chat %s: %w", slug, err)
		}
	}
	return nil
}

// writeMessage writes m and returns its relative path.
func (s *seeder) writeMessage(m store.Message) (string, error) {
	if m.Date.IsZero() {
		return "", fmt.Errorf("message %q has no date", m.Title)
	}
	abs, err := s.st.WriteMessage(m)
	if err != nil {
		return "", err
	}
	return s.rel(abs)
}

// toCEO files a CEO-bound message (approval request, notification)
// and lands it in the CEO's inbox the way messaging.DeliverToCEONow
// does, with the row stamped at the message's date.
func (s *seeder) toCEO(m store.Message) (string, error) {
	rel, err := s.writeMessage(m)
	if err != nil {
		return "", err
	}
	return rel, s.chat(agent.CEOSlug, store.ChatMessage{
		Role: store.RoleReceived, Content: m.Title, Kind: "ceo_inbox", MessageRef: rel,
		Attachments: m.Attachments, TS: m.Date,
	})
}

// deliver puts a message into an agent's chat as a released delivery,
// the row messaging.DeliverToAgent appends.
func (s *seeder) deliver(to string, m store.Message, rel string, at time.Time) error {
	return s.chat(to, store.ChatMessage{
		Role: store.RoleReceived, Content: agent.RenderInboxBody(m, rel), Kind: "inbox_delivery",
		MessageRef: rel, Attachments: m.Attachments, Quiet: m.Quiet, TS: at,
	})
}

// enqueue adds rel to each recipient's release inbox.
func (s *seeder) enqueue(rel string, to ...string) error {
	s.st.LockMessageQueue()
	defer s.st.UnlockMessageQueue()
	q, err := s.st.ReadMessageQueue()
	if err != nil {
		return err
	}
	for _, slug := range to {
		aq := q.Agents[slug]
		aq.Inbox = append(aq.Inbox, rel)
		q.Agents[slug] = aq
	}
	return s.st.WriteMessageQueue(q)
}

// route says where a change's wakes go.
type route int

const (
	// released: wakes to agents land in their chats, as if the CEO
	// released them (or, for a change the CEO made, as delivered at
	// once, which is what the tracker does).
	released route = iota
	// queued: wakes to agents wait in the release queue.
	queued
)

// assignment applies one tracker change at `at`, exactly as tracker.apply
// does (rules, prior-text snapshot, the file, the touched parent), and
// routes its wakes as assignment events (stored type assignment_event) the way tracker.deliver
// writes them. The tracker itself is not used because its routing
// stamps the CEO-inbox and chat rows with the wall clock; this keeps
// every row on the seed's timeline. No pending-wakes record is written:
// every wake lands before this returns.
func (s *seeder) assignment(at time.Time, by string, r route, fn func(assignments.Rules) (assignments.Change, error)) (assignments.Change, error) {
	s.st.LockAssignments()
	set, err := s.st.ReadAssignmentSet()
	if err != nil {
		s.st.UnlockAssignments()
		return assignments.Change{}, err
	}
	active := func(slug string) bool {
		_, err := s.st.GetAgent(slug)
		return err == nil
	}
	ch, err := fn(assignments.Rules{Set: set, Active: active, Now: at})
	if err == nil && ch.PriorText != "" {
		_, err = s.st.SnapshotAssignmentText(ch.Assignment.ID, ch.PriorText)
	}
	if err == nil {
		err = s.st.WriteAssignment(ch.Assignment)
	}
	for _, other := range ch.Touched {
		if err == nil {
			err = s.st.WriteAssignment(other)
		}
	}
	s.st.UnlockAssignments()
	if err != nil {
		return assignments.Change{}, fmt.Errorf("assignment change by %s: %w", by, err)
	}
	for _, w := range ch.Wakes {
		if w.To != assignments.CEO && !active(w.To) {
			continue
		}
		m := store.Message{
			Type:       store.MsgAssignmentEvent,
			Title:      w.Title,
			From:       by,
			To:         store.Recipients{w.To},
			Date:       at,
			Assignment: &store.AssignmentRef{ID: ch.Assignment.ID, Seq: ch.Entries[0].Seq, Op: string(w.Op)},
			Body:       w.Body,
			Quiet:      w.Quiet,
		}
		switch {
		case w.To == assignments.CEO:
			if _, err := s.toCEO(m); err != nil {
				return ch, err
			}
		case r == queued && by != assignments.CEO:
			rel, err := s.writeMessage(m)
			if err != nil {
				return ch, err
			}
			if err := s.enqueue(rel, w.To); err != nil {
				return ch, err
			}
		default:
			rel, err := s.writeMessage(m)
			if err != nil {
				return ch, err
			}
			if err := s.deliver(w.To, m, rel, at); err != nil {
				return ch, err
			}
		}
	}
	return ch, nil
}

// create opens an assignment.
func (s *seeder) create(at time.Time, by string, r route, in assignments.CreateInput) (int, error) {
	ch, err := s.assignment(at, by, r, func(rl assignments.Rules) (assignments.Change, error) { return rl.Create(by, in) })
	if err != nil {
		return 0, fmt.Errorf("create %q: %w", in.Title, err)
	}
	return ch.Assignment.ID, nil
}

// subagentMeta mirrors the meta.json the web layer's SubagentService
// writes beside a background task's transcript.
type subagentMeta struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	Model       string `json:"model"`
	Effort      string `json:"effort"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	UpdatedAt   string `json:"updated_at"`
}

// writeSubagentMeta is a RAW FILE WRITE: agents/<parent>/subagents/<id>/meta.json.
// The store has no API for it; internal/web's SubagentService writes it
// directly, and devseed may not import internal/web. The chat and the
// transcript read it for each task's model, effort and state.
func (s *seeder) writeSubagentMeta(parent string, m subagentMeta) error {
	dir := filepath.Join(s.st.Root(), "agents", parent, "subagents", m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "meta.json"), append(b, '\n'), 0o644)
}

// publicDir is an agent's published tree, public/<slug>, the tree the
// knowledge-graph pass walks and the pod mounts at
// /files/artifacts/public/.
func (s *seeder) publicDir(slug string) string {
	return files.PublishedDir(s.st.Root(), slug)
}

// writePublic is a RAW FILE WRITE into an agent's published tree.
// Agents publish through core (artifact_publish), which copies from
// their workspace; the seed has no workspace file to copy, so it
// writes the published copy directly, as core would.
func (s *seeder) writePublic(slug, name, body string) error {
	dir := s.publicDir(slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
}

// archiveTS is the name of an archived chat generation at t.
func archiveTS(t time.Time) string { return t.UTC().Format("20060102T150405.000000000Z") }

// md trims a leading newline from a raw string literal so bodies can
// start on their own line in source.
func md(s string) string { return strings.TrimPrefix(s, "\n") }
