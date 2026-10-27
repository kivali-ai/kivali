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

// The addresses a team signs in at: one on this computer, and a
// server's with the team's address for other devices.
var (
	loopback  = []string{"http://127.0.0.1:8080/connect/callback"}
	elsewhere = []string{"https://kivali.example.ts.net/connect/callback", "http://127.0.0.1:8080/connect/callback"}
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

// walk takes the hand steps through to the Form for the client's JSON,
// with the team signing in at addresses, checking each step on the
// way: a Form with no fields that opens the expected page of Google
// Cloud console, keeping nothing.
func walk(t *testing.T, addresses []string) builtinintegrations.Step {
	t.Helper()
	last := builtinintegrations.Answer{Leg: 1, Addresses: addresses}
	for i, page := range []string{pageAPIs, pageScreen, pageAudience, pageClient} {
		st := step(t, last)
		if st.Form == nil || len(st.Form.Fields) != 0 || st.Form.URL != page || st.Form.Words == "" || st.Keep != nil || st.Done {
			t.Fatalf("step %d = %+v", i+1, st)
		}
		last = builtinintegrations.Answer{State: st.State, Form: map[string]string{}, Leg: i + 2, Addresses: addresses}
	}
	form := step(t, last)
	if form.Form == nil || len(form.Form.Fields) != 1 || form.Form.Fields[0] != "client_json" || form.Form.Words != formWords || form.Form.URL != "" ||
		form.Keep != nil || form.Done {
		t.Fatalf("the form = %+v", form)
	}
	return form
}

// answered is the Answer after the Form, with pasted as the client JSON.
func answered(form builtinintegrations.Step, pasted string) builtinintegrations.Answer {
	return builtinintegrations.Answer{State: form.State, Form: map[string]string{"client_json": pasted}, Leg: 6}
}

// Why this test exists: the setup is a walk through Google Cloud
// console, one page at a time, each a step the owner takes by hand
// with that page opened; the owner must never read a list of steps.
// For a team opened on this computer alone the client step must say
// Desktop app, with nothing to copy, since Google signs a desktop
// client in at any loopback port. Then one Form for the client's JSON,
// and a pasted file of either client type Google makes (a Desktop
// app's installed, a Web application's web) must keep exactly that
// client's id and secret and end the flow, so core's browser sign-in
// takes over with them, carrying nothing of the pasted file in State.
func TestSetupWalksTheConsoleThenKeepsTheClient(t *testing.T) {
	first := step(t, builtinintegrations.Answer{Leg: 1, Addresses: loopback})
	if first.Form == nil || first.Form.URL != pageAPIs || len(first.Form.Copy) != 0 {
		t.Fatalf("first step = %+v", first)
	}
	form := walk(t, loopback)
	client := step(t, builtinintegrations.Answer{State: map[string]string{keyAt: atPublish}, Form: map[string]string{}, Leg: 4, Addresses: loopback})
	if client.Form == nil || client.Form.Words != wordsDesktop || len(client.Form.Copy) != 0 || client.State[keyNeedWeb] != "" {
		t.Errorf("client step at loopback = %+v", client)
	}
	for _, tc := range []struct{ pasted, id, secret string }{
		{desktopJSON, "123-desk.apps.googleusercontent.com", "GOCSPX-desk-secret"},
		{webJSON, "456-web.apps.googleusercontent.com", "GOCSPX-web-secret"},
		// As a single-line field may leave it: line breaks gone, spaces
		// around.
		{"  " + strings.ReplaceAll(desktopJSON, ",", ",  ") + "\n", "123-desk.apps.googleusercontent.com", "GOCSPX-desk-secret"},
	} {
		done := step(t, answered(form, tc.pasted))
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

// Why this test exists: Google signs a Web application client in only
// at an address registered with it, and a Desktop app client at
// loopback alone. For a team opened at another address the client
// step must say Web application and give every address the team signs
// in at to copy, loopback ones included, and the JSON of a Desktop app
// client must be asked again rather than kept, since the sign-in
// after it would fail in Google's words.
func TestSetupNeedsAWebClientOffLoopback(t *testing.T) {
	client := step(t, builtinintegrations.Answer{State: map[string]string{keyAt: atPublish}, Form: map[string]string{}, Leg: 4, Addresses: elsewhere})
	if client.Form == nil || client.Form.Words != wordsWebMany || strings.Join(client.Form.Copy, " ") != strings.Join(elsewhere, " ") ||
		client.Form.URL != pageClient || client.State[keyNeedWeb] != "1" {
		t.Fatalf("client step elsewhere = %+v", client)
	}
	one := step(t, builtinintegrations.Answer{State: map[string]string{keyAt: atPublish}, Form: map[string]string{}, Leg: 4, Addresses: elsewhere[:1]})
	if one.Form == nil || one.Form.Words != wordsWeb || len(one.Form.Copy) != 1 {
		t.Errorf("client step at one address = %+v", one)
	}
	form := walk(t, elsewhere)
	if form.State[keyNeedWeb] != "1" {
		t.Fatalf("the form forgot the client type: %v", form.State)
	}
	again := step(t, answered(form, desktopJSON))
	if again.Form == nil || again.Done || again.Keep != nil || again.Form.Words != clientJSONNeedsWeb || again.State[keyNeedWeb] != "1" {
		t.Errorf("a desktop client elsewhere = %+v", again)
	}
	if done := step(t, answered(again, webJSON)); !done.Done || done.Keep["client_id"] != "456-web.apps.googleusercontent.com" {
		t.Errorf("a web client elsewhere = %+v", done)
	}
	// No addresses known: a Desktop app, which works wherever a team on
	// this computer is opened.
	if none := step(t, builtinintegrations.Answer{State: map[string]string{keyAt: atPublish}, Form: map[string]string{}, Leg: 4}); none.Form == nil || none.Form.Words != wordsDesktop {
		t.Errorf("client step with no addresses = %+v", none)
	}
}

// Why this test exists: a paste that is not an OAuth client's JSON
// would otherwise reach Google's sign-in as a client that does not
// exist, failing there in Google's words. Each kind of wrong paste must
// ask again on the same Form, in words saying what to paste instead,
// short enough that core shows them whole and never quoting what was
// pasted, keeping nothing. Every step's words must fit too.
func TestSetupAsksAgainForWhatDoesNotRead(t *testing.T) {
	form := walk(t, loopback)
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
		again := step(t, answered(form, pasted))
		if again.Form == nil || again.Done || again.Keep != nil || again.Form.Words != words ||
			len(again.Form.Fields) != 1 || again.Form.Fields[0] != "client_json" || again.State[keyAt] != atForm {
			t.Errorf("%.60q: %+v", pasted, again)
		}
		if pasted != "" && strings.Contains(again.Form.Words, pasted) {
			t.Errorf("the words quote the paste: %q", again.Form.Words)
		}
	}
	for _, w := range []string{wordsAPIs, wordsScreen, wordsPublish, wordsDesktop, wordsWeb, wordsWebMany, formWords,
		clientJSONEmpty, clientJSONNotJSON, clientJSONServiceAccount, clientJSONNoClient, clientJSONNoID, clientJSONNoSecret, clientJSONNeedsWeb} {
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
		builtinintegrations.Answer{State: map[string]string{keyAt: "elsewhere"}}, noProgress{}); err == nil {
		t.Errorf("an unknown state gave %+v", st)
	}
}
