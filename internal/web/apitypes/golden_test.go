package apitypes

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/roleicon"
)

// The golden fixtures under testdata/ are the wire contract as JSON.
// The web app's vitest suite imports them and assigns each to its
// generated type with `satisfies`, so a Go change that is not mirrored
// in web/src/api/types.gen.ts fails the TypeScript build.
//
// `make api-types` rewrites them (go test -run TestGolden -update);
// without -update this test fails when a file is stale, which is what
// `make api-types-check` and CI rely on.
var update = flag.Bool("update", false, "rewrite testdata/*.json from the fixtures")

var (
	t0 = time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)
	t1 = time.Date(2026, 9, 20, 17, 0, 0, 0, time.UTC)
)

func goldenSummary() AgentSummary {
	return AgentSummary{
		Slug: "engineering-lead", Name: "Engineering lead", RoleTitle: "Engineering lead", Icon: "code", ReportsTo: "chief-of-staff",
		Depth: 2, Model: "claude-opus-5-5", ModelLabel: "Opus 5.5", Effort: "high", ContextPct: 82, State: AgentStateRunning, Created: t0,
	}
}

// goldenPendingMessage is one pending CEO message that carries a file,
// as the stream's pending_message and GET …/chat's pending[] describe
// it.
func goldenPendingMessage() PendingMessage {
	return PendingMessage{
		ID:          "7c1d9e4b2a6f",
		Text:        "Also check the staging deploy.",
		QueuedAt:    t1.UnixMilli(),
		Attachments: []PendingAttachment{{SHA: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", Name: "quote.pdf"}},
		Deletable:   true,
	}
}

// goldenFixtures is one fully populated value per response type, with
// every optional field set somewhere so the TypeScript side sees it.
func goldenFixtures() map[string]any {
	archivedAt := t1
	deliveredTS := t1.UnixMilli()
	retired := goldenSummary()
	retired.Slug, retired.Name, retired.RoleTitle, retired.Depth, retired.State, retired.ContextPct = "buyer", "Buyer", "Buyer", 0, AgentStateIdle, 0
	retired.ArchivedAt = &archivedAt
	chatMessage, chatMessageTaskResult := goldenChatMessages()

	return map[string]any{
		"role_icons": roleicon.Icons,
		"error":      ErrorBody{Error: "unauthenticated", Who: "sign in again"},
		"me": Me{
			User:    MeUser{Email: "maya@example.com", Name: "Maya", Initials: "M"},
			Org:     MeOrg{Name: "Plainsong", HasLogo: true},
			Version: "v0.15.0",
			DevMode: false,
		},
		"snapshot": OrgSnapshot{
			Agents: []SnapshotAgent{
				{Slug: "chief-of-staff", Name: "Chief of Staff", RoleTitle: "Chief of Staff", Icon: "compass", ReportsTo: "ceo", Depth: 1, State: AgentStateWaiting, ContextPct: 41, WaitingTasks: 2},
				{Slug: "engineering-lead", Name: "Engineering lead", RoleTitle: "Engineering lead", Icon: "code", ReportsTo: "chief-of-staff", Depth: 2, State: AgentStateRunning, ContextPct: 82},
				{Slug: "buyer", Name: "Buyer", RoleTitle: "Buyer", Icon: "shopping-cart", ReportsTo: "engineering-lead", Depth: 3, State: AgentStateNeedsHelp},
				{Slug: "tester", Name: "tester", RoleTitle: "", ReportsTo: "engineering-lead", Depth: 3, State: AgentStateQuarantined},
			},
			Inbox: SnapshotInbox{
				Unactioned:   1,
				PendingPaths: []string{"messages/2026-09-28/0007-engineering-lead.md"},
				CEOPaths:     []string{"messages/2026-09-28/0003-chief-of-staff.md"},
				AutoRelease:  AutoRelease30s,
			},
			Release:            SnapshotRelease{Running: true, ActiveAgents: 1, PendingDocs: 1},
			MessagesTotal:      214,
			AssignmentsVersion: 17,
			Goals: []Goal{
				{
					ID: 40, Title: "Ship the release", Owner: "engineering-lead", Done: 7, Total: 12,
					Blocked: []GoalBlocker{{ID: 42, On: 45, OnTitle: "Run the tests", OnAssignee: &PersonRef{Slug: "test-runner", Name: "Test runner"}}},
					Workers: []string{"buyer", "tester"},
				},
				{ID: 50, Title: "Hire a CFO", Owner: "chief-of-staff", Done: 0, Total: 3, Blocked: []GoalBlocker{}, Workers: []string{}},
			},
			Readouts: Readouts{Working: 2, Blocked: 1, ClosedWeek: 9, SpendToday: 4.25, Spend7d: 61.5},
		},
		"agents": AgentsResponse{
			Agents:   []AgentSummary{goldenSummary()},
			Archived: []AgentSummary{retired},
		},
		"agent_detail": AgentDetail{
			AgentSummary: goldenSummary(),
			RoleLine:     "Owns the product from plan to release.",
			Models: []ModelOption{
				goldenModelOption("claude-opus-5-5", "Opus 5.5"),
				goldenModelOption("claude-sonnet-4-6", "Sonnet 4.6"),
				// A model with no reasoning control: no efforts, the
				// chip hides.
				{ID: "claude-haiku-3", Label: "Haiku 3", Current: true, ContextWindow: 200000,
					Efforts: []EffortOption{}, Provider: "claude"},
				// A legacy pin the provider no longer knows: only its id.
				{ID: "claude-opus-4-1", Label: "claude-opus-4-1", Legacy: true, ContextWindow: 200000,
					Efforts: []EffortOption{}, Provider: "claude"},
			},
			Context:  ContextFill{Pct: 82, Tokens: 164000, Limit: 200000},
			Counts:   AgentCounts{Background: 2, PastChats: 5},
			Archived: false,
		},
		"auto_release_request":  AutoReleaseRequest{Value: AutoRelease2m},
		"auto_release_response": AutoReleaseResponse{Value: AutoRelease2m},
		// Pending messages: the agent stream's pending_* and chat_marker
		// payloads, and the two responses that name a pending id. See
		// pending.go.
		"pending_message":          goldenPendingMessage(),
		"pending_ref":              PendingRef{ID: "7c1d9e4b2a6f"},
		"pending_delivered":        PendingDelivered{ID: "7c1d9e4b2a6f", TS: t1.UnixMilli()},
		"chat_marker":              ChatMarker{Kind: ChatMarkerPausedToDeliver, Content: "Paused to deliver your message", TS: t1.UnixMilli()},
		"pending_restore_response": PendingRestoreResponse{PendingMessage: goldenPendingMessage(), DeliveredTS: &deliveredTS},
		"message_post_request":     MessagePostRequest{Text: "Can you check the quote?"},
		"message_post_response": MessagePostResponse{
			TS:          t1.UnixMilli(),
			Attachments: []PendingAttachment{{SHA: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", Name: "quote.pdf"}},
			PendingID:   "7c1d9e4b2a6f",
		},
		// Agent chat and its actions: fixtures in chat_golden_test.go.
		"chat":                     goldenChat(),
		"chat_message":             chatMessage,
		"chat_message_task_result": chatMessageTaskResult,
		"subagent_transcript":      goldenSubagentTranscript(),
		"past_chats":               PastChats{Chats: []PastChat{goldenPastChat()}},
		"past_chat_detail":         goldenPastChatDetail(),
		"tool_call_detail":         ToolCallDetail{ToolUseID: "toolu_06", Input: `{"path":"notes/plan.md","content":"# Plan\n\nThe whole file."}`, Output: "Wrote notes/plan.md (48213 bytes) and indexed it."},
		"model_request":            ModelRequest{Model: "claude-opus-5-5"},
		"effort_request":           EffortRequest{Effort: "high"},
		"chat_settings":            ChatSettings{CurrentModel: "claude-opus-5-5", CurrentEffort: "high"},
		"stop_response":            StopResponse{Stopped: true},
		"new_chat_response":        NewChatResponse{Started: true},
		// Home and its actions: fixtures in home_golden_test.go.
		"home":                      goldenHome(),
		"home_history":              goldenHomeHistory(),
		"queue_release_request":     QueueReleaseRequest{Path: "messages/2026-09-28/0007-engineering-lead.md", Note: "Go ahead."},
		"queue_release_all_request": QueueReleaseAllRequest{Paths: []string{"messages/2026-09-28/0007-engineering-lead.md"}, Notes: map[string]string{"messages/2026-09-28/0007-engineering-lead.md": "Go ahead."}},
		"queue_release_response":    QueueReleaseResponse{Released: 2},
		"queue_bounce_request":      QueueBounceRequest{Path: "messages/2026-09-28/0007-engineering-lead.md", Comment: "Not this week."},
		"need_action_response":      NeedActionResponse{Result: "Garden advisor is hired and reports to Chief of Staff", Link: "/agents/garden-advisor"},
		"assignment_close_request":  AssignmentCloseRequest{Resolution: AssignmentResolutionDone, Outcome: "Budget is $40k a quarter."},
		"empty":                     Empty{},
		// Work, assignment detail and Graph: work_golden_test.go.
		"work":                      goldenWorkBoard(),
		"assignment":                goldenAssignment(),
		"assignment_reopen_request": AssignmentReopenRequest{Note: "The notes are missing a section."},
		"assignment_hold_request":   AssignmentHoldRequest{Held: true, Note: "Wait for the review."},
		"assignment_update_request": goldenAssignmentUpdateRequest(),
		"graph":                     goldenGraph(),
		"graph_node":                goldenGraphNode(),
		// Agent Background and About tabs: agent_work_golden_test.go.
		"background":             goldenBackground(),
		"background_empty":       Background{Sessions: []BackgroundSession{}},
		"agent_doc":              goldenAgentDoc(),
		"agent_doc_empty_habits": AgentDoc{Kind: AgentDocKindHabits, Content: ""},
		"agent_doc_put":          AgentDocPut{Content: "# Engineering lead\n\nOwns the release.\n"},
		// The proposal page: fixtures in proposals_golden_test.go.
		"proposal_role_update": goldenProposalRoleUpdate(),
		"proposal_reorg":       goldenProposalReorg(),
		"proposal_offboard":    goldenProposalOffboard(),
		// The Org page: fixtures in org_golden_test.go.
		"org_usage":         goldenUsage(),
		"org":               goldenOrg(),
		"org_put":           OrgPut{Name: "Plainsong"},
		"handbook":          goldenHandbook(),
		"handbook_put":      HandbookPut{Content: "We ship.\n## Money\n- Under budget\n"},
		"project_files":     goldenProjectFiles(),
		"files_bulk_delete": FilesBulkDelete{SHAs: []string{"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"}},
		"skills":            goldenSkills(),
		"skill_downgrade":   goldenSkillDowngrade(),
		"network":           Network{Hosts: []NetworkHost{{Host: "*.pypi.org"}, {Host: "api.anthropic.com"}}},
		"network_add":       NetworkAdd{Host: "api.github.com"},
		// Sign-in and setup: setup_golden_test.go.
		"login":                 Login{Org: MeOrg{Name: "Plainsong", HasLogo: true}, AuthReady: true, DevMode: false},
		"setup":                 goldenSetup(),
		"setup_org_request":     SetupOrgRequest{Name: "Plainsong"},
		"seed_cos_request":      goldenSeedCoSRequest(),
		"setup_progress":        goldenSetupProgress(),
		"setup_progress_failed": goldenSetupProgressFailed(),
	}
}

func TestGolden(t *testing.T) {
	for name, v := range goldenFixtures() {
		t.Run(name, func(t *testing.T) {
			NoNilSlices(t, v)
			got, err := json.MarshalIndent(v, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", name+".json")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run `make api-types` to write it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s is stale; run `make api-types`.\ngot:\n%s", path, got)
			}
		})
	}
}

// testdata holds exactly the fixtures above: a renamed fixture must
// not leave its old file for the TypeScript side to keep compiling.
func TestGoldenNoStrays(t *testing.T) {
	fixtures := goldenFixtures()
	files, _ := filepath.Glob(filepath.Join("testdata", "*.json"))
	for _, f := range files {
		name := filepath.Base(f)
		name = name[:len(name)-len(".json")]
		if _, ok := fixtures[name]; !ok {
			t.Errorf("%s has no fixture; delete it or add one", f)
		}
	}
}

type recorder struct{ errs []string }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, format)
}

func TestNoNilSlicesFindsThem(t *testing.T) {
	var r recorder
	NoNilSlices(&r, OrgSnapshot{Goals: []Goal{{}}})
	// agents, inbox.pending_paths, inbox.ceo_paths, goals[0].blocked,
	// goals[0].workers.
	if len(r.errs) != 5 {
		t.Errorf("found %d nil slices, want 5", len(r.errs))
	}
	r = recorder{}
	d := AgentDetail{Models: []ModelOption{}}
	NoNilSlices(&r, &d)
	if len(r.errs) != 0 {
		t.Errorf("a detail with empty slices and a nil archived_at reported %d", len(r.errs))
	}
}

// goldenEfforts is a model's five reasoning levels, high the default.
func goldenEfforts() []EffortOption {
	return []EffortOption{
		{ID: "low", Label: "Low"},
		{ID: "medium", Label: "Medium"},
		{ID: "high", Label: "High", Default: true},
		{ID: "xhigh", Label: "Extra high"},
		{ID: "max", Label: "Max"},
	}
}

// goldenModelOption is a current model with the five levels.
func goldenModelOption(id, label string) ModelOption {
	return ModelOption{ID: id, Label: label, Current: true, ContextWindow: 1000000, Efforts: goldenEfforts(), Provider: "claude"}
}
