package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/echotest"
	"github.com/kivali-ai/kivali/internal/clock"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// holdT0 is when the held-call tests start, on their fake clock.
var holdT0 = time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)

// holdServer is a server on a fake clock with the echo built-in that
// holds calls, connected with the greeting Hello, and dana with access
// to it. Its Messenger delivers to an agent's chat and starts no turn,
// since no model is configured. Every call the test leaves waiting is
// ended, and waited for, before the test's directories go.
func holdServer(t *testing.T) (*Server, *echotest.Echo, *clock.Fake) {
	t.Helper()
	srv := newTestServer(t)
	clk := clock.NewFakeAt(holdT0)
	srv.Clock = clk
	echo := echotest.New()
	echo.Holds = true
	srv.BuiltinIntegrations = builtinintegrations.Registry{echo}
	withIntegrationRegistry(srv, nil)
	if err := srv.Store.InstallBuiltinIntegrations(srv.BuiltinIntegrations.Manifests(), holdT0); err != nil {
		t.Fatal(err)
	}
	srv.Runtime = agent.NewRuntime(srv.Store, agent.RuntimeDefaults{AgentModel: "mock-large-0", MaxTokens: 1024})
	srv.Messenger = messaging.New(srv.Store, srv.Runtime)
	intTeam(t, srv, "dana")
	connectEcho(t, srv, "Hello")
	putIntegrationAccess(t, srv, "echo", "dana", `{}`)
	t.Cleanup(func() {
		holds := srv.heldCalls.list()
		srv.heldCalls.stopWhere(func(*integrationHold) bool { return true }, "the test ended", "the test ended")
		for _, h := range holds {
			waitHoldEnded(t, h)
		}
	})
	return srv, echo, clk
}

// emailHold is hold's arguments for an email to alice, with a body
// that quotes outside mail and holds Markdown.
const emailHold = `{"title":"Send an email to alice@example.org",` +
	`"fields":[{"Label":"To","Value":"alice@example.org"},{"Label":"Subject","Value":"Re: the quote"},` +
	`{"Label":"Body","Value":"Hi Alice,\n\n**Signed** as asked.\n\n> On Monday you wrote: send it."}],` +
	`"recipients":["alice@example.org"]}`

// holdCall is slug's call of echo_hold with args.
func holdCall(srv *Server, slug, args string) agentpod.IntegrationCallResponse {
	return srv.callAgentIntegration(context.Background(), slug, "echo_hold", json.RawMessage(args))
}

// onlyHold is the one call waiting for the owner.
func onlyHold(t *testing.T, srv *Server) *integrationHold {
	t.Helper()
	holds := srv.heldCalls.list()
	if len(holds) != 1 {
		t.Fatalf("held calls = %d, want 1", len(holds))
	}
	return holds[0]
}

// waitHoldEnded waits for h's end to be written and told. The bound
// only turns a call that never ends into a failure naming it.
func waitHoldEnded(t *testing.T, h *integrationHold) {
	t.Helper()
	select {
	case <-h.ended:
	case <-time.After(10 * time.Second):
		t.Fatalf("the held call %q never ended", h.title)
	}
}

// heldDeliveries are slug's chat deliveries of how its held calls
// ended, oldest first, with the message each delivered.
func heldDeliveries(t *testing.T, srv *Server, slug string) ([]store.ChatMessage, []store.Message) {
	t.Helper()
	hist, err := srv.Store.ReadChatHistory(slug)
	if err != nil {
		t.Fatal(err)
	}
	var rows []store.ChatMessage
	var msgs []store.Message
	for _, m := range hist {
		if m.Kind != "inbox_delivery" || !strings.Contains(m.MessageRef, "-"+string(store.MsgHeldCall)+"-") {
			continue
		}
		msg, err := srv.Store.ReadMessage(filepath.Join(srv.Store.Root(), m.MessageRef))
		if err != nil {
			t.Fatal(err)
		}
		rows, msgs = append(rows, m), append(msgs, msg)
	}
	return rows, msgs
}

// lastAudit is echo's newest audit row.
func lastAudit(t *testing.T, srv *Server) store.IntegrationAuditRow {
	t.Helper()
	rows, err := srv.Store.ReadIntegrationAuditTail("echo", 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit = %+v, %v", rows, err)
	}
	return rows[0]
}

// ran reports whether a held call's Run was called.
func ran(echo *echotest.Echo) []echotest.Seen {
	var out []echotest.Seen
	for _, c := range echo.Calls() {
		if c.Action == "hold run" {
			out = append(out, c)
		}
	}
	return out
}

// Why this test exists: a held call is the only thing between an agent
// and an outward action, so the call must do nothing but wait. The
// agent must hear at once, not as an error, that it waits and where
// the outcome comes; the audit must say it waits, naming what; and the
// owner's row must carry every field exactly as the built-in captured
// it (scrubbed of the connection's secrets), under an id nobody can
// guess, in the shape the web app reads. A Run called here, or a row
// missing a field, would let something go out that the owner never
// read.
func TestHeldCallWaitsForTheOwner(t *testing.T) {
	srv, echo, _ := holdServer(t)
	args := strings.Replace(emailHold, "send it.", "send it. "+echoToken, 1)
	resp := holdCall(srv, "dana", args)
	if resp.IsError || len(resp.Content) != 1 || resp.Content[0] != "Waiting for the owner's approval. You will hear in your chat whether it was done. If you hear nothing within a day, it was not done." {
		t.Fatalf("the agent's answer = %+v", resp)
	}
	if r := ran(echo); len(r) != 0 {
		t.Fatalf("Run was called before the owner answered: %+v", r)
	}
	if row := lastAudit(t, srv); row.Agent != "dana" || row.Action != "hold" || !row.Allowed || row.Reason != "waiting for the owner's approval: Send an email to alice@example.org" {
		t.Errorf("audit = %+v", row)
	}

	h := onlyHold(t, srv)
	if len(h.id) != 32 {
		t.Errorf("id = %q, want 128 random bits", h.id)
	}
	n, ok := needWithID(t, srv, integrationHoldNeedPrefix+h.id)
	if !ok {
		t.Fatal("Home does not list the held call")
	}
	want := apitypes.IntegrationHoldRef{
		Integration: "echo", Display: "Echo", Agent: "dana", Action: "hold", Title: "Send an email to alice@example.org",
		Fields: []apitypes.HoldField{
			{Label: "To", Value: "alice@example.org"},
			{Label: "Subject", Value: "Re: the quote"},
			{Label: "Body", Value: "Hi Alice,\n\n**Signed** as asked.\n\n> On Monday you wrote: send it. [secret]"},
		},
		Recipients: []string{"alice@example.org"},
	}
	if n.Kind != apitypes.NeedKindIntegrationHold || n.Title != want.Title || n.From != (apitypes.PersonRef{Slug: "dana", Name: "Dana"}) ||
		!n.At.Equal(holdT0) || n.BodyMD != "" || n.RawURL != nil || n.IntegrationHold == nil {
		t.Fatalf("row = %+v", n)
	}
	if got, _ := json.Marshal(n.IntegrationHold); string(got) != string(must(json.Marshal(want))) {
		t.Errorf("integration_hold = %s\nwant %s", got, must(json.Marshal(want)))
	}

	// The wire's keys, as the web app reads them.
	rr := homeDo(t, srv, http.MethodGet, "/api/v1/home", "", nil, nil)
	var raw struct {
		Needs []map[string]json.RawMessage `json:"needs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	var ref map[string]json.RawMessage
	for _, need := range raw.Needs {
		if string(need["id"]) == `"`+integrationHoldNeedPrefix+h.id+`"` {
			if string(need["kind"]) != `"integration_hold"` {
				t.Errorf("kind = %s", need["kind"])
			}
			_ = json.Unmarshal(need["integration_hold"], &ref)
		}
	}
	for _, k := range []string{"integration", "display", "agent", "action", "title", "fields", "recipients", "taint"} {
		if _, ok := ref[k]; !ok {
			t.Errorf("integration_hold has no %q: %v", k, ref)
		}
	}

	// The org stream counts it, so Home refetches.
	snap := srv.orgSnapshotValue()
	if !strings.Contains(strings.Join(snap.Inbox.CEOPaths, ","), integrationHoldNeedPrefix+h.id) || snap.Inbox.Unactioned < 1 {
		t.Errorf("snapshot inbox = %+v", snap.Inbox)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// Why this test exists: approving is the moment the action happens, so
// it must run what the owner read, once, with the connection as it is
// then (a sign-in's token renewed since the call), and the agent must
// hear in its chat that it was approved and what the integration
// answered. The answer is the integration's text, so the agent reads
// it fenced, never as words of the owner's. The audit ends the call
// with the built-in's own words, and a second answer finds nothing to
// answer.
func TestHeldCallApprovedRunsWithTheConnectionReadThen(t *testing.T) {
	srv, echo, _ := holdServer(t)
	holdCall(srv, "dana", emailHold)
	h := onlyHold(t, srv)
	connectEcho(t, srv, "Hi again")

	code, out, body := answerNeed(t, srv, "approve", integrationHoldNeedPrefix+h.id, "Keep it short.")
	if code != http.StatusOK || out.Result != "Approved and done. Dana is told in its chat" || out.Link != "/agents/dana" {
		t.Fatalf("approve: %d %+v %s", code, out, body)
	}
	runs := ran(echo)
	if len(runs) != 1 || runs[0].Conn.Fields["greeting"] != "Hi again" || runs[0].Conn.Fields["token"] != echoToken || runs[0].Caller.Agent != "dana" {
		t.Fatalf("Run = %+v", runs)
	}
	if row := lastAudit(t, srv); row.Action != "hold" || !row.Allowed || row.Reason != "approved: echoed to alice@example.org" {
		t.Errorf("audit = %+v", row)
	}
	rows, msgs := heldDeliveries(t, srv, "dana")
	if len(rows) != 1 {
		t.Fatalf("deliveries = %+v", rows)
	}
	const told = "Kivali reports how your integration call \"Send an email to alice@example.org\", which waited for the owner's approval, ended. Nothing is owed back on this message.\n\n" +
		"The owner approved it, and Kivali ran it. The Echo integration answered:\n\n```text\nEchoed 3 fields; Hi again; token s3c...\n```\n\nThe owner's note: Keep it short."
	if rows[0].Content != told || rows[0].Role != store.RoleReceived {
		t.Errorf("dana was told %q", rows[0].Content)
	}
	if m := msgs[0]; m.Type != store.MsgHeldCall || m.From != agent.CEOSlug || m.To.String() != "dana" || m.Title != "Approved: Send an email to alice@example.org" {
		t.Errorf("message = %+v", m)
	}
	if _, listed := needWithID(t, srv, integrationHoldNeedPrefix+h.id); listed {
		t.Error("the row stayed on Home after the approval")
	}
	if code, _, _ := answerNeed(t, srv, "deny", integrationHoldNeedPrefix+h.id, ""); code != http.StatusConflict {
		t.Errorf("a second answer: %d", code)
	}
	if len(ran(echo)) != 1 {
		t.Error("Run was called again")
	}
}

// Why this test exists: a denial is the owner keeping something from
// going out. Run must never be called, the agent must hear it was not
// done along with the owner's note (bounded, as a web upload's is),
// and a late approval of the same row must be refused, not run.
func TestHeldCallDeniedTellsTheAgentWithTheNote(t *testing.T) {
	srv, echo, _ := holdServer(t)
	holdCall(srv, "dana", emailHold)
	h := onlyHold(t, srv)
	if code, _, body := answerNeed(t, srv, "deny", integrationHoldNeedPrefix+h.id, strings.Repeat("x", maxHeldCallNote+1)); code != http.StatusBadRequest {
		t.Fatalf("a note over 2 KB: %d %s", code, body)
	}
	if code, _, body := answerNeed(t, srv, "ack", integrationHoldNeedPrefix+h.id, ""); code != http.StatusBadRequest {
		t.Fatalf("acknowledged: %d %s", code, body)
	}
	code, out, body := answerNeed(t, srv, "deny", integrationHoldNeedPrefix+h.id, "Not to Alice.")
	if code != http.StatusOK || out.Result != "Denied. Dana is told in its chat" {
		t.Fatalf("deny: %d %+v %s", code, out, body)
	}
	if r := ran(echo); len(r) != 0 {
		t.Fatalf("a denied call ran: %+v", r)
	}
	if row := lastAudit(t, srv); row.Allowed || row.Reason != "denied: Not to Alice." {
		t.Errorf("audit = %+v", row)
	}
	rows, msgs := heldDeliveries(t, srv, "dana")
	if len(rows) != 1 || !strings.HasSuffix(rows[0].Content, "\n\nThe owner denied it, so it was not done.\n\nThe owner's note: Not to Alice.") || msgs[0].Title != "Denied: Send an email to alice@example.org" {
		t.Fatalf("deliveries = %+v", rows)
	}
	if code, _, _ := answerNeed(t, srv, "approve", integrationHoldNeedPrefix+h.id, ""); code != http.StatusConflict {
		t.Errorf("a late approval: %d", code)
	}
	if r := ran(echo); len(r) != 0 {
		t.Errorf("a late approval ran the call: %+v", r)
	}
}

// Why this test exists: the agent was told that no word within a day
// means it was not done, so after heldCallLimit on core's clock the
// call must end unrun, the audit and the agent must say nobody
// answered, and an approval after that must be refused.
func TestHeldCallNotAnsweredWithinADay(t *testing.T) {
	srv, echo, clk := holdServer(t)
	holdCall(srv, "dana", emailHold)
	h := onlyHold(t, srv)
	clk.Advance(heldCallLimit - time.Second)
	if _, listed := needWithID(t, srv, integrationHoldNeedPrefix+h.id); !listed {
		t.Fatal("the row left Home before the limit")
	}
	clk.Advance(time.Second)
	waitHoldEnded(t, h)
	if row := lastAudit(t, srv); row.Allowed || row.Reason != "nobody answered within a day" {
		t.Errorf("audit = %+v", row)
	}
	rows, msgs := heldDeliveries(t, srv, "dana")
	if len(rows) != 1 || !strings.HasSuffix(rows[0].Content, "\n\nNobody answered within a day, so it was not done.") ||
		!strings.Contains(rows[0].Content, `"Send an email to alice@example.org"`) || msgs[0].Title != "Not answered: Send an email to alice@example.org" {
		t.Fatalf("deliveries = %+v", rows)
	}
	if code, _, _ := answerNeed(t, srv, "approve", integrationHoldNeedPrefix+h.id, ""); code != http.StatusConflict {
		t.Errorf("a late approval: %d", code)
	}
	if r := ran(echo); len(r) != 0 {
		t.Errorf("an unanswered call ran: %+v", r)
	}
}

// Why this test exists: an agent led by a page or a message could ask
// the owner for send after send. Five of one agent's calls may wait at
// once and a sixth is refused before it reaches the owner, telling the
// agent to wait; another agent is not held to the first one's count,
// and an answer frees a place.
func TestHeldCallsAreFiveAtATimeForAnAgent(t *testing.T) {
	srv, _, _ := holdServer(t)
	intTeam(t, srv, "lee")
	putIntegrationAccess(t, srv, "echo", "lee", `{}`)
	for i := 0; i < maxHeldCallsPerAgent; i++ {
		if resp := holdCall(srv, "dana", emailHold); resp.IsError {
			t.Fatalf("call %d = %+v", i+1, resp)
		}
	}
	resp := holdCall(srv, "dana", emailHold)
	if !resp.IsError || resp.Content[0] != "The owner has five of your requests waiting for approval. Wait for their answers in your chat before asking for another." {
		t.Fatalf("the sixth = %+v", resp)
	}
	if row := lastAudit(t, srv); row.Allowed || row.Reason != "the owner has five of the agent's requests waiting" {
		t.Errorf("audit = %+v", row)
	}
	if n := len(srv.heldCalls.list()); n != maxHeldCallsPerAgent {
		t.Errorf("waiting = %d", n)
	}
	if resp := holdCall(srv, "lee", emailHold); resp.IsError {
		t.Errorf("lee's call = %+v", resp)
	}
	var first *integrationHold
	for _, h := range srv.heldCalls.list() {
		if h.agent == "dana" {
			first = h
			break
		}
	}
	if code, _, body := answerNeed(t, srv, "deny", integrationHoldNeedPrefix+first.id, ""); code != http.StatusOK {
		t.Fatalf("deny: %d %s", code, body)
	}
	if resp := holdCall(srv, "dana", emailHold); resp.IsError {
		t.Errorf("after an answer = %+v", resp)
	}
}

// Why this test exists: the owner may approve hours after the call,
// and the agent's access may have gone since. Approve must work the
// call out again as a call would and, refused, run nothing and say why
// in words for the owner, leaving the call waiting; once access is
// given back, the same row approves and runs.
func TestHeldCallApproveAfterAccessRemovedKeepsItWaiting(t *testing.T) {
	srv, echo, _ := holdServer(t)
	holdCall(srv, "dana", emailHold)
	h := onlyHold(t, srv)
	if rr := intDelete(t, srv, intBase+"/echo/access/dana"); rr.Code != http.StatusOK {
		t.Fatalf("remove access: %d %s", rr.Code, rr.Body.String())
	}
	code, _, body := answerNeed(t, srv, "approve", integrationHoldNeedPrefix+h.id, "")
	if code != http.StatusConflict {
		t.Fatalf("approve with no access: %d %s", code, body)
	}
	var e apitypes.ErrorBody
	_ = json.Unmarshal([]byte(body), &e)
	if e.Error != "Dana has no access to Echo, so it was not done. It is still waiting: approve it again once that changes, or deny it" || e.Who != whoYou {
		t.Errorf("refusal = %+v", e)
	}
	if r := ran(echo); len(r) != 0 {
		t.Fatalf("a refused approval ran: %+v", r)
	}
	if _, listed := needWithID(t, srv, integrationHoldNeedPrefix+h.id); !listed {
		t.Fatal("the row left Home after a refused approval")
	}
	if rows, _ := heldDeliveries(t, srv, "dana"); len(rows) != 0 {
		t.Errorf("dana was told of a call still waiting: %+v", rows)
	}
	putIntegrationAccess(t, srv, "echo", "dana", `{}`)
	if code, out, body := answerNeed(t, srv, "approve", integrationHoldNeedPrefix+h.id, ""); code != http.StatusOK || out.Result != "Approved and done. Dana is told in its chat" {
		t.Fatalf("approve with access again: %d %+v %s", code, out, body)
	}
	if r := ran(echo); len(r) != 1 {
		t.Errorf("runs = %+v", r)
	}
}

// Why this test exists: a Run that fails or refuses is still the end
// of the call. The owner must be told it did not go well, the agent
// must read the integration's words as an error, and the audit must
// say a refusal was not let through; a Run that hung up the owner's
// answer, or a failure told as success, would mislead both.
func TestHeldCallRunThatFailsOrRefuses(t *testing.T) {
	srv, echo, _ := holdServer(t)
	echo.HoldRun = func(context.Context, builtinintegrations.Connection) (*builtinintegrations.Result, error) {
		return &builtinintegrations.Result{Content: []string{"Gmail refused the message: quota"}, IsError: true}, nil
	}
	holdCall(srv, "dana", emailHold)
	h := onlyHold(t, srv)
	code, out, _ := answerNeed(t, srv, "approve", integrationHoldNeedPrefix+h.id, "")
	if code != http.StatusOK || out.Result != "Approved, but Echo answered with an error: Gmail refused the message: quota. Dana is told in its chat" {
		t.Fatalf("approve: %d %+v", code, out)
	}
	if row := lastAudit(t, srv); !row.Allowed || row.Reason != "approved: Gmail refused the message: quota" {
		t.Errorf("audit = %+v", row)
	}
	rows, _ := heldDeliveries(t, srv, "dana")
	if len(rows) != 1 || !strings.Contains(rows[0].Content, "The owner approved it, and Kivali ran it, but the Echo integration answered with an error:\n\n```text\nGmail refused the message: quota\n```") {
		t.Fatalf("deliveries = %+v", rows)
	}

	echo.HoldRun = func(context.Context, builtinintegrations.Connection) (*builtinintegrations.Result, error) {
		return nil, &builtinintegrations.Refusal{Text: "alice@example.org is not one you may send to.", Reason: "outside send_to"}
	}
	holdCall(srv, "dana", emailHold)
	h = onlyHold(t, srv)
	code, out, _ = answerNeed(t, srv, "approve", integrationHoldNeedPrefix+h.id, "")
	if code != http.StatusOK || out.Result != "Approved, but Echo refused it: alice@example.org is not one you may send to. Dana is told in its chat" {
		t.Fatalf("approve: %d %+v", code, out)
	}
	if row := lastAudit(t, srv); row.Allowed || row.Reason != "approved, then refused: outside send_to" {
		t.Errorf("audit = %+v", row)
	}
	if rows, _ := heldDeliveries(t, srv, "dana"); len(rows) != 2 || !strings.Contains(rows[1].Content, "refused it, and nothing was done") {
		t.Errorf("deliveries = %+v", rows)
	}
}

// Why this test exists: the taint line is the owner's one warning that
// the agent asking may be acting on outside text. It must count what
// this agent's calls of this integration returned from outside the
// owner's domain in the day before asking, as the audit names it, and
// name the latest sender; rows of another agent, of more than a day
// ago, or with nobody outside must not count, and with none the line
// is empty rather than a reassurance.
func TestHeldCallTaintLine(t *testing.T) {
	srv, echo, _ := holdServer(t)
	intTeam(t, srv, "lee")
	putIntegrationAccess(t, srv, "echo", "lee", `{}`)
	seed := func(row store.IntegrationAuditRow) {
		t.Helper()
		if err := srv.Store.AppendIntegrationAudit("echo", row); err != nil {
			t.Fatal(err)
		}
	}
	seed(store.IntegrationAuditRow{At: holdT0.Add(-25 * time.Hour), Agent: "dana", Action: "echo", Allowed: true, Outside: []string{"old@example.net"}})
	seed(store.IntegrationAuditRow{At: holdT0.Add(-2 * time.Hour), Agent: "dana", Action: "echo", Allowed: true, Outside: []string{"bob@example.net", "carol@example.net"}})
	seed(store.IntegrationAuditRow{At: holdT0.Add(-time.Hour), Agent: "lee", Action: "echo", Allowed: true, Outside: []string{"lee-only@example.net"}})
	seed(store.IntegrationAuditRow{At: holdT0.Add(-time.Minute), Agent: "dana", Action: "whoami", Allowed: true})
	// The latest, through a real call of the built-in.
	echo.Outside = []string{"alice@example.org"}
	srv.callAgentIntegration(context.Background(), "dana", "echo_echo", nil)

	holdCall(srv, "dana", emailHold)
	if got := onlyHold(t, srv).taint; got != "Before asking, this agent read 3 messages or documents from outside your domain, the latest from alice@example.org." {
		t.Errorf("dana's taint = %q", got)
	}
	holdCall(srv, "lee", emailHold)
	for _, h := range srv.heldCalls.list() {
		if h.agent == "lee" && h.taint != "Before asking, this agent read 1 message or document from outside your domain, from lee-only@example.net." {
			t.Errorf("lee's taint = %q", h.taint)
		}
	}
	n, _ := needWithID(t, srv, integrationHoldNeedPrefix+srv.heldCalls.list()[0].id)
	if n.IntegrationHold == nil || !strings.HasPrefix(n.IntegrationHold.Taint, "Before asking") {
		t.Errorf("row = %+v", n.IntegrationHold)
	}
}

// Why this test exists: with nothing from outside in the audit, the
// row must carry no taint line at all, so the owner learns to read one
// as a real warning.
func TestHeldCallWithNothingFromOutsideHasNoTaintLine(t *testing.T) {
	srv, _, _ := holdServer(t)
	srv.callAgentIntegration(context.Background(), "dana", "echo_echo", nil)
	holdCall(srv, "dana", emailHold)
	h := onlyHold(t, srv)
	if h.taint != "" {
		t.Errorf("taint = %q", h.taint)
	}
	if n, _ := needWithID(t, srv, integrationHoldNeedPrefix+h.id); n.IntegrationHold == nil || n.IntegrationHold.Taint != "" {
		t.Errorf("row = %+v", n.IntegrationHold)
	}
}

// Why this test exists: disabling or disconnecting an integration is
// the owner turning it off for every agent. A call held on it must not
// stay approvable, to run the moment it is on again: each ends unrun
// as denied, with the reason in the audit and the agent told.
func TestHeldCallsEndWhenTheIntegrationIsTurnedOff(t *testing.T) {
	srv, echo, _ := holdServer(t)
	holdCall(srv, "dana", emailHold)
	h := onlyHold(t, srv)
	if rr := intPost(t, srv, intBase+"/echo/disable"); rr.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rr.Code, rr.Body.String())
	}
	waitHoldEnded(t, h)
	if row := lastAudit(t, srv); row.Allowed || row.Reason != "denied: the owner disabled the integration" {
		t.Errorf("audit = %+v", row)
	}
	rows, msgs := heldDeliveries(t, srv, "dana")
	if len(rows) != 1 || !strings.HasSuffix(rows[0].Content, "\n\nIt was not done: the owner disabled the Echo integration.") || msgs[0].Title != "Denied: Send an email to alice@example.org" {
		t.Fatalf("deliveries = %+v", rows)
	}
	if len(srv.heldCalls.list()) != 0 {
		t.Error("a call stayed held on a disabled integration")
	}

	intPost(t, srv, intBase+"/echo/enable")
	holdCall(srv, "dana", emailHold)
	h = onlyHold(t, srv)
	if rr := intDelete(t, srv, intBase+"/echo/connection"); rr.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", rr.Code, rr.Body.String())
	}
	waitHoldEnded(t, h)
	if row := lastAudit(t, srv); row.Allowed || row.Reason != "denied: the owner disconnected the integration" {
		t.Errorf("audit = %+v", row)
	}
	if r := ran(echo); len(r) != 0 {
		t.Errorf("a call ran after its integration was turned off: %+v", r)
	}
}

// Why this test exists: an offboarded agent's calls must not wait for
// an approval that would act for an agent no longer on the team. They
// end unrun with the reason in the audit, and nothing is written for
// an agent that is gone.
func TestHeldCallsEndWhenTheAgentIsOffboarded(t *testing.T) {
	srv, echo, _ := holdServer(t)
	if err := srv.Store.CreateAgent(store.Agent{Slug: "chief-of-staff", Role: "CoS", ReportsTo: agent.CEOSlug}, "# role\n"); err != nil {
		t.Fatal(err)
	}
	holdCall(srv, "dana", emailHold)
	h := onlyHold(t, srv)
	if rr := approve(t, srv, seedOffboardProposal(t, srv, "dana")); rr.Code != http.StatusOK {
		t.Fatalf("offboard: %d %s", rr.Code, rr.Body.String())
	}
	waitHoldEnded(t, h)
	if row := lastAudit(t, srv); row.Agent != "dana" || row.Allowed || row.Reason != "denied: the agent left the team" {
		t.Errorf("audit = %+v", row)
	}
	msgs, err := srv.Store.ListMessages(store.MessageFilter{Type: store.MsgHeldCall})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("a message was written for an agent that left: %+v", msgs)
	}
	if r := ran(echo); len(r) != 0 {
		t.Errorf("runs = %+v", r)
	}
}
