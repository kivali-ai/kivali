package web

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/store"
)

func TestAutoReleaseKeyMapsSettingToDetent(t *testing.T) {
	cases := []struct {
		in   store.AutoRelease
		want string
	}{
		{store.AutoRelease{}, "off"},
		{store.AutoRelease{Enabled: true}, "now"},
		{store.AutoRelease{Enabled: true, Delay: 30 * time.Second}, "30s"},
		{store.AutoRelease{Enabled: true, Delay: 20 * time.Minute}, "20m"},
		// Hand-edited delays show as the nearest stop so the slider
		// still has a position.
		{store.AutoRelease{Enabled: true, Delay: 3 * time.Minute}, "2m"},
		{store.AutoRelease{Enabled: true, Delay: time.Hour}, "20m"},
	}
	for _, c := range cases {
		if got := autoReleaseKey(c.in); got != c.want {
			t.Errorf("autoReleaseKey(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Moving the slider persists the detent, dated, and the snapshot
// carries it, which is how every other open tab learns the new
// position.
func TestAutoReleasePostPersistsAndPublishes(t *testing.T) {
	srv := newTestServer(t)

	rr := apiDo(t, srv, http.MethodPost, "/api/v1/auto-release", `{"value":"2m"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rr.Code, rr.Body.String())
	}
	got := srv.Store.ReadAutoRelease()
	if !got.Enabled || got.Delay != 2*time.Minute {
		t.Fatalf("stored %+v, want enabled 2m", got)
	}
	// The move is dated: the setting governs what is sent from now on.
	if got.Since.IsZero() {
		t.Fatalf("stored %+v, want Since set to the time of the move", got)
	}

	var snap struct {
		Inbox struct {
			AutoRelease string `json:"auto_release"`
		} `json:"inbox"`
	}
	if err := json.Unmarshal(srv.buildOrgSnapshot(), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Inbox.AutoRelease != "2m" {
		t.Fatalf("snapshot auto_release = %q, want 2m", snap.Inbox.AutoRelease)
	}

	rr = apiDo(t, srv, http.MethodPost, "/api/v1/auto-release", `{"value":"off"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("off: code = %d", rr.Code)
	}
	if srv.Store.ReadAutoRelease().Enabled {
		t.Fatal("off did not disable auto-release")
	}
}

// Only the slider's own stops are accepted: the client never sends a
// duration, and a typo must not become a delay nobody chose.
func TestAutoReleasePostRejectsUnknownDetent(t *testing.T) {
	srv := newTestServer(t)
	for _, bad := range []string{"", "1h", "soon", "2m0s"} {
		rr := apiDo(t, srv, http.MethodPost, "/api/v1/auto-release", `{"value":"`+bad+`"}`, nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("value=%q: code = %d, want 400", bad, rr.Code)
		}
	}
	if srv.Store.ReadAutoRelease().Enabled {
		t.Fatal("a rejected post changed the setting")
	}
}
