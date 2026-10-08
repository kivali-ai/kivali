package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/claudeauth"
	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// CredentialStatus is the answer to GET /v1/credential: how the org's
// Claude CLI is signed in, as `claude auth status` in the server
// container reports it. It is signed in by the CLI's own sign-in there (the
// supervisor's terminal) or by a sign-in setup (ApplySetup); agent pods
// share that sign-in.
type CredentialStatus struct {
	SignedIn bool `json:"signed_in"`
	// Email is the signed-in account, when the CLI reports one (a
	// Claude subscription or Console login).
	Email string `json:"email,omitempty"`
	// Billing names what model calls are billed to: "Claude Max",
	// "Anthropic Console", "Amazon Bedrock", "Google Vertex AI"; for a
	// sign-in setup, the provider's name and the setup's target
	// ("<provider> · <target>"). Empty when not signed in or not
	// reported.
	Billing   string `json:"billing,omitempty"`
	CheckedAt string `json:"checked_at"`
}

// NotReadyError is a request that needs the org running (the VM up and
// Kivali installed); the RPC answers it with 409 and the sentence.
type NotReadyError struct{ Msg string }

func (e *NotReadyError) Error() string { return e.Msg }

func notReady(msg string) error { return &NotReadyError{Msg: msg} }

// credentialTimeout bounds the one exec a credential status costs.
const credentialTimeout = 20 * time.Second

// Credential reports how the org's Claude CLI is signed in, with one
// exec of `claude auth status --json` in the server container, as the
// server's own user and HOME (and, for a provider a sign-in setup
// configures, one more to read the setup's target from the CLI's
// settings). Like status it
// never takes the operation lock, so it answers while an operation
// runs.
func (s *Supervisor) Credential(ctx context.Context) (CredentialStatus, error) {
	g, err := s.serverReady()
	if err != nil {
		return CredentialStatus{}, err
	}
	return s.credentialOn(ctx, g)
}

func (s *Supervisor) credentialOn(ctx context.Context, g Guest) (CredentialStatus, error) {
	c, cancel := context.WithTimeout(ctx, credentialTimeout)
	defer cancel()
	argv := append(append(ServerExec(false), "claude"), claudeauth.Args()...)
	var out, errb bytes.Buffer
	code, err := g.Exec(c, guestapi.ExecSpec{Argv: argv}, nil, &out, &errb)
	if err != nil {
		return CredentialStatus{}, fmt.Errorf("credential: %w", err)
	}
	// Signed out exits 1 with the same JSON on stdout.
	st, perr := claudeauth.Parse(out.Bytes())
	if perr != nil {
		// Never echo stdout: only the parsed fields leave here.
		if code != 0 {
			return CredentialStatus{}, fmt.Errorf("credential: exit %d: %s", code, strings.TrimSpace(errb.String()))
		}
		return CredentialStatus{}, errors.New("credential: unexpected answer from the server container")
	}
	billing := st.Billing()
	if st.LoggedIn {
		// "<provider> · <target>": a setup's target is in the
		// settings, not in the status.
		if d := setupDetail(c, g, st.APIProvider); d != "" && billing != "" {
			billing += " · " + d
		}
	}
	return CredentialStatus{
		SignedIn:  st.LoggedIn,
		Email:     st.Email,
		Billing:   billing,
		CheckedAt: s.o.Clock.Now().UTC().Format(time.RFC3339),
	}, nil
}

// awaitNewValues waits, after a values rewrite, for the Helm controller
// to apply them (the deployment's next generation: until then the old
// one is still ready) and for the server to serve again.
func (s *Supervisor) awaitNewValues(ctx context.Context, g Guest, gen int64, logf Logf) error {
	if err := poll(ctx, s.o.Clock, deployTimeout, pollInterval, "the Helm controller to apply the new values", func(ctx context.Context) error {
		got, err := deploymentGeneration(ctx, g)
		if err != nil {
			return err
		}
		if got <= gen {
			if jerr := helmJobFailed(ctx, g); jerr != nil {
				return permanent(jerr)
			}
			return errNotYet
		}
		return nil
	}); err != nil {
		return err
	}
	return s.waitServing(ctx, "", "", logf)
}

// AddressRequest is the body of POST /v1/address.
type AddressRequest struct {
	// ExternalURL is the https origin an operator put in front of the org
	// (Tailscale, a reverse proxy), or empty for none. It becomes the
	// chart's externalURL (KIVALI_EXTERNAL_URL): besides loopback, the one
	// host the server derives a sign-in callback for.
	ExternalURL string `json:"external_url"`
}

// SetAddress records the org's external https address (empty clears
// it) in the HelmChart's values, and waits for the server to serve again
// when that changed anything.
func (s *Supervisor) SetAddress(ctx context.Context, r AddressRequest, logf Logf) error {
	external := strings.TrimRight(strings.TrimSpace(r.ExternalURL), "/")
	if external != "" {
		u, err := url.Parse(external)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return fmt.Errorf("address: %q is not an https origin (https://<host>[:port])", external)
		}
	}
	ctx, end, err := s.beginNow(ctx, "address", true)
	if err != nil {
		return err
	}
	defer end()
	setStage(ctx, StageSettingUp)
	logf = s.tee(ctx, logf)
	if j := s.State().Upgrade; j != nil {
		return fmt.Errorf("an upgrade journal from %s to %s is open at step %s; run `up` to recover it first", j.From, j.To, j.Step)
	}
	g, err := s.running()
	if err != nil {
		return err
	}
	if s.State().Kivali == "" {
		return errors.New("not installed yet; nothing to change")
	}
	gen, err := deploymentGeneration(ctx, g)
	if err != nil {
		return err
	}
	changed, err := rewriteManifest(ctx, g, "", func(vals map[string]any) {
		if external != "" {
			vals["externalURL"] = external
		} else {
			delete(vals, "externalURL")
		}
	})
	if err != nil {
		return fmt.Errorf("rewrite HelmChart: %w", err)
	}
	if !changed {
		logf("the external address is unchanged")
		return nil
	}
	if external != "" {
		logf("external address set to %s", external)
	} else {
		logf("external address cleared")
	}
	return s.awaitNewValues(ctx, g, gen, logf)
}

// deploymentGeneration is the server deployment's metadata.generation.
func deploymentGeneration(ctx context.Context, g Guest) (int64, error) {
	out, err := kubectl(ctx, g, "-n", Namespace, "get", "deployment", "kivali", "-o", "json")
	if err != nil {
		return 0, err
	}
	var d deployment
	if err := json.Unmarshal(out, &d); err != nil {
		return 0, fmt.Errorf("deployment: %w", err)
	}
	return d.Metadata.Generation, nil
}
