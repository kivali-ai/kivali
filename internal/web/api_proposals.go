package web

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The proposal page's JSON. A proposal is a ceo_approval_request that
// carries a change to the org (proposalKind); it is answered through
// POST /api/v1/needs/approve and deny like any other request, and
// resolved.result here is that answer's own sentence (proposalOutcome),
// so the page reads the same before and after a reload.

// wireAPIProposalRoutes registers the proposal page's route on the API
// mux. The path is the request message's path, as Needs you's
// review_path names it after /proposals/.
func (s *Server) wireAPIProposalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/proposals/{path...}", s.handleAPIProposal)
}

// offboardSentence is said beside an offboard's Approve button.
const offboardSentence = "Its files move to the archive. Nothing is deleted, and you can restore it later."

func (s *Server) handleAPIProposal(w http.ResponseWriter, r *http.Request) {
	notFound := func() {
		writeAPIError(w, http.StatusNotFound, "there is no proposal at that address", whoNoOne)
	}
	// A path that cannot name a message (outside messages/, or
	// climbing out of it) is answered like one that names nothing.
	reqPath, ok := cleanMessagePath(r.PathValue("path"))
	if !ok {
		notFound()
		return
	}
	req, err := s.Store.ReadMessage(filepath.Join(s.Store.Root(), reqPath))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound()
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "the proposal could not be read", whoServer)
		return
	}
	kind, isProposal := proposalKind(req)
	if req.Type != store.MsgCEOApprovalRequest || !isProposal {
		// A plain approval is answered on Home, not here.
		notFound()
		return
	}
	resp, err := s.ceoResponseTo(reqPath)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the answer to this proposal could not be read", whoServer)
		return
	}
	writeJSON(w, http.StatusOK, s.proposal(reqPath, kind, req, resp))
}

// ceoResponseTo is the CEO's answer to the request at reqPath, or nil
// while it waits. Paired the way buildCEOInbox pairs them: the CEO's
// ceo_reply chat entries, the newest answer winning.
func (s *Server) ceoResponseTo(reqPath string) (*store.Message, error) {
	hist, err := s.Store.ReadChatHistory(agent.CEOSlug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	for i := len(hist) - 1; i >= 0; i-- {
		e := hist[i]
		if e.Kind != "ceo_reply" || e.MessageRef == "" {
			continue
		}
		if e.ReplyToMessageRef != "" && e.ReplyToMessageRef != reqPath {
			continue
		}
		resp, err := s.Store.ReadMessage(filepath.Join(s.Store.Root(), e.MessageRef))
		if err != nil || resp.InReplyTo != reqPath {
			continue
		}
		return &resp, nil
	}
	return nil, nil
}

// proposal assembles the page for one proposal request and, when it
// has been answered, its response.
func (s *Server) proposal(reqPath string, kind apitypes.ProposalKind, req store.Message, resp *store.Message) apitypes.Proposal {
	v := s.newHomeView()
	v.addArchivedNames()
	p := apitypes.Proposal{
		Path:        reqPath,
		Kind:        kind,
		Title:       req.Title,
		Proposer:    v.person(req.From),
		ProposedAt:  req.Date,
		ReasonMD:    strings.TrimRight(req.Body, "\n"),
		Attachments: v.attachments(req.Attachments),
		Summary: apitypes.ProposalSummary{
			Moves: []apitypes.ProposalMove{},
			Facts: []apitypes.ProposalFact{},
		},
		Docs: []apitypes.ProposalDoc{},
	}
	approved := resp != nil && resp.Approved != nil && *resp.Approved
	if resp != nil {
		out := v.proposalOutcome(ceoResponse{Request: req, Response: *resp})
		p.Resolved = &apitypes.ProposalResolution{
			Approved: approved,
			At:       resp.Date,
			Result:   out.Result,
			Link:     out.Link,
		}
	}

	switch kind {
	case apitypes.ProposalKindHire:
		h := req.Hire
		a := apitypes.ProposalAgent{
			Slug:      h.Slug,
			Name:      cmp.Or(h.Role, h.Slug),
			RoleTitle: h.Role,
			Icon:      h.Icon,
			ReportsTo: v.person(cmp.Or(h.ReportsTo, agent.CEOSlug)),
			Model:     s.proposalHireModel(),
			Effort:    s.effectiveEffort(s.proposalHireModel(), ""),
		}
		a.ModelLabel = s.modelLabel(a.Model)
		p.Summary.Agent = &a
		p.Summary.Facts = proposalAgentFacts(a)
		p.Docs = append(p.Docs, proposalDoc(apitypes.ProposalDocKeyRole, "Role", "role.md", nil, h.Body))
		if strings.TrimSpace(h.InitialAgentMemory) != "" {
			p.Docs = append(p.Docs, proposalDoc(apitypes.ProposalDocKeyMemory, "Initial memory", "memory.md", nil, h.InitialAgentMemory))
		}

	case apitypes.ProposalKindRoleUpdate:
		slug := req.RoleUpdate.Slug
		a := s.proposalAgent(v, slug)
		p.Summary.Agent = &a
		p.Summary.Facts = proposalAgentFacts(a)
		// An agent offboarded since has no role.md where ReadRole looks;
		// no before then, rather than an empty one that would show the
		// whole role as added.
		var before *string
		if !approved {
			if current, err := s.Store.ReadRole(slug); err == nil {
				before = &current
			}
		}
		d := proposalDoc(apitypes.ProposalDocKeyRole, "Role", "role.md", before, req.RoleUpdate.Body)
		d.BeforeIsCurrent = before != nil && resp != nil
		p.Docs = append(p.Docs, d)

	case apitypes.ProposalKindHandbookUpdate:
		var before *string
		if !approved {
			current, _ := s.Store.ReadHandbook()
			before = &current
		}
		if n := s.proposalTeamSize(); n > 0 {
			p.Summary.Facts = append(p.Summary.Facts, apitypes.ProposalFact{Label: "Applies to", Value: proposalTeamWords(n)})
		}
		d := proposalDoc(apitypes.ProposalDocKeyHandbook, "Handbook", "handbook.md", before, req.HandbookUpdate.Body)
		d.BeforeIsCurrent = before != nil && resp != nil
		p.Docs = append(p.Docs, d)

	case apitypes.ProposalKindOffboard:
		o := req.Offboard
		a := s.proposalAgent(v, o.Slug)
		p.Summary.Agent = &a
		p.Summary.Facts = append(proposalAgentFacts(a),
			// Its open assignments go to its manager (respondAsCEO).
			apitypes.ProposalFact{Label: "Its assignments move to", Value: a.ReportsTo.Name})
		if reason := strings.TrimSpace(o.Reason); reason != "" && reason != strings.TrimSpace(req.Body) {
			p.Summary.Facts = append(p.Summary.Facts, apitypes.ProposalFact{Label: "Reason", Value: reason})
		}
		sentence := offboardSentence
		p.OffboardSentence = &sentence

	case apitypes.ProposalKindReorg:
		failed := map[store.ReorgMove]string{}
		if resp != nil && resp.Reorg != nil {
			for _, f := range resp.Reorg.Failed {
				failed[f.Move] = f.Reason
			}
		}
		for _, m := range req.Reorg.Moves {
			move := apitypes.ProposalMove{
				Slug: m.Slug,
				Name: v.person(m.Slug).Name,
				To:   v.person(m.NewManager),
			}
			if cur, ok := s.proposalCurrentManager(m.Slug); ok && cur != m.NewManager {
				from := v.person(cur)
				move.From = &from
			}
			if reason, ok := failed[m]; ok {
				move.Failed = &reason
			}
			p.Summary.Moves = append(p.Summary.Moves, move)
		}
		p.Summary.Facts = append(p.Summary.Facts, apitypes.ProposalFact{Label: "Changes", Value: proposalReportingLines(len(req.Reorg.Moves))})
	}
	return p
}

// addArchivedNames adds the display names of agents no longer on the
// team, so a proposal about one (an approved offboard) still names it
// as its answer did.
func (v *homeView) addArchivedNames() {
	archived, _ := v.s.Store.ListArchivedAgents()
	for _, a := range archived {
		if _, ok := v.names[a.Slug]; !ok {
			v.names[a.Slug] = agentDisplayName(a)
		}
	}
}

// proposalAgent is the tile for an existing agent, active or archived.
func (s *Server) proposalAgent(v *homeView, slug string) apitypes.ProposalAgent {
	a, err := s.Store.GetAgent(slug)
	if err != nil {
		a, err = s.Store.GetArchivedAgent(slug)
	}
	if err != nil {
		a = store.Agent{Slug: slug}
	}
	model := cmp.Or(a.Model, s.AgentModel)
	return apitypes.ProposalAgent{
		Slug:       slug,
		Name:       v.person(slug).Name,
		RoleTitle:  a.Role,
		Icon:       a.Icon,
		ReportsTo:  v.person(cmp.Or(a.ReportsTo, agent.CEOSlug)),
		Model:      model,
		ModelLabel: s.modelLabel(model),
		Effort:     s.effectiveEffort(model, a.Effort),
	}
}

// proposalHireModel is the model a hire starts on: the runtime's default,
// which ApplyHire pins on the new agent.
func (s *Server) proposalHireModel() string {
	if s.Runtime != nil && s.Runtime.Defaults.AgentModel != "" {
		return s.Runtime.Defaults.AgentModel
	}
	return s.AgentModel
}

// proposalCurrentManager is whom slug reports to now.
func (s *Server) proposalCurrentManager(slug string) (string, bool) {
	a, err := s.Store.GetAgent(slug)
	if err != nil {
		return "", false
	}
	return cmp.Or(a.ReportsTo, agent.CEOSlug), true
}

// proposalTeamSize is how many agents are on the team, not counting you.
func (s *Server) proposalTeamSize() int {
	actives, _ := s.Store.ListActiveAgents()
	n := 0
	for _, a := range actives {
		if a.Slug != agent.CEOSlug {
			n++
		}
	}
	return n
}

// proposalAgentFacts are the tile's lines: whom it reports to and what it runs
// on.
func proposalAgentFacts(a apitypes.ProposalAgent) []apitypes.ProposalFact {
	facts := []apitypes.ProposalFact{{Label: "Reports to", Value: a.ReportsTo.Name}}
	if model := cmp.Or(a.ModelLabel, a.Model); model != "" {
		if a.Effort != "" {
			model += " · " + a.Effort
		}
		facts = append(facts, apitypes.ProposalFact{Label: "Model", Value: model})
	}
	return facts
}

// proposalDoc is one document block. Both sides are normalized the way
// the diff pages normalize them (normalizeForDiff), so line endings and
// a trailing newline lost to the YAML round trip never show as edits.
func proposalDoc(key apitypes.ProposalDocKey, title, file string, before *string, after string) apitypes.ProposalDoc {
	after = normalizeForDiff(after)
	d := apitypes.ProposalDoc{
		Key:   key,
		Title: title,
		Meta:  file + " · " + proposalLineCount(after),
		After: after,
	}
	if before != nil {
		b := normalizeForDiff(*before)
		if strings.TrimSpace(*before) == "" {
			b = ""
		}
		d.Before = &b
	}
	return d
}

// proposalLineCount counts a document's lines: "84 lines".
func proposalLineCount(doc string) string {
	n := 0
	if strings.TrimSpace(doc) != "" {
		n = strings.Count(strings.TrimRight(doc, "\n"), "\n") + 1
	}
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}

func proposalTeamWords(n int) string {
	if n == 1 {
		return "The 1 agent on the team"
	}
	return fmt.Sprintf("All %d agents on the team", n)
}

func proposalReportingLines(n int) string {
	if n == 1 {
		return "1 reporting line"
	}
	return fmt.Sprintf("%d reporting lines", n)
}
