package googleworkspace

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// The client files Google's Auth Platform gives to download, one per
// client type, as Google writes them.
const (
	desktopJSON = `{"installed":{"client_id":"123-desk.apps.googleusercontent.com","project_id":"kivali-1","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token","auth_provider_x509_cert_url":"https://www.googleapis.com/oauth2/v1/certs","client_secret":"GOCSPX-desk-secret","redirect_uris":["http://localhost"]}}`
	webJSON     = `{"web":{"client_id":"456-web.apps.googleusercontent.com","project_id":"kivali-1","auth_uri":"https://accounts.google.com/o/oauth2/auth","token_uri":"https://oauth2.googleapis.com/token","client_secret":"GOCSPX-web-secret","redirect_uris":["https://kivali.example.ts.net/connect/callback"]}}`
)

type noProgress struct{}

func (noProgress) Stage(string, int, int) {}

// step runs one call of the install flow.
func step(t *testing.T, last builtinintegrations.Answer) builtinintegrations.Step {
	t.Helper()
	st, err := New().Setup(context.Background(), builtinintegrations.Connection{}, builtinintegrations.FlowSetup, last, noProgress{})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// answered is the Answer after the Form, with pasted as the client JSON.
func answered(first builtinintegrations.Step, pasted string) builtinintegrations.Answer {
	return builtinintegrations.Answer{State: first.State, Form: map[string]string{"client_json": pasted}, Leg: 2}
}

// Why this test exists: setup is how the connection gets the client
// core signs in with. Its first step must ask for the client JSON
// alone, keeping nothing; a pasted file of either client type Google
// makes (a Desktop app's installed, a Web application's web) must keep
// exactly that client's id and secret and end the flow, so core's
// browser sign-in takes over with them, carrying nothing of the pasted
// file in State.
func TestSetupKeepsTheClientOfEitherType(t *testing.T) {
	first := step(t, builtinintegrations.Answer{Leg: 1})
	if first.Form == nil || len(first.Form.Fields) != 1 || first.Form.Fields[0] != "client_json" || first.Form.Words != formWords ||
		first.Keep != nil || first.Done {
		t.Fatalf("first step = %+v", first)
	}
	for _, tc := range []struct{ pasted, id, secret string }{
		{desktopJSON, "123-desk.apps.googleusercontent.com", "GOCSPX-desk-secret"},
		{webJSON, "456-web.apps.googleusercontent.com", "GOCSPX-web-secret"},
		// As a single-line field may leave it: line breaks gone, spaces
		// around.
		{"  " + strings.ReplaceAll(desktopJSON, ",", ",  ") + "\n", "123-desk.apps.googleusercontent.com", "GOCSPX-desk-secret"},
	} {
		done := step(t, answered(first, tc.pasted))
		if !done.Done || done.Form != nil || len(done.Keep) != 2 || done.Keep["client_id"] != tc.id || done.Keep["client_secret"] != tc.secret {
			t.Errorf("done = %+v", done)
		}
		for _, v := range done.State {
			if strings.Contains(v, "GOCSPX") {
				t.Errorf("State carries the secret: %v", done.State)
			}
		}
	}
}

// Why this test exists: a paste that is not an OAuth client's JSON
// would otherwise reach Google's sign-in as a client that does not
// exist, failing there in Google's words. Each kind of wrong paste must
// ask again on the same Form, in words saying what to paste instead,
// short enough that core shows them whole and never quoting what was
// pasted, keeping nothing.
func TestSetupAsksAgainForWhatDoesNotRead(t *testing.T) {
	first := step(t, builtinintegrations.Answer{Leg: 1})
	for pasted, words := range map[string]string{
		"":                       clientJSONEmpty,
		"client_secret_123.json": clientJSONNotJSON,
		`{"installed":`:          clientJSONNotJSON,
		`[1, 2]`:                 clientJSONNotJSON,
		strings.Repeat(" ", 10) + `{"installed":{}}` + strings.Repeat(" ", maxClientJSON):                                  clientJSONNoID,
		`{"type":"service_account","project_id":"p","private_key":"-----BEGIN PRIVATE KEY-----","client_id":"1234567890"}`: clientJSONServiceAccount,
		`{"project_id":"p"}`: clientJSONNoClient,
		`{"installed":{"client_id":"abc","client_secret":"GOCSPX-x"}}`:                                                                  clientJSONNoID,
		`{"installed":{"client_id":".apps.googleusercontent.com","client_secret":"GOCSPX-x"}}`:                                          clientJSONNoID,
		`{"web":{"client_id":"1-a b.apps.googleusercontent.com","client_secret":"GOCSPX-x"}}`:                                           clientJSONNoID,
		`{"web":{"client_id":"1-a.apps.googleusercontent.com"}}`:                                                                        clientJSONNoSecret,
		`{"installed":{"client_id":"1-a.apps.googleusercontent.com","client_secret":"GOCSPX x"}}`:                                       clientJSONNoSecret,
		`{"installed":{"client_id":"1-a.apps.googleusercontent.com","client_secret":"` + strings.Repeat("s", 600) + `"}}`:               clientJSONNoSecret,
		`{"web":{"client_id":"1-a.apps.googleusercontent.com","client_secret":"x","pad":"` + strings.Repeat("p", maxClientJSON) + `"}}`: clientJSONNotJSON,
	} {
		again := step(t, answered(first, pasted))
		if again.Form == nil || again.Done || again.Keep != nil || again.Form.Words != words ||
			len(again.Form.Fields) != 1 || again.Form.Fields[0] != "client_json" || again.State["at"] != "form" {
			t.Errorf("%.60q: %+v", pasted, again)
		}
		if pasted != "" && strings.Contains(again.Form.Words, pasted) {
			t.Errorf("the words quote the paste: %q", again.Form.Words)
		}
	}
	for _, w := range []string{formWords, clientJSONEmpty, clientJSONNotJSON, clientJSONServiceAccount, clientJSONNoClient, clientJSONNoID, clientJSONNoSecret} {
		if n := utf8.RuneCountInString(w); n > 200 {
			t.Errorf("%d characters, more than core shows: %q", n, w)
		}
	}
}

// Why this test exists: Google Workspace's requirements name no Fix of
// its own, so a flow name other than setup reaching Setup is a mistake
// that must fail, not run the install over the connection; and a
// State it did not write must fail rather than keep anything.
func TestSetupRefusesOtherFlowsAndStates(t *testing.T) {
	g := New()
	if _, err := g.Setup(context.Background(), builtinintegrations.Connection{}, "permissions", builtinintegrations.Answer{}, noProgress{}); err == nil {
		t.Error("another flow ran")
	}
	if st, err := g.Setup(context.Background(), builtinintegrations.Connection{}, builtinintegrations.FlowSetup,
		builtinintegrations.Answer{State: map[string]string{"at": "elsewhere"}}, noProgress{}); err == nil {
		t.Errorf("an unknown state gave %+v", st)
	}
}
