package internal_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// What agents read uses one vocabulary. Agent-facing text says an
// assignment; parts and "part of"; habits; the background workspace;
// offboarded; needs help; the handbook. Text every team's agents share
// calls the person the owner, and so does the seed text (checked in
// internal/seed). The person's chosen name reaches agents only through
// the system prompt's owner section (internal/owner).
//
// This test holds the agent-facing prose to it by rejecting
// bannedWords and ceoWord: every line of the seeded markdown and the
// built-in skills, and every string literal in the Go files that write
// tool descriptions, prompts and model-visible replies. Comments and
// identifiers are not agent-facing and are not checked.
//
// The same check works with grep, minus the Go-literal parsing:
//
//	grep -rn -i -E 'issue|principle|initiative|\bchild|children|\bfired\b|errored|constitution' internal/seed internal/builtinskills
func TestAgentFacingProseUsesTheVocabulary(t *testing.T) {
	var failures []string
	for _, file := range globAll(t, "seed/*.md", "builtinskills/skills/*/SKILL.md") {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(body), "\n") {
			if w := bannedWord(line); w != "" {
				failures = append(failures, file+":"+strconv.Itoa(n+1)+": "+w+": "+line)
			}
			if w := ceoWord.FindString(line); w != "" {
				failures = append(failures, file+":"+strconv.Itoa(n+1)+": "+w+" (say the owner): "+line)
			}
		}
	}
	for _, file := range globAll(t,
		"agent/agent.go", "agent/*_tool.go", "agent/*_tools.go", "agent/wake_update.go", "agent/inbox.go",
		"mcp/*.go", "web/handoff.go", "web/agentpod_socket.go", "web/ceo.go",
		"agentpod/subagent_spec.go", "message/schemas.go", "message/parse.go",
		"assignments/*.go", "tracker/*.go",
		"agent/chat.go", "agent/provisioning.go", "message/message.go", "messaging/*.go",
		"web/api_setup.go", "web/api_org.go", "web/api_home.go", "web/api_proposals.go",
		"web/onboarding.go", "web/chat.go", "store/handbook.go", "store/message.go",
		"store/graph_index.go", "web/episode_writer.go",
	) {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || allowedLiterals[s] {
				return true
			}
			for _, line := range strings.Split(s, "\n") {
				if w := bannedWord(line); w != "" {
					failures = append(failures, fset.Position(lit.Pos()).String()+": "+w+": "+line)
				}
				if w := ceoWord.FindString(line); w != "" {
					failures = append(failures, fset.Position(lit.Pos()).String()+": "+w+" (say the owner): "+line)
				}
			}
			return true
		})
	}
	for _, f := range failures {
		t.Error(f)
	}
}

// allowedLiterals are Go string literals that may carry a checked word.
var allowedLiterals = map[string]bool{
	// The ceo pseudo-agent's stored role, which no agent or page shows
	// (the org chart and the graph name the person themselves).
	"CEO": true,
}

// ceoWord is the word shared agent text must not use for the person.
var ceoWord = regexp.MustCompile(`\bCEO\b`)

var constitutionWord = regexp.MustCompile(`(?i)constitution`)

// bannedWords are the words agent-facing text does not use; each has
// its word in the vocabulary above.
var bannedWords = regexp.MustCompile(`(?i)\bissues?\b|\bissue_|operating principles?|agent_memory_principles|\binitiatives?\b|\bfired\b|\berrored\b|\bchild(ren)?\b|constitution`)

// bannedWord returns the first banned word on line, or "".
func bannedWord(line string) string {
	return bannedWords.FindString(line)
}

func globAll(t *testing.T, patterns ...string) []string {
	t.Helper()
	var out []string
	for _, p := range patterns {
		m, err := filepath.Glob(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(m) == 0 {
			t.Fatalf("%s matches nothing; the guard would check nothing there", p)
		}
		out = append(out, m...)
	}
	return out
}

// TestWebAppSaysHandbook: the web app's source says handbook too.
func TestWebAppSaysHandbook(t *testing.T) {
	err := filepath.WalkDir(filepath.Join("..", "web", "src"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || (!strings.HasSuffix(p, ".ts") && !strings.HasSuffix(p, ".tsx")) {
			return err
		}
		if strings.Contains(p, ".test.") {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for n, line := range strings.Split(string(body), "\n") {
			if constitutionWord.MatchString(line) {
				t.Errorf("%s:%d: %s", p, n+1, line)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
