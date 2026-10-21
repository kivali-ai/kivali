package agent

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/graph"
)

// Kivali OS is the one document that tells an agent how the runtime
// works, and it ships in the binary so that it is current on every
// install. These tests hold it to the code it describes: the graph's
// kinds and statuses, the read-only subtrees of /files/, and the
// sections other text points at. The tools it names are checked
// against the registered toolkit in internal/claudeagent
// (TestKivaliOSNamesEveryRegisteredTool), where the full set is
// built. A kind or subtree added to the code fails here until the
// document says so, which is the whole reason the document lives in
// the binary rather than in a team's data.

// What the document says about the knowledge graph has to match the
// allow list the index enforces, or an agent is taught a kind that is
// rejected on write. Driven off graph.Kinds so a new kind fails here
// until the document catches up.
func TestKivaliOSNamesEveryGraphKindAndBothTools(t *testing.T) {
	if !strings.Contains(KivaliOS, "\n## The knowledge graph\n") {
		t.Fatal("Kivali OS has no knowledge graph section")
	}
	for _, k := range graph.Kinds {
		if !strings.Contains(KivaliOS, "`"+string(k)+"`") {
			t.Errorf("Kivali OS does not describe kind %q", k)
		}
	}
	for _, tool := range []string{GraphQueryToolName, GraphNodeToolName} {
		if !strings.Contains(KivaliOS, "`"+tool+"`") {
			t.Errorf("Kivali OS never names %s", tool)
		}
	}
	for _, status := range graph.DeclaredStatuses {
		if !strings.Contains(KivaliOS, "`"+string(status)+"`") {
			t.Errorf("Kivali OS does not describe status %q", status)
		}
	}
	// Every front-matter field the parser knows.
	for _, field := range []string{"id", "kind", "about", "status", "condition", "depends_on", "supersedes", "file", "check", "source", "evidence"} {
		if !strings.Contains(KivaliOS, field+":") && !strings.Contains(KivaliOS, "`"+field+"`") {
			t.Errorf("Kivali OS does not name front-matter field %q", field)
		}
	}
}

// The document carries the full assignment rules
// (docs/developers/assignments.md) under a section the tool
// descriptions point at by name.
func TestKivaliOSTeachesTheTracker(t *testing.T) {
	if !strings.Contains(KivaliOS, "\n### Assignments\n") {
		t.Fatal("Kivali OS has no assignments section")
	}
	for _, tool := range []string{AssignmentCreateToolName, AssignmentUpdateToolName, AssignmentCloseToolName, AssignmentReopenToolName, AssignmentListToolName, AssignmentViewToolName, "publish_notice"} {
		if !strings.Contains(KivaliOS, "`"+tool+"`") {
			t.Errorf("Kivali OS never names %s", tool)
		}
	}
}

// Every read-only subtree of /files/ and the shared workspace are
// named: an agent cannot use a subtree its system prompt never
// mentions, and one that believes /files/background/ is read-only
// never starts the fan-out procedure that depends on it. Driven off
// files.DefaultReadOnlyRoots so a new subtree fails here first.
func TestKivaliOSNamesEveryFilesSubtree(t *testing.T) {
	for _, root := range files.DefaultReadOnlyRoots {
		if !strings.Contains(KivaliOS, root) {
			t.Errorf("Kivali OS does not mention read-only root %q", root)
		}
	}
	if want := "/files/" + files.SharedWorkspaceDir + "/"; !strings.Contains(KivaliOS, want) {
		t.Errorf("Kivali OS does not mention the shared workspace %s", want)
	}
}

// Text elsewhere in the binary sends agents to these sections by
// name: the runtime notices ("When you're interrupted"), the subagent,
// memory and messaging tool descriptions, and the Chief of Staff's
// role. A renamed section would leave each of them pointing at
// nothing.
func TestKivaliOSHasTheSectionsOtherTextPointsAt(t *testing.T) {
	for _, h := range []string{
		"# Kivali OS",
		"## How the org works",
		"## How time works",
		"## Your memory and storage",
		"## The knowledge graph",
		"## Delegating with `subagent`",
		"## Breaking work down",
		"## How you communicate",
		"### Assignments",
		"### When you're interrupted",
		"## Owner interaction",
	} {
		if !strings.Contains(KivaliOS, "\n"+h+"\n") && !strings.HasPrefix(KivaliOS, h+"\n") {
			t.Errorf("Kivali OS has no heading %q", h)
		}
	}
	for _, want := range []string{"Keep your agent memory lean and load-bearing", "/files/past-chats/", "/files/episodes/"} {
		if !strings.Contains(KivaliOS, want) {
			t.Errorf("Kivali OS lost %q, which other text points at", want)
		}
	}
}

var ceoWord = regexp.MustCompile(`\bCEO\b`)

// Kivali OS is the same for every team and never carries a name: the
// person is "the owner", and the system prompt's owner section says
// what they are called. It does not say CEO (the tool names
// publish_ceo_* and the slug ceo aside) and teaches no literal markup
// marker, since the owner section names the current one.
func TestKivaliOSSaysTheOwner(t *testing.T) {
	if i := ceoWord.FindStringIndex(KivaliOS); i != nil {
		t.Errorf("Kivali OS says CEO: …%s…", KivaliOS[max(0, i[0]-60):min(len(KivaliOS), i[1]+40)])
	}
	if strings.Contains(KivaliOS, "`> ") {
		t.Error("Kivali OS teaches a literal markup marker")
	}
	rule := "Notes the owner\n  adds to a message appear under a line starting with `>` and their\n  name (your system prompt says what they are called)"
	if !strings.Contains(KivaliOS, rule) {
		t.Error("Kivali OS lost the markup rule that points at the system prompt")
	}
	for _, line := range strings.Split(KivaliOS, "\n") {
		if strings.Contains(line, "!") && !strings.Contains(line, "`") {
			t.Errorf("exclamation mark: %s", line)
		}
	}
}
