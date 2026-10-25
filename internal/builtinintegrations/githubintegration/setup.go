package githubintegration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// The flows Setup runs besides installing (builtinintegrations.
// FlowSetup): the Fixes the requirements name.
const (
	// FlowInstall installs the app on GitHub again, once it was
	// uninstalled there: GitHub's page for a new installation, then the
	// app's installations checked.
	FlowInstall = "install"
	// FlowPermissions sends the owner to the page on GitHub where the
	// missing permissions are granted, then checks the installation
	// until it has every one this version needs.
	FlowPermissions = "permissions"
	// FlowSettings sends the owner to the installation's settings on
	// GitHub, then checks it until it is not suspended and reaches a
	// repository.
	FlowSettings = "settings"
)

// webBase is GitHub's site: every page a Go or a Wait names is on it.
const webBase = "https://github.com"

// appNameMax is the most characters GitHub takes in an app's name.
const appNameMax = 34

// needed is what this version needs of the installation: the
// permissions the app is made with.
func needed() permissions {
	return perms("contents", "read", "issues", "write", "pull_requests", "write")
}

// accountName is a GitHub account's name: letters, digits and hyphens,
// not starting with a hyphen, at most 39.
var accountName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// appSlug is an app's name in GitHub's addresses.
var appSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)

// appManifest is GitHub's app manifest: what the app is made with.
type appManifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	RedirectURL        string            `json:"redirect_url"`
	SetupURL           string            `json:"setup_url"`
	SetupOnUpdate      bool              `json:"setup_on_update"`
	Public             bool              `json:"public"`
	HookAttributes     hookAttributes    `json:"hook_attributes"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events"`
}

// hookAttributes is the manifest's webhook. GitHub takes no manifest
// whose hook_attributes lacks a url, an inactive webhook's included,
// and names the lack "url" alone; and it takes no url that is not
// reachable over the public Internet, so a team's loopback address
// (127.0.0.1) is refused as "Hook url is not supported". The webhook
// is inactive and nothing is ever sent to it, so its address is
// hookPlaceholder, a public name that is nobody's.
type hookAttributes struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// hookPlaceholder is the inactive webhook's address: example.com is
// reserved for exactly this use and resolves on the public Internet,
// which GitHub requires of a webhook's host; GitHub's own manifest
// example names it.
const hookPlaceholder = "https://example.com/no-webhook"

// appName is the name the app is made with: "Kivali" and the team's
// name, cut to the most GitHub takes.
func appName(team string) string {
	name := strings.Join(strings.Fields("Kivali "+team), " ")
	if r := []rune(name); len(r) > appNameMax {
		name = strings.TrimSpace(string(r[:appNameMax]))
	}
	return name
}

// callbackParts reads the callback core gave a call: the team's
// address, its callback without a query (the setup address GitHub
// appends the installation to) and the state the callback carries.
func callbackParts(callback string) (base, setupURL, state string, err error) {
	u, err := url.Parse(callback)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Path != "/connect/callback" {
		return "", "", "", errors.New("this page has no address GitHub could send the browser back to; open Kivali at its address and start again")
	}
	base = u.Scheme + "://" + u.Host
	return base, base + u.Path, u.Query().Get("state"), nil
}

// installURL is GitHub's page for a new installation of the app slug,
// carrying state for the setup address when there is one.
func installURL(slug, state string) string {
	u := webBase + "/apps/" + url.PathEscape(slug) + "/installations/new"
	if state != "" {
		u += "?" + url.Values{"state": {state}}.Encode()
	}
	return u
}

// Setup is GitHub's setup flow. Installing (FlowSetup) makes a private
// GitHub App from a manifest and installs it:
//
//  1. A Form for the account: an organization's name, or blank for the
//     owner's personal account.
//  2. A Go that posts the manifest to GitHub's new-app page for that
//     account. The owner chooses Create GitHub App there, and GitHub
//     sends the browser back to the callback with a code.
//  3. The code traded for the app (POST /app-manifests/{code}/conversions),
//     whose id, slug, client id and secret and private key are kept,
//     then a Go to the app's new-installation page. GitHub sends the
//     browser back to the setup address, with no state of Kivali's.
//  4. The app's installations listed with its JWT: the installation id
//     GitHub sent back is not trusted, since anyone can make the
//     browser load the setup address with one. The one installation is
//     kept, and setup is done. With none yet, the Go to the
//     new-installation page again, or, for an installation an
//     organization's owners have yet to approve, a Wait that checks
//     again.
//
// Nothing is kept before GitHub made the app. The Fixes are FlowInstall
// (steps 3's Go and 4 again), FlowPermissions and FlowSettings.
//
// Core scrubs every secret kept value (the client secret, the private
// key) and every State value of six characters or more from what a
// step shows, so State holds short markers alone. The app's id, slug,
// client id and installation id are declared not secret: a page a step
// names may carry them.
func (g *GitHub) Setup(ctx context.Context, conn builtinintegrations.Connection, flow string, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
	switch flow {
	case builtinintegrations.FlowSetup:
		return g.install(ctx, conn, last, p)
	case FlowInstall:
		if last.State["at"] == "" {
			return goInstall(conn.Fields[FieldSlug], last.Callback, "Install the app on GitHub again, and choose the repositories agents may reach.")
		}
		return g.checkInstalled(ctx, conn, last, p)
	case FlowPermissions, FlowSettings:
		return g.mend(ctx, conn, flow, last)
	}
	return builtinintegrations.Step{}, errors.New("GitHub has no flow named " + flow)
}

// install is the install flow's next step after last.
func (g *GitHub) install(ctx context.Context, conn builtinintegrations.Connection, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
	const formWords = "Name the GitHub account to make the app on: an organization's name, or leave it blank for your personal account."
	switch last.State["at"] {
	case "":
		return builtinintegrations.Step{
			Form:  &builtinintegrations.Form{Words: formWords, Fields: []string{"account"}},
			State: map[string]string{"at": "form"},
		}, nil

	case "form":
		account := strings.TrimPrefix(strings.TrimSpace(last.Form["account"]), "@")
		if account != "" && !accountName.MatchString(account) {
			return builtinintegrations.Step{
				Form:  &builtinintegrations.Form{Words: "That is not the name of a GitHub account: letters, digits and hyphens. " + formWords, Fields: []string{"account"}},
				State: last.State,
			}, nil
		}
		return createApp(account, last)

	case "make":
		p.Stage("Creating the app on GitHub", 0, 0)
		return g.convert(ctx, last)

	case "inst", "wait":
		return g.checkInstalled(ctx, conn, last, p)
	}
	return builtinintegrations.Step{}, fmt.Errorf("GitHub's setup is at %q, which it does not know", last.State["at"])
}

// createApp is the Go that posts the app's manifest to GitHub's
// new-app page for account ("" for the owner's personal account). The
// manifest's redirect_url is the bare callback: the flow's state goes
// on the new-app page's address alone, and GitHub sends it back with
// the code. The manifest gives no callback_urls, the addresses a
// user's sign-in with the app returns to: the app signs no user in,
// since setup hands over to no sign-in. One that did would list every
// address in Answer.Addresses there.
func createApp(account string, last builtinintegrations.Answer) (builtinintegrations.Step, error) {
	base, setupURL, state, err := callbackParts(last.Callback)
	if err != nil {
		return builtinintegrations.Step{}, err
	}
	m, err := json.Marshal(appManifest{
		Name:               appName(last.Team),
		URL:                base,
		RedirectURL:        setupURL,
		SetupURL:           setupURL,
		SetupOnUpdate:      true,
		Public:             false,
		HookAttributes:     hookAttributes{URL: hookPlaceholder, Active: false},
		DefaultPermissions: needed(),
		DefaultEvents:      []string{},
	})
	if err != nil {
		return builtinintegrations.Step{}, err
	}
	target := webBase + "/settings/apps/new"
	if account != "" {
		target = webBase + "/organizations/" + url.PathEscape(account) + "/settings/apps/new"
	}
	if state != "" {
		target += "?" + url.Values{"state": {state}}.Encode()
	}
	words := "Create the app on GitHub: choose Create GitHub App. GitHub may ask you to change the name first."
	if strings.HasPrefix(base, "http://") {
		words += " GitHub may refuse this team's address, which is not https; if it does, give the team an https address under Other devices in Kivali Desktop, and start again from there."
	}
	return builtinintegrations.Step{
		Go:    &builtinintegrations.Go{Words: words, URL: target, Post: url.Values{"manifest": {string(m)}}},
		State: map[string]string{"at": "make"},
	}, nil
}

// conversion is GitHub's answer to a manifest's code: the app it made.
type conversion struct {
	ID           int64  `json:"id"`
	Slug         string `json:"slug"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	PEM          string `json:"pem"`
}

// convert trades the code GitHub sent the browser back with for the
// app (POST /app-manifests/{code}/conversions, with no credential),
// keeps it, and sends the owner to install it. The code works once,
// within the hour, and is kept nowhere; the webhook secret GitHub gives
// is dropped.
func (g *GitHub) convert(ctx context.Context, last builtinintegrations.Answer) (builtinintegrations.Step, error) {
	code := last.Return.Get("code")
	if code == "" || len(code) > 200 || strings.ContainsAny(code, "/?#% ") {
		return builtinintegrations.Step{}, errors.New("GitHub sent the browser back without the app's code; start again")
	}
	var app conversion
	err := g.sendJSON(ctx, request{method: http.MethodPost, path: "/app-manifests/" + url.PathEscape(code) + "/conversions"}, &app)
	if err != nil {
		var ae *apiError
		switch {
		case !errors.As(err, &ae):
			return builtinintegrations.Step{}, err
		case ae.Timeout:
			return builtinintegrations.Step{}, errors.New("GitHub did not answer in time while making the app; start again")
		case ae.Status == 0:
			return builtinintegrations.Step{}, errors.New("GitHub did not answer while making the app: " + sentence(ae.Message) + ". Start again")
		case ae.Status == http.StatusNotFound:
			return builtinintegrations.Step{}, fmt.Errorf("GitHub did not take the app's code (HTTP 404): %s. A code works once, within an hour; start again", sentence(ae.Message))
		}
		return builtinintegrations.Step{}, fmt.Errorf("GitHub did not make the app (HTTP %d): %s. Start again", ae.Status, sentence(ae.Message))
	}
	var missing []string
	if app.ID <= 0 {
		missing = append(missing, "its id")
	}
	if !appSlug.MatchString(app.Slug) {
		missing = append(missing, "its name")
	}
	if app.ClientID == "" || app.ClientSecret == "" {
		missing = append(missing, "its client")
	}
	if _, err := parsePrivateKey(app.PEM); err != nil {
		missing = append(missing, "its private key")
	}
	if len(missing) > 0 {
		return builtinintegrations.Step{}, errors.New("GitHub's answer for the new app does not hold " + strings.Join(missing, ", ") + "; start again")
	}
	step, err := goInstall(app.Slug, last.Callback, "Install the app on GitHub: choose the account and the repositories agents may reach.")
	if err != nil {
		return step, err
	}
	step.Keep = map[string]string{
		FieldAppID:        strconv.FormatInt(app.ID, 10),
		FieldSlug:         app.Slug,
		FieldClientID:     app.ClientID,
		FieldClientSecret: app.ClientSecret,
		FieldPrivateKey:   app.PEM,
	}
	return step, nil
}

// goInstall is the Go to the app slug's new-installation page. GitHub
// sends the browser back to the setup address made with the app, which
// may carry no state of this flow's, so the step is Stateless; the
// state goes with the page's address too, for GitHub to pass on.
func goInstall(slug, callback, words string) (builtinintegrations.Step, error) {
	if !appSlug.MatchString(slug) {
		return builtinintegrations.Step{}, errors.New("the connection does not hold the app's name on GitHub; set GitHub up again")
	}
	_, _, state, _ := callbackParts(callback)
	return builtinintegrations.Step{
		Go:    &builtinintegrations.Go{Words: words, URL: installURL(slug, state), Stateless: true},
		State: map[string]string{"at": "inst"},
	}, nil
}

// checkInstalled lists the app's installations with its JWT and keeps
// the one there is: setup is done. With none yet, the owner is sent to
// GitHub's new-installation page again; or, when GitHub sent the
// browser back with an installation an organization's owners have yet
// to approve (setup_action=request), or a Wait is being checked, a
// Wait checks again. The installation id GitHub sent the browser back
// with only picks among the app's own installations, should it have
// more than one.
func (g *GitHub) checkInstalled(ctx context.Context, conn builtinintegrations.Connection, last builtinintegrations.Answer, p builtinintegrations.Progress) (builtinintegrations.Step, error) {
	p.Stage("Waiting for the installation", 0, 0)
	app, err := readApp(conn)
	if err != nil {
		return builtinintegrations.Step{}, errors.New("the connection does not hold the app GitHub made; start again")
	}
	// The Wait names the new-installation page, without the flow's
	// state: an owner who left it there without installing finishes it
	// from this page, and the check finds the installation.
	wait := builtinintegrations.Step{
		Wait: &builtinintegrations.Wait{
			Words: "Waiting for the app's installation on GitHub. An organization's owner may have to approve it there; this page carries on once it is installed.",
		},
		State: map[string]string{"at": "wait"},
	}
	if appSlug.MatchString(app.slug) {
		wait.Wait.URL = installURL(app.slug, "")
	}
	list, err := g.installations(ctx, app)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			if transient(ae) {
				return wait, nil
			}
			return builtinintegrations.Step{}, errors.New("GitHub did not list the app's installations: " + sentence(ownerWords(ae)))
		}
		return builtinintegrations.Step{}, err
	}
	pick := func(in Installation) builtinintegrations.Step {
		return builtinintegrations.Step{Done: true, Keep: map[string]string{FieldInstallationID: strconv.FormatInt(in.ID, 10)}}
	}
	switch len(list) {
	case 0:
		if last.Checked || last.Return.Get("setup_action") == "request" {
			return wait, nil
		}
		return goInstall(app.slug, last.Callback, "GitHub lists no installation of the app yet. Install it on GitHub: choose the account and the repositories agents may reach.")
	case 1:
		return pick(list[0]), nil
	}
	if id := last.Return.Get("installation_id"); id != "" {
		for _, in := range list {
			if strconv.FormatInt(in.ID, 10) == id {
				return pick(in), nil
			}
		}
	}
	return builtinintegrations.Step{}, errors.New("GitHub lists more than one installation of the app; keep one on GitHub, and start again")
}

// transient reports whether a GitHub failure may pass by itself: no
// answer, a limit on the app's requests, or GitHub's own failure.
func transient(e *apiError) bool {
	return e.Status == 0 || e.RateLimited || e.Status >= 500
}

// mend is a Fix that sends the owner to a page on GitHub (FlowPermissions:
// where the missing permissions are granted; FlowSettings: the
// installation's settings), then checks the installation until it is
// mended. Already mended, it is done at once.
func (g *GitHub) mend(ctx context.Context, conn builtinintegrations.Connection, flow string, last builtinintegrations.Answer) (builtinintegrations.Step, error) {
	c, err := readConnection(conn)
	if err != nil {
		return builtinintegrations.Step{}, errors.New("the connection does not hold all of the app's values; set GitHub up again")
	}
	later := last.State["at"] != ""
	inst, err := g.installationOf(ctx, c)
	if err != nil {
		if later && isTransient(err) {
			return waitMend(flow, ""), nil
		}
		return builtinintegrations.Step{}, mendError(err)
	}
	// The repositories are counted for the settings Fix alone: it costs
	// a token and a request, on every check of the Wait.
	repos := -1
	if flow == FlowSettings {
		repos = g.reaches(ctx, c, inst)
	}
	if mended(flow, inst, repos) {
		return builtinintegrations.Step{Done: true}, nil
	}
	page, words, err := g.mendPage(ctx, c, flow, inst)
	switch {
	case err != nil && later && isTransient(err):
		return waitMend(flow, ""), nil
	case err != nil:
		return builtinintegrations.Step{}, mendError(err)
	case later:
		return waitMend(flow, page), nil
	}
	return builtinintegrations.Step{
		Go:    &builtinintegrations.Go{Words: words, URL: page, NoReturn: true},
		State: map[string]string{"at": "went"},
	}, nil
}

// mendPage is the page on GitHub where flow's mending is done, and what
// the owner does there. The installation's settings choose its
// repositories, unsuspend it and approve the permissions its app asks
// for; a permission the app itself does not ask for yet is given in the
// app's settings first, and the installation then approves it.
func (g *GitHub) mendPage(ctx context.Context, c connection, flow string, inst *installationInfo) (page, words string, err error) {
	if flow != FlowPermissions {
		return inst.settingsURL(), "On GitHub, choose the repositories the app reaches, and unsuspend it if it is suspended.", nil
	}
	app, err := g.appOf(ctx, c)
	if err != nil {
		return "", "", err
	}
	if lack := lacking(app.Permissions); len(lack) > 0 {
		return app.settingsURL() + "/permissions",
			"On GitHub, give the app these permissions and save: " + strings.Join(lack, ", ") + ". Then approve them for its installation, where GitHub asks.", nil
	}
	return inst.settingsURL(), "On GitHub, approve the permissions the app asks for: " + strings.Join(lacking(inst.Permissions), ", ") + ".", nil
}

// waitMend is the Wait of a mending Fix, naming page ("" for none).
func waitMend(flow, page string) builtinintegrations.Step {
	words := "Kivali checks GitHub until the installation is not suspended and reaches a repository."
	if flow == FlowPermissions {
		words = "Kivali checks GitHub until the installation has every permission this version needs."
	}
	return builtinintegrations.Step{Wait: &builtinintegrations.Wait{Words: words, URL: page}, State: map[string]string{"at": "wait"}}
}

// mended reports whether the installation is as flow mends it, reaching
// repos repositories (-1: not known).
func mended(flow string, inst *installationInfo, repos int) bool {
	if flow == FlowPermissions {
		return len(lacking(inst.Permissions)) == 0
	}
	return !inst.suspended() && repos != 0
}

func isTransient(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && transient(ae)
}

func mendError(err error) error {
	var ae *apiError
	if errors.As(err, &ae) {
		return errors.New(sentence(ownerWords(ae)))
	}
	return err
}

// readApp reads conn's app id and private key, which a JWT needs: what
// setup kept before the app is installed.
func readApp(conn builtinintegrations.Connection) (connection, error) {
	c := connection{id: conn.ID, appID: strings.TrimSpace(conn.Fields[FieldAppID]), slug: strings.TrimSpace(conn.Fields[FieldSlug])}
	if !digits(c.appID) {
		return c, errors.New("the connection holds no app id")
	}
	key, err := parsePrivateKey(conn.Fields[FieldPrivateKey])
	if err != nil {
		return c, errors.New("the connection's private key does not read")
	}
	c.key = key
	return c, nil
}

// account is a GitHub account as an answer names it.
type account struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// settingsURL is the account's settings on GitHub: an organization's,
// or the owner's own.
func (a account) settingsURL() string {
	if a.Type == "Organization" {
		return webBase + "/organizations/" + url.PathEscape(a.Login) + "/settings"
	}
	return webBase + "/settings"
}

// appInfo is GET /app's answer: the app the JWT is signed for.
type appInfo struct {
	Slug        string            `json:"slug"`
	Name        string            `json:"name"`
	Owner       account           `json:"owner"`
	Permissions map[string]string `json:"permissions"`
}

// settingsURL is the app's settings on GitHub, where it is deleted.
func (a *appInfo) settingsURL() string {
	return a.Owner.settingsURL() + "/apps/" + url.PathEscape(a.Slug)
}

// installationInfo is GET /app/installations/{id}'s answer.
type installationInfo struct {
	ID                  int64             `json:"id"`
	Account             account           `json:"account"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	SuspendedAt         *string           `json:"suspended_at"`
}

func (in *installationInfo) suspended() bool { return in.SuspendedAt != nil && *in.SuspendedAt != "" }

// settingsURL is the installation's settings on GitHub, where its
// repositories are chosen and its permissions approved.
func (in *installationInfo) settingsURL() string {
	return in.Account.settingsURL() + "/installations/" + strconv.FormatInt(in.ID, 10)
}

// appOf is the app c's key signs for (GET /app, with its JWT).
func (g *GitHub) appOf(ctx context.Context, c connection) (*appInfo, error) {
	jwt, err := g.jwt(c)
	if err != nil {
		return nil, err
	}
	var app appInfo
	if err := g.getJSON(ctx, jwt, "/app", &app); err != nil {
		return nil, err
	}
	if app.Slug == "" {
		app.Slug = c.slug
	}
	return &app, nil
}

// installationOf is c's installation (GET /app/installations/{id},
// with the app's JWT).
func (g *GitHub) installationOf(ctx context.Context, c connection) (*installationInfo, error) {
	jwt, err := g.jwt(c)
	if err != nil {
		return nil, err
	}
	var in installationInfo
	if err := g.getJSON(ctx, jwt, "/app/installations/"+url.PathEscape(c.installationID), &in); err != nil {
		return nil, err
	}
	if in.ID == 0 {
		in.ID, _ = strconv.ParseInt(c.installationID, 10, 64)
	}
	return &in, nil
}

// repositoryCount is how many repositories c's installation reaches
// (GET /installation/repositories, with a token of the installation).
func (g *GitHub) repositoryCount(ctx context.Context, c connection) (int, error) {
	tok, err := g.installationToken(ctx, c, "", perms())
	if err != nil {
		return 0, err
	}
	var out struct {
		TotalCount int `json:"total_count"`
	}
	if err := g.getJSON(ctx, tok, "/installation/repositories?per_page=1", &out); err != nil {
		return 0, err
	}
	return out.TotalCount, nil
}

// reaches is how many repositories inst reaches: -1 for all, or when
// it is not known (suspended, or GitHub did not say).
func (g *GitHub) reaches(ctx context.Context, c connection, inst *installationInfo) int {
	if inst.RepositorySelection == "all" || inst.suspended() {
		return -1
	}
	n, err := g.repositoryCount(ctx, c)
	if err != nil {
		return -1
	}
	return n
}

// lacking is each permission this version needs that have does not
// grant, in words ("issues: write"), in order. write covers read, and
// admin covers both.
func lacking(have map[string]string) []string {
	need := needed()
	names := make([]string, 0, len(need))
	for n := range need {
		names = append(names, n)
	}
	slices.Sort(names)
	var out []string
	for _, n := range names {
		want, got := need[n], have[n]
		if got == want || got == "admin" || (got == "write" && want == "read") {
			continue
		}
		out = append(out, strings.ReplaceAll(n, "_", " ")+": "+want)
	}
	return out
}

var (
	_ builtinintegrations.SetupIntegration = (*GitHub)(nil)
	_ builtinintegrations.RequirementsOf   = (*GitHub)(nil)
	_ builtinintegrations.Uninstaller      = (*GitHub)(nil)
	_ builtinintegrations.SignInReplacer   = (*GitHub)(nil)
)
