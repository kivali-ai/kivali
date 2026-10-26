// Package web (integration_tools.go) — agents' calls through the
// integrations.
//
// An agent with valid access to a running integration has its actions
// as tools named with the integration's prefix
// (integration.ToolPrefix: hubspot_create_post). Core lists them into
// each turn's request, where they ride into the CLI's --allowedTools
// (marked provider.Tool.Integration, so a change in the set respawns
// the runner), and for the agent's MCP server when it starts
// (POST /v1/agent/{slug}/integrations/tools). Every call comes back
// here (POST /v1/agent/{slug}/integrations/call): core checks the
// access as it stands, stamps the call with the agent and its access
// (_meta.kivali), sends it to the integration (or, for one that ships
// with Kivali, calls its Go function with the agent, its access and the
// connection: integration_builtin.go), scrubs the answer of the
// connection's secrets and writes an audit row. The integration never
// authenticates an agent: core is the only place access is enforced.

package web

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// integrationAuditActionMax bounds the action an audit row keeps of a
// name core could not match to an action.
const integrationAuditActionMax = 128

// agentIntegrationTool is one action an agent may call: its name with
// the integration's prefix, as the agent sees it, and the integration
// and action it stands for.
type agentIntegrationTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Integration string
	Tool        string
}

// agentIntegration is one integration as one agent's calls find it.
type agentIntegration struct {
	name    string
	display string
	// manifest is the approved version's; access the agent's filled
	// access form. Both set only when the agent may call it.
	manifest *integration.Manifest
	access   json.RawMessage
	// builtin is its code when it ships with Kivali (integration_builtin.go):
	// core calls it directly. nil for a bundle, which runs in its pod.
	builtin builtinintegrations.Integration
	// refusal is why the agent may not call it now, in words for the
	// agent; reason is the same for the owner's audit. Both empty when
	// it may.
	refusal string
	reason  string
}

// The words a refused call answers with. Each names the cause and who
// can change it, and nothing else.
func refuseNoVersion(display string) string {
	return fmt.Sprintf("The %s integration has no approved version. Ask the owner if your work needs it.", display)
}

func refuseDisabled(display string) string {
	return fmt.Sprintf("The %s integration is disabled. Ask the owner if your work needs it.", display)
}

func refuseNoAccess(display string) string {
	return fmt.Sprintf("You have no access to the %s integration. Ask the owner if your work needs it.", display)
}

func refuseInvalid(display, why string) string {
	return fmt.Sprintf("Your access to the %s integration no longer applies (%s). Ask the owner to set it again.", display, why)
}

func refuseNotRunning(display string) string {
	return fmt.Sprintf("The %s integration is not running right now; try again later or tell the owner.", display)
}

func refuseNotIncluded(display, tool string) string {
	return fmt.Sprintf("Your access to the %s integration does not include %s. Ask the owner if your work needs it.", display, tool)
}

// reasonNoShellJob is the audit's reason for a request that carries a
// shell job's result for a call that asked for no job.
const reasonNoShellJob = "a shell job's result for a call that asked for none"

func refuseNoShellJob(display string) string {
	return fmt.Sprintf("The %s integration asked your shell for nothing in this call, so its result was not read. Call the action again.", display)
}

func refuseSettingUp(display string) string {
	return fmt.Sprintf("The %s integration is being set up; ask the owner to finish it.", display)
}

// reasonSettingUp is the audit's reason for a call refused while the
// integration's connection is not an installed one.
const reasonSettingUp = "the connection is being set up"

// connectionBeingSetUp reports whether name's connection is not one an
// agent may call through, though it may exist: an install of it under
// way, one a stopped Kivali left unfinished (SetupPending), one being
// discarded, or one an earlier version made that m's setup does not fit
// (setupMisfit). Agents call through a connected connection alone, as
// the lifecycle (integrationView.state) and AgentIntegration.usable say.
func (s *Server) connectionBeingSetUp(name string, m *integration.Manifest) bool {
	if _, installing, discarding := s.setupPeek(name); installing || discarding {
		return true
	}
	rec, err := s.Store.GetIntegrationConnection(name)
	if err != nil {
		return false
	}
	return rec.SetupPending || (m != nil && setupMisfit(m.Kivali.Connection.Setup, rec))
}

// agentIntegrationFor is it as slug's calls find it: refused when no
// version is approved, it is disabled, the agent has no access or
// access that no longer applies, it is not running, or its connection
// is being set up (connectionBeingSetUp).
func (s *Server) agentIntegrationFor(it store.Integration, slug string) (agentIntegration, error) {
	v := agentIntegration{name: it.Name, display: cmp.Or(it.DisplayName, it.Name)}
	refuse := func(refusal, reason string) (agentIntegration, error) {
		v.refusal, v.reason = refusal, reason
		return v, nil
	}
	if it.CurrentVersion == "" {
		return refuse(refuseNoVersion(v.display), "no version is approved")
	}
	if !it.Enabled {
		return refuse(refuseDisabled(v.display), "the integration is disabled")
	}
	rows, err := s.Store.ListIntegrationAccess(it.Name)
	if err != nil {
		return v, err
	}
	i := slices.IndexFunc(rows, func(a store.IntegrationAccess) bool { return a.Slug == slug })
	if i < 0 {
		return refuse(refuseNoAccess(v.display), "no access")
	}
	if why := rows[i].Invalid; why != "" {
		return refuse(refuseInvalid(v.display, why), "the access no longer applies: "+why)
	}
	if !s.integrationRunning(it) {
		return refuse(refuseNotRunning(v.display), errNotRunning)
	}
	_, m, err := s.Store.CurrentIntegrationVersion(it.Name)
	if err != nil {
		return v, err
	}
	if s.connectionBeingSetUp(it.Name, m) {
		return refuse(refuseSettingUp(v.display), reasonSettingUp)
	}
	v.manifest, v.access, v.builtin = m, rows[i].AccessJSON(), s.builtinFor(it)
	return v, nil
}

// listedTools is the part of an MCP tools/list result Kivali reads.
type listedTools struct {
	Tools []listedTool `json:"tools"`
}

// listedTool is one tool of a tools/list result.
type listedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// objectSchema is the input schema of an action whose server gives
// none.
var objectSchema = json.RawMessage(`{"type":"object"}`)

// isObjectSchema reports whether raw is a JSON Schema of an object,
// the only input schema a model's tool can have.
func isObjectSchema(raw json.RawMessage) bool {
	var schema map[string]json.RawMessage
	if json.Unmarshal(raw, &schema) != nil {
		return false
	}
	var typ string
	return json.Unmarshal(schema["type"], &typ) == nil && typ == "object"
}

// agentToolsOf is v's actions for slug: the integration's tools/list
// answer for the agent (asked with its _meta, kept by the registry),
// or a built-in's Tools for the agent and its access, keeping only the
// actions the approved version's manifest declares, which are the ones
// its install screen listed. An action whose input schema is not an
// object's is left out: a model cannot be given it.
func (s *Server) agentToolsOf(ctx context.Context, v agentIntegration, slug string) ([]agentIntegrationTool, error) {
	var list listedTools
	if v.builtin != nil {
		var err error
		if list, err = s.builtinListedTools(ctx, v, slug); err != nil {
			return nil, err
		}
	} else {
		raw, err := s.Integrations.AgentTools(ctx, v.name, slug, v.access)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("the answer is not a list of actions: %w", err)
		}
	}
	declared := map[string]bool{}
	for _, t := range v.manifest.Tools {
		declared[t.Name] = true
	}
	prefix := integration.ToolPrefix(v.name)
	suffix := "Through the " + v.display + " integration; the owner decides what you may do with it."
	var out []agentIntegrationTool
	for _, t := range list.Tools {
		if !declared[t.Name] {
			continue
		}
		declared[t.Name] = false
		schema := t.InputSchema
		if len(schema) == 0 || string(schema) == "null" {
			schema = objectSchema
		}
		if !isObjectSchema(schema) {
			log.Printf("integration %s: the action %s has an input schema that is not an object's; left out", v.name, t.Name)
			continue
		}
		desc := strings.TrimSpace(t.Description)
		if desc != "" {
			desc += " "
		}
		out = append(out, agentIntegrationTool{
			Name:        prefix + t.Name,
			Description: desc + suffix,
			InputSchema: schema,
			Integration: v.name,
			Tool:        t.Name,
		})
	}
	return out, nil
}

// agentIntegrationTools is every action slug may call now: those of
// each approved, switched-on, running integration it has valid access
// to, as the integration lists them for that access, sorted by name.
// An integration that does not answer its list is left out, and
// logged; the lists are asked for at once, so a slow one holds the
// answer for no longer than its own deadline.
func (s *Server) agentIntegrationTools(ctx context.Context, slug string) ([]agentIntegrationTool, error) {
	if s.Store == nil || s.Integrations == nil {
		return nil, nil
	}
	its, err := s.Store.ListIntegrations()
	if err != nil {
		return nil, err
	}
	var granted []agentIntegration
	for _, it := range its {
		v, err := s.agentIntegrationFor(it, slug)
		if err != nil {
			log.Printf("integration %s: %s's access: %v (its actions left out)", it.Name, slug, err)
			continue
		}
		if v.refusal == "" {
			granted = append(granted, v)
		}
	}
	lists := make([][]agentIntegrationTool, len(granted))
	var wg sync.WaitGroup
	for i, v := range granted {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tools, err := s.agentToolsOf(ctx, v, slug)
			if err != nil {
				log.Printf("integration %s: list %s's actions: %v (left out)", v.name, slug, err)
				return
			}
			lists[i] = tools
		}()
	}
	wg.Wait()
	var out []agentIntegrationTool
	for _, l := range lists {
		out = append(out, l...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	// Two integrations whose prefixes overlap ("my", "my-crm") could
	// name two actions alike; the first in order is the one listed.
	out = slices.CompactFunc(out, func(a, b agentIntegrationTool) bool { return a.Name == b.Name })
	return out, nil
}

// integrationTurnTools is slug's integration actions for its turn
// request, each marked Integration. A failure to read them leaves them
// out of this turn.
func (s *Server) integrationTurnTools(slug string) []provider.Tool {
	tools, err := s.agentIntegrationTools(context.Background(), slug)
	if err != nil {
		log.Printf("chat %s: integration actions: %v (none this turn)", slug, err)
		return nil
	}
	out := make([]provider.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, provider.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, Integration: true})
	}
	return out
}

// handleAgentpodIntegrationTools serves POST
// /v1/agent/{slug}/integrations/tools: the actions the agent may call
// now, for its MCP server to register when it starts.
func (s *Server) handleAgentpodIntegrationTools(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	tools, err := s.agentIntegrationTools(r.Context(), slug)
	if err != nil {
		log.Printf("integrations: %s's actions: %v", slug, err)
		http.Error(w, "the integrations could not be read", http.StatusInternalServerError)
		return
	}
	out := agentpod.IntegrationToolsResponse{Tools: make([]agentpod.IntegrationTool, 0, len(tools))}
	for _, t := range tools {
		out.Tools = append(out.Tools, agentpod.IntegrationTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAgentpodIntegrationCall serves POST
// /v1/agent/{slug}/integrations/call: one call of an action, answered
// with its result or the refusal. Tool-level outcomes are 200s.
func (s *Server) handleAgentpodIntegrationCall(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !checkSlugHeader(w, r, slug) {
		return
	}
	if s.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return
	}
	var req agentpod.IntegrationCallRequest
	if err := decodeJSONBounded(w, r, &req); err != nil {
		http.Error(w, "decode IntegrationCallRequest: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, s.callAgentIntegrationJob(r.Context(), slug, req.Name, req.Arguments, req.ShellJobResult))
}

// integrationCallRefusal is a call refused before it reached the
// integration, and the integration whose audit it goes in.
type integrationCallRefusal struct {
	integration string
	tool        string
	text        string
	reason      string
}

// callAgentIntegration runs slug's call of the action named name. The
// name is matched against the actions slug may call now, worked out
// afresh from the store and each integration's list for the agent:
// every integration whose prefix it starts with is tried, the longest
// prefix first. A call that matches goes to its integration with the
// agent's _meta; one that does not is refused with the first cause
// found. Every call, allowed or refused, appends a row to its
// integration's audit.
func (s *Server) callAgentIntegration(ctx context.Context, slug, name string, args json.RawMessage) agentpod.IntegrationCallResponse {
	return s.callAgentIntegrationJob(ctx, slug, name, args, nil)
}

// callAgentIntegrationJob is callAgentIntegration for a request that
// may carry what the agent's shell gave back for the job the call's
// first request was answered with (result). The call is worked out
// again exactly as on the first request; result reaches only the
// integration the name matches.
func (s *Server) callAgentIntegrationJob(ctx context.Context, slug, name string, args json.RawMessage, result *agentpod.IntegrationShellJobResult) agentpod.IntegrationCallResponse {
	start := s.clk().Now()
	its, err := s.Store.ListIntegrations()
	if err != nil {
		log.Printf("integrations: %s's call of %s: %v", slug, name, err)
		return integrationError("Kivali could not read its integrations; try again later or tell the owner.")
	}
	var cands []store.Integration
	for _, it := range its {
		if p := integration.ToolPrefix(it.Name); len(name) > len(p) && strings.HasPrefix(name, p) {
			cands = append(cands, it)
		}
	}
	if len(cands) == 0 {
		return integrationError(fmt.Sprintf("No integration has an action named %s.", name))
	}
	sort.SliceStable(cands, func(i, j int) bool { return len(cands[i].Name) > len(cands[j].Name) })
	var refused *integrationCallRefusal
	refuse := func(c integrationCallRefusal) {
		if refused == nil {
			refused = &c
		}
	}
	for _, it := range cands {
		tool := strings.TrimPrefix(name, integration.ToolPrefix(it.Name))
		v, err := s.agentIntegrationFor(it, slug)
		if err != nil {
			log.Printf("integration %s: %s's access: %v", it.Name, slug, err)
			refuse(integrationCallRefusal{it.Name, tool, fmt.Sprintf("Kivali could not read your access to the %s integration; try again later or tell the owner.", cmp.Or(it.DisplayName, it.Name)), "the access could not be read"})
			continue
		}
		if v.refusal != "" {
			refuse(integrationCallRefusal{it.Name, tool, v.refusal, v.reason})
			continue
		}
		tools, err := s.agentToolsOf(ctx, v, slug)
		switch {
		case errors.Is(err, errIntegrationNotRunning):
			refuse(integrationCallRefusal{it.Name, tool, refuseNotRunning(v.display), errNotRunning})
			continue
		case err != nil:
			why := integrationErrorWords(err)
			refuse(integrationCallRefusal{it.Name, tool,
				fmt.Sprintf("The %s integration did not list your actions (%s); try again later or tell the owner.", v.display, why),
				"the integration did not list the agent's actions: " + why})
			continue
		}
		if !slices.ContainsFunc(tools, func(t agentIntegrationTool) bool { return t.Tool == tool }) {
			refuse(integrationCallRefusal{it.Name, tool, refuseNotIncluded(v.display, tool), "the agent's access does not include it"})
			continue
		}
		return s.runAgentIntegrationCall(ctx, start, slug, v, tool, args, result)
	}
	s.auditAgentCall(refused.integration, start, slug, refused.tool, false, refused.reason)
	return integrationError(refused.text)
}

// runAgentIntegrationCall sends v's server the call of tool for slug,
// stamped with the agent and its access, and returns its result for the
// model, scrubbed of the connection's secrets, with the audit row
// written. result is a shell job's outcome, on the second request of a
// call whose first was answered with a job; only a built-in asks for
// one, so a bundle's call that carries one is refused.
func (s *Server) runAgentIntegrationCall(ctx context.Context, start time.Time, slug string, v agentIntegration, tool string, args json.RawMessage, result *agentpod.IntegrationShellJobResult) agentpod.IntegrationCallResponse {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		s.auditAgentCall(v.name, start, slug, tool, false, "the arguments are not JSON")
		return integrationError("The arguments are not JSON.")
	}
	if v.builtin != nil {
		return s.builtinAgentCall(ctx, start, slug, v, tool, args, result)
	}
	if result != nil {
		s.auditAgentCall(v.name, start, slug, tool, false, reasonNoShellJob)
		return integrationError(refuseNoShellJob(v.display))
	}
	raw, callErr := s.Integrations.Call(ctx, v.name, "tools/call", map[string]any{
		"name":      tool,
		"arguments": args,
		"_meta":     kivaliMeta(slug, v.access),
	})
	resp, failed := integrationCallAnswer(v.display, raw, callErr)
	scrub := s.Integrations.scrubber(ctx, v.name)
	if scrub == nil {
		// Not knowing the secrets is no reason to pass the answer on:
		// it may quote one.
		s.auditAgentCall(v.name, start, slug, tool, true, "the answer could not be checked for secrets")
		return integrationError(fmt.Sprintf("Kivali could not check the %s integration's answer for secrets, so it held the answer back; tell the owner.", v.display))
	}
	for i := range resp.Content {
		resp.Content[i] = scrub.Replace(resp.Content[i])
	}
	s.auditAgentCall(v.name, start, slug, tool, true, scrub.Replace(failed))
	return resp
}

// integrationCallAnswer is a tools/call outcome as the model reads it,
// and the integration's words when the call did not go through (the
// audit's reason): the text of each text block; for a result that has
// none, its structured content; a note naming what else the answer
// held, which is not passed on; the server's error, or the deadline.
func integrationCallAnswer(display string, raw json.RawMessage, callErr error) (agentpod.IntegrationCallResponse, string) {
	var rpcErr *integrationpod.RPCError
	switch {
	case errors.Is(callErr, errIntegrationNotRunning):
		return integrationError(refuseNotRunning(display)), "the integration stopped before it answered"
	case errors.Is(callErr, errIntegrationTimeout):
		secs := int(integrationCallTimeout / time.Second)
		return integrationError(fmt.Sprintf("The %s integration did not answer within %d seconds; try again later or tell the owner.", display, secs)),
			fmt.Sprintf("the integration did not answer within %d seconds", secs)
	case errors.As(callErr, &rpcErr):
		return integrationError(fmt.Sprintf("The %s integration answered with an error: %s", display, rpcErr.Message)), clipText(rpcErr.Message, integrationVerifyMessageMax)
	case callErr != nil:
		return integrationError(fmt.Sprintf("The call to the %s integration did not finish: %v.", display, callErr)), callErr.Error()
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return integrationError(fmt.Sprintf("The %s integration's answer is not one Kivali reads; tell the owner.", display)), "the integration's answer is not one Kivali reads"
	}
	var texts, other []string
	for _, c := range res.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		} else if t := cmp.Or(c.Type, "unnamed"); !slices.Contains(other, t) {
			other = append(other, t)
		}
	}
	if len(texts) == 0 && len(res.StructuredContent) > 0 && string(res.StructuredContent) != "null" {
		texts = append(texts, string(res.StructuredContent))
	}
	failed := ""
	if res.IsError {
		failed = clipText(cmp.Or(firstNonEmpty(texts), "the integration reported an error"), integrationVerifyMessageMax)
	}
	if len(other) > 0 {
		texts = append(texts, fmt.Sprintf("The answer also held content Kivali does not pass on (%s).", strings.Join(other, ", ")))
	}
	if len(texts) == 0 {
		texts = []string{fmt.Sprintf("The %s integration's answer was empty.", display)}
	}
	return agentpod.IntegrationCallResponse{Content: texts, IsError: res.IsError}, failed
}

func firstNonEmpty(texts []string) string {
	for _, t := range texts {
		if t != "" {
			return t
		}
	}
	return ""
}

// integrationErrorWords is a registry error in words.
func integrationErrorWords(err error) string {
	var rpcErr *integrationpod.RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr.Message
	}
	return err.Error()
}

func integrationError(text string) agentpod.IntegrationCallResponse {
	return agentpod.IntegrationCallResponse{Content: []string{text}, IsError: true}
}

// auditAgentCall appends one row for slug's call of tool to name's
// audit.
func (s *Server) auditAgentCall(name string, start time.Time, slug, tool string, allowed bool, reason string) {
	row := store.IntegrationAuditRow{
		At:         start,
		Agent:      slug,
		Action:     clipText(tool, integrationAuditActionMax),
		Allowed:    allowed,
		Reason:     clipText(reason, integrationVerifyMessageMax),
		DurationMS: s.clk().Now().Sub(start).Milliseconds(),
	}
	if err := s.Store.AppendIntegrationAudit(name, row); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("integration %s: audit: %v", name, err)
	}
}
