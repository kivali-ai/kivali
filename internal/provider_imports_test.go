package internal_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The provider boundary, as an import rule (docs/developers/context-serialization.md
// "The provider seam"):
//
//   - internal/provider is the seam and holds only provider-neutral
//     things, so it imports nothing from this module.
//   - internal/claudeagent is the one driver. Nothing else under
//     internal/ imports it or any package below it: common code
//     reaches it only as a provider.Client and a provider.Provider,
//     handed in by a composition root.
//   - Outside internal/, only the files listed in claudeagentImporters
//     may import it. Adding one is a decision, made here.
//
// Every .go file is checked, tests and build-tagged files included,
// since go/parser reads imports without evaluating build constraints.

const module = "github.com/kivali-ai/kivali"

// claudeagentImporters are the files outside internal/claudeagent that
// may import it, by path from the repository root (a directory entry
// covers every file in it).
var claudeagentImporters = map[string]string{
	// The composition roots: each builds the one driver value and
	// hands it to whatever needs a Client or a Provider.
	"main.go":      "the server's composition root",
	"agent_cmd.go": "the agent pod's composition root",
	// `kivali mcp`: serves the agent's tools and makes no model call;
	// it asks the driver, as a Provider, for the subagent tool schema's
	// models and efforts.
	"mcp_cmd.go": "the MCP server's composition root",
	// The desktop supervisor: hands the catalog's model ids to the
	// sign-in setup's per-model check.
	"cmd/kivali-supervisor/main.go": "the supervisor's composition root: the model ids a sign-in setup checks",
	// Dev tools that stand in for or inspect the Claude CLI itself.
	"cmd/fake-claude":     "the fake Claude CLI e2e tests run against: it accepts the ids the driver's catalog resolves",
	"cmd/devseed":         "the demo seed: Claude ids, efforts and prices for a realistic org",
	"cmd/inspect_request": "dumps the request the driver would send",
	// Prompt evals call the real model; build tag prompteval, never in
	// the binary or the default test run.
	"internal/web/memory_eval_test.go": "prompt evals against the real model (build tag prompteval)",
}

func TestProviderImportRule(t *testing.T) {
	root := ".."
	var failures []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		for _, imp := range fileImports(t, path) {
			if f := checkImport(rel, imp); f != "" {
				failures = append(failures, f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(failures)
	for _, f := range failures {
		t.Error(f)
	}
}

// checkImport is the rule for one import of one file; "" when allowed.
func checkImport(rel, imp string) string {
	const (
		seam   = module + "/internal/provider"
		driver = module + "/internal/claudeagent"
	)
	switch {
	case strings.HasPrefix(rel, "internal/provider/") && (imp == module || strings.HasPrefix(imp, module+"/")):
		return rel + " imports " + imp + ": internal/provider imports nothing from the module"
	case (imp != driver && !strings.HasPrefix(imp, driver+"/")) || strings.HasPrefix(rel, "internal/claudeagent/"):
		return ""
	case allowedImporter(rel):
		return ""
	case strings.HasPrefix(rel, "internal/"):
		return rel + " imports internal/claudeagent (or a sub-package): common code takes a provider.Client and a provider.Provider from a composition root"
	default:
		return rel + " imports internal/claudeagent but is not in claudeagentImporters"
	}
}

func allowedImporter(rel string) bool {
	if _, ok := claudeagentImporters[rel]; ok {
		return true
	}
	for p := range claudeagentImporters {
		if !strings.HasSuffix(p, ".go") && strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

func fileImports(t *testing.T, path string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := make([]string, 0, len(f.Imports))
	for _, s := range f.Imports {
		p, err := strconv.Unquote(s.Path.Value)
		if err != nil {
			t.Fatalf("%s: import %s: %v", path, s.Path.Value, err)
		}
		out = append(out, p)
	}
	return out
}

// The rule itself: what it refuses and what it lets through.
func TestProviderImportRuleCases(t *testing.T) {
	driver := module + "/internal/claudeagent"
	refused := []struct{ file, imp string }{
		{"internal/web/models.go", driver},
		{"internal/web/models_test.go", driver},
		{"internal/agentpod/runtime.go", driver},
		{"internal/web/models.go", driver + "/cli"}, // a sub-package of the driver
		{"cmd/new-tool/main.go", driver + "/cli"},
		{"cmd/new-tool/main.go", driver},
		{"internal/provider/model.go", module + "/internal/store"},
	}
	for _, c := range refused {
		if checkImport(c.file, c.imp) == "" {
			t.Errorf("%s importing %s was allowed", c.file, c.imp)
		}
	}
	allowed := []struct{ file, imp string }{
		{"main.go", driver},
		{"agent_cmd.go", driver},
		{"cmd/fake-claude/engine_test.go", driver},
		{"main.go", driver + "/cli"},
		{"internal/claudeagent/runner.go", driver},
		{"internal/claudeagent/provider.go", module + "/internal/provider"},
		{"internal/web/models.go", module + "/internal/provider"},
		{"internal/provider/model.go", "context"},
	}
	for _, c := range allowed {
		if f := checkImport(c.file, c.imp); f != "" {
			t.Errorf("refused: %s", f)
		}
	}
}
