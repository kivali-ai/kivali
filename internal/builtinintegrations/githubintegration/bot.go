package githubintegration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// botUser is the app's bot user on GitHub: the login it writes as,
// <slug>[bot], and its user id, which its no-reply email address names.
type botUser struct {
	login string
	id    int64
}

// noreply is the bot's no-reply email address, the one GitHub resolves
// to the bot user in a commit.
func (b botUser) noreply() string {
	return fmt.Sprintf("%d+%s@users.noreply.github.com", b.id, b.login)
}

// botKey is what a kept bot user is for: the connection, its app and
// the slug the connection keeps ("" for none).
type botKey struct {
	conn, app, slug string
}

// botLogin is the login of the app's bot user: the connection's slug's,
// or for a connection that keeps none (one set up before it was kept),
// the slug GitHub gives the app (GET /app, with the app's JWT).
func (g *GitHub) botLogin(ctx context.Context, c connection) (string, error) {
	if appSlug.MatchString(c.slug) {
		return c.slug + "[bot]", nil
	}
	app, err := g.appOf(ctx, c)
	if err != nil {
		return "", err
	}
	if !appSlug.MatchString(app.Slug) {
		return "", refuse(notSetUp, "GitHub names no slug for the app")
	}
	return app.Slug + "[bot]", nil
}

// bot is the app's bot user (GET /users/<login>, with an installation
// token narrowed to repo that reads metadata alone: the app's JWT
// reaches only the /app endpoints). It is read once per connection and
// kept in this process's memory, and nowhere else; it names nothing
// secret.
func (g *GitHub) bot(ctx context.Context, c connection, repo string) (botUser, error) {
	k := botKey{conn: c.id, app: c.appID, slug: c.slug}
	g.botsMu.Lock()
	b, ok := g.bots[k]
	g.botsMu.Unlock()
	if ok {
		return b, nil
	}
	login, err := g.botLogin(ctx, c)
	if err != nil {
		return botUser{}, err
	}
	tok, err := g.installationToken(ctx, c, repo, perms())
	if err != nil {
		return botUser{}, err
	}
	b, err = g.botNamed(ctx, tok, login)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound && appSlug.MatchString(c.slug) {
		// The kept slug may be the app's name before a rename; GitHub
		// gives the one it has now.
		if app, aerr := g.appOf(ctx, c); aerr == nil && appSlug.MatchString(app.Slug) && app.Slug != c.slug {
			b, err = g.botNamed(ctx, tok, app.Slug+"[bot]")
		}
	}
	if err != nil {
		return botUser{}, err
	}
	g.botsMu.Lock()
	if g.bots == nil {
		g.bots = map[botKey]botUser{}
	}
	g.bots[k] = b
	g.botsMu.Unlock()
	return b, nil
}

// botNamed is the bot user login names (GET /users/<login>, with tok):
// an error unless GitHub answers a Bot of that login.
func (g *GitHub) botNamed(ctx context.Context, tok, login string) (botUser, error) {
	var u struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	}
	if err := g.getJSON(ctx, tok, "/users/"+url.PathEscape(login), &u); err != nil {
		return botUser{}, err
	}
	if u.ID <= 0 || u.Type != "Bot" || !strings.EqualFold(u.Login, login) {
		return botUser{}, &apiError{Message: "its answer for " + login + " is not the app's bot user"}
	}
	return botUser{login: login, id: u.ID}, nil
}
