package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/convert"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/seed"
	"github.com/kivali-ai/kivali/internal/store"
)

// maxUploadBytes caps a single multipart upload. 200MB keeps the memory
// footprint bounded while allowing decks / research PDFs of reasonable size.
const maxUploadBytes = 200 << 20

// processUpload canonicalizes one file and returns its SHA. The
// caller uses the returned SHA to queue a Haiku summarization pass
// in the background. Empty SHA means "nothing to summarize" (no
// canonical text was produced — e.g. an image or a PDF whose text
// extraction failed).
func (s *Server) processUpload(ctx context.Context, fh *multipart.FileHeader, kind string) (string, error) {
	src, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer func() { _ = src.Close() }()
	pf, err := s.Store.AddProjectFile(fh.Filename, src)
	if err != nil {
		return "", err
	}
	if kind != "" {
		if err := s.Store.SetProjectFileKind(pf.SHA, kind); err != nil {
			return "", err
		}
	}
	ext := filepath.Ext(pf.OriginalName)
	origRelName := "original" + ext
	origPath := filepath.Join(s.Store.Root(), "project_files", pf.SHA, origRelName)

	res, err := convert.Canonicalize(ctx, origPath, ext, filepath.Dir(origPath))
	if err != nil {
		return "", err
	}
	if res.Name == "" {
		return "", nil
	}
	if res.UsesOriginal {
		if err := s.Store.SetCanonical(pf.SHA, origRelName, res.MIME); err != nil {
			return "", err
		}
		return pf.SHA, nil
	}
	if err := s.Store.SetCanonical(pf.SHA, res.Name, res.MIME); err != nil {
		return "", err
	}
	return pf.SHA, nil
}

// removeProjectFile deletes one project file, sweeps the now-dead
// symlink out of every agent's filesystem view, and reindexes the
// graph. Shared by DELETE /api/v1/org/files/{sha} and DELETE
// /api/v1/setup/files/{sha}.
func (s *Server) removeProjectFile(sha string) error {
	if err := s.Store.RemoveProjectFile(sha); err != nil {
		return err
	}
	s.afterProjectFilesChanged("project file delete")
	return nil
}

// defaultSeedInstruction is the closing instruction sent to CoS along
// with the handbook, role template, and project files.
const defaultSeedInstruction = `Read through the project files above, if there are any, and the handbook. Write down what they say about the business, the market, the product, the operating model, and anything else that will be load-bearing for you as Chief of Staff, drawing only on what they say. Where they say little or nothing, say so and note what you will ask the owner; never fill a gap with a guess. Write this as your initial agent memory — markdown, first-person where natural. It will be inlined into your system prompt on every future Claude call and evolves over time as your chats rotate.

Respond with only the agent memory content (no preamble, no meta-commentary).`

// personalSeedInstruction closes the seed call for a personal team,
// whose Chief of Staff works for one person rather than a business and
// may have no project files to read.
const personalSeedInstruction = `Read through the project files above, if there are any. Synthesize what you now understand about the person this team works for: who they are, who is in their life and home, what they want help with, what is off limits, and anything else that will be load-bearing for you as Chief of Staff. Where you know little yet, say so and note what you will ask them; never fill a gap with a guess. Write this as your initial agent memory — markdown, first-person where natural. It will be inlined into your system prompt on every future Claude call and evolves over time as your chats rotate.

Respond with only the agent memory content (no preamble, no meta-commentary).`

// personalFirstMessageLabel is the first-message stage in words for a
// personal team, whose Chief of Staff starts by asking questions.
const personalFirstMessageLabel = "Asking it to get to know you"

// workFirstMessageLabel is the first-message stage in words for a
// work team whose handbook is not drafted from the files: its Chief
// of Staff starts by asking about the business.
const workFirstMessageLabel = "Asking it to get to know your business"

// cosSlug is the Chief of Staff's slug: the agent setup hires, and
// whose existence means setup is done.
const cosSlug = "chief-of-staff"

// cosExists reports whether the Chief of Staff has been hired.
func (s *Server) cosExists() bool {
	_, err := s.Store.GetAgent(cosSlug)
	return err == nil
}

// EnsureKivaliDeveloper hires the Kivali Developer under the Chief of
// Staff, with Kivali's role, when the team has a Chief of Staff and no
// Kivali Developer: setup calls it right after the Chief of Staff, and
// boot and a restore call it for a team that has none. created is
// false when there was nothing to do. It provisions no pod: setup
// provisions the Kivali Developer's with the Chief of Staff's, and
// BulkProvisionAgentPods every agent's at boot and after a restore.
func (s *Server) EnsureKivaliDeveloper() (created bool, err error) {
	if !s.cosExists() {
		return false, nil
	}
	_, err = s.Store.GetAgent(agent.KivaliDeveloperSlug)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	if err := s.Store.CreateAgent(store.Agent{
		Slug:      agent.KivaliDeveloperSlug,
		Role:      "Kivali Developer",
		Icon:      "code",
		ReportsTo: cosSlug,
		Model:     s.AgentModel,
	}, seed.KivaliDeveloperRole); err != nil {
		return false, err
	}
	s.NotifyOrgState()
	return true, nil
}

// RefreshKivaliRoles writes Kivali's role (seed.KivaliRole) as the
// role.md of the Chief of Staff and the Kivali Developer wherever it
// differs, so both run on the role this binary ships for the team's
// kind. Boot and a restore call it after EnsureKivaliDeveloper, and a
// change of the team's kind does. rewritten names the agents whose role
// it wrote; an agent the team does not have yet is skipped. An error
// for one agent does not stop the other.
func (s *Server) RefreshKivaliRoles() (rewritten []string, err error) {
	kind := s.teamKind()
	var errs []error
	for _, slug := range seed.KivaliRoleSlugs {
		role, _ := seed.KivaliRole(slug, kind)
		if _, gerr := s.Store.GetAgent(slug); gerr != nil {
			if !errors.Is(gerr, store.ErrNotFound) {
				errs = append(errs, fmt.Errorf("%s: %w", slug, gerr))
			}
			continue
		}
		if current, rerr := s.Store.ReadRole(slug); rerr == nil && current == role {
			continue
		}
		if werr := s.Store.WriteRole(slug, role); werr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", slug, werr))
			continue
		}
		rewritten = append(rewritten, slug)
	}
	return rewritten, errors.Join(errs...)
}

// The Chief of Staff seed runs in these stages, in order; the hiring
// panel shows one line per stage. seedStageFirstMessage runs when the
// seed carries a first message, which every setup path does: the
// label below is the files path's, and the other paths replace it.
const (
	seedStageRead         = iota // read the handbook and every project file's text
	seedStageBrief               // the one long model call: the Chief of Staff writes its memory
	seedStageHire                // store the handbook, the CEO and the Chief of Staff, its role and memory, and the Kivali Developer
	seedStageFirstMessage        // deliver the first message and wake the Chief of Staff
)

// seedStageLabels are the stages in words, for the hiring panel.
var seedStageLabels = [...]string{
	seedStageRead:         "Reading your files",
	seedStageBrief:        "Writing its first briefing",
	seedStageHire:         "Hiring",
	seedStageFirstMessage: "Asking it to draft the handbook",
}

// cosSeed is one Chief of Staff hire.
type cosSeed struct {
	// Handbook is the text the seed call runs under.
	Handbook string
	// WriteHandbook stores Handbook as the org's handbook
	// when the hire lands; false when it is already the stored one.
	WriteHandbook bool
	// Role becomes the Chief of Staff's role.md.
	Role string
	// Instruction closes the seed call; the reply becomes the Chief of
	// Staff's memory.
	Instruction string
	// FirstMessage, when set, is delivered to the new Chief of Staff as
	// the CEO's first message, and wakes it.
	FirstMessage string
	// FirstMessageLabel, when set, replaces the first-message stage's
	// label in the hiring panel.
	FirstMessageLabel string
}

// stageLabels are the labels of the stages this seed runs.
func (c cosSeed) stageLabels() []string {
	n := seedStageFirstMessage
	if c.FirstMessage != "" {
		n++
	}
	labels := append([]string(nil), seedStageLabels[:n]...)
	if c.FirstMessage != "" && c.FirstMessageLabel != "" {
		labels[seedStageFirstMessage] = c.FirstMessageLabel
	}
	return labels
}

// seedFailure is a seed that stopped at Stage. Status is what the form
// handler answers with; Error() keeps the form's wording. A failed
// briefing call also carries the provider's failure kind
// (CompleteResponse.Failure) and the label of the model it ran on.
type seedFailure struct {
	Stage   int
	Status  int
	Msg     string
	Err     error
	Failure string
	Model   string
}

func (f *seedFailure) Error() string {
	if f.Err == nil {
		return f.Msg
	}
	return f.Msg + ": " + f.Err.Error()
}

func (f *seedFailure) Unwrap() error { return f.Err }

// seedCoS hires the Chief of Staff: one model call over the
// handbook, the role and every project file writes its memory,
// then the agent is created with that memory, and, when the seed
// carries one, its first message is delivered. stage is called as each
// stage starts. Shared by the setup form (synchronous) and POST
// /api/v1/setup/seed-cos (in the background). The caller has checked
// that a model is connected and no Chief of Staff exists.
func (s *Server) seedCoS(ctx context.Context, c cosSeed, stage func(int)) error {
	stage(seedStageRead)
	req, err := s.buildCoSSeedRequest(ctx, c.Handbook, c.Role, c.Instruction)
	if err != nil {
		return &seedFailure{Stage: seedStageRead, Status: http.StatusInternalServerError, Msg: "could not read the project files", Err: err}
	}

	stage(seedStageBrief)
	resp, err := s.Claude.Complete(ctx, req)
	// briefFailed carries the provider's account of a failed call: its
	// text joins the error, so the log has it, and its kind picks the
	// words the hiring panel shows.
	briefFailed := func(msg string, err error) error {
		f := &seedFailure{Stage: seedStageBrief, Status: http.StatusBadGateway, Msg: msg, Err: err,
			Model: provider.Label(s.Provider, req.Model)}
		if resp != nil {
			f.Failure = resp.Failure
			if resp.Error != "" {
				f.Err = errors.Join(err, errors.New(resp.Error))
			}
		}
		return f
	}
	if err != nil {
		return briefFailed("seed call failed", err)
	}
	// A reply that lands after the hire was cancelled (its deadline
	// passed, or the form's request ended) is not used: the hire stops
	// here as failed rather than hiring past its deadline.
	if err := context.Cause(ctx); err != nil {
		return &seedFailure{Stage: seedStageBrief, Status: http.StatusGatewayTimeout, Msg: "seed call ran past its deadline", Err: err}
	}
	memory := concatText(resp.Content)
	if memory == "" {
		return briefFailed("seed call produced no text", nil)
	}

	stage(seedStageHire)
	hireFailed := func(err error) error {
		return &seedFailure{Stage: seedStageHire, Status: http.StatusInternalServerError, Msg: "could not save the chief of staff", Err: err}
	}
	if c.WriteHandbook {
		if err := s.Store.WriteHandbook(c.Handbook); err != nil {
			return hireFailed(err)
		}
	}
	if _, err := s.Store.GetAgent(agent.CEOSlug); err != nil {
		_ = s.Store.CreateAgent(store.Agent{Slug: agent.CEOSlug, Role: "CEO"}, "")
	}
	if err := s.Store.CreateAgent(store.Agent{
		Slug:      cosSlug,
		Role:      "Chief of Staff",
		Icon:      "compass",
		ReportsTo: agent.CEOSlug,
		Model:     s.AgentModel,
	}, c.Role); err != nil {
		return hireFailed(err)
	}
	if err := s.Store.WriteAgentMemory(cosSlug, memory); err != nil {
		return hireFailed(err)
	}
	// The Kivali Developer, hired with the team under the Chief of
	// Staff. A failure is logged, not fatal: the Chief of Staff is
	// hired, so setup is done, and the next boot hires it.
	developer, err := s.EnsureKivaliDeveloper()
	if err != nil {
		log.Printf("setup: hire the Kivali Developer: %v", err)
	}
	// The team's starting auto-release. Non-fatal: without it the Queue
	// holds everything, which is safe, and the slider still works.
	if err := s.Store.SeedAutoRelease(store.AutoRelease{Enabled: true, Delay: newTeamAutoRelease, Since: s.clk().Now()}); err != nil {
		log.Printf("setup: seed auto-release: %v", err)
	} else {
		s.NotifyAutoRelease()
	}
	// Their pods, as every other hire gets one (ApplyHire, and the boot's
	// BulkProvisionAgentPods): without it the first turn has nowhere to
	// run and is dropped. Through ProvisionAgentPod, which syncs the
	// agent's files first and takes the agent's provision lock. A
	// failure is logged, not fatal: the next boot provisions it.
	if s.AgentPod != nil {
		slugs := []string{cosSlug}
		if developer {
			slugs = append(slugs, agent.KivaliDeveloperSlug)
		}
		for _, slug := range slugs {
			if err := s.ProvisionAgentPod(ctx, slug); err != nil {
				log.Printf("agentpod: provision %s: %v", slug, err)
			} else {
				log.Printf("agentpod: provisioned %s", slug)
			}
		}
	}
	// Persist the seed prompt + raw response under .seed/ for post-hoc
	// debugging. Non-fatal: a failure here is a UX / diagnostics issue,
	// not a correctness issue — the CoS is already provisioned by the
	// time we reach this. Log but don't abort.
	if err := s.writeSeedTrace(req, resp); err != nil {
		fmt.Printf("seed trace write: %v\n", err)
	}
	s.NotifyOrgState()

	if c.FirstMessage == "" {
		return nil
	}
	stage(seedStageFirstMessage)
	if err := s.deliverToAgent(cosSlug, store.ChatMessage{
		Role:    store.RoleReceived,
		Content: c.FirstMessage,
		Kind:    "direct_chat",
		TS:      s.clk().Now().UTC(),
	}); err != nil {
		return &seedFailure{Stage: seedStageFirstMessage, Status: http.StatusInternalServerError, Msg: "could not send the chief of staff its first message", Err: err}
	}
	// "chat": the message stands in for the CEO's own first message. The
	// pod was only just created: the turn starts once it has connected.
	go s.spawnWhenSubscribed(cosSlug, "chat")
	return nil
}

// writeSeedTrace persists the exact prompt sent to Claude and the raw
// response to agents/chief-of-staff/.seed/. Dot-prefix keeps it out
// of directory listings + MCP resource enumeration. Markdown-formatted
// so an operator can cat or grep without a viewer.
func (s *Server) writeSeedTrace(req provider.CompleteRequest, resp *provider.CompleteResponse) error {
	dir := filepath.Join(s.Store.Root(), "agents", "chief-of-staff", ".seed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(renderSeedPrompt(req, ts)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "response.md"), []byte(renderSeedResponse(resp, ts)), 0o644); err != nil {
		return err
	}
	return nil
}

// renderSeedPrompt formats the Complete request as a readable
// markdown transcript — one section per system block, then the
// joined user-side content blocks.
func renderSeedPrompt(req provider.CompleteRequest, ts string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# CoS seed — prompt\n\n")
	fmt.Fprintf(&sb, "- generated: %s\n", ts)
	fmt.Fprintf(&sb, "- model: %s\n", req.Model)
	fmt.Fprintf(&sb, "- max_tokens: %d\n\n", req.MaxTokens)
	for i, sysb := range req.System {
		fmt.Fprintf(&sb, "## System block %d\n\n%s\n\n", i+1, sysb.Text)
	}
	for i, m := range req.Messages {
		fmt.Fprintf(&sb, "## Message %d (%s)\n\n", i+1, m.Role)
		for _, c := range m.Content {
			if c.Type == provider.ContentText {
				sb.WriteString(c.Text)
				sb.WriteString("\n\n")
			}
		}
	}
	return sb.String()
}

// renderSeedResponse formats the raw response text — all we care
// about at this callsite is the text; tool-use blocks don't show up
// in a seed call.
func renderSeedResponse(resp *provider.CompleteResponse, ts string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# CoS seed — response\n\n- generated: %s\n\n", ts)
	for _, c := range resp.Content {
		if c.Type == provider.ContentText {
			sb.WriteString(c.Text)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// buildCoSSeedRequest composes the seed Claude call. All files with a
// text canonical are inlined as text content blocks; files without one
// (images, unclassified) are named only.
func (s *Server) buildCoSSeedRequest(ctx context.Context, handbook, roleTemplate, instruction string) (provider.CompleteRequest, error) {
	files, err := s.Store.ListProjectFiles()
	if err != nil {
		return provider.CompleteRequest{}, err
	}
	model := s.AgentModel
	if model == "" {
		model = s.Provider.Defaults().Agent
	}
	req := provider.CompleteRequest{
		Model:     model,
		MaxTokens: 8192,
		Purpose:   "seed",
		Agent:     "chief-of-staff",
		// The same opening as every turn's prompt: how the runtime
		// works, then the team's handbook, then the role the Chief of
		// Staff is about to have.
		System: []provider.SystemBlock{
			{Text: agent.KivaliOS, Cache: true},
			{Text: handbook, Cache: true},
			{Text: roleTemplate, Cache: true},
		},
	}

	var content []provider.ContentBlock
	var skipped []string
	for _, f := range files {
		text, err := s.Store.ReadCanonicalText(ctx, f.SHA)
		if err != nil || text == "" {
			skipped = append(skipped, f.OriginalName)
			continue
		}
		content = append(content, provider.ContentBlock{
			Type: provider.ContentText,
			Text: fmt.Sprintf("## Project file: %s\n\n%s", f.OriginalName, strings.TrimSpace(text)),
		})
	}
	if len(skipped) > 0 {
		content = append(content, provider.ContentBlock{
			Type: provider.ContentText,
			Text: "## Files without extractable text\n\nThese files are uploaded but have no text form available (images / unknown types):\n- " + strings.Join(skipped, "\n- "),
		})
	}
	content = append(content, provider.ContentBlock{
		Type: provider.ContentText,
		Text: instruction,
	})
	req.Messages = []provider.Message{{Role: provider.RoleUser, Content: content}}
	return req, nil
}

func concatText(blocks []provider.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == provider.ContentText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
