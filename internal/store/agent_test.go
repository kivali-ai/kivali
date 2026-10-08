package store

import (
	"errors"
	"testing"
	"time"
)

func TestClaudeSessionIDRoundTrip(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "cos", Role: "Chief"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// Unset reads as empty string with no error.
	got, err := s.ReadClaudeSessionID("cos")
	if err != nil {
		t.Fatalf("ReadClaudeSessionID (unset): %v", err)
	}
	if got != "" {
		t.Errorf("unset read = %q, want empty", got)
	}
	if err := s.WriteClaudeSessionID("cos", "sess-abc-123"); err != nil {
		t.Fatalf("WriteClaudeSessionID: %v", err)
	}
	got, err = s.ReadClaudeSessionID("cos")
	if err != nil {
		t.Fatalf("ReadClaudeSessionID: %v", err)
	}
	if got != "sess-abc-123" {
		t.Errorf("got = %q, want sess-abc-123", got)
	}
	// Overwrite keeps latest.
	if err := s.WriteClaudeSessionID("cos", "sess-xyz-789"); err != nil {
		t.Fatalf("WriteClaudeSessionID (overwrite): %v", err)
	}
	got, _ = s.ReadClaudeSessionID("cos")
	if got != "sess-xyz-789" {
		t.Errorf("overwrite = %q, want sess-xyz-789", got)
	}
	// Clear removes it.
	if err := s.ClearClaudeSessionID("cos"); err != nil {
		t.Fatalf("ClearClaudeSessionID: %v", err)
	}
	got, err = s.ReadClaudeSessionID("cos")
	if err != nil {
		t.Fatalf("ReadClaudeSessionID (cleared): %v", err)
	}
	if got != "" {
		t.Errorf("cleared read = %q, want empty", got)
	}
	// Clear is idempotent when there's nothing to clear.
	if err := s.ClearClaudeSessionID("cos"); err != nil {
		t.Errorf("ClearClaudeSessionID (second): %v", err)
	}
	// Write to nonexistent agent returns ErrNotFound.
	if err := s.WriteClaudeSessionID("ghost", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("WriteClaudeSessionID(ghost) err = %v, want ErrNotFound", err)
	}
	// Empty id is a no-op (no file created).
	if err := s.WriteClaudeSessionID("cos", ""); err != nil {
		t.Errorf("WriteClaudeSessionID(empty): %v", err)
	}
	got, _ = s.ReadClaudeSessionID("cos")
	if got != "" {
		t.Errorf("after empty write: got %q, want empty", got)
	}
}

func TestCreateAndGetAgent(t *testing.T) {
	s := mustStore(t)
	a := Agent{Slug: "chief-of-staff", Role: "Chief of Staff", ReportsTo: "ceo"}
	if err := s.CreateAgent(a, "# Role\n"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	got, err := s.GetAgent("chief-of-staff")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.Role != "Chief of Staff" {
		t.Errorf("Role = %q", got.Role)
	}
	if got.ReportsTo != "ceo" {
		t.Errorf("ReportsTo = %q", got.ReportsTo)
	}
	if got.Status != StatusActive {
		t.Errorf("Status = %q", got.Status)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt unset")
	}
	kick, err := s.ReadRole("chief-of-staff")
	if err != nil {
		t.Fatalf("ReadRole: %v", err)
	}
	if kick != "# Role\n" {
		t.Errorf("role = %q", kick)
	}
}

func TestCreateAgentEmptySlug(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Role: "x"}, ""); err == nil {
		t.Fatal("expected error for empty slug")
	}
}

func TestCreateAgentDuplicate(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "r"}, ""); err != nil {
		t.Fatalf("first CreateAgent: %v", err)
	}
	if err := s.CreateAgent(Agent{Slug: "alice", Role: "r"}, ""); err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestGetAgentMissing(t *testing.T) {
	s := mustStore(t)
	if _, err := s.GetAgent("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListActiveAgents(t *testing.T) {
	s := mustStore(t)
	for _, slug := range []string{"bob", "alice", "carol"} {
		if err := s.CreateAgent(Agent{Slug: slug, Role: "r"}, ""); err != nil {
			t.Fatalf("CreateAgent %s: %v", slug, err)
		}
	}
	got, err := s.ListActiveAgents()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"alice", "bob", "carol"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, a := range got {
		if a.Slug != want[i] {
			t.Errorf("[%d] = %q, want %q", i, a.Slug, want[i])
		}
	}
}

func TestArchiveAgent(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "dave", Role: "r"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := s.ArchiveAgent("dave"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := s.GetAgent("dave"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetAgent after archive: %v, want ErrNotFound", err)
	}
	got, err := s.GetArchivedAgent("dave")
	if err != nil {
		t.Fatalf("GetArchivedAgent: %v", err)
	}
	if got.Status != StatusArchived {
		t.Errorf("Status = %q", got.Status)
	}
	if got.ArchivedAt == nil {
		t.Error("ArchivedAt unset")
	}
	archived, err := s.ListArchivedAgents()
	if err != nil {
		t.Fatalf("ListArchived: %v", err)
	}
	if len(archived) != 1 {
		t.Fatalf("archived count = %d", len(archived))
	}
}

func TestCreateAgentArchivedSlugConflict(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "eve", Role: "r"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := s.ArchiveAgent("eve"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := s.CreateAgent(Agent{Slug: "eve", Role: "r"}, ""); err == nil {
		t.Fatal("expected conflict with archived slug")
	}
}

func TestChatAppendAndRead(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "f", Role: "r"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	msgs := []ChatMessage{
		{Role: RoleReceived, Content: "hi"},
		{Role: RoleSent, Content: "hello"},
		{Role: RoleReceived, Content: "again", Kind: "direct_chat", TS: time.Now().UTC()},
	}
	for _, m := range msgs {
		if err := s.AppendChatMessage("f", m); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := s.ReadChatHistory("f")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("len = %d, want %d", len(got), len(msgs))
	}
	for i, m := range got {
		if m.Content != msgs[i].Content || m.Role != msgs[i].Role {
			t.Errorf("[%d] = %+v, want %+v", i, m, msgs[i])
		}
		if m.TS.IsZero() {
			t.Errorf("[%d] TS zero", i)
		}
	}
}

func TestChatAppendMissingAgent(t *testing.T) {
	s := mustStore(t)
	err := s.AppendChatMessage("nope", ChatMessage{Role: RoleReceived, Content: "x"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAgentMemoryRoundtrip(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "g", Role: "r"}, "role"); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if _, err := s.ReadAgentMemory("g"); !errors.Is(err, ErrNotFound) {
		t.Errorf("agent memory: want ErrNotFound, got %v", err)
	}
	if err := s.WriteAgentMemory("g", "biz notes"); err != nil {
		t.Fatalf("WriteAgentMemory: %v", err)
	}
	if b, _ := s.ReadAgentMemory("g"); b != "biz notes" {
		t.Errorf("agent memory = %q", b)
	}
}

func TestWriteAgentFileMissingAgent(t *testing.T) {
	s := mustStore(t)
	if err := s.WriteAgentMemory("ghost", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestActiveTurnMarkerRoundTrip exercises the four marker primitives
// the chat loop and boot-recovery path share. The marker is the
// durable signal that distinguishes a graceful end-of-turn (marker
// cleared) from a kill mid-turn (marker survives).
func TestActiveTurnMarkerRoundTrip(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "cos", Role: "Chief"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// Unset → zero marker, no error.
	m, err := s.ReadActiveTurn("cos")
	if err != nil {
		t.Fatalf("ReadActiveTurn (unset): %v", err)
	}
	if !m.StartedAt.IsZero() {
		t.Errorf("unset read = %+v, want zero", m)
	}
	// Mark it. The store stamps StartedAt if missing.
	if err := s.MarkActiveTurn("cos", ActiveTurnMarker{Source: "chat", Model: "claude-opus-4-7"}); err != nil {
		t.Fatalf("MarkActiveTurn: %v", err)
	}
	got, err := s.ReadActiveTurn("cos")
	if err != nil {
		t.Fatalf("ReadActiveTurn: %v", err)
	}
	if got.StartedAt.IsZero() || got.Source != "chat" || got.Model != "claude-opus-4-7" {
		t.Errorf("got = %+v, want StartedAt set, Source=chat, Model=claude-opus-4-7", got)
	}
	// Orphan list includes this slug.
	orphans, err := s.ListOrphanActiveTurns()
	if err != nil {
		t.Fatalf("ListOrphanActiveTurns: %v", err)
	}
	if len(orphans) != 1 || orphans[0] != "cos" {
		t.Errorf("orphans = %v, want [cos]", orphans)
	}
	// Clear → unset, orphan list empties.
	if err := s.ClearActiveTurn("cos"); err != nil {
		t.Fatalf("ClearActiveTurn: %v", err)
	}
	got, _ = s.ReadActiveTurn("cos")
	if !got.StartedAt.IsZero() {
		t.Errorf("after Clear got = %+v, want zero", got)
	}
	orphans, _ = s.ListOrphanActiveTurns()
	if len(orphans) != 0 {
		t.Errorf("orphans after Clear = %v, want []", orphans)
	}
	// Clear is idempotent on missing.
	if err := s.ClearActiveTurn("cos"); err != nil {
		t.Errorf("ClearActiveTurn (already gone): %v", err)
	}
}

// TestPendingRotationRoundTrip exercises the three rotation-marker
// primitives. The marker is the durable signal handleNewChat (and
// future agent-self-rotation tools) write so a Kivali crash mid-
// rotation doesn't lose the archive semantics — the next finalize
// after recovery still finds the marker.
func TestPendingRotationRoundTrip(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "cos", Role: "Chief"}, ""); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	// Unset → not present, no error.
	if _, ok, err := s.ReadPendingRotation("cos"); err != nil || ok {
		t.Errorf("unset Read returned ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	// Write it. Store stamps RequestedAt if missing.
	if err := s.WritePendingRotation("cos", PendingRotation{
		PriorMemory: "old memory",
		RequestedBy: "ceo",
	}); err != nil {
		t.Fatalf("WritePendingRotation: %v", err)
	}
	got, ok, err := s.ReadPendingRotation("cos")
	if err != nil || !ok {
		t.Fatalf("ReadPendingRotation: ok=%v err=%v", ok, err)
	}
	if got.PriorMemory != "old memory" || got.RequestedBy != "ceo" || got.RequestedAt.IsZero() {
		t.Errorf("got = %+v, want PriorMemory=old memory RequestedBy=ceo RequestedAt set", got)
	}
	// Clear → not present.
	if err := s.ClearPendingRotation("cos"); err != nil {
		t.Fatalf("ClearPendingRotation: %v", err)
	}
	if _, ok, _ := s.ReadPendingRotation("cos"); ok {
		t.Errorf("ReadPendingRotation after Clear returned ok=true")
	}
	// Clear is idempotent on missing.
	if err := s.ClearPendingRotation("cos"); err != nil {
		t.Errorf("ClearPendingRotation (already gone): %v", err)
	}
}

func TestWritePendingRotationMissingAgent(t *testing.T) {
	s := mustStore(t)
	if err := s.WritePendingRotation("ghost", PendingRotation{RequestedBy: "ceo"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestMarkActiveTurnMissingAgent(t *testing.T) {
	s := mustStore(t)
	if err := s.MarkActiveTurn("ghost", ActiveTurnMarker{Source: "chat"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListOrphanActiveTurnsMixed(t *testing.T) {
	s := mustStore(t)
	if err := s.CreateAgent(Agent{Slug: "alpha", Role: "x"}, ""); err != nil {
		t.Fatalf("CreateAgent alpha: %v", err)
	}
	if err := s.CreateAgent(Agent{Slug: "beta", Role: "y"}, ""); err != nil {
		t.Fatalf("CreateAgent beta: %v", err)
	}
	if err := s.CreateAgent(Agent{Slug: "gamma", Role: "z"}, ""); err != nil {
		t.Fatalf("CreateAgent gamma: %v", err)
	}
	// Mark alpha and gamma; leave beta clean.
	if err := s.MarkActiveTurn("alpha", ActiveTurnMarker{Source: "chat"}); err != nil {
		t.Fatalf("mark alpha: %v", err)
	}
	if err := s.MarkActiveTurn("gamma", ActiveTurnMarker{Source: "release"}); err != nil {
		t.Fatalf("mark gamma: %v", err)
	}
	orphans, err := s.ListOrphanActiveTurns()
	if err != nil {
		t.Fatalf("ListOrphanActiveTurns: %v", err)
	}
	// Order is map-iteration-dependent; check membership.
	want := map[string]bool{"alpha": true, "gamma": true}
	got := map[string]bool{}
	for _, slug := range orphans {
		got[slug] = true
	}
	for s := range want {
		if !got[s] {
			t.Errorf("missing %s in orphans %v", s, orphans)
		}
	}
	for s := range got {
		if !want[s] {
			t.Errorf("unexpected %s in orphans %v", s, orphans)
		}
	}
}
