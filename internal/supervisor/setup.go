package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/kivali-ai/kivali/internal/claudeauth"
	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// A sign-in setup signs the org's CLI in to a provider the CLI has no
// interactive sign-in for: the Claude driver (claudeauth's setups)
// turns the setup's values into the env block of the CLI's settings
// file, $HOME/.claude/settings.json in the server container (HOME is
// /data/claude-home, which every agent pod mounts). The supervisor
// writes that file through an exec into the server container, passing
// the contents on stdin. Values are never in an argv, a log line or an
// answer; which of them are secret, and what they become, only the
// driver knows.

// SetupRequest is the body of POST /v1/credential/setup: a setup the
// driver declares, by id, and its values.
type SetupRequest struct {
	Setup  string            `json:"setup"`
	Values map[string]string `json:"values"`
}

// ModelCheck is one model's answer from the provider after a setup:
// Missing when the provider has no model by that name (Problem says so
// in the setup's words), otherwise Problem is why it did not answer.
type ModelCheck struct {
	Model   string `json:"model"`
	OK      bool   `json:"ok"`
	Missing bool   `json:"missing,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// SetupResult answers POST /v1/credential/setup: the sign-in the CLI
// reports after the setup, and a check of each model Kivali runs.
type SetupResult struct {
	Credential CredentialStatus `json:"credential"`
	Models     []ModelCheck     `json:"models"`
}

// SavedSetup is the setup in force, with its values minus secrets.
type SavedSetup struct {
	Setup  string            `json:"setup"`
	Values map[string]string `json:"values"`
}

// SetupInfo answers GET /v1/credential/setup: the model ids Kivali runs
// the CLI with (a setup's form names them), and the setup saved now, if
// any, for the form to start from.
type SetupInfo struct {
	Models  []string    `json:"models"`
	Current *SavedSetup `json:"current,omitempty"`
}

// ClearResult answers POST /v1/credential/clear-provider: the settings
// variables removed, by name.
type ClearResult struct {
	Cleared []string `json:"cleared"`
}

// BadRequestError is a request whose body is wrong; the RPC answers it
// with 400 and the sentence.
type BadRequestError struct{ Msg string }

func (e *BadRequestError) Error() string { return e.Msg }

// probeTimeout bounds one model check.
const probeTimeout = 60 * time.Second

// settingsPath is the CLI's settings file, under the server
// container's HOME.
const settingsPath = `"$HOME/.claude/settings.json"`

// readSettingsScript prints the settings file, or exits 3 when there is
// none.
const readSettingsScript = `f=` + settingsPath + `; [ -e "$f" ] || exit 3; cat "$f"`

// writeSettingsScript replaces the settings file with stdin, readable
// by the server's user alone, by a rename so no CLI reads half of it.
const writeSettingsScript = `set -e; d="$HOME/.claude"; mkdir -p "$d"; umask 077; t="$d/.settings.json.kivali"; ` +
	`cat > "$t"; chmod 600 "$t"; mv -f "$t" ` + settingsPath

// serverReady is the guest, when the org is running with Kivali
// installed.
func (s *Supervisor) serverReady() (Guest, error) {
	g, err := s.running()
	if err != nil {
		return nil, notReady("the VM is not running; start the org first")
	}
	if s.State().Kivali == "" {
		return nil, notReady("Kivali is not installed yet; finish setting up the org first")
	}
	return g, nil
}

// readClaudeSettings reads the CLI's settings file in the server
// container; nil when there is none. Its contents never go into an
// error.
func readClaudeSettings(ctx context.Context, g Guest) ([]byte, error) {
	var out, errb bytes.Buffer
	argv := append(ServerExec(false), "sh", "-c", readSettingsScript)
	code, err := g.Exec(ctx, guestapi.ExecSpec{Argv: argv}, nil, &out, &errb)
	switch {
	case err != nil:
		return nil, fmt.Errorf("read Claude settings: %w", err)
	case code == 3:
		return nil, nil
	case code != 0:
		return nil, fmt.Errorf("read Claude settings: exit %d: %s", code, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// writeClaudeSettings replaces the CLI's settings file in the server
// container with b, passed on stdin.
func writeClaudeSettings(ctx context.Context, g Guest, b []byte) error {
	var errb bytes.Buffer
	argv := append(ServerExec(false), "sh", "-c", writeSettingsScript)
	code, err := g.Exec(ctx, guestapi.ExecSpec{Argv: argv}, bytes.NewReader(b), nil, &errb)
	if err != nil {
		return fmt.Errorf("write Claude settings: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("write Claude settings: exit %d: %s", code, strings.TrimSpace(errb.String()))
	}
	return nil
}

// SetupInfo reports the model ids Kivali runs and the setup saved now.
func (s *Supervisor) SetupInfo(ctx context.Context) (SetupInfo, error) {
	g, err := s.serverReady()
	if err != nil {
		return SetupInfo{}, err
	}
	c, cancel := context.WithTimeout(ctx, credentialTimeout)
	defer cancel()
	b, err := readClaudeSettings(c, g)
	if err != nil {
		return SetupInfo{}, err
	}
	info := SetupInfo{Models: append([]string{}, s.o.Models...)}
	if id, v, ok := claudeauth.ReadSetup(b); ok {
		info.Current = &SavedSetup{Setup: id, Values: v}
	}
	return info, nil
}

// ApplySetup signs the org's CLI in with a setup: the driver's env
// block goes into the CLI's settings file (replacing any provider's,
// keeping every other setting), the CLI says how it is signed in now,
// and one tiny request per model Kivali runs checks the provider
// serves it. A model that fails does not undo the setup; it is in the
// answer.
func (s *Supervisor) ApplySetup(ctx context.Context, r SetupRequest) (SetupResult, error) {
	if err := claudeauth.CheckSetup(r.Setup, r.Values); err != nil {
		return SetupResult{}, &BadRequestError{Msg: err.Error()}
	}
	g, err := s.serverReady()
	if err != nil {
		return SetupResult{}, err
	}
	c, cancel := context.WithTimeout(ctx, credentialTimeout)
	defer cancel()
	before, err := readClaudeSettings(c, g)
	if err != nil {
		return SetupResult{}, err
	}
	after, err := claudeauth.ApplySetup(before, r.Setup, r.Values)
	if err != nil {
		return SetupResult{}, fmt.Errorf("the CLI's settings: %w", err)
	}
	if err := writeClaudeSettings(c, g, after); err != nil {
		return SetupResult{}, err
	}
	s.o.Logf("credential: sign-in setup %s saved", r.Setup)
	cred, err := s.credentialOn(ctx, g)
	if err != nil {
		return SetupResult{}, err
	}
	models := s.checkModels(ctx, g, s.o.Models, func(m string) string {
		return claudeauth.MissingModel(r.Setup, r.Values, m)
	})
	for _, m := range models {
		s.o.Logf("credential: model %s: ok=%v %s", m.Model, m.OK, m.Problem)
	}
	return SetupResult{Credential: cred, Models: models}, nil
}

// checkModels runs the check for every model at once, each bounded by
// probeTimeout, and answers in the models' order.
func (s *Supervisor) checkModels(ctx context.Context, g Guest, models []string, missing func(string) string) []ModelCheck {
	out := make([]ModelCheck, len(models))
	var wg sync.WaitGroup
	for i, m := range models {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = probeModel(ctx, g, m, missing)
		}()
	}
	wg.Wait()
	return out
}

// probeModel asks the CLI in the server container for one tiny answer
// from model.
func probeModel(ctx context.Context, g Guest, model string, missing func(string) string) ModelCheck {
	c, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	argv := append(ServerExec(false), "sh", "-c", `cd /tmp && exec env "$@"`, "sh")
	argv = append(append(append(argv, claudeauth.ProbeEnv()...), "claude"), claudeauth.ProbeArgs(model)...)
	var out, errb bytes.Buffer
	code, err := g.Exec(c, guestapi.ExecSpec{Argv: argv}, nil, &out, &errb)
	if err != nil {
		if c.Err() != nil && ctx.Err() == nil {
			return ModelCheck{Model: model, Problem: fmt.Sprintf("no answer within %s", probeTimeout)}
		}
		return ModelCheck{Model: model, Problem: err.Error()}
	}
	switch state, detail := claudeauth.ClassifyProbe(out.Bytes(), errb.Bytes(), code); state {
	case claudeauth.ModelOK:
		return ModelCheck{Model: model, OK: true}
	case claudeauth.ModelMissing:
		return ModelCheck{Model: model, Missing: true, Problem: missing(model)}
	default:
		return ModelCheck{Model: model, Problem: detail}
	}
}

// ClearProvider removes every cloud provider's variables from the CLI's
// settings file, keeping everything else, so that the sign-in Claude
// Code's /login makes next is the one the CLI uses. The desktop runs
// it before it opens the sign-in in Terminal.
func (s *Supervisor) ClearProvider(ctx context.Context) (ClearResult, error) {
	g, err := s.serverReady()
	if err != nil {
		return ClearResult{}, err
	}
	c, cancel := context.WithTimeout(ctx, credentialTimeout)
	defer cancel()
	before, err := readClaudeSettings(c, g)
	if err != nil {
		return ClearResult{}, err
	}
	after, cleared, err := claudeauth.ClearProvider(before)
	if err != nil {
		return ClearResult{}, fmt.Errorf("the CLI's settings: %w", err)
	}
	if cleared == nil {
		return ClearResult{Cleared: []string{}}, nil
	}
	if err := writeClaudeSettings(c, g, after); err != nil {
		return ClearResult{}, err
	}
	s.o.Logf("credential: cleared %s from Claude's settings", strings.Join(cleared, " "))
	return ClearResult{Cleared: cleared}, nil
}

// setupDetail is the billing line's suffix for a status whose provider
// a setup configures: the setup's target ("my-resource"), read from the
// settings; empty otherwise.
func setupDetail(ctx context.Context, g Guest, apiProvider string) string {
	if _, ok := claudeauth.SetupForProvider(apiProvider); !ok {
		return ""
	}
	b, err := readClaudeSettings(ctx, g)
	if err != nil {
		return ""
	}
	id, v, ok := claudeauth.ReadSetup(b)
	if !ok {
		return ""
	}
	return claudeauth.SetupDetail(id, maps.Clone(v))
}

// errorStatus is the RPC status for an error from the credential
// endpoints: 409 for an org that is not running, 400 for a bad body,
// 502 for anything the guest answered.
func errorStatus(err error) (int, string) {
	var nr *NotReadyError
	var br *BadRequestError
	switch {
	case errors.As(err, &nr):
		return 409, nr.Msg
	case errors.As(err, &br):
		return 400, br.Msg
	}
	return 502, err.Error()
}
