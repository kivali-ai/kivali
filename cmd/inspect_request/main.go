// inspect_request prints the full Claude request body that BuildRequest
// would produce for a given agent, given a /data dir to read from.
//
//	go run ./cmd/inspect_request -data /tmp/kivali-data -agent chief-of-staff > /tmp/req.json
//
// The output is the assembled request — system prompt layers, tool
// schemas and chat history — for inspecting what an agent's prompt
// contains.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/claudeagent"
	"github.com/kivali-ai/kivali/internal/store"
)

func main() {
	dataDir := flag.String("data", "/data", "path to the Kivali data dir")
	slug := flag.String("agent", "chief-of-staff", "agent slug to build the request for")
	flag.Parse()

	s, err := store.New(*dataDir)
	if err != nil {
		die("store.New: %v", err)
	}
	a, err := s.GetAgent(*slug)
	if err != nil {
		die("GetAgent %s: %v", *slug, err)
	}
	handbook, _ := s.ReadHandbook()
	actives, _ := s.ListActiveAgents()
	files, _ := s.ListProjectFiles()
	skills, _ := s.ListSkills()
	role, _ := s.ReadRole(*slug)
	mem, _ := s.ReadAgentMemory(*slug)
	principles, _ := s.ReadAgentHabits(*slug)
	hist, _ := s.ReadChatHistory(*slug)
	br, _ := s.ReadBranding()

	ctx := agent.Context{
		Agent:                  a,
		IsChiefOfStaff:         a.Slug == "chief-of-staff",
		Handbook:               handbook,
		Owner:                  br.Owner(),
		OrgChart:               agent.OrgChartMarkdown(actives, br.Owner().Label()),
		ProjectFiles:           files,
		Skills:                 skills,
		Role:                   role,
		AgentMemory:            mem,
		AgentHabits:            principles,
		ChatHistory:            hist,
		FileText:               agent.FileTextFn(context.Background(), s),
		Attachments:            s,
		IncludeFilesystemTools: true,
		FilesystemAvailable:    true,
		Model:                  claudeagent.DefaultAgentModel,
		MaxTokens:              32768,
	}
	req := ctx.BuildRequest()

	b, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		die("marshal: %v", err)
	}
	fmt.Printf("# meta\nmessages: %d\nsystem blocks: %d\ntools: %d\n\n",
		len(req.Messages), len(req.System), len(req.Tools))
	fmt.Println("# full body:")
	fmt.Println(string(b))
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
