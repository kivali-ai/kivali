package message

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/roleicon"
	"github.com/kivali-ai/kivali/internal/store"
)

func toolUse(name string, input map[string]any) provider.ContentBlock {
	raw, _ := json.Marshal(input)
	return provider.ContentBlock{
		Type:      provider.ContentToolUse,
		ToolUseID: "toolu_1",
		ToolName:  name,
		ToolInput: raw,
	}
}

func fixedCtx() ParseContext {
	return ParseContext{
		From: "chief-of-staff",
		Now:  time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC),
	}
}

// ctxWithFiles returns a ParseContext whose ResolveBodyPath looks
// up paths in the provided map. Tests use it to exercise the
// artifact-path resolution (role_path, handbook_path,
// initial_memory_path) without needing a real filesystem.
func ctxWithFiles(files map[string]string) ParseContext {
	c := fixedCtx()
	c.ResolveBodyPath = func(p string) (string, error) {
		body, ok := files[p]
		if !ok {
			return "", errors.New("path not found in test fixture: " + p)
		}
		return body, nil
	}
	return c
}

// TestParseAttachmentPathMode verifies path mode parses to a
// ParsedAttachment with Path set + name defaulting to basename.
// Final access check + ingestion happens at the dispatcher; here we
// only test the parser's normalisation.
func TestParseAttachmentPathMode(t *testing.T) {
	block := toolUse(ToolCEONotification, map[string]any{
		"title": "t",
		"body":  "b",
		"attachments": []map[string]any{
			// Name omitted on purpose — should default to basename.
			{"path": "  /files/artifacts/private/data.bin  "},
			// Two attachments to confirm batch parsing works.
			{"path": "/files/project/spec.md", "name": "Q3 spec.md"},
		},
	})
	res, err := Parse(block, fixedCtx())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Attachments) != 2 {
		t.Fatalf("attachments = %d, want 2", len(res.Attachments))
	}
	if res.Attachments[0].Name != "data.bin" {
		t.Errorf("[0] Name = %q, want data.bin (basename default)", res.Attachments[0].Name)
	}
	if res.Attachments[0].Path != "/files/artifacts/private/data.bin" {
		t.Errorf("[0] Path = %q, want trimmed path", res.Attachments[0].Path)
	}
	if res.Attachments[1].Name != "Q3 spec.md" {
		t.Errorf("[1] Name = %q, want Q3 spec.md (explicit override)", res.Attachments[1].Name)
	}
}

// TestParseAttachmentRejectsInlineModes: an attachment is a path only;
// inline content / base64 / zip-entries are unknown fields, which
// strictUnmarshal rejects, so the parser errors loudly rather than
// silently dropping the bytes.
func TestParseAttachmentRejectsInlineModes(t *testing.T) {
	cases := map[string]map[string]any{
		"inline content":   {"name": "x.md", "content": "a"},
		"base64 content":   {"name": "x.bin", "content_base64": "YQ==", "mime": "application/octet-stream"},
		"zip bundle":       {"name": "x.zip", "zip_entries": []map[string]string{{"path": "a", "content": "a"}}},
		"missing path":     {"name": "x.md"},
		"empty path":       {"path": "   "},
		"path + name only": {"path": "/files/artifacts/private/ok.md"},
	}
	for name, att := range cases {
		t.Run(name, func(t *testing.T) {
			block := toolUse(ToolCEONotification, map[string]any{
				"title":       "t",
				"body":        "b",
				"attachments": []map[string]any{att},
			})
			_, err := Parse(block, fixedCtx())
			// "path + name only" is the one valid case — confirm it parses.
			if name == "path + name only" {
				if err != nil {
					t.Fatalf("path-only attachment should parse: %v", err)
				}
				return
			}
			if err == nil {
				t.Errorf("%s: expected error", name)
			}
		})
	}
}

func TestParseMissingFields(t *testing.T) {
	// Genuine required-field omissions across the publish_* family.
	// body itself is OPTIONAL on publish_* (just title + to required);
	// we only assert what the parser actually rejects.
	cases := map[string]map[string]any{
		"notice missing to":                                  {"title": "t", "body": "b"},
		"notice empty title":                                 {"to": []string{"x"}, "title": "", "body": "b"},
		"ceo_notification empty title":                       {"title": "", "body": "b"},
		"ceo_notification rejects body_path (dropped field)": {"title": "t", "body_path": "/files/artifacts/private/x.md"},
	}
	tools := map[string]string{
		"notice missing to":                                  ToolNotice,
		"notice empty title":                                 ToolNotice,
		"ceo_notification empty title":                       ToolCEONotification,
		"ceo_notification rejects body_path (dropped field)": ToolCEONotification,
	}
	for name, input := range cases {
		block := toolUse(tools[name], input)
		if _, err := Parse(block, fixedCtx()); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// TestParseBodyCap pins the message-body cap: bodies exceeding
// MaxMessageBodyBytes (4096) are rejected with a hint pointing at
// attachments.
func TestParseBodyCap(t *testing.T) {
	long := strings.Repeat("x", MaxMessageBodyBytes+1)
	block := toolUse(ToolCEONotification, map[string]any{
		"title": "t",
		"body":  long,
	})
	_, err := Parse(block, fixedCtx())
	if err == nil {
		t.Fatal("expected cap violation")
	}
	if !strings.Contains(err.Error(), "cap is 4096") {
		t.Errorf("error should mention the cap value: %v", err)
	}
	if !strings.Contains(err.Error(), "attachments") {
		t.Errorf("error should hint at attachments: %v", err)
	}
}

func TestParseUnknownTool(t *testing.T) {
	block := toolUse("publish_nothing", map[string]any{"x": 1})
	_, err := Parse(block, fixedCtx())
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("err = %v, want ErrUnknownTool", err)
	}
}

func TestParseUnknownFieldRejected(t *testing.T) {
	block := toolUse(ToolCEONotification, map[string]any{
		"title": "t",
		"body":  "b",
		"bogus": "nope",
	})
	if _, err := Parse(block, fixedCtx()); err == nil {
		t.Error("expected error for unknown field")
	}
}

func TestParseRoundtripToStore(t *testing.T) {
	// Full round-trip: parse tool_use → store.Message → write → read.
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	block := toolUse(ToolNotice, map[string]any{
		"to":    []string{"market-analyst"},
		"title": "Q3 analysis is posted",
		"body":  "see the shared artifact",
	})
	res, err := Parse(block, fixedCtx())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	message := res.Message
	path, err := s.WriteMessage(message)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := s.ReadMessage(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Type != message.Type || got.From != message.From || got.To.Primary() != message.To.Primary() || got.Title != message.Title {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got, message)
	}
}

func TestParseDefaultsNowWhenZero(t *testing.T) {
	block := toolUse(ToolCEONotification, map[string]any{"title": "t", "body": "b"})
	before := time.Now().UTC()
	res, err := Parse(block, ParseContext{From: "x"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	after := time.Now().UTC()
	if res.Message.Date.Before(before) || res.Message.Date.After(after) {
		t.Errorf("Date = %v, not in [%v, %v]", res.Message.Date, before, after)
	}
}

func TestParseProposeHireProducesApprovalRequestWithHire(t *testing.T) {
	block := toolUse(ToolProposeHire, map[string]any{
		"title":               "Hire: Market Analyst",
		"slug":                "market-analyst",
		"role":                "Market Analyst",
		"icon":                "chart-line",
		"reports_to":          "chief-of-staff",
		"role_path":           "/files/artifacts/private/role-market-analyst.md",
		"initial_memory_path": "/files/artifacts/private/seed.md",
		"rationale":           "We keep re-deriving sizing by hand.",
	})
	ctx := ctxWithFiles(map[string]string{
		"/files/artifacts/private/role-market-analyst.md": "# Market Analyst\nowns sizing\n",
		"/files/artifacts/private/seed.md":                "starting fresh\n",
	})
	res, err := Parse(block, ctx)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Message.Type != store.MsgCEOApprovalRequest {
		t.Errorf("type = %q, want ceo_approval_request", res.Message.Type)
	}
	if got := res.Message.To.Primary(); got != CEO {
		t.Errorf("to = %q, want ceo", got)
	}
	if res.Message.Body != "We keep re-deriving sizing by hand." {
		t.Errorf("rationale should ride on the body, got %q", res.Message.Body)
	}
	if res.Message.Hire == nil {
		t.Fatal("Hire should be set on the parsed message")
	}
	h := res.Message.Hire
	if h.Slug != "market-analyst" || h.Role != "Market Analyst" || h.Icon != "chart-line" || h.ReportsTo != "chief-of-staff" {
		t.Errorf("hire fields wrong: %+v", h)
	}
	if !strings.Contains(h.Body, "owns sizing") {
		t.Errorf("hire body not captured: %q", h.Body)
	}
	if !strings.Contains(h.InitialAgentMemory, "starting fresh") {
		t.Errorf("hire initial memory not captured: %q", h.InitialAgentMemory)
	}
}

func TestParseProposeHireRequiresFields(t *testing.T) {
	block := toolUse(ToolProposeHire, map[string]any{
		"title": "Hire someone",
		"slug":  "x",
		// missing role, reports_to, role_path
	})
	_, err := Parse(block, fixedCtx())
	if err == nil {
		t.Fatal("expected error on incomplete hire")
	}
}

// Hiring is CoS-only. The tool list already omits propose_hire for
// everyone else; this is the parser-side half of that gate.
func TestParseProposeHireRefusesNonChiefOfStaff(t *testing.T) {
	block := toolUse(ToolProposeHire, map[string]any{
		"title":      "Hire: my own deputy",
		"slug":       "deputy",
		"role":       "Deputy",
		"icon":       "user",
		"reports_to": "market-analyst",
		"role_path":  "/files/artifacts/private/role-deputy.md",
	})
	ctx := ctxWithFiles(map[string]string{"/files/artifacts/private/role-deputy.md": "# Deputy\n"})
	ctx.From = "market-analyst"
	_, err := Parse(block, ctx)
	if err == nil {
		t.Fatal("expected refusal: only the Chief of Staff may propose hires")
	}
	if !strings.Contains(err.Error(), "Chief of Staff") {
		t.Errorf("error should name the gate, got %q", err)
	}
}

func TestParseProposeHireRefusesReservedSlugs(t *testing.T) {
	for _, slug := range []string{CEO, ChiefOfStaff} {
		block := toolUse(ToolProposeHire, map[string]any{
			"title":      "Hire: shadow",
			"slug":       slug,
			"role":       "Shadow",
			"icon":       "user",
			"reports_to": "ceo",
			"role_path":  "/files/artifacts/private/role.md",
		})
		ctx := ctxWithFiles(map[string]string{"/files/artifacts/private/role.md": "# Shadow\n"})
		if _, err := Parse(block, ctx); err == nil {
			t.Errorf("hiring into reserved slug %q should be refused", slug)
		}
	}
}

// The icon is the hiring agent's pick from the role icon set; a name
// outside it is refused, and the error lists the choices.
func TestParseProposeHireRefusesAnUnknownIcon(t *testing.T) {
	block := toolUse(ToolProposeHire, map[string]any{
		"title":      "Hire: Market Analyst",
		"slug":       "market-analyst",
		"role":       "Market Analyst",
		"icon":       "robot",
		"reports_to": "chief-of-staff",
		"role_path":  "/files/artifacts/private/role.md",
	})
	ctx := ctxWithFiles(map[string]string{"/files/artifacts/private/role.md": "# Market Analyst\n"})
	_, err := Parse(block, ctx)
	if err == nil {
		t.Fatal("expected refusal of an icon outside the set")
	}
	if !strings.Contains(err.Error(), `"robot" is not a role icon`) || !strings.Contains(err.Error(), "chart-line") {
		t.Errorf("error should name the bad icon and the choices, got %q", err)
	}
}

func TestParseCEONotificationRejectsHire(t *testing.T) {
	block := toolUse(ToolCEONotification, map[string]any{
		"title": "FYI",
		"body":  "heads up",
		"hire": map[string]any{
			"slug": "x", "role": "R", "reports_to": "ceo", "role_path": "/files/artifacts/private/r.md",
		},
	})
	_, err := Parse(block, fixedCtx())
	if err == nil {
		t.Fatal("expected error: a notification cannot carry a hire")
	}
}

/* ---------- propose_offboard ------------------------------------- */

func TestParseProposeOffboardProducesApprovalRequestWithOffboard(t *testing.T) {
	block := toolUse(ToolProposeOffboard, map[string]any{
		"title":     "Offboard: Market Analyst",
		"slug":      "market-analyst",
		"rationale": "Scope folded into Strategy; no longer a distinct role.",
	})
	res, err := Parse(block, fixedCtx())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Message.Type != store.MsgCEOApprovalRequest {
		t.Errorf("type = %q, want ceo_approval_request", res.Message.Type)
	}
	if got := res.Message.To.Primary(); got != CEO {
		t.Errorf("to = %q, want ceo", got)
	}
	if res.Message.Offboard == nil {
		t.Fatal("Offboard should be set on the parsed message")
	}
	if got := res.Message.Offboard.Slug; got != "market-analyst" {
		t.Errorf("offboard slug = %q", got)
	}
	if res.Message.Offboard.Reason == "" {
		t.Error("rationale should be kept on the proposal for the record")
	}
}

func TestParseProposeOffboardRefusesNonChiefOfStaff(t *testing.T) {
	block := toolUse(ToolProposeOffboard, map[string]any{
		"title": "Offboard: my rival",
		"slug":  "market-analyst",
	})
	ctx := fixedCtx()
	ctx.From = "strategy-lead"
	_, err := Parse(block, ctx)
	if err == nil {
		t.Fatal("expected refusal: only the Chief of Staff may propose offboarding")
	}
}

// The two slugs that cannot be offboarded. CoS proposing its own
// offboard is the interesting one — it would leave the org with
// no way to propose anything, including undoing it.
func TestParseProposeOffboardRefusesLoadBearingSlugs(t *testing.T) {
	for _, slug := range []string{CEO, ChiefOfStaff} {
		block := toolUse(ToolProposeOffboard, map[string]any{
			"title": "Offboard",
			"slug":  slug,
		})
		if _, err := Parse(block, fixedCtx()); err == nil {
			t.Errorf("offboarding %q should be refused", slug)
		}
	}
}

func TestParseProposeOffboardRequiresSlug(t *testing.T) {
	block := toolUse(ToolProposeOffboard, map[string]any{"title": "Offboard someone"})
	if _, err := Parse(block, fixedCtx()); err == nil {
		t.Fatal("expected error when slug is missing")
	}
}

func TestParseProposeRoleUpdateProducesApprovalRequestWithRoleUpdate(t *testing.T) {
	block := toolUse(ToolProposeRoleUpdate, map[string]any{
		"slug":      "market-analyst",
		"title":     "Update market-analyst role: APAC",
		"role_path": "/files/artifacts/private/role-market-analyst-v2.md",
		"rationale": "Adding APAC scope; rest unchanged.",
	})
	ctx := ctxWithFiles(map[string]string{
		"/files/artifacts/private/role-market-analyst-v2.md": "# Market Analyst\nNew scope including APAC.\n",
	})
	res, err := Parse(block, ctx)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Message.Type != store.MsgCEOApprovalRequest {
		t.Errorf("Type = %q, want ceo_approval_request — propose_role_update routes through the approval flow", res.Message.Type)
	}
	if res.Message.To.Primary() != CEO {
		t.Errorf("To = %q, want %q (always CEO-bound)", res.Message.To, CEO)
	}
	if res.Message.Title != "Update market-analyst role: APAC" {
		t.Errorf("Title = %q", res.Message.Title)
	}
	// The rationale becomes the approval request body — what the CEO
	// reads in their inbox to decide.
	if !strings.Contains(res.Message.Body, "APAC scope") {
		t.Errorf("rationale not captured as body: %q", res.Message.Body)
	}
	if res.Message.RoleUpdate == nil {
		t.Fatal("RoleUpdate should be set on the parsed approval request")
	}
	if res.Message.RoleUpdate.Slug != "market-analyst" {
		t.Errorf("role_update slug = %q", res.Message.RoleUpdate.Slug)
	}
	if !strings.Contains(res.Message.RoleUpdate.Body, "Including APAC") &&
		!strings.Contains(res.Message.RoleUpdate.Body, "APAC") {
		t.Errorf("role_update body not captured: %q", res.Message.RoleUpdate.Body)
	}
}

func TestParseProposeRoleUpdateRejectsNonCoSCaller(t *testing.T) {
	block := toolUse(ToolProposeRoleUpdate, map[string]any{
		"slug":      "market-analyst",
		"title":     "Update market-analyst role",
		"role_path": "/files/x.md",
		"rationale": "reason",
	})
	ctx := fixedCtx()
	ctx.From = "market-analyst" // not chief-of-staff
	_, err := Parse(block, ctx)
	if err == nil {
		t.Fatal("expected error: only CoS may call propose_role_update")
	}
	if !strings.Contains(err.Error(), "Chief of Staff") {
		t.Errorf("error should mention CoS gating: %v", err)
	}
}

func TestParseProposeRoleUpdateRequiresFields(t *testing.T) {
	files := map[string]string{
		"/files/artifacts/private/role.md":  "# Replacement\n",
		"/files/artifacts/private/empty.md": "   ",
	}
	for name, tc := range map[string]map[string]any{
		"missing slug": {
			"title": "x", "role_path": "/files/artifacts/private/role.md", "rationale": "r",
		},
		"missing title": {
			"slug": "alice", "role_path": "/files/artifacts/private/role.md", "rationale": "r",
		},
		"missing role_path": {
			"slug": "alice", "title": "x", "rationale": "r",
		},
		"role_path resolves empty": {
			"slug": "alice", "title": "x", "role_path": "/files/artifacts/private/empty.md", "rationale": "r",
		},
		"rejects body field (dropped)": {
			"slug": "alice", "title": "x", "body": "# r\n", "role_path": "/files/artifacts/private/role.md", "rationale": "r",
		},
		"rejects body_path (dropped)": {
			"slug": "alice", "title": "x", "body_path": "/files/x.md", "role_path": "/files/artifacts/private/role.md", "rationale": "r",
		},
		"rejects rationale_path (dropped)": {
			"slug": "alice", "title": "x", "role_path": "/files/artifacts/private/role.md", "rationale_path": "/files/x.md",
		},
	} {
		t.Run(name, func(t *testing.T) {
			block := toolUse(ToolProposeRoleUpdate, tc)
			if _, err := Parse(block, ctxWithFiles(files)); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestParseProposeHandbookUpdateProducesApprovalRequest(t *testing.T) {
	block := toolUse(ToolProposeHandbookUpdate, map[string]any{
		"title":         "Handbook: tighten escalation rule",
		"handbook_path": "/files/artifacts/private/handbook-v2.md",
		"rationale":     "Customer issues need a stricter escalation path.",
	})
	ctx := ctxWithFiles(map[string]string{
		"/files/artifacts/private/handbook-v2.md": "# Handbook\nRevised rules.\n",
	})
	res, err := Parse(block, ctx)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Message.Type != store.MsgCEOApprovalRequest {
		t.Errorf("Type = %q, want ceo_approval_request", res.Message.Type)
	}
	if res.Message.To.Primary() != CEO {
		t.Errorf("To = %q, want %q (always CEO-bound)", res.Message.To, CEO)
	}
	if res.Message.Title != "Handbook: tighten escalation rule" {
		t.Errorf("Title = %q", res.Message.Title)
	}
	if !strings.Contains(res.Message.Body, "stricter escalation") {
		t.Errorf("rationale not captured as body: %q", res.Message.Body)
	}
	if res.Message.HandbookUpdate == nil {
		t.Fatal("HandbookUpdate should be set on the parsed approval request")
	}
	if !strings.Contains(res.Message.HandbookUpdate.Body, "Revised rules") {
		t.Errorf("handbook_update body not captured: %q", res.Message.HandbookUpdate.Body)
	}
}

func TestParseProposeHandbookUpdateRejectsNonCoSCaller(t *testing.T) {
	block := toolUse(ToolProposeHandbookUpdate, map[string]any{
		"title":         "Handbook change",
		"handbook_path": "/files/x.md",
		"rationale":     "reason",
	})
	ctx := fixedCtx()
	ctx.From = "market-analyst" // not chief-of-staff
	_, err := Parse(block, ctx)
	if err == nil {
		t.Fatal("expected error: only CoS may call propose_handbook_update")
	}
	if !strings.Contains(err.Error(), "Chief of Staff") {
		t.Errorf("error should mention CoS gating: %v", err)
	}
}

func TestParseProposeHandbookUpdateRequiresFields(t *testing.T) {
	files := map[string]string{
		"/files/artifacts/private/c.md":     "# Handbook\n",
		"/files/artifacts/private/empty.md": "   ",
	}
	for name, tc := range map[string]map[string]any{
		"missing title": {
			"handbook_path": "/files/artifacts/private/c.md", "rationale": "r",
		},
		"missing handbook_path": {
			"title": "x", "rationale": "r",
		},
		"handbook_path resolves empty": {
			"title": "x", "handbook_path": "/files/artifacts/private/empty.md", "rationale": "r",
		},
		"rejects body field (dropped)": {
			"title": "x", "body": "# r\n", "handbook_path": "/files/artifacts/private/c.md", "rationale": "r",
		},
		"rejects body_path field (dropped)": {
			"title": "x", "body_path": "/files/x.md", "handbook_path": "/files/artifacts/private/c.md", "rationale": "r",
		},
	} {
		t.Run(name, func(t *testing.T) {
			block := toolUse(ToolProposeHandbookUpdate, tc)
			if _, err := Parse(block, ctxWithFiles(files)); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestToolsForGatesProposeHandbookUpdate(t *testing.T) {
	// Only CoS sees the tool; non-CoS agents must not be offered it.
	cosTools := ToolsFor("chief-of-staff", true)
	var found bool
	for _, td := range cosTools {
		if td.Name == ToolProposeHandbookUpdate {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("CoS should see propose_handbook_update; got tools %v", toolNames(cosTools))
	}
	other := ToolsFor("market-analyst", false)
	for _, td := range other {
		if td.Name == ToolProposeHandbookUpdate {
			t.Errorf("non-CoS must not see propose_handbook_update; got tools %v", toolNames(other))
		}
	}
}

func TestParseProposeReorgProducesApprovalRequest(t *testing.T) {
	block := toolUse(ToolProposeReorg, map[string]any{
		"title": "Reorg: lift Strategy under CEO",
		"moves": []any{
			map[string]any{"slug": "market-analyst", "new_manager": "strategy-lead"},
			map[string]any{"slug": "strategy-lead", "new_manager": "ceo"},
		},
		"rationale": "Strategy needs a direct line; analyst rolls under it.",
	})
	res, err := Parse(block, fixedCtx())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Message.Type != store.MsgCEOApprovalRequest {
		t.Errorf("Type = %q, want ceo_approval_request", res.Message.Type)
	}
	if res.Message.To.Primary() != CEO {
		t.Errorf("To = %q, want %q (always CEO-bound)", res.Message.To, CEO)
	}
	if !strings.Contains(res.Message.Body, "Strategy needs a direct line") {
		t.Errorf("rationale not captured as body: %q", res.Message.Body)
	}
	if res.Message.Reorg == nil {
		t.Fatal("Reorg should be set on the parsed approval request")
	}
	if got, want := len(res.Message.Reorg.Moves), 2; got != want {
		t.Fatalf("Moves count = %d, want %d", got, want)
	}
	if res.Message.Reorg.Moves[1].Slug != "strategy-lead" || res.Message.Reorg.Moves[1].NewManager != "ceo" {
		t.Errorf("second move = %+v", res.Message.Reorg.Moves[1])
	}
	// Applied/Failed live on the response, not the request.
	if len(res.Message.Reorg.Applied) != 0 || len(res.Message.Reorg.Failed) != 0 {
		t.Errorf("request must not carry Applied/Failed; got %+v", res.Message.Reorg)
	}
}

func TestParseProposeReorgRejectsNonCoSCaller(t *testing.T) {
	block := toolUse(ToolProposeReorg, map[string]any{
		"title": "Reorg",
		"moves": []any{map[string]any{"slug": "alice", "new_manager": "bob"}},
	})
	ctx := fixedCtx()
	ctx.From = "market-analyst"
	_, err := Parse(block, ctx)
	if err == nil {
		t.Fatal("expected error: only CoS may call propose_reorg")
	}
	if !strings.Contains(err.Error(), "Chief of Staff") {
		t.Errorf("error should mention CoS gating: %v", err)
	}
}

func TestParseProposeReorgValidation(t *testing.T) {
	for name, tc := range map[string]map[string]any{
		"missing title": {
			"moves": []any{map[string]any{"slug": "a", "new_manager": "b"}},
		},
		"empty moves": {
			"title": "x", "moves": []any{},
		},
		"missing slug": {
			"title": "x",
			"moves": []any{map[string]any{"new_manager": "b"}},
		},
		"missing new_manager": {
			"title": "x",
			"moves": []any{map[string]any{"slug": "a"}},
		},
		"slug == new_manager": {
			"title": "x",
			"moves": []any{map[string]any{"slug": "a", "new_manager": "a"}},
		},
		"moving ceo": {
			"title": "x",
			"moves": []any{map[string]any{"slug": "ceo", "new_manager": "alice"}},
		},
		"duplicate target": {
			"title": "x",
			"moves": []any{
				map[string]any{"slug": "a", "new_manager": "b"},
				map[string]any{"slug": "a", "new_manager": "c"},
			},
		},
		"bad slug format": {
			"title": "x",
			"moves": []any{map[string]any{"slug": "bad slug!", "new_manager": "b"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			block := toolUse(ToolProposeReorg, tc)
			if _, err := Parse(block, fixedCtx()); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestToolsForGatesProposeReorg(t *testing.T) {
	cos := ToolsFor("chief-of-staff", true)
	var found bool
	for _, td := range cos {
		if td.Name == ToolProposeReorg {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("CoS should see propose_reorg; got %v", toolNames(cos))
	}
	other := ToolsFor("market-analyst", false)
	for _, td := range other {
		if td.Name == ToolProposeReorg {
			t.Errorf("non-CoS must not see propose_reorg; got %v", toolNames(other))
		}
	}
}

func toolNames(defs []ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func TestParseCEOApprovalRequestNoHire(t *testing.T) {
	block := toolUse(ToolCEOApprovalRequest, map[string]any{
		"title": "Approve this direction change",
		"body":  "pls say yes",
	})
	res, err := Parse(block, fixedCtx())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Message.Hire != nil {
		t.Errorf("Hire should be nil when not supplied; got %+v", res.Message.Hire)
	}
}

// The hiring agent chooses from propose_hire's schema: icon is
// required, its enum is exactly the role icon set, and its description
// carries every icon with what it shows.
func TestProposeHireSchemaOffersEveryRoleIcon(t *testing.T) {
	var schema struct {
		Properties map[string]struct {
			Enum        []string `json:"enum"`
			Description string   `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(proposeHireTool.Schema, &schema); err != nil {
		t.Fatalf("propose_hire schema is not JSON: %v", err)
	}
	if !slices.Contains(schema.Required, "icon") {
		t.Errorf("icon is not required: %v", schema.Required)
	}
	icon := schema.Properties["icon"]
	if len(icon.Enum) != len(roleicon.Icons) {
		t.Fatalf("enum has %d names, want %d", len(icon.Enum), len(roleicon.Icons))
	}
	for i, ic := range roleicon.Icons {
		if icon.Enum[i] != ic.Name {
			t.Errorf("enum[%d] = %q, want %q", i, icon.Enum[i], ic.Name)
		}
		if !strings.Contains(icon.Description, "- "+ic.Name+": "+ic.Description+"\n") {
			t.Errorf("description lacks %q", ic.Name)
		}
	}
}
