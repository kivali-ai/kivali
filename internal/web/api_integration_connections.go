// Package web (api_integration_connections.go) — an integration's
// connections under Org: saving, deleting and testing them.
//
// Every integration has the singular routes (PUT, DELETE
// .../connection and POST .../connection/verify), which act on its one
// connection, the oldest, and the plural ones (POST .../connections,
// PUT, DELETE .../connections/{id} and POST .../connections/{id}/verify),
// which act on the connection the id names. An integration whose
// manifest takes several connections (kivali.connection.multiple, only
// a built-in) has one per account the owner connects, each with a
// label unique within it; any other takes one, and a second is a 409.
// A connection's secret fields go to s.Secrets and nowhere else.

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
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/integration"
	"github.com/kivali-ai/kivali/internal/integrationpod"
	"github.com/kivali-ai/kivali/internal/secrets"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// connectionLabelMax bounds a connection's label, in characters.
const connectionLabelMax = 64

// errConnectionGone answers a route whose {id} names no connection.
const errConnectionGone = "that connection is no longer there"

// newConnectionID is a fresh connection id: 8 hex digits.
func newConnectionID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// connLabel is c's label: the stored one, or, for an integration that
// takes several connections and a connection stored with none, the one
// the built-in gives it from its fields that are not secret.
func (v *integrationView) connLabel(c *connView) string {
	if c.rec.Label != "" || !v.multiple() {
		return c.rec.Label
	}
	if l, ok := v.builtin.(builtinintegrations.ConnectionLabeler); ok {
		return l.ConnectionLabel(builtinintegrations.Connection{ID: c.rec.ID, Fields: c.rec.Fields})
	}
	return ""
}

// connectionState is c on the wire.
func (v *integrationView) connectionState(c *connView) apitypes.ConnectionState {
	return apitypes.ConnectionState{
		ID:              c.rec.ID,
		Label:           v.connLabel(c),
		CreatedAt:       c.rec.CreatedAt,
		Fields:          connectionFields(v.manifest, c),
		VerifiedAt:      c.rec.VerifiedAt,
		Verified:        c.rec.Verified,
		VerifyMessage:   optString(c.rec.VerifyMessage),
		ReconnectNeeded: c.reconnect,
	}
}

// connectionOr404 is the connection {id} names, answering 404 when v
// has none of that id.
func connectionOr404(w http.ResponseWriter, r *http.Request, v *integrationView) (*connView, bool) {
	c := v.connByID(r.PathValue("id"))
	if c == nil {
		writeAPIError(w, http.StatusNotFound, errConnectionGone, whoLink)
		return nil, false
	}
	return c, true
}

// handleAPIIntegrationConnectionPut writes the integration's one
// connection, the oldest, or makes it when there is none.
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
	s.saveConnection(w, r, v, v.first(), req, http.StatusOK)
}

// handleAPIIntegrationConnectionCreate adds a connection, answering 201
// with the page. An integration that takes one connection and has it
// is a 409.
func (s *Server) handleAPIIntegrationConnectionCreate(w http.ResponseWriter, r *http.Request) {
	var req apitypes.ConnectionWrite
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	if v.it.CurrentVersion != "" && !v.multiple() && len(v.conns) > 0 {
		writeAPIError(w, http.StatusConflict, v.displayName()+" takes one connection", whoYou)
		return
	}
	s.saveConnection(w, r, v, nil, req, http.StatusCreated)
}

// handleAPIIntegrationConnectionPutByID writes the connection {id}
// names.
func (s *Server) handleAPIIntegrationConnectionPutByID(w http.ResponseWriter, r *http.Request) {
	var req apitypes.ConnectionWrite
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	c, ok := connectionOr404(w, r, v)
	if !ok {
		return
	}
	s.saveConnection(w, r, v, c, req, http.StatusOK)
}

// saveConnection writes cur, or a new connection when cur is nil, from
// the approved version's connection form: the fields that are not
// sensitive to the store, the sensitive ones to the connection's
// Secret. A sensitive field left out keeps its value and an empty one
// clears it; a field that is not sensitive is what is sent. The label
// sent replaces the stored one, and an empty one keeps it; for an
// integration that takes several connections a label is required (the
// built-in's ConnectionLabel when the owner gives none) and unique
// ignoring case. The connection's last Test connection result is
// cleared. The caller holds lockIntegrations. The page answers with
// done, the route's status code.
func (s *Server) saveConnection(w http.ResponseWriter, r *http.Request, v *integrationView, cur *connView, req apitypes.ConnectionWrite, done int) {
	name := v.it.Name
	if v.it.CurrentVersion == "" {
		writeAPIError(w, http.StatusConflict, "approve a version of "+v.displayName()+" first; the connection follows the approved version's form", whoYou)
		return
	}
	m := v.manifest
	for _, k := range sortedFieldKeys(req.Fields) {
		if _, ok := m.UserConfig.Field(k); !ok {
			writeAPIError(w, http.StatusBadRequest, k+" is not a field of this integration's connection", whoDevelopers)
			return
		}
	}
	stored := map[string]string{}
	if cur != nil && cur.rec.SecretName != "" {
		got, err := s.Secrets.Get(r.Context(), cur.rec.SecretName)
		switch {
		case err == nil:
			stored = got
		case !errors.Is(err, secrets.ErrNotFound):
			log.Printf("integration %s: read the connection's Secret: %v", name, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection's secrets could not be read, so nothing was changed", whoServer)
			return
		}
	}
	fields, secret := map[string]string{}, map[string]string{}
	for _, f := range m.UserConfig {
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
				writeAPIError(w, http.StatusBadRequest, "the connection needs "+f.Title, whoYou)
				return
			}
			continue
		}
		if msg := connectionValueProblem(f, have); msg != "" {
			writeAPIError(w, http.StatusBadRequest, msg, whoYou)
			return
		}
	}
	// The connection as a built-in reads it: every field, secrets
	// included.
	conn := builtinintegrations.Connection{Fields: map[string]string{}}
	if cur != nil {
		conn.ID = cur.rec.ID
	}
	for _, f := range m.UserConfig {
		if val := cmp.Or(fields[f.Key], secret[f.Key]); val != "" {
			conn.Fields[f.Key] = val
		}
	}
	// A built-in may hold the connection to more than its form does.
	if c, ok := v.builtin.(builtinintegrations.ConnectionChecker); ok {
		if err := c.CheckConnection(conn); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error(), whoYou)
			return
		}
	}
	rec := store.IntegrationConnection{ID: newConnectionID(), CreatedAt: s.clk().Now(), Fields: fields}
	if cur != nil {
		rec.ID, rec.CreatedAt = cur.rec.ID, cur.rec.CreatedAt
	} else {
		for v.connByID(rec.ID) != nil {
			rec.ID = newConnectionID()
		}
	}
	label, msg, code := v.connectionLabelFor(cur, req.Label, rec)
	if msg != "" {
		writeAPIError(w, code, msg, whoYou)
		return
	}
	rec.Label = label
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
	if hostsOf != nil {
		proxySecret := stored[builtinintegrations.ProxySecretKey]
		if proxySecret == "" {
			var err error
			if proxySecret, err = egresscred.NewSecret(); err != nil {
				writeAPIError(w, http.StatusInternalServerError, "the connection's secrets could not be saved, so nothing was changed", whoServer)
				return
			}
		}
		secret[builtinintegrations.ProxySecretKey] = proxySecret
	}
	if len(secret) > 0 {
		if err := s.Secrets.Put(r.Context(), secretName, secret); err != nil {
			log.Printf("integration %s: write the connection's Secret: %v", name, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection's secrets could not be saved, so nothing was changed", whoServer)
			return
		}
		rec.SecretName = secretName
		rec.SecretKeys = keys
	} else if cur != nil && cur.rec.SecretName != "" {
		if err := s.Secrets.Delete(r.Context(), cur.rec.SecretName); err != nil {
			log.Printf("integration %s: delete the connection's Secret: %v", name, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection's secrets could not be cleared, so nothing was changed", whoServer)
			return
		}
	}
	if err := s.Store.PutIntegrationConnection(name, rec); err != nil {
		log.Printf("integration %s: write the connection: %v", name, err)
		writeAPIError(w, http.StatusInternalServerError, "the connection could not be saved", whoServer)
		return
	}
	// Registered on every save, so the proxy admits the host as it now
	// stands; only while the integration is on.
	if hostsOf != nil && v.it.Enabled {
		if err := s.registerBuiltinEgress(name, hostsOf, conn, secret[builtinintegrations.ProxySecretKey]); err != nil {
			log.Printf("integration %s: register the connection's proxy credential: %v", name, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection was saved, but its network access could not be set up; save it again", whoServer)
			return
		}
	}
	// The running server starts again with the new values.
	s.Integrations.ConnectionChanged(name)
	s.Integrations.Changed()
	s.writeIntegrationPageStatus(w, r, done)
}

// connectionLabelFor is the label rec is saved with: sent, trimmed; or,
// sent empty, cur's; or, for an integration that takes several
// connections, the built-in's ConnectionLabel from rec's fields. A
// label that is required and missing, too long, or another
// connection's (ignoring case) is refused with msg and code.
func (v *integrationView) connectionLabelFor(cur *connView, sent string, rec store.IntegrationConnection) (label, msg string, code int) {
	label = strings.TrimSpace(sent)
	if label == "" && cur != nil {
		label = cur.rec.Label
	}
	if label == "" && v.multiple() {
		label = v.connLabel(&connView{rec: rec})
	}
	switch {
	case label == "" && v.multiple():
		return "", "the connection needs a label", http.StatusBadRequest
	case utf8.RuneCountInString(label) > connectionLabelMax:
		return "", fmt.Sprintf("a connection's label is at most %d characters", connectionLabelMax), http.StatusBadRequest
	case label == "":
		return "", "", 0
	}
	for _, c := range v.conns {
		if c != cur && strings.EqualFold(v.connLabel(c), label) {
			return "", v.displayName() + " already has a connection called " + v.connLabel(c), http.StatusConflict
		}
	}
	return label, "", 0
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

// handleAPIIntegrationConnectionDelete removes every connection: their
// Secrets, their records and every agent's access, which was set for
// one of them.
func (s *Server) handleAPIIntegrationConnectionDelete(w http.ResponseWriter, r *http.Request) {
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	if !s.deleteConnectionSecrets(w, r, v.it.Name) {
		return
	}
	removed, err := s.Store.DeleteIntegrationConnection(v.it.Name)
	if err != nil {
		log.Printf("integration %s: delete the connection: %v", v.it.Name, err)
		writeAPIError(w, http.StatusInternalServerError, "the connection could not be removed", whoServer)
		return
	}
	// A built-in's connection's proxy credential goes with it.
	if err := s.dropBuiltinEgress(v.it.Name, removed); err != nil {
		log.Printf("integration %s: drop the connection's proxy credential: %v", v.it.Name, err)
	}
	// The running server starts again without the values.
	s.Integrations.ConnectionChanged(v.it.Name)
	s.Integrations.Changed()
	s.writeIntegrationPage(w, r)
}

// handleAPIIntegrationConnectionDeleteByID removes the connection {id}
// names: its Secret, its scoped proxy credential, its record and the
// access of every agent whose row names it. The other connections, and
// the access set for them, stay.
func (s *Server) handleAPIIntegrationConnectionDeleteByID(w http.ResponseWriter, r *http.Request) {
	defer s.lockIntegrations()()
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	c, ok := connectionOr404(w, r, v)
	if !ok {
		return
	}
	name := v.it.Name
	// The Secret goes first: on a failure the record stays, so the
	// Secret is not left behind with nothing naming it.
	if c.rec.SecretName != "" {
		if err := s.Secrets.Delete(r.Context(), c.rec.SecretName); err != nil {
			log.Printf("integration %s: delete Secret %s: %v", name, c.rec.SecretName, err)
			writeAPIError(w, http.StatusInternalServerError, "the connection's secrets could not be removed, so nothing was changed", whoServer)
			return
		}
	}
	removed, _, err := s.Store.DeleteIntegrationConnectionByID(name, c.rec.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("integration %s: delete connection %s: %v", name, c.rec.ID, err)
		writeAPIError(w, http.StatusInternalServerError, "the connection could not be removed", whoServer)
		return
	}
	if err == nil {
		if err := s.dropBuiltinEgress(name, []store.IntegrationConnection{removed}); err != nil {
			log.Printf("integration %s: drop the connection's proxy credential: %v", name, err)
		}
	}
	s.Integrations.ConnectionChanged(name)
	s.Integrations.Changed()
	s.writeIntegrationPage(w, r)
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
func (s *Server) callIntegrationTool(r *http.Request, v *integrationView, tool string, builtin func(context.Context) (json.RawMessage, *strings.Replacer, error)) (res toolResult, failed string, err error) {
	name := v.it.Name
	start := s.clk().Now()
	var raw json.RawMessage
	var scrub *strings.Replacer
	var callErr error
	if v.builtin != nil {
		raw, scrub, callErr = builtin(r.Context())
	} else {
		raw, callErr = s.Integrations.Call(r.Context(), name, "tools/call", map[string]any{"name": tool, "arguments": map[string]any{}})
	}
	var rpcErr *integrationpod.RPCError
	switch {
	case errors.Is(callErr, errIntegrationNotRunning), r.Context().Err() != nil:
		return res, "", cmp.Or(callErr, r.Context().Err())
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
	// or audited. With no scrubber (the secrets could not be read) the
	// words are held back the way an agent's answer is.
	if scrub == nil {
		scrub = s.Integrations.scrubber(r.Context(), name)
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
// one connection, the oldest.
func (s *Server) handleAPIIntegrationVerify(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	s.verifyConnection(w, r, v, v.first())
}

// handleAPIIntegrationVerifyByID is Test connection of the connection
// {id} names.
func (s *Server) handleAPIIntegrationVerifyByID(w http.ResponseWriter, r *http.Request) {
	v, ok := s.integrationOr404(w, r)
	if !ok {
		return
	}
	c, ok := connectionOr404(w, r, v)
	if !ok {
		return
	}
	s.verifyConnection(w, r, v, c)
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
		writeAPIError(w, http.StatusConflict, "connect the integration first", whoYou)
		return
	case v.manifest.Kivali.Verify == "":
		writeAPIError(w, http.StatusConflict, v.displayName()+" has no connection test", v.whoBuilt())
		return
	case !v.running:
		writeAPIError(w, http.StatusConflict, errNotRunning, whoBundle)
		return
	}
	connID := c.rec.ID
	res, failed, err := s.callIntegrationTool(r, v, v.manifest.Kivali.Verify, func(ctx context.Context) (json.RawMessage, *strings.Replacer, error) {
		return s.builtinOwnerCall(ctx, v.builtin, name, connID, true, "")
	})
	if errors.Is(err, errIntegrationNotRunning) {
		writeAPIError(w, http.StatusConflict, errNotRunning, v.whoBuilt())
		return
	}
	if err != nil {
		return
	}
	verified, message := failed == "", failed
	if verified {
		message = clipText(res.firstText(), integrationVerifyMessageMax)
	}
	unlock := s.lockIntegrations()
	conn, err := s.Store.GetIntegrationConnectionByID(name, connID)
	if err == nil {
		at := s.clk().Now().UTC()
		conn.VerifiedAt, conn.Verified, conn.VerifyMessage = &at, &verified, message
		err = s.Store.PutIntegrationConnection(name, conn)
	}
	unlock()
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.Printf("integration %s: keep the connection test: %v", name, err)
		writeAPIError(w, http.StatusInternalServerError, "the connection test's result could not be saved", whoServer)
		return
	}
	s.writeIntegrationPage(w, r)
}
