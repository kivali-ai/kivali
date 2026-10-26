// Package web (api_integration_connections.go) — an integration's
// connection under Org: saving and testing it.
//
// An integration has one connection: PUT .../connection writes it (or
// makes it when there is none) and POST .../connection/verify tests
// it. Uninstall (DELETE .../integrations/{name}) removes it. A
// connection's secret fields go to s.Secrets and nowhere else.

package web

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// errConnectionGone answers a sign-in whose connection went while it
// ran.
const errConnectionGone = "that connection is no longer there"

// newConnectionID is a fresh connection id for name: 8 hex digits that
// name no record of it, its connection's or one set aside
// (store.IntegrationConnectionIDTaken), so the new connection's Secret
// name is its own.
func (s *Server) newConnectionID(name string) string {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		if id := hex.EncodeToString(b[:]); !s.Store.IntegrationConnectionIDTaken(name, id) {
			return id
		}
	}
}

// connectionTest reports whether the version in use has a connection
// test: its manifest names kivali.verify.
func (v *integrationView) connectionTest() bool {
	return v.manifest != nil && v.manifest.Kivali.Verify != ""
}

// connectionState is c on the wire. A result Test connection kept
// under a version that had a test is left out once the version in use
// has none, since the owner can no longer run it again.
func (v *integrationView) connectionState(c *connView) apitypes.ConnectionState {
	state := apitypes.ConnectionState{
		ID:              c.rec.ID,
		CreatedAt:       c.rec.CreatedAt,
		Fields:          connectionFields(v.manifest, c),
		ReconnectNeeded: c.reconnect,
	}
	if v.connectionTest() {
		state.VerifiedAt, state.Verified, state.VerifyMessage = c.rec.VerifiedAt, c.rec.Verified, optString(c.rec.VerifyMessage)
	}
	if v.signIn() {
		state.SignIn = v.connectionSignIn(c)
	}
	return state
}

// connectionSignIn is c's sign-in on the wire: how the owner may sign
// in and, while the connection works, how and when they did and when
// its token is next refreshed; or why they must sign in again.
func (v *integrationView) connectionSignIn(c *connView) *apitypes.ConnectionSignIn {
	out := &apitypes.ConnectionSignIn{Ways: offeredSignInWays(v.manifest.Kivali.Connection), SignedIn: !c.rec.NeedsSignIn(c.hasToken)}
	si := c.rec.SignIn
	if si == nil {
		return out
	}
	if out.SignedIn {
		out.Method = si.Method
		at := si.SignedInAt
		out.SignedInAt = &at
		if !si.ExpiresAt.IsZero() {
			exp := si.ExpiresAt
			out.ExpiresAt = &exp
		}
	}
	out.Error = optString(si.Error)
	return out
}

// handleAPIIntegrationConnectionPut writes the integration's
// connection, or makes it when there is none.
func (s *Server) handleAPIIntegrationConnectionPut(w http.ResponseWriter, r *http.Request) {
	var req apitypes.ConnectionWrite
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	if werr := s.writeConnection(r.Context(), v, v.conn, req, true); werr != nil {
		writeAPIError(w, werr.status, werr.words, werr.who)
		return
	}
	s.writeIntegrationPage(w, r)
}

// connWriteError is why a connection was not written: the status and
// words to answer, and who fixes it.
type connWriteError struct {
	status     int
	words, who string
}

func (e *connWriteError) Error() string { return e.words }

// connectionWrite is a save worked out (planConnectionWrite): the
// fields that are not sensitive, the sensitive ones, what the Secret
// held, and the connection as a built-in reads it.
type connectionWrite struct {
	fields, secret, stored map[string]string
	conn                   builtinintegrations.Connection
}

// planConnectionWrite works out a save of cur, or of a new connection
// when cur is nil, from the approved version's connection form, and
// refuses one the form or the built-in does not take: a field the form
// has not, a required one with no value, a value of the wrong type,
// the built-in's own check (ConnectionChecker). A sensitive field left
// out keeps its value and an empty one clears it; a field that is not
// sensitive is what is sent. Nothing is written.
func (s *Server) planConnectionWrite(ctx context.Context, v *integrationView, cur *connView, req apitypes.ConnectionWrite) (*connectionWrite, *connWriteError) {
	name := v.it.Name
	if v.it.CurrentVersion == "" {
		return nil, &connWriteError{http.StatusConflict, "approve a version of " + v.displayName() + " first; the connection follows the approved version's form", whoYou}
	}
	m := v.manifest
	form := integration.ConnectionFields(m)
	for _, k := range sortedFieldKeys(req.Fields) {
		if _, ok := form.Field(k); !ok {
			return nil, &connWriteError{http.StatusBadRequest, k + " is not a field of this integration's connection", whoDevelopers}
		}
	}
	stored := map[string]string{}
	if cur != nil && cur.rec.SecretName != "" {
		got, err := s.Secrets.Get(ctx, cur.rec.SecretName)
		switch {
		case err == nil:
			stored = got
		case !errors.Is(err, secrets.ErrNotFound):
			log.Printf("integration %s: read the connection's Secret: %v", name, err)
			return nil, &connWriteError{http.StatusInternalServerError, "the connection's secrets could not be read, so nothing was changed", whoServer}
		}
	}
	fields, secret := map[string]string{}, map[string]string{}
	for _, f := range form {
		val, sent := req.Fields[f.Key]
		val = strings.TrimSpace(val)
		switch {
		case f.Sensitive && sent:
			if val != "" {
				secret[f.Key] = val
			}
		case f.Sensitive:
			if old := stored[f.Key]; old != "" {
				secret[f.Key] = old
			}
		case val != "":
			fields[f.Key] = val
		}
		have := cmp.Or(fields[f.Key], secret[f.Key])
		if have == "" {
			if f.Required {
				return nil, &connWriteError{http.StatusBadRequest, "the connection needs " + f.Title, whoYou}
			}
			continue
		}
		if msg := connectionValueProblem(f, have); msg != "" {
			return nil, &connWriteError{http.StatusBadRequest, msg, whoYou}
		}
	}
	// The connection as a built-in reads it: every field, secrets
	// included.
	conn := builtinintegrations.Connection{Fields: map[string]string{}}
	if cur != nil {
		conn.ID = cur.rec.ID
	}
	for _, f := range form {
		if f.Key == integration.FieldClientSecret && m.Kivali.Connection.SignIn() {
			// Core's alone, as on every call.
			continue
		}
		if val := cmp.Or(fields[f.Key], secret[f.Key]); val != "" {
			conn.Fields[f.Key] = val
		}
	}
	// A built-in may hold the connection to more than its form does.
	if c, ok := v.builtin.(builtinintegrations.ConnectionChecker); ok {
		if err := c.CheckConnection(conn); err != nil {
			return nil, &connWriteError{http.StatusBadRequest, err.Error(), whoYou}
		}
	}
	return &connectionWrite{fields: fields, secret: secret, stored: stored, conn: conn}, nil
}

// writeConnection writes cur, or a new connection when cur is nil, as
// planConnectionWrite works it out: the fields that are not sensitive
// to the store, the sensitive ones to the connection's Secret. The
// connection's last Test connection result is
// cleared, and the running server starts again with the new values.
// With endFlows, a sign-in connection's flow under way ends when the
// app's client id or secret changes. The caller holds lockIntegrations.
func (s *Server) writeConnection(ctx context.Context, v *integrationView, cur *connView, req apitypes.ConnectionWrite, endFlows bool) *connWriteError {
	plan, werr := s.planConnectionWrite(ctx, v, cur, req)
	if werr != nil {
		return werr
	}
	name, m := v.it.Name, v.manifest
	fields, secret, stored, conn := plan.fields, plan.secret, plan.stored, plan.conn
	rec := store.IntegrationConnection{CreatedAt: s.clk().Now(), Fields: fields}
	if cur != nil {
		// A sign-in stays: saving the form changes its fields, not the
		// tokens the owner signed in for.
		rec.ID, rec.CreatedAt, rec.SignIn = cur.rec.ID, cur.rec.CreatedAt, cur.rec.SignIn
		// A flow's write leaves an install's connection unfinished as it
		// was; the owner's own save makes it theirs (setupAdopt).
		if !endFlows {
			rec.SetupPending, rec.SetupTakenOver = cur.rec.SetupPending, cur.rec.SetupTakenOver
		}
	} else {
		rec.ID = s.newConnectionID(name)
	}
	conn.ID = rec.ID
	secretName := secrets.ConnectionSecretName(name, rec.ID)
	if cur != nil && cur.rec.SecretName != "" {
		secretName = cur.rec.SecretName
	}
	// A built-in whose connection reaches hosts through the proxy keeps
	// the connection's proxy credential in the same Secret: the one
	// stored, or a new one. It is no field of the form, so it is not
	// among the keys the owner is shown.
	hostsOf := builtinHostsOf(v.builtin)
	keys := sortedFieldKeys(secret)
	// A sign-in's tokens and a registered client are kept in the same
	// Secret, and are no field of the form either.
	for _, k := range connect.SecretKeys {
		if t := stored[k]; t != "" {
			secret[k] = t
		}
	}
	if hostsOf != nil {
		proxySecret := stored[builtinintegrations.ProxySecretKey]
		if proxySecret == "" {
			var err error
			if proxySecret, err = egresscred.NewSecret(); err != nil {
				return &connWriteError{http.StatusInternalServerError, "the connection's secrets could not be saved, so nothing was changed", whoServer}
			}
		}
		secret[builtinintegrations.ProxySecretKey] = proxySecret
	}
	if len(secret) > 0 {
		if err := s.Secrets.Put(ctx, secretName, secret); err != nil {
			log.Printf("integration %s: write the connection's Secret: %v", name, err)
			return &connWriteError{http.StatusInternalServerError, "the connection's secrets could not be saved, so nothing was changed", whoServer}
		}
		rec.SecretName = secretName
		rec.SecretKeys = keys
	} else if cur != nil && cur.rec.SecretName != "" {
		if err := s.Secrets.Delete(ctx, cur.rec.SecretName); err != nil {
			log.Printf("integration %s: delete the connection's Secret: %v", name, err)
			return &connWriteError{http.StatusInternalServerError, "the connection's secrets could not be cleared, so nothing was changed", whoServer}
		}
	}
	if err := s.Store.PutIntegrationConnection(name, rec); err != nil {
		log.Printf("integration %s: write the connection: %v", name, err)
		return &connWriteError{http.StatusInternalServerError, "the connection could not be saved", whoServer}
	}
	// Registered on every save, so the proxy admits the host as it now
	// stands; only while the integration is on.
	if hostsOf != nil && v.it.Enabled {
		if err := s.registerBuiltinEgress(name, hostsOf, conn, secret[builtinintegrations.ProxySecretKey]); err != nil {
			log.Printf("integration %s: register the connection's proxy credential: %v", name, err)
			return &connWriteError{http.StatusInternalServerError, "the connection was saved, but its network access could not be set up; save it again", whoServer}
		}
	}
	// The owner's own save: the connection is theirs, whatever becomes
	// of an install under way.
	if endFlows {
		s.setupAdopt(name)
	}
	// A sign-in under way was started with the app's old client id and
	// secret: its tokens would be the old app's.
	if endFlows && cur != nil && m.Kivali.Connection.SignIn() &&
		(cur.rec.Fields[integration.FieldClientID] != fields[integration.FieldClientID] || stored[integration.FieldClientSecret] != secret[integration.FieldClientSecret]) {
		s.setupCancelKeep(name)
	}
	// The running server starts again with the new values.
	s.Integrations.ConnectionChanged(name)
	s.Integrations.Changed()
	return nil
}

// connectionValueProblem is what is wrong with a connection field's
// value for its type, or "". It never quotes the value, which may be a
// secret.
func connectionValueProblem(f integration.UserConfigField, val string) string {
	switch f.Type {
	case "number":
		if _, err := strconv.ParseFloat(val, 64); err != nil {
			return f.Title + " must be a number"
		}
	case "boolean":
		if val != "true" && val != "false" {
			return f.Title + " must be true or false"
		}
	}
	return ""
}

func sortedFieldKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// integrationVerifyMessageMax bounds the integration's words kept from
// Test connection.
const integrationVerifyMessageMax = 1000

// toolResult is the part of an MCP tools/call result Kivali reads.
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

// firstText is the result's first text content.
func (t toolResult) firstText() string {
	for _, c := range t.Content {
		if c.Type == "text" {
			return c.Text
		}
	}
	return ""
}

// callIntegrationTool calls one of v's tools for the owner (no agent),
// writes the audit row with tool as its action, and returns the result.
// For an integration that ships with Kivali, builtin makes the call
// instead, answering as the server's tools/call would, with the
// scrubber of the connection it called with. failed is the
// integration's words when the call did not go through: the server's
// error, its tool's error result, or the deadline.
// errIntegrationNotRunning and a caller that went away are returned as
// err, with no audit row.
func (s *Server) callIntegrationTool(ctx context.Context, v *integrationView, tool string, builtin func(context.Context) (json.RawMessage, *strings.Replacer, error)) (res toolResult, failed string, err error) {
	name := v.it.Name
	start := s.clk().Now()
	var raw json.RawMessage
	var scrub *strings.Replacer
	var callErr error
	if v.builtin != nil {
		raw, scrub, callErr = builtin(ctx)
	} else {
		raw, callErr = s.Integrations.Call(ctx, name, "tools/call", map[string]any{"name": tool, "arguments": map[string]any{}})
	}
	var rpcErr *integrationpod.RPCError
	switch {
	case errors.Is(callErr, errIntegrationNotRunning), ctx.Err() != nil:
		return res, "", cmp.Or(callErr, ctx.Err())
	case errors.Is(callErr, errIntegrationTimeout):
		failed = fmt.Sprintf("the integration did not answer within %d seconds", int(integrationCallTimeout/time.Second))
	case errors.As(callErr, &rpcErr):
		failed = rpcErr.Message
	case callErr != nil:
		failed = callErr.Error()
	default:
		if json.Unmarshal(raw, &res) != nil {
			failed = "the integration's answer is not one Kivali reads"
		} else if res.IsError {
			failed = cmp.Or(res.firstText(), "the integration reported an error")
		}
	}
	failed = clipText(failed, integrationVerifyMessageMax)
	// The integration's words may quote a connection secret, as an
	// agent's call's answer may: scrubbed before they are kept, shown
	// or audited, a built-in's with the secrets of the connection it
	// called with, a pod's with its one connection's. With no scrubber
	// (the secrets could not be read) the words are held back the way
	// an agent's answer is.
	if v.builtin == nil {
		scrub = s.Integrations.scrubber(ctx, name)
	}
	if scrub != nil {
		failed = scrub.Replace(failed)
		for i := range res.Content {
			res.Content[i].Text = scrub.Replace(res.Content[i].Text)
		}
	} else {
		res = toolResult{}
		failed = "Kivali could not check the integration's answer for secrets, so it held the answer back"
	}
	row := store.IntegrationAuditRow{
		At:         start,
		Agent:      agent.CEOSlug,
		Action:     tool,
		Allowed:    true,
		Reason:     failed,
		DurationMS: s.clk().Now().Sub(start).Milliseconds(),
	}
	if err := s.Store.AppendIntegrationAudit(name, row); err != nil {
		log.Printf("integration %s: audit: %v", name, err)
	}
	return res, failed, nil
}

// handleAPIIntegrationVerify is Test connection of the integration's
// connection.
func (s *Server) handleAPIIntegrationVerify(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	s.verifyConnection(w, r, v, v.conn)
}

// verifyConnection is Test connection of c (nil: none): it calls the
// integration's verify action through the running server, or a
// built-in's Verify with c, and keeps what it found on c.
func (s *Server) verifyConnection(w http.ResponseWriter, r *http.Request, v *integrationView, c *connView) {
	name := v.it.Name
	switch {
	case v.staleBuiltin():
		writeAPIError(w, http.StatusConflict, builtinRefusal(v.displayName(), builtinNotIncluded), whoYou)
		return
	case v.it.CurrentVersion == "":
		writeAPIError(w, http.StatusConflict, "approve a version of "+v.displayName()+" first", whoYou)
		return
	case c == nil:
		writeAPIError(w, http.StatusConflict, "connect "+v.displayName()+" first", whoYou)
		return
	case v.several:
		writeAPIError(w, http.StatusConflict, v.displayName()+" holds more than one connection record and Kivali could not bring it to one; see the log", whoServer)
		return
	case v.manifest.Kivali.Verify == "":
		writeAPIError(w, http.StatusConflict, v.displayName()+" has no connection test", v.whoBuilt())
		return
	case !v.running:
		writeAPIError(w, http.StatusConflict, errNotRunning, v.whoBuilt())
		return
	}
	callErr, keepErr := s.testConnection(r.Context(), v, c.rec.ID)
	switch {
	case errors.Is(callErr, errIntegrationNotRunning):
		writeAPIError(w, http.StatusConflict, errNotRunning, v.whoBuilt())
		return
	case callErr != nil:
		return
	case keepErr != nil:
		log.Printf("integration %s: keep the connection test: %v", name, keepErr)
		writeAPIError(w, http.StatusInternalServerError, "the connection test's result could not be saved", whoServer)
		return
	}
	s.writeIntegrationPage(w, r)
}

// testConnection runs v's connection test (kivali.verify) on its
// connection connID for the owner and keeps what it found on the
// connection's record: verified or not, the integration's words, when.
// callErr is errIntegrationNotRunning, or ctx's error, when the test
// did not run; keepErr is the store's when the result could not be
// kept. A connection deleted meanwhile keeps nothing, and that is no
// error.
func (s *Server) testConnection(ctx context.Context, v *integrationView, connID string) (callErr, keepErr error) {
	name := v.it.Name
	res, failed, err := s.callIntegrationTool(ctx, v, v.manifest.Kivali.Verify, func(ctx context.Context) (json.RawMessage, *strings.Replacer, error) {
		return s.builtinOwnerCall(ctx, v.builtin, name, connID, true, "")
	})
	if err != nil {
		return err, nil
	}
	verified, message := failed == "", failed
	if verified {
		message = clipText(res.firstText(), integrationVerifyMessageMax)
	}
	unlock := s.lockIntegrations()
	defer unlock()
	conn, err := s.Store.GetIntegrationConnectionByID(name, connID)
	if err == nil {
		at := s.clk().Now().UTC()
		conn.VerifiedAt, conn.Verified, conn.VerifyMessage = &at, &verified, message
		err = s.Store.PutIntegrationConnection(name, conn)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return nil, nil
}
