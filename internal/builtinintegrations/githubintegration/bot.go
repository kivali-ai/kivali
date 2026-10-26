package githubintegration

import (
	"context"
	"fmt"
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

// bot is the app's bot user (GET /users/<slug>[bot], with an
// installation token narrowed to repo that reads metadata alone: the
// app's JWT reaches only the /app endpoints). The slug is the one
// GitHub gives the app now (GET /app, with the app's JWT), so an app
// renamed since setup is known by its new name; the connection's when
// GitHub does not answer with one. It is read once per connection and
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
	slug := c.slug
	app, err := g.appOf(ctx, c)
	switch {
	case err == nil && appSlug.MatchString(app.Slug):
		slug = app.Slug
	case appSlug.MatchString(slug):
	case err != nil:
		return botUser{}, err
	default:
		return botUser{}, refuse(notSetUp, "GitHub names no slug for the app")
	}
	tok, err := g.installationToken(ctx, c, repo, perms())
	if err != nil {
		return botUser{}, err
	}
	login := slug + "[bot]"
	var u struct {
		Login string `json:"login"`
		ID    int64  `json:"id"`
		Type  string `json:"type"`
	}
	if err := g.getJSON(ctx, tok, "/users/"+url.PathEscape(login), &u); err != nil {
		return botUser{}, err
	}
	if u.ID <= 0 || u.Type != "Bot" || !strings.EqualFold(u.Login, login) {
		return botUser{}, failure("GitHub's user " + login + " is not the GitHub App's bot user; ask the owner.")
	}
	b = botUser{login: login, id: u.ID}
	g.botsMu.Lock()
	if g.bots == nil {
		g.bots = map[botKey]botUser{}
	}
	g.bots[k] = b
	g.botsMu.Unlock()
	return b, nil
}
