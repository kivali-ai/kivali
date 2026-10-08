package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/auth"
)

// HandoffResult is the answer to POST /v1/handoff.
type HandoffResult struct {
	Token string `json:"token"`
}

// Handoff mints a one-time token that signs the org's first owner in on
// its server's GET /auth/handoff (docs/developers/auth.md, Desktop handoff), from
// the session key and owner in the HelmChart's values on the data disk.
// The token is valid for auth.HandoffTTL. Like the credential status it
// never takes the operation lock; it is refused while an upgrade
// journal is open, when the values on the disk may be about to change.
// Neither the token nor the session key is logged.
func (s *Supervisor) Handoff(ctx context.Context) (HandoffResult, error) {
	g, err := s.running()
	if err != nil {
		return HandoffResult{}, notReady("the VM is not running; start the org first")
	}
	st := s.State()
	if st.Kivali == "" {
		return HandoffResult{}, notReady("Kivali is not installed yet; finish setting up the org first")
	}
	if j := st.Upgrade; j != nil {
		return HandoffResult{}, notReady(fmt.Sprintf("an upgrade journal from %s to %s is open at step %s; run `up` to recover it first", j.From, j.To, j.Step))
	}
	cur, err := readManifest(ctx, g)
	if err != nil {
		return HandoffResult{}, fmt.Errorf("handoff: %w", err)
	}
	_, vals, err := ParseManifest(cur)
	if err != nil {
		return HandoffResult{}, fmt.Errorf("handoff: %w", err)
	}
	secrets, _ := vals["secrets"].(map[string]any)
	key, _ := secrets["sessionKey"].(string)
	owners, _ := secrets["ownerEmails"].(string)
	owner, _, _ := strings.Cut(owners, ",")
	owner = strings.TrimSpace(owner)
	if key == "" || owner == "" {
		return HandoffResult{}, errors.New("handoff: the install values hold no session key or no owner")
	}
	tok, err := auth.NewHandoffToken([]byte(key), owner, s.o.Clock.Now().Add(auth.HandoffTTL))
	if err != nil {
		return HandoffResult{}, fmt.Errorf("handoff: %w", err)
	}
	s.o.Logf("handoff token minted for the owner")
	return HandoffResult{Token: tok}, nil
}
