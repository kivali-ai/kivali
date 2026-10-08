package seed

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/owner"
)

// The handbook is the one place the full graph rules live for an
// agent, and it is seeded once, so what it says about the kinds has to
// match the allow list the index enforces or an agent is taught a kind
// that is rejected on write. Driven off graph.Kinds so a new kind fails
// here until the handbook catches up.
func TestHandbookNamesEveryGraphKindAndBothTools(t *testing.T) {
	if !strings.Contains(Handbook, "## The knowledge graph") {
		t.Fatal("handbook has no knowledge graph section")
	}
	for _, k := range graph.Kinds {
		if !strings.Contains(Handbook, "`"+string(k)+"`") {
			t.Errorf("handbook does not describe kind %q", k)
		}
	}
	for _, tool := range []string{"graph_query", "graph_node"} {
		if !strings.Contains(Handbook, "`"+tool+"`") {
			t.Errorf("handbook never names %s", tool)
		}
	}
	for _, status := range graph.DeclaredStatuses {
		if !strings.Contains(Handbook, "`"+string(status)+"`") {
			t.Errorf("handbook does not describe status %q", status)
		}
	}
}

// The handbook carries the full assignment rules
// (docs/developers/assignments.md) and names every tool an agent
// reaches for.
func TestHandbookTeachesTheTracker(t *testing.T) {
	if !strings.Contains(Handbook, "### Assignments") {
		t.Fatal("handbook has no assignments section")
	}
	for _, tool := range []string{"assignment_create", "assignment_update", "assignment_close", "assignment_reopen", "assignment_list", "assignment_view", "publish_notice"} {
		if !strings.Contains(Handbook, "`"+tool+"`") {
			t.Errorf("handbook never names %s", tool)
		}
	}
}

// The handbook-from-files first message points the Chief of Staff
// at a section and at tools by name; each must still exist where the
// message says it is, or the Chief of Staff is sent after nothing.
func TestHandbookFromFilesMessageNamesWhatExists(t *testing.T) {
	msg := HandbookFromFilesMessage
	if strings.TrimSpace(msg) == "" {
		t.Fatal("message is empty")
	}
	if !strings.Contains(msg, `"The company"`) || !strings.Contains(Handbook, "\n## The company\n") {
		t.Error(`message and the default handbook must agree on the section "The company"`)
	}
	for _, tool := range []string{"list_project_files", "file_view", "publish_ceo_notification"} {
		if !strings.Contains(msg, "`"+tool+"`") {
			t.Errorf("message never names %s", tool)
		}
		if !strings.Contains(Handbook, tool) {
			t.Errorf("message names %s, which the handbook does not teach", tool)
		}
	}
	if !strings.Contains(msg, "`propose_handbook_update`") || !strings.Contains(ChiefOfStaffRole, "propose_handbook_update") {
		t.Error("message and cos_role.md must both name propose_handbook_update")
	}
	if strings.Contains(msg, "!") {
		t.Error("message has an exclamation mark")
	}
}

// The work handbook is handbook.md byte for byte; the personal one
// swaps only what comes before the operating sections, which both
// share, and then speaks of the person in a personal team's words.
func TestHandbookKinds(t *testing.T) {
	raw, err := os.ReadFile("handbook.md")
	if err != nil {
		t.Fatal(err)
	}
	if Handbook != string(raw) {
		t.Fatal("the work handbook is not handbook.md byte for byte")
	}
	for _, kind := range []string{"", "work"} {
		if HandbookFor(kind) != Handbook {
			t.Errorf("HandbookFor(%q) is not the work handbook", kind)
		}
	}
	if HandbookFor("personal") != PersonalHandbook {
		t.Error(`HandbookFor("personal") is not the personal handbook`)
	}
	assembled := personalHandbook(Handbook)
	i := strings.Index(Handbook, operatingSectionsStart)
	if i < 0 {
		t.Fatal("handbook.md has no operating sections heading")
	}
	if !strings.HasSuffix(assembled, Handbook[i:]) {
		t.Error("the personal handbook does not end with the shared operating sections")
	}
	if strings.Count(PersonalHandbook, operatingSectionsStart) != 1 {
		t.Error("the personal handbook repeats the operating sections")
	}
	head := strings.TrimSuffix(assembled, Handbook[i+1:])
	if !strings.HasSuffix(head, "\n\n") {
		t.Error("the personal head must end with a blank line before the operating sections")
	}
	for _, want := range []string{"# Kivali Handbook\n", "\n## What this is\n", "\n## About you\n", "A person\nruns parts of their life with a team of agents"} {
		if !strings.Contains(PersonalHandbook, want) {
			t.Errorf("the personal handbook lacks %q", want)
		}
	}
	if strings.Contains(PersonalHandbook, "\n## The company\n") {
		t.Error(`the personal handbook keeps "The company"`)
	}
	if !strings.Contains(PersonalHandbook, "\n## Behavior rules\n") {
		t.Error("the personal handbook lost the last operating section")
	}
}

var ceoWord = regexp.MustCompile(`\bCEO\b`)

// Seed text is the same for every team of a kind and never carries a
// name: the person is "the owner", and the system prompt's owner
// section says what they are called. No seed text says CEO (the tool
// names publish_ceo_* and the slug ceo aside), and none teaches a
// literal markup marker: the owner section names the current one.
func TestSeedTextSaysTheOwner(t *testing.T) {
	docs := map[string]string{
		"work handbook":          HandbookFor("work"),
		"personal handbook":      HandbookFor("personal"),
		"work cos role":          ChiefOfStaffRoleFor("work"),
		"personal cos role":      ChiefOfStaffRoleFor("personal"),
		"files message":          HandbookFromFilesMessage,
		"personal first message": PersonalFirstMessage,
	}
	for name, text := range docs {
		if i := ceoWord.FindStringIndex(text); i != nil {
			t.Errorf("%s still says CEO: …%s…", name, text[max(0, i[0]-60):min(len(text), i[1]+40)])
		}
		if strings.Contains(text, "`> ") {
			t.Errorf("%s teaches a literal markup marker", name)
		}
		if !strings.Contains(text, "the owner") {
			t.Errorf("%s never says the owner", name)
		}
	}
	rule := "Notes the owner\n  adds to a message appear under a line starting with `>` and their\n  name (your system prompt says what they are called)"
	for _, kind := range []string{"work", "personal"} {
		if !strings.Contains(HandbookFor(kind), rule) {
			t.Errorf("the %s handbook lost the markup rule that points at the system prompt", kind)
		}
	}
	role := ChiefOfStaffRoleFor("personal")
	for _, w := range []string{"business", "biz plan"} {
		if strings.Contains(strings.ToLower(role), w) {
			t.Errorf("the personal Chief of Staff role still talks about %q", w)
		}
	}
	if !strings.Contains(role, "You report directly to the owner.") {
		t.Error("the personal role does not say who the Chief of Staff reports to")
	}
	if bad := owner.PersonalRulesUnmatched(ChiefOfStaffRole); len(bad) > 0 {
		t.Errorf("cos_role.md lacks the wording these personal rewrites expect: %v", bad)
	}
	if ChiefOfStaffRoleFor("work") != ChiefOfStaffRole || ChiefOfStaffRoleFor("") != ChiefOfStaffRole {
		t.Error("the work role is not cos_role.md byte for byte")
	}
}

// The personal first message names a section and tools; each must
// exist where it says.
func TestPersonalFirstMessageNamesWhatExists(t *testing.T) {
	msg := PersonalFirstMessage
	if strings.TrimSpace(msg) == "" {
		t.Fatal("message is empty")
	}
	if !strings.Contains(msg, `"About you"`) || !strings.Contains(PersonalHandbook, "\n## About you\n") {
		t.Error(`message and the personal handbook must agree on the section "About you"`)
	}
	for _, tool := range []string{"list_project_files", "file_view", "publish_ceo_notification"} {
		if !strings.Contains(msg, "`"+tool+"`") {
			t.Errorf("message never names %s", tool)
		}
		if !strings.Contains(PersonalHandbook, tool) {
			t.Errorf("message names %s, which the personal handbook does not teach", tool)
		}
	}
	if !strings.Contains(msg, "`propose_handbook_update`") || !strings.Contains(ChiefOfStaffRole, "propose_handbook_update") {
		t.Error("message and cos_role.md must both name propose_handbook_update")
	}
	if strings.Contains(msg, "!") || strings.Contains(handbookPersonalHead, "!") {
		t.Error("personal seed text has an exclamation mark")
	}
}

func TestChiefOfStaffRoleOwnsTheGraphSplit(t *testing.T) {
	if !strings.Contains(ChiefOfStaffRole, "knowledge graph") {
		t.Error("cos_role.md should give the Chief of Staff the graph-hygiene job")
	}
}
