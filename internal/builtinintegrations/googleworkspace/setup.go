package googleworkspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/integration"
)

// fieldClientJSON is setup's one field: the client's JSON as Google's
// Auth Platform gives it to download, pasted whole. It is setup-only:
// the client id and secret read from it are kept, and nothing else.
const fieldClientJSON = "client_json"

// clientJSONWords say what the field takes.
const clientJSONWords = "The file you downloaded for the client, pasted whole. Kivali reads the client id and secret from it."

// setupNotes is what the page shows above Connect, Markdown: what the
// setup is for and what it asks of the owner, in two sentences. The
// steps themselves are the flow's, one page of Google Cloud console at
// a time.
const setupNotes = `Google signs agents in through an app you make in Google Cloud console, so the sign-in is yours alone. Connect walks you through making it, one page at a time: about five minutes, once. Then you sign in to Google, and agents can start.`

// The pages of Google Cloud console the steps open, each on the one
// host setup declares.
const (
	consoleBase = "https://console.cloud.google.com"
	// pageAPIs turns on the four APIs agents use, in a project chosen
	// or created there.
	pageAPIs = consoleBase + "/flows/enableapi?apiid=gmail.googleapis.com,calendar-json.googleapis.com,docs.googleapis.com,drive.googleapis.com"
	// pageScreen is Google Auth Platform's overview, where the sign-in
	// screen is set up.
	pageScreen = consoleBase + "/auth/overview"
	// pageAudience is where an External app is published.
	pageAudience = consoleBase + "/auth/audience"
	// pageClient creates the OAuth client.
	pageClient = consoleBase + "/auth/clients/create"
)

// The words of each step, one thing to do on the page it opens. Core
// cuts a step's words at 200 characters, and each is shorter.
const (
	wordsAPIs     = "Sign in with the Google account agents will work in. Pick a project, or create one, then turn on the four APIs the page lists: Gmail, Google Calendar, Google Docs and Google Drive."
	wordsScreen   = "If the page shows an app already, go on. Else choose Get started, name the app Kivali, give your email address, and choose External for a personal Gmail account or Internal for work."
	wordsPublish  = "If the audience is External, choose Publish app, so Google keeps you signed in; Internal needs nothing here. When Google later says it has not verified the app, choose Advanced and go on."
	wordsDesktop  = "Create a client: choose Desktop app as the type, name it Kivali, and choose Create."
	wordsWeb      = "Create a client: choose Web application as the type, name it Kivali, add the address below under Authorized redirect URIs, and choose Create."
	wordsWebMany  = "Create a client: choose Web application as the type, name it Kivali, add each address below under Authorized redirect URIs, and choose Create."
	wordsDownload = "Google shows the client it made. Choose Download JSON, open the file, and paste everything in it here."
)

// formWords say what the Form asks for: the client's JSON.
const formWords = wordsDownload

// Where the flow is, carried in each step's State: under "at", the
// page the owner is on, then the Form; under "web", "1" when the team
// is opened off loopback, so the client must be a Web application's.
const (
	keyAt      = "at"
	keyNeedWeb = "web"
	atAPIs     = "apis"
	atScreen   = "screen"
	atPublish  = "publish"
	atClient   = "client"
	atForm     = "form"
)

// Setup is Google Workspace's setup flow. Installing (FlowSetup) walks
// the owner through Google Cloud console one page at a time, each a
// Form with no fields that opens the page and says what to do there:
// a project with the four APIs on, the sign-in screen, publishing the
// app, and the client, whose type Kivali picks from where the team is
// opened (Answer.Addresses): a Desktop app for a team opened at
// loopback alone, which Google signs in at any loopback port, else a
// Web application with every address to register. Then one Form for
// the client's JSON, asked again with words saying what is wrong until
// it reads as an OAuth client's of a type that works where the team is
// opened; then Done, keeping the client id and secret, and core's
// browser sign-in with that client takes over (setup.then). Nothing is
// made on Google, and nothing is kept before the JSON reads. State
// holds a short marker of the step alone, and whether a Web client is
// needed; the pasted JSON is setup-only, in the Answer's Form, and is
// never carried.
func (g *GoogleWorkspace) Setup(_ context.Context, _ builtinintegrations.Connection, flow string, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
	if flow != builtinintegrations.FlowSetup {
		return builtinintegrations.Step{}, errors.New("Google Workspace has no flow named " + flow)
	}
	switch last.State[keyAt] {
	case "":
		return byHand(wordsAPIs, pageAPIs, atAPIs, nil), nil
	case atAPIs:
		return byHand(wordsScreen, pageScreen, atScreen, nil), nil
	case atScreen:
		return byHand(wordsPublish, pageAudience, atPublish, nil), nil
	case atPublish:
		return clientStep(last.Addresses), nil
	case atClient:
		return clientForm(formWords, last.State[keyNeedWeb]), nil
	case atForm:
		c, problem := readClientJSON(last.Form[fieldClientJSON])
		if problem == "" && last.State[keyNeedWeb] == "1" && !c.Web {
			problem = clientJSONNeedsWeb
		}
		if problem != "" {
			return clientForm(problem, last.State[keyNeedWeb]), nil
		}
		return builtinintegrations.Step{
			Done: true,
			Keep: map[string]string{integration.FieldClientID: c.ClientID, integration.FieldClientSecret: c.ClientSecret},
		}, nil
	}
	return builtinintegrations.Step{}, fmt.Errorf("the setup is at %q, which Google Workspace does not know", last.State[keyAt])
}

// byHand is a step the owner takes on page by hand: words, the page,
// what to copy there, and Continue alone.
func byHand(words, page, at string, copy []string) builtinintegrations.Step {
	return builtinintegrations.Step{
		Form:  &builtinintegrations.Form{Words: words, URL: page, Copy: copy},
		State: map[string]string{keyAt: at},
	}
}

// clientStep is the step that creates the client, of the type that
// works where the team is opened: a Desktop app when every address the
// team signs in at is loopback (or none is known), else a Web
// application with every address to register, loopback ones included,
// since Google takes a Web client's sign-in only at an address it was
// given. The step carries whether a Web client is needed, so the JSON
// of a Desktop app is refused where it would fail at sign-in.
func clientStep(addresses []string) builtinintegrations.Step {
	if loopbackOnly(addresses) {
		return byHand(wordsDesktop, pageClient, atClient, nil)
	}
	words := wordsWeb
	if len(addresses) > 1 {
		words = wordsWebMany
	}
	st := byHand(words, pageClient, atClient, slices.Clone(addresses))
	st.State[keyNeedWeb] = "1"
	return st
}

// loopbackOnly reports whether every address is at a loopback host,
// or there are none.
func loopbackOnly(addresses []string) bool {
	for _, a := range addresses {
		u, err := url.Parse(a)
		if err != nil {
			return false
		}
		if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
			return false
		}
	}
	return true
}

// clientForm is the Form for the client's JSON, with words, carrying
// whether a Web client is needed (needWeb, "1" or "").
func clientForm(words, needWeb string) builtinintegrations.Step {
	st := builtinintegrations.Step{
		Form:  &builtinintegrations.Form{Words: words, Fields: []string{fieldClientJSON}},
		State: map[string]string{keyAt: atForm},
	}
	if needWeb != "" {
		st.State[keyNeedWeb] = needWeb
	}
	return st
}

// clientFile is the JSON Google gives to download for an OAuth client:
// a Desktop app's under installed, a Web application's under web. A
// service account's key has type service_account and neither.
type clientFile struct {
	Type      string         `json:"type"`
	Installed *clientSection `json:"installed"`
	Web       *clientSection `json:"web"`
}

// clientSection is a client file's client, as far as Kivali reads it:
// its id and secret, and whether it is a Web application's (web), as
// against a Desktop app's (installed).
type clientSection struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Web          bool   `json:"-"`
}

// Bounds on what the client JSON may hold: the whole paste, and its id
// and its secret each.
const (
	maxClientJSON  = 64 << 10
	maxClientValue = 512
)

// clientIDSuffix ends every OAuth client id Google issues.
const clientIDSuffix = ".apps.googleusercontent.com"

// The words of a client JSON that does not read, each a sentence for
// the owner that says what to paste instead. Core cuts a step's words
// at 200 characters, and each is shorter.
const (
	clientJSONEmpty          = "Paste the client's JSON: the whole file you downloaded for the client from Google's Auth Platform."
	clientJSONNotJSON        = "That is not a client's JSON. Paste the whole file you downloaded for the client from Google's Auth Platform, from its first { to its last }."
	clientJSONServiceAccount = "That is a service account's key. Kivali signs in with an OAuth client: create one of type Desktop app or Web application, and paste its JSON."
	clientJSONNoClient       = "That JSON holds no OAuth client. Create a client of type Desktop app or Web application on Google's Auth Platform, and paste its JSON."
	clientJSONNoID           = "That client JSON has no client id ending in .apps.googleusercontent.com. Paste the whole file you downloaded for the client."
	clientJSONNoSecret       = "That client JSON has no client secret. Download the JSON where Google's Auth Platform shows the client's secret, and paste it whole."
	clientJSONNeedsWeb       = "That is a Desktop app client, which Google signs in only at 127.0.0.1, and this team is opened elsewhere. Create a Web application client with the addresses added, and paste its JSON."
)

// readClientJSON reads a pasted client JSON: the installed or web
// client in it (installed first, when a file had both), whose id and
// secret it must hold, and which it was. problem is "" when it reads, else the words that
// ask for it again.
func readClientJSON(pasted string) (c clientSection, problem string) {
	s := strings.TrimSpace(pasted)
	switch {
	case s == "":
		return c, clientJSONEmpty
	case len(s) > maxClientJSON:
		return c, clientJSONNotJSON
	}
	var f clientFile
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return c, clientJSONNotJSON
	}
	sec, web := f.Installed, false
	if sec == nil {
		sec, web = f.Web, f.Web != nil
	}
	switch {
	case sec == nil && f.Type == "service_account":
		return c, clientJSONServiceAccount
	case sec == nil:
		return c, clientJSONNoClient
	}
	c = clientSection{ClientID: strings.TrimSpace(sec.ClientID), ClientSecret: strings.TrimSpace(sec.ClientSecret), Web: web}
	switch {
	case !clientValue(c.ClientID) || !strings.HasSuffix(c.ClientID, clientIDSuffix) || len(c.ClientID) == len(clientIDSuffix):
		return clientSection{}, clientJSONNoID
	case !clientValue(c.ClientSecret):
		return clientSection{}, clientJSONNoSecret
	}
	return c, ""
}

// clientValue reports whether v can be a client id or secret: not
// empty, at most maxClientValue bytes, with no space or control
// character.
func clientValue(v string) bool {
	if v == "" || len(v) > maxClientValue {
		return false
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}
