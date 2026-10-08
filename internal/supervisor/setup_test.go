package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kivali-ai/kivali/internal/claudeauth"
)

// The setup the driver declares today is the test data here: Microsoft
// Foundry, whose status `claude auth status` reports like this.
const foundryStatus = `{"loggedIn":true,"authMethod":"third_party","apiProvider":"foundry"}`

// testModels are the harness's Options.Models.
var testModels = []string{"claude-haiku-4-5", "claude-sonnet-5", "claude-opus-5-5"}

func foundryValues(resource, key string) map[string]string {
	return map[string]string{"resource": resource, "auth": "api_key", "api_key": key}
}

// captureLog sends the supervisor's log to a slice the test reads.
func captureLog(h *harness) func() string {
	var mu sync.Mutex
	var lines []string
	h.sup.o.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(lines, "\n")
	}
}

func settingsEnv(t *testing.T, h *harness) (map[string]any, map[string]any) {
	t.Helper()
	h.vm.mu.Lock()
	defer h.vm.mu.Unlock()
	if h.vm.claudeSettings == nil {
		t.Fatal("no settings file")
	}
	var all map[string]any
	if err := json.Unmarshal([]byte(*h.vm.claudeSettings), &all); err != nil {
		t.Fatal(err)
	}
	env, _ := all["env"].(map[string]any)
	return all, env
}

// Applying a setup writes the driver's env block into the CLI's
// settings, keeping the rest and replacing the provider there before;
// the secret goes on stdin only, and the answer is the CLI's new
// sign-in and each model's check, missing ones included.
func TestApplySetup(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	log := captureLog(h)
	before := `{"theme":"dark","env":{"CLAUDE_CODE_USE_BEDROCK":"1","AWS_REGION":"us-east-1","DISABLE_TELEMETRY":"1"}}`
	h.vm.claudeSettings = &before
	h.vm.authStatus = foundryStatus
	models := testModels
	missing, refused := models[len(models)-1], models[0]
	h.vm.probes = map[string]string{
		missing: `{"is_error":true,"api_error_status":404,"result":"The model ` + missing + ` is not available on your foundry deployment."}`,
		refused: `{"is_error":true,"api_error_status":401,"result":"authentication failed · API Error: 401 Access denied"}`,
	}
	res, err := h.sup.ApplySetup(context.Background(), SetupRequest{Setup: claudeauth.SetupMicrosoftFoundry, Values: foundryValues(" my-resource ", "SECRET-KEY")})
	if err != nil {
		t.Fatal(err)
	}
	all, env := settingsEnv(t, h)
	if all["theme"] != "dark" || env["DISABLE_TELEMETRY"] != "1" || env["CLAUDE_CODE_USE_BEDROCK"] != nil || env["AWS_REGION"] != nil {
		t.Errorf("settings %v", all)
	}
	if env["CLAUDE_CODE_USE_FOUNDRY"] != "1" || env["ANTHROPIC_FOUNDRY_RESOURCE"] != "my-resource" || env["ANTHROPIC_FOUNDRY_API_KEY"] != "SECRET-KEY" {
		t.Errorf("env %v", env)
	}
	if !res.Credential.SignedIn || res.Credential.Billing != "Microsoft Foundry · my-resource" {
		t.Errorf("credential %+v", res.Credential)
	}
	var got []string
	for _, m := range res.Models {
		got = append(got, m.Model)
		var want ModelCheck
		switch m.Model {
		case missing:
			want = ModelCheck{Model: m.Model, Missing: true, Problem: "No deployment named " + missing + " in my-resource"}
		case refused:
			want = ModelCheck{Model: m.Model, Problem: "the provider refused the sign-in (HTTP 401)"}
		default:
			want = ModelCheck{Model: m.Model, OK: true}
		}
		if m != want {
			t.Errorf("%+v, want %+v", m, want)
		}
	}
	if !slices.Equal(got, models) {
		t.Errorf("checked %v, want %v", got, models)
	}
	h.vm.mu.Lock()
	argv := strings.Join(append(append([]string(nil), h.vm.settingsWrites...), h.vm.probed...), "\n")
	probes := len(h.vm.probed)
	h.vm.mu.Unlock()
	if probes != len(models) {
		t.Errorf("%d checks for %d models", probes, len(models))
	}
	b, _ := json.Marshal(res)
	for where, s := range map[string]string{"argv": argv, "answer": string(b), "log": log()} {
		if strings.Contains(s, "SECRET") {
			t.Errorf("the secret is in the %s: %s", where, s)
		}
	}

	// The status names the target from then on; another way of signing
	// in replaces the key.
	if st, err := h.sup.Credential(context.Background()); err != nil || st.Billing != "Microsoft Foundry · my-resource" {
		t.Errorf("credential %+v %v", st, err)
	}
	sp := map[string]string{"resource": "res2", "auth": "service_principal", "tenant_id": "t", "client_id": "c", "client_secret": "SECRET-2"}
	if _, err := h.sup.ApplySetup(context.Background(), SetupRequest{Setup: claudeauth.SetupMicrosoftFoundry, Values: sp}); err != nil {
		t.Fatal(err)
	}
	_, env = settingsEnv(t, h)
	if env["ANTHROPIC_FOUNDRY_API_KEY"] != nil || env["AZURE_CLIENT_SECRET"] != "SECRET-2" || env["ANTHROPIC_FOUNDRY_RESOURCE"] != "res2" {
		t.Errorf("second setup env %v", env)
	}
	info, err := h.sup.SetupInfo(context.Background())
	if err != nil || !slices.Equal(info.Models, models) || info.Current == nil || info.Current.Setup != claudeauth.SetupMicrosoftFoundry ||
		info.Current.Values["resource"] != "res2" || info.Current.Values["client_secret"] != "" {
		t.Errorf("info %+v %v", info, err)
	}
}

// A settings file that is not JSON is left alone, and nothing is
// checked.
func TestApplySetupKeepsAnUnreadableSettingsFile(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	bad := `{"env": `
	h.vm.claudeSettings = &bad
	if _, err := h.sup.ApplySetup(context.Background(), SetupRequest{Setup: claudeauth.SetupMicrosoftFoundry, Values: foundryValues("r", "k")}); err == nil {
		t.Fatal("saved over a broken settings file")
	}
	h.vm.mu.Lock()
	defer h.vm.mu.Unlock()
	if *h.vm.claudeSettings != bad || len(h.vm.settingsWrites) != 0 || len(h.vm.probed) != 0 {
		t.Errorf("settings %q, writes %v, checks %v", *h.vm.claudeSettings, h.vm.settingsWrites, h.vm.probed)
	}
}

// Sign in again starts from a CLI with no provider variables: clearing
// removes them and keeps the rest, and with none there writes nothing.
func TestClearProvider(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	ctx := context.Background()
	if res, err := h.sup.ClearProvider(ctx); err != nil || len(res.Cleared) != 0 {
		t.Fatalf("no settings: %+v %v", res, err)
	}
	s := `{"model":"opus","env":{"CLAUDE_CODE_USE_FOUNDRY":"1","ANTHROPIC_FOUNDRY_RESOURCE":"r","ANTHROPIC_FOUNDRY_API_KEY":"k","DISABLE_TELEMETRY":"1"}}`
	h.vm.claudeSettings = &s
	res, err := h.sup.ClearProvider(ctx)
	if err != nil || !slices.Equal(res.Cleared, []string{"ANTHROPIC_FOUNDRY_API_KEY", "ANTHROPIC_FOUNDRY_RESOURCE", "CLAUDE_CODE_USE_FOUNDRY"}) {
		t.Fatalf("clear %+v %v", res, err)
	}
	all, env := settingsEnv(t, h)
	if all["model"] != "opus" || len(env) != 1 || env["DISABLE_TELEMETRY"] != "1" {
		t.Errorf("after %v", all)
	}
	if res, err := h.sup.ClearProvider(ctx); err != nil || len(res.Cleared) != 0 {
		t.Fatalf("again: %+v %v", res, err)
	}
	h.vm.mu.Lock()
	writes := len(h.vm.settingsWrites)
	h.vm.mu.Unlock()
	if writes != 1 {
		t.Errorf("%d writes, want 1", writes)
	}
}

// Over the RPC: 409 before the org runs, 400 for values the driver
// refuses (never echoing them), the JSON shapes the shell reads.
func TestSetupRPC(t *testing.T) {
	h := newHarness(t)
	srv := httptest.NewServer(NewRPCServer(h.sup).Handler())
	defer srv.Close()
	post := func(path, body string) (int, string) {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	good := `{"setup":"microsoft-foundry","values":{"resource":"my-resource","auth":"api_key","api_key":"SECRET-KEY"}}`
	if code, body := post("/v1/credential/setup", good); code != http.StatusConflict || !strings.Contains(body, "not running") {
		t.Fatalf("not running: %d %q", code, body)
	}
	if code, _ := post("/v1/credential/clear-provider", `{}`); code != http.StatusConflict {
		t.Fatalf("clear, not running: %d", code)
	}
	h.mustUp(UpOptions{Install: owner})
	for _, body := range []string{
		`{"setup":"microsoft-foundry","values":{"resource":"https://r.services.ai.azure.com","auth":"api_key","api_key":"SECRET-KEY"}}`,
		`{"setup":"microsoft-foundry","values":{"resource":"r","auth":"service_principal","client_secret":"SECRET-KEY"}}`,
		`{"setup":"nope","values":{"api_key":"SECRET-KEY"}}`,
		`{"setup":"microsoft-foundry"`,
	} {
		code, msg := post("/v1/credential/setup", body)
		if code != http.StatusBadRequest || strings.Contains(msg, "SECRET") {
			t.Errorf("%s: %d %q", body, code, msg)
		}
	}
	h.vm.authStatus = foundryStatus
	code, body := post("/v1/credential/setup", good)
	if code != http.StatusOK || strings.Contains(body, "SECRET") {
		t.Fatalf("apply: %d %q", code, body)
	}
	var res SetupResult
	if err := json.Unmarshal([]byte(body), &res); err != nil || res.Credential.Billing != "Microsoft Foundry · my-resource" || len(res.Models) == 0 {
		t.Fatalf("apply answer %s: %v", body, err)
	}
	resp, err := http.Get(srv.URL + "/v1/credential/setup")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(b), "SECRET") ||
		!strings.Contains(string(b), `"current":{"setup":"microsoft-foundry","values":{"auth":"api_key","resource":"my-resource"}}`) {
		t.Fatalf("info: %s %s", resp.Status, b)
	}
	if code, body := post("/v1/credential/clear-provider", `{}`); code != http.StatusOK || !strings.Contains(body, `"CLAUDE_CODE_USE_FOUNDRY"`) {
		t.Fatalf("clear: %d %s", code, body)
	}
}

// The JSON the shell's wire.rs (credential_setup_matches_the_supervisor)
// reads and sends.
func TestSetupWireShapes(t *testing.T) {
	enc := func(v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := enc(SetupInfo{Models: []string{"claude-haiku-4-5"}, Current: &SavedSetup{Setup: "s", Values: map[string]string{"resource": "r"}}}); got != `{"models":["claude-haiku-4-5"],"current":{"setup":"s","values":{"resource":"r"}}}` {
		t.Errorf("info %s", got)
	}
	if got := enc(SetupInfo{Models: []string{}}); got != `{"models":[]}` {
		t.Errorf("empty info %s", got)
	}
	res := SetupResult{
		Credential: CredentialStatus{SignedIn: true, Billing: "P · r", CheckedAt: "2026-10-01T12:00:00Z"},
		Models:     []ModelCheck{{Model: "a", OK: true}, {Model: "b", Missing: true, Problem: "No b in r"}, {Model: "c", Problem: "refused"}},
	}
	if got := enc(res); got != `{"credential":{"signed_in":true,"billing":"P · r","checked_at":"2026-10-01T12:00:00Z"},"models":[{"model":"a","ok":true},{"model":"b","ok":false,"missing":true,"problem":"No b in r"},{"model":"c","ok":false,"problem":"refused"}]}` {
		t.Errorf("result %s", got)
	}
	if got := enc(ClearResult{Cleared: []string{"X"}}); got != `{"cleared":["X"]}` {
		t.Errorf("clear %s", got)
	}
	var req SetupRequest
	if err := json.Unmarshal([]byte(`{"setup":"s","values":{"a":"1","b":"2"}}`), &req); err != nil || req.Setup != "s" || len(req.Values) != 2 {
		t.Errorf("request %+v %v", req, err)
	}
	if code, _ := errorStatus(notReady("x")); code != 409 {
		t.Errorf("not ready → %d", code)
	}
	if code, _ := errorStatus(&BadRequestError{Msg: "x"}); code != 400 {
		t.Errorf("bad request → %d", code)
	}
}
