package googleworkspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/integration"
)

// fieldClientJSON is setup's one field: the client's JSON as Google's
// Auth Platform gives it to download, pasted whole. It is setup-only:
// the client id and secret read from it are kept, and nothing else.
const fieldClientJSON = "client_json"

// clientJSONWords say what the field takes.
const clientJSONWords = "The client's JSON file from Google's Auth Platform, pasted whole. Kivali reads the client id and secret from it."

// setupNotes is the guide the page shows above the setup, Markdown:
// Google's console, step by step, with a link to each page. Core puts
// the redirect address of the page's own address for {{redirect_url}},
// the one a Web application client registers; a Desktop app client
// registers none, since Google takes a desktop client's sign-in at any
// loopback port.
const setupNotes = `Google Workspace signs in with an OAuth client of your own, made once on Google's Auth Platform in a Google Cloud project of yours. Sign in to Google Cloud console with the Google account agents will work in.

1. [Create a project](https://console.cloud.google.com/projectcreate), or pick one you have.
2. [Turn on the APIs](https://console.cloud.google.com/flows/enableapi?apiid=gmail.googleapis.com,calendar-json.googleapis.com,docs.googleapis.com,drive.googleapis.com) agents use in the project: Gmail, Google Calendar, Google Docs and Google Drive.
3. Open [Google Auth Platform](https://console.cloud.google.com/auth/overview) and choose **Get started**. Name the app, such as Kivali, and give your email address where it asks. For the audience, choose **External** for a Gmail account, or **Internal** for a Google Workspace account, which keeps the app to your organization.
4. An External app starts in Testing, where Google ends every sign-in after seven days. Under [Audience](https://console.cloud.google.com/auth/audience), choose **Publish app**. Google does not review an app only you use, so the first time you sign in it says "Google hasn't verified this app": choose **Advanced**, then go on to the app. An Internal app has nothing to publish.
5. Under [Clients](https://console.cloud.google.com/auth/clients/create), create a client of one of these types:
   - **Desktop app**, for a team on this computer that you open at 127.0.0.1. There is nothing to register: Google takes its sign-in at any port.
   - **Web application**, for a team you open at another address, such as a server's or the team's address for other devices. Under **Authorized redirect URIs**, add the redirect address below. This page shows the address it is open at, so open it at the address you will sign in from.

` + "```Redirect address\n{{redirect_url}}\n```" + `

6. Download the client's JSON where Google shows the client's secret, and paste the whole file below. Kivali keeps the client id and secret, then you sign in to Google with the client.`

// formWords say what the Form asks for.
const formWords = "Paste the JSON of the client you made on Google's Auth Platform, as the steps above say. Kivali keeps its client id and secret, then you sign in to Google with the client."

// Setup is Google Workspace's setup flow. Installing (FlowSetup) is one
// Form for the client's JSON, asked again with words saying what is
// wrong until it reads as an OAuth client's; then Done, keeping the
// client id and secret, and core's browser sign-in with that client
// takes over (setup.then). Nothing is made on Google, and nothing is
// kept before the JSON reads. State holds a short marker of the step
// alone; the pasted JSON is setup-only, in the Answer's Form, and is
// never carried.
func (g *GoogleWorkspace) Setup(_ context.Context, _ builtinintegrations.Connection, flow string, last builtinintegrations.Answer, _ builtinintegrations.Progress) (builtinintegrations.Step, error) {
	if flow != builtinintegrations.FlowSetup {
		return builtinintegrations.Step{}, errors.New("Google Workspace has no flow named " + flow)
	}
	switch last.State["at"] {
	case "":
		return clientForm(formWords), nil
	case "form":
		c, problem := readClientJSON(last.Form[fieldClientJSON])
		if problem != "" {
			return clientForm(problem), nil
		}
		return builtinintegrations.Step{
			Done: true,
			Keep: map[string]string{integration.FieldClientID: c.ClientID, integration.FieldClientSecret: c.ClientSecret},
		}, nil
	}
	return builtinintegrations.Step{}, fmt.Errorf("the setup is at %q, which Google Workspace does not know", last.State["at"])
}

// clientForm is the Form for the client's JSON, with words.
func clientForm(words string) builtinintegrations.Step {
	return builtinintegrations.Step{
		Form:  &builtinintegrations.Form{Words: words, Fields: []string{fieldClientJSON}},
		State: map[string]string{"at": "form"},
	}
}

// clientFile is the JSON Google gives to download for an OAuth client:
// a Desktop app's under installed, a Web application's under web. A
// service account's key has type service_account and neither.
type clientFile struct {
	Type      string         `json:"type"`
	Installed *clientSection `json:"installed"`
	Web       *clientSection `json:"web"`
}

// clientSection is a client file's client, as far as Kivali reads it.
type clientSection struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
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
)

// readClientJSON reads a pasted client JSON: the installed or web
// client in it (installed first, when a file had both), whose id and
// secret it must hold. problem is "" when it reads, else the words that
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
	sec := f.Installed
	if sec == nil {
		sec = f.Web
	}
	switch {
	case sec == nil && f.Type == "service_account":
		return c, clientJSONServiceAccount
	case sec == nil:
		return c, clientJSONNoClient
	}
	c = clientSection{ClientID: strings.TrimSpace(sec.ClientID), ClientSecret: strings.TrimSpace(sec.ClientSecret)}
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
