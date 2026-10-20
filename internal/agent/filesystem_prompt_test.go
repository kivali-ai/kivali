package agent

import (
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// The agent learns the shape of /files/ from Kivali OS (kivali_os_test.go)
// and from the file tools' descriptions. Both ship in the binary, so a
// subtree missing from them is invisible to a running agent. These
// tests are driven off files.DefaultReadOnlyRoots rather than a
// hardcoded list, so the next read-only subtree added to the
// filesystem cannot be forgotten here: adding it to that slice is what
// makes these fail until the prompt text catches up.

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
	for _, tool := range FilesystemTools() {
		switch tool.Name {
		case files.ToolCreate, files.ToolDelete, files.ToolRename, files.ToolCopy:
			if !strings.Contains(tool.Description, want) {
				t.Errorf("%s description does not name %s as writable; the agent is told a writable set that omits it", tool.Name, want)
			}
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

// The identity block: habits ride above semantic memory on every call,
// and the precedence line is what keeps a self-written habit from
// reading with the same authority as the role above it, and the role
// from reading with the authority of Kivali OS above that.
func TestPersonaIdentityRendersHabitsAboveMemory(t *testing.T) {
	out := renderPersonaIdentity(store.Agent{Slug: "alice", Role: "Engineer"}, "role text", "P: ship small. Why: smaller diffs get reviewed.", "M: the build is green.")
	pIdx := strings.Index(out, "## Habits")
	mIdx := strings.Index(out, "Agent memory")
	switch {
	case pIdx < 0:
		t.Fatalf("identity block has no Habits section:\n%s", out)
	case mIdx < 0:
		t.Fatalf("identity block has no Agent memory section:\n%s", out)
	case pIdx > mIdx:
		t.Errorf("habits render below agent memory; precedence is habits over memory")
	}
	if !strings.Contains(out, "Precedence: Kivali OS") {
		t.Errorf("identity block omits the precedence line, or it no longer starts from Kivali OS:\n%s", out)
	}
	if !strings.Contains(out, "ship small") || !strings.Contains(out, "the build is green") {
		t.Errorf("identity block dropped one of the two memory tiers:\n%s", out)
	}
}
