package agent

import (
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/store"
)

// The agent learns the shape of /files/ from two places that ship in
// the binary and therefore reach every deployed install immediately:
// the per-call filesystem section in the system prompt, and the
// file_create tool description. The handbook describes the tree
// too, but it is a SEEDED file — an install set up before a new
// subtree existed keeps its old copy until someone splices it. So
// these two are the only surfaces that can be relied on to be current,
// and a subtree missing from them is invisible to a running agent.
//
// Both tests are driven off files.DefaultReadOnlyRoots rather than a
// hardcoded list, so the next read-only subtree added to the
// filesystem cannot be forgotten here: adding it to that slice is what
// makes these fail until the prompt text catches up.

func TestFilesystemSectionNamesEveryReadOnlyRoot(t *testing.T) {
	section := Context{}.renderFilesystemSection()
	for _, root := range files.DefaultReadOnlyRoots {
		if !strings.Contains(section, root) {
			t.Errorf("renderFilesystemSection does not mention read-only root %q; an agent cannot use a subtree its system prompt never names", root)
		}
	}
}

func TestFileCreateDescriptionNamesEveryReadOnlyRoot(t *testing.T) {
	var desc string
	for _, tool := range FilesystemTools() {
		if tool.Name == files.ToolCreate {
			desc = tool.Description
			break
		}
	}
	if desc == "" {
		t.Fatalf("no %s tool found in FilesystemTools()", files.ToolCreate)
	}
	for _, root := range files.DefaultReadOnlyRoots {
		if !strings.Contains(desc, root) {
			t.Errorf("%s description does not list read-only root %q; the agent is told a read-only set that omits a subtree its writes will be rejected from", files.ToolCreate, root)
		}
	}
}

// The shared workspace is the one writable subtree outside artifacts/,
// and it is writable by OMISSION from DefaultReadOnlyRoots — so no
// list drives it into the prompt text the way the read-only roots are
// driven in above. Left unnamed, the file tools told an agent that the
// two artifact roots were the only places it could write, and an
// agent that believes /files/background/ is read-only never starts the
// fan-out procedure that depends on it. Every surface that names the
// writable set has to name this one too.
func TestFilesystemSurfacesNameTheSharedWorkspace(t *testing.T) {
	want := "/files/" + files.SharedWorkspaceDir + "/"
	section := Context{}.renderFilesystemSection()
	if !strings.Contains(section, want) {
		t.Errorf("renderFilesystemSection does not mention the shared workspace %s", want)
	}
	for _, tool := range FilesystemTools() {
		switch tool.Name {
		case files.ToolCreate, files.ToolDelete, files.ToolRename, files.ToolCopy:
			if !strings.Contains(tool.Description, want) {
				t.Errorf("%s description does not name %s as writable; the agent is told a writable set that omits it", tool.Name, want)
			}
		}
	}
}

// The graph paragraph is the third thing the filesystem section must
// carry: it is the only in-binary surface that tells an agent its
// public files are nodes, what kinds exist, and which tools read them.
// Kinds come from graph.Kinds so the text cannot fall behind the list.
func TestFilesystemSectionNamesEveryGraphKindAndBothTools(t *testing.T) {
	section := Context{}.renderFilesystemSection()
	for _, k := range graph.Kinds {
		if !strings.Contains(section, string(k)) {
			t.Errorf("renderFilesystemSection does not name graph kind %q", k)
		}
	}
	for _, tool := range []string{GraphQueryToolName, GraphNodeToolName} {
		if !strings.Contains(section, tool) {
			t.Errorf("renderFilesystemSection does not name %s", tool)
		}
	}
	// Every front-matter field the parser knows, since on an unspliced
	// install this paragraph is the only place an agent learns them.
	for _, field := range []string{"id", "kind", "about", "status", "condition", "depends_on", "supersedes", "file", "check", "source", "evidence", "summary"} {
		if !strings.Contains(section, field) {
			t.Errorf("renderFilesystemSection does not name front-matter field %q", field)
		}
	}
}

func TestFileCreateDescriptionMentionsTheGraph(t *testing.T) {
	for _, tool := range FilesystemTools() {
		if tool.Name != files.ToolCreate {
			continue
		}
		if !strings.Contains(tool.Description, "knowledge-graph node") {
			t.Errorf("%s description should say a public markdown file with front matter is a graph node", files.ToolCreate)
		}
	}
}

// The identity block is the other half: principles ride above semantic
// memory on every call, and the precedence line is what keeps a
// self-written principle from reading with the same authority as the
// role above it.
func TestPersonaIdentityRendersPrinciplesAboveMemory(t *testing.T) {
	out := renderPersonaIdentity(store.Agent{Slug: "alice", Role: "Engineer"}, "role text", "P: ship small. Why: smaller diffs get reviewed.", "M: the build is green.")
	pIdx := strings.Index(out, "## Habits")
	mIdx := strings.Index(out, "Agent memory")
	switch {
	case pIdx < 0:
		t.Fatalf("identity block has no Habits section:\n%s", out)
	case mIdx < 0:
		t.Fatalf("identity block has no Agent memory section:\n%s", out)
	case pIdx > mIdx:
		t.Errorf("principles render below agent memory; precedence is principles over memory")
	}
	if !strings.Contains(out, "Precedence:") {
		t.Errorf("identity block omits the precedence line:\n%s", out)
	}
	if !strings.Contains(out, "ship small") || !strings.Contains(out, "the build is green") {
		t.Errorf("identity block dropped one of the two memory tiers:\n%s", out)
	}
}
