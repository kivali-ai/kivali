package gitintegration

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/egresscred"
	"github.com/kivali-ai/kivali/internal/gitrepo"
)

// The account endpoints Test connection asks when the connection names
// no repository.
const (
	gitHubAPI = "https://api.github.com"
	gitLabAPI = "https://gitlab.com/api/v4"
)

// accountAnswerMax bounds what is read of an account endpoint's answer.
const accountAnswerMax = 64 << 10

// ownerWords is an error whose text is a sentence for the owner, as
// Test connection shows it.
type ownerWords string

func (e ownerWords) Error() string { return string(e) }

// errNoAccountEndpoint is Test connection for a host other than
// github.com and gitlab.com with no repository named.
const errNoAccountEndpoint = ownerWords("Name a repository for Test connection: this host has no account endpoint Kivali knows.")

// Verify is Test connection, run in core, which reaches the host
// directly. With a test repository it lists the repository's refs in
// memory and counts its branches; without one it asks GitHub's or
// GitLab's account endpoint who the token is. A failure is the host's
// words with the token taken out.
func (g *Git) Verify(ctx context.Context, conn builtinintegrations.Connection) (string, error) {
	host, err := gitrepo.NormalizeHost(conn.Fields["host"])
	if err != nil {
		return "", fmt.Errorf("the host is not a host name: give one such as github.com, without https://")
	}
	token := conn.Fields["token"]
	if token == "" {
		return "", errors.New("the connection has no access token")
	}
	scrub := egresscred.Combine(egresscred.NewScrubber(tokenUser, token, ""), egresscred.NewScrubber("Bearer", token, ""))
	if ref := strings.TrimSpace(conn.Fields["test_repository"]); ref != "" {
		msg, err := g.listBranches(ctx, host, token, ref)
		if err != nil {
			return "", errors.New(scrub.String(err.Error()))
		}
		return msg, nil
	}
	var who string
	switch gitrepo.HostName(host) {
	case "github.com":
		who, err = g.account(ctx, cmp.Or(g.GitHubAPI, gitHubAPI)+"/user", "Authorization", "Bearer "+token, "login")
	case "gitlab.com":
		who, err = g.account(ctx, cmp.Or(g.GitLabAPI, gitLabAPI)+"/user", "PRIVATE-TOKEN", token, "username")
	default:
		return "", errNoAccountEndpoint
	}
	if err != nil {
		return "", errors.New(scrub.String(fmt.Sprintf("%s did not accept the token: %v", host, err)))
	}
	return fmt.Sprintf("Connected to %s as %s.", host, who), nil
}

// listBranches lists ref's refs with the token and counts its branches.
func (g *Git) listBranches(ctx context.Context, host, token, ref string) (string, error) {
	repo, err := gitrepo.Parse(ref, host, g.InsecureHTTP)
	if err != nil {
		return "", fmt.Errorf("the repository for Test connection: %v", err)
	}
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{repo.URL()}})
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: &githttp.BasicAuth{Username: tokenUser, Password: token}})
	if err != nil {
		return "", fmt.Errorf("%s did not list %s: %v", host, repo.Path, err)
	}
	n := 0
	for _, r := range refs {
		if r.Name().IsBranch() {
			n++
		}
	}
	noun := "branches"
	if n == 1 {
		noun = "branch"
	}
	return fmt.Sprintf("Connected. %d %s in %s.", n, noun, repo.Path), nil
}

// account asks an account endpoint who the token is: a GET with the
// token in header, answered with a JSON object whose field names them.
func (g *Git) account(ctx context.Context, url, header, value, field string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(header, value)
	req.Header.Set("Accept", "application/json")
	client := g.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, accountAnswerMax))
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		msg, _ := out["message"].(string)
		if msg == "" {
			msg = clip(strings.TrimSpace(string(body)), 200)
		}
		return "", fmt.Errorf("%s (%s)", cmp.Or(msg, "no reason given"), resp.Status)
	}
	name, _ := out[field].(string)
	if name == "" {
		return "", errors.New("the answer does not name the account")
	}
	return clip(name, 200), nil
}
