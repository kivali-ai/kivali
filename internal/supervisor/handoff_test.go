package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/auth"
)

// redeemOn trades tok on a server whose session key and owners are the
// manifest's, at the harness clock plus after, and returns the email
// signed in ("" for none).
func redeemOn(t *testing.T, h *harness, tok string, after time.Duration) string {
	t.Helper()
	_, v := h.manifestValues()
	codec, err := auth.NewCodec([]byte(getPath(t, v, "secrets", "sessionKey").(string)))
	if err != nil {
		t.Fatal(err)
	}
	owners := strings.Split(getPath(t, v, "secrets", "ownerEmails").(string), ",")
	al := auth.NewAllowlist(owners...)
	srv := &auth.Handoff{Codec: codec, Allowlist: al, Now: func() time.Time { return h.clock.Now().Add(after) }}
	req := httptest.NewRequest(http.MethodGet, auth.HandoffPath+"?t="+tok, nil)
	req.Host = "127.0.0.1:18081"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	for _, c := range rr.Result().Cookies() {
		if c.Name == auth.CookieName {
			s, err := codec.DecodeSession(c.Value)
			if err != nil {
				t.Fatal(err)
			}
			return s.Email
		}
	}
	return ""
}

func TestHandoffMintsAOneTimeTokenForTheFirstOwner(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	served := &serveLog{}
	h.sup.o.Logf = served.logf
	ctx := context.Background()

	srv := httptest.NewServer(NewRPCServer(h.sup).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/handoff", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var res HandoffResult
	err = json.NewDecoder(resp.Body).Decode(&res)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || res.Token == "" {
		t.Fatalf("POST /v1/handoff: %s %+v %v", resp.Status, res, err)
	}
	if got := redeemOn(t, h, res.Token, time.Minute); got != owner.Owner {
		t.Fatalf("token signed in %q, want %q", got, owner.Owner)
	}
	// Valid for two minutes, not more.
	res2, err := h.sup.Handoff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := redeemOn(t, h, res2.Token, auth.HandoffTTL); got != "" {
		t.Fatalf("token still signed in %q after %v", got, auth.HandoffTTL)
	}

	_, v := h.manifestValues()
	key := getPath(t, v, "secrets", "sessionKey").(string)
	lines := served.get()
	if len(lines) != 2 || lines[0] != "handoff token minted for the owner" {
		t.Fatalf("serve logged %q", lines)
	}
	for _, line := range lines {
		if strings.Contains(line, res.Token) || strings.Contains(line, key) {
			t.Fatalf("logged a secret: %q", line)
		}
	}

	// With several owners the first one is signed in.
	g, err := h.sup.running()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rewriteManifest(ctx, g, "", func(vals map[string]any) {
		setValue(vals, []string{"secrets", "ownerEmails"}, " first@example.com , second@example.com")
	}); err != nil {
		t.Fatal(err)
	}
	res, err = h.sup.Handoff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := redeemOn(t, h, res.Token, 0); got != "first@example.com" {
		t.Fatalf("several owners: signed in %q", got)
	}
}

func TestHandoffRefusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var nr *NotReadyError
	if _, err := h.sup.Handoff(ctx); !errors.As(err, &nr) {
		t.Fatalf("handoff with no VM: %v", err)
	}
	srv := httptest.NewServer(NewRPCServer(h.sup).Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/handoff", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "not running") {
		t.Fatalf("POST /v1/handoff: %s %q", resp.Status, body)
	}

	h.mustUp(UpOptions{Install: owner})
	// It never takes the lock; were it to wait, this would hang.
	h.sup.op <- struct{}{}
	if _, err := h.sup.Handoff(ctx); err != nil {
		t.Fatalf("handoff during an operation: %v", err)
	}
	<-h.sup.op

	if err := h.sup.update(func(st *State) {
		st.Upgrade = &Journal{From: "0.15.0", To: "0.16.0", Step: StepApplying, Snapshot: h.snapPath(), SnapshotComplete: true}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sup.Handoff(ctx); err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("handoff with a journal: %v", err)
	}
}
