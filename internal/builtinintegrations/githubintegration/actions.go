package githubintegration

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// The actions, unprefixed.
const (
	actionListRepositories  = "list_repositories"
	actionGetRepository     = "get_repository"
	actionListIssues        = "list_issues"
	actionGetIssue          = "get_issue"
	actionCreateIssue       = "create_issue"
	actionComment           = "comment"
	actionCloseIssue        = "close_issue"
	actionListPullRequests  = "list_pull_requests"
	actionGetPullRequest    = "get_pull_request"
	actionCreatePullRequest = "create_pull_request"
	actionClosePullRequest  = "close_pull_request"
	actionListBranches      = "list_branches"
	actionDeleteBranch      = "delete_branch"
	actionCompare           = "compare"
	actionWhoami            = "whoami"
)

// action is one of GitHub's REST actions. description is what the agent's
// model reads, ownerDescription the manifest's, for the owner. write
// marks an action that writes to GitHub, which an agent has only when
// its access allows writing (its manifest tool's access is the write
// switch, and its ownerDescription ends with needsWrite); ownerOnly
// keeps an action for Test connection, out of the manifest's tools.
type action struct {
	name             string
	description      string
	ownerDescription string
	schema           string
	write            bool
	ownerOnly        bool
	run              func(ctx context.Context, c *call) (any, error)
}

// ownerDecides ends every description an agent reads.
const ownerDecides = " The owner decides which repositories you may use and whether you may open, comment on and close issues and pull requests, push and delete branches."

// needsWrite ends the owner's description of every write action: the
// write switch, in its title's words.
const needsWrite = " Only when the agent's access may create, comment, push and close."

// actions is every REST action: the manifest's tools are these in this
// order, less the ownerOnly one, then clone, fetch and push (git.go).
var actions = []action{
	{
		name:             actionListRepositories,
		description:      "List the GitHub repositories your access includes, one per line, with whether each is private, when it was last updated and its description.",
		ownerDescription: "List the repositories the agent's access names, with whether each is private and when it last changed. Reads only.",
		schema:           schema(nil),
		run:              listRepositories,
	},
	{
		name:             actionGetRepository,
		description:      "Read a GitHub repository's details: its description, default branch, whether it is private, its address and when it was last updated.",
		ownerDescription: "Read a repository's details: description, default branch, visibility, address. Reads only.",
		schema:           schema(map[string]any{"repository": repositoryProperty}, "repository"),
		run:              getRepository,
	},
	{
		name:             actionListIssues,
		description:      "List a GitHub repository's issues, open ones unless you choose another state, newest first, filtered by labels or search words.",
		ownerDescription: "List a repository's issues by state, labels or search words, at most 50. Reads only.",
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"state":      map[string]any{"type": "string", "enum": []string{"open", "closed", "all"}, "description": "open when left out."},
			"limit":      limit50,
			"labels":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Only issues with every one of these labels."},
			"search":     map[string]any{"type": "string", "description": `GitHub search words within this repository, such as "crash in:title"; it cannot name other repositories, organizations or users.`},
		}, "repository"),
		run: listIssues,
	},
	{
		name:             actionGetIssue,
		description:      "Read one GitHub issue: its title, state, body (the first 20,000 characters), labels and its latest 20 comments.",
		ownerDescription: "Read one issue with its body and latest 20 comments. Reads only.",
		schema:           schema(map[string]any{"repository": repositoryProperty, "number": numberProperty}, "repository", "number"),
		run:              getIssue,
	},
	{
		name:             actionCreateIssue,
		description:      "Open an issue in a GitHub repository, as the owner's GitHub App. Answers the issue's address.",
		ownerDescription: "Open an issue in a repository, as the GitHub App." + needsWrite,
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"title":      map[string]any{"type": "string"},
			"body":       markdown,
			"labels":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Labels the repository already has."},
		}, "repository", "title"),
		write: true,
		run:   createIssue,
	},
	{
		name:             actionComment,
		description:      "Comment on a GitHub issue or pull request by its number, as the owner's GitHub App. Answers the comment's address.",
		ownerDescription: "Comment on an issue or pull request, as the GitHub App." + needsWrite,
		schema:           schema(map[string]any{"repository": repositoryProperty, "number": numberProperty, "body": markdown}, "repository", "number", "body"),
		write:            true,
		run:              comment,
	},
	{
		name:             actionCloseIssue,
		description:      "Close an issue in a GitHub repository that the owner's GitHub App opened, posting your comment on it first when you give one. Only an issue the app opened can be closed; a pull request is closed with github_close_pull_request. Answers the issue's address.",
		ownerDescription: "Close an issue the GitHub App opened, with the agent's comment when it gives one; never an issue anyone else opened." + needsWrite,
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"number":     map[string]any{"type": "integer", "description": "The issue's number."},
			"comment":    map[string]any{"type": "string", "description": "Markdown, posted on the issue before it is closed."},
		}, "repository", "number"),
		write: true,
		run:   closeIssue,
	},
	{
		name:             actionListPullRequests,
		description:      "List a GitHub repository's pull requests, open ones unless you choose another state, optionally only those into one base branch.",
		ownerDescription: "List a repository's pull requests by state or base branch, at most 50. Reads only.",
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"state":      map[string]any{"type": "string", "enum": []string{"open", "closed", "merged", "all"}, "description": "open when left out."},
			"limit":      limit50,
			"base":       map[string]any{"type": "string", "description": "Only pull requests into this branch."},
		}, "repository"),
		run: listPullRequests,
	},
	{
		name:             actionGetPullRequest,
		description:      "Read one GitHub pull request: its title, state, body, branches, whether it can be merged and its size, and its diff when you ask for it.",
		ownerDescription: "Read one pull request, and its diff when asked. Reads only.",
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"number":     numberProperty,
			"diff":       map[string]any{"type": "boolean", "description": "Also answer the diff, cut at 50,000 characters."},
		}, "repository", "number"),
		run: getPullRequest,
	},
	{
		name:             actionCreatePullRequest,
		description:      "Open a pull request in a GitHub repository from the head branch into the base branch, as the owner's GitHub App. The head branch must already be on GitHub: this action moves no commits, so push it first with github_push, or ask whoever owns the branch. Answers the pull request's address.",
		ownerDescription: "Open a pull request from a branch already on GitHub, as the GitHub App; moves no commits." + needsWrite,
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"title":      map[string]any{"type": "string"},
			"head":       map[string]any{"type": "string", "description": "The branch with the changes, already on GitHub in this repository."},
			"base":       map[string]any{"type": "string", "description": "The branch to merge into, such as main."},
			"body":       markdown,
			"draft":      map[string]any{"type": "boolean", "description": "Open it as a draft."},
		}, "repository", "title", "head", "base"),
		write: true,
		run:   createPullRequest,
	},
	{
		name:             actionClosePullRequest,
		description:      "Close a pull request in a GitHub repository that the owner's GitHub App opened, without merging it, posting your comment on it first when you give one. Only a pull request the app opened can be closed, and a merged one cannot be. Answers the pull request's address.",
		ownerDescription: "Close a pull request the GitHub App opened, unmerged, with the agent's comment when it gives one; never a pull request anyone else opened." + needsWrite,
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"number":     map[string]any{"type": "integer", "description": "The pull request's number."},
			"comment":    map[string]any{"type": "string", "description": "Markdown, posted on the pull request before it is closed."},
		}, "repository", "number"),
		write: true,
		run:   closePullRequest,
	},
	{
		name:             actionListBranches,
		description:      "List a GitHub repository's branches, one per line with the commit each points at.",
		ownerDescription: "List a repository's branches and the commit each points at. Reads only.",
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"limit":      map[string]any{"type": "integer", "description": "How many to list, at most 100; 30 when left out."},
		}, "repository"),
		run: listBranches,
	},
	{
		name:             actionDeleteBranch,
		description:      "Delete a branch of a GitHub repository whose work is done: every commit it has beyond the repository's default branch must have been authored and committed by the owner's GitHub App's bot, as git records them, or it has none, being merged. The default branch, and a branch an open pull request in the repository has as its head or its base, cannot be deleted. Answers what it deleted.",
		ownerDescription: "Delete a branch whose commits beyond the default branch the GitHub App's bot authored and committed, every one, or a branch merged into it; never the default branch, nor a branch an open pull request comes from or goes into." + needsWrite,
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"branch":     map[string]any{"type": "string", "description": "The branch's name, such as agent/fix-1."},
		}, "repository", "branch"),
		write: true,
		run:   deleteBranch,
	},
	{
		name:             actionCompare,
		description:      "Compare two branches, tags or commits of a GitHub repository: how far head is ahead of and behind base, its commits, and the files changed with lines added and removed (no patches).",
		ownerDescription: "Compare two branches, tags or commits: commits ahead and behind, files changed. Reads only.",
		schema: schema(map[string]any{
			"repository": repositoryProperty,
			"base":       map[string]any{"type": "string", "description": "A branch, tag or commit."},
			"head":       map[string]any{"type": "string", "description": "A branch, tag or commit."},
		}, "repository", "base", "head"),
		run: compare,
	},
	{
		name:             actionWhoami,
		description:      "Name the GitHub App the connection acts as, and the account it is installed on.",
		ownerDescription: "Name the GitHub App the connection acts as and the account it is installed on, for Test connection. Reads only.",
		schema:           schema(nil),
		ownerOnly:        true,
	},
}

// actionNamed is the action called name, nil for none.
func actionNamed(name string) *action {
	for i := range actions {
		if actions[i].name == name {
			return &actions[i]
		}
	}
	return nil
}

var (
	repositoryProperty = map[string]any{"type": "string", "description": "The repository as owner/name, such as acme/widgets. list_repositories names the ones your access includes."}
	numberProperty     = map[string]any{"type": "integer", "description": "The issue's or pull request's number."}
	limit50            = map[string]any{"type": "integer", "description": "How many to list, at most 50; 30 when left out."}
	markdown           = map[string]any{"type": "string", "description": "Markdown."}
)

// schema is an object's input schema with props, required naming the
// ones it must have.
func schema(props map[string]any, required ...string) string {
	if props == nil {
		props = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	b, err := json.Marshal(map[string]any{"type": "object", "properties": props, "required": required})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// call is one action's run.
type call struct {
	g      *GitHub
	conn   connection
	access access
	args   args
	// current is the login of the app's bot user by the slug GitHub
	// gives the app now, once isAppBot has asked (asked).
	current string
	asked   bool
}

// token is an installation token for repo (owner/name; "" for the
// whole installation) that may do p.
func (c *call) token(ctx context.Context, repo string, p permissions) (string, error) {
	return c.g.installationToken(ctx, c.conn, repo, p)
}

// maxListPages bounds the pages a listing filtered after GitHub's
// answer reads to fill its limit.
const maxListPages = 10

// The shapes GitHub answers in, as far as the actions read them.
type (
	ghUser struct {
		Login string `json:"login"`
		// Type is User, Organization or, for an app's bot user, Bot.
		Type string `json:"type"`
	}
	ghRef struct {
		Ref string `json:"ref"`
	}
	ghIssue struct {
		Number      int64           `json:"number"`
		Title       string          `json:"title"`
		State       string          `json:"state"`
		Body        string          `json:"body"`
		User        ghUser          `json:"user"`
		Labels      labelNames      `json:"labels"`
		Comments    int             `json:"comments"`
		UpdatedAt   string          `json:"updated_at"`
		HTMLURL     string          `json:"html_url"`
		PullRequest json.RawMessage `json:"pull_request"`
	}
	ghComment struct {
		User      ghUser `json:"user"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		HTMLURL   string `json:"html_url"`
	}
	ghPull struct {
		Number       int64      `json:"number"`
		Title        string     `json:"title"`
		State        string     `json:"state"`
		Body         string     `json:"body"`
		User         ghUser     `json:"user"`
		Head         ghRef      `json:"head"`
		Base         ghRef      `json:"base"`
		Draft        bool       `json:"draft"`
		MergedAt     *string    `json:"merged_at"`
		Mergeable    *bool      `json:"mergeable"`
		Additions    int        `json:"additions"`
		Deletions    int        `json:"deletions"`
		ChangedFiles int        `json:"changed_files"`
		Labels       labelNames `json:"labels"`
		UpdatedAt    string     `json:"updated_at"`
		HTMLURL      string     `json:"html_url"`
	}
	ghCreated struct {
		HTMLURL string `json:"html_url"`
	}
)

// isPullRequest reports whether an issue GitHub answered is a pull
// request: the issues endpoints answer both, a pull request with a
// pull_request object.
func (i ghIssue) isPullRequest() bool {
	s := strings.TrimSpace(string(i.PullRequest))
	return s != "" && s != "null"
}

// labelNames reads an issue's labels, each a label object or its name.
type labelNames []string

func (l *labelNames) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil
	}
	out := labelNames{}
	for _, r := range raw {
		var name string
		if json.Unmarshal(r, &name) == nil {
			out = append(out, name)
			continue
		}
		var obj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(r, &obj) == nil {
			out = append(out, obj.Name)
		}
	}
	*l = out
	return nil
}

// decode reads a GitHub answer into out.
func decode(b []byte, out any) error {
	if err := json.Unmarshal(b, out); err != nil {
		return &apiError{Message: "its answer is not JSON Kivali reads"}
	}
	return nil
}

// created is the answer of an action that made something on GitHub:
// its address, which the owner's audit row keeps too.
type created string

// done is the answer of an action that closed or deleted something on
// GitHub: what it did, "closed <address>", which the owner's audit row
// keeps as it is and the agent reads as a sentence.
type done string

// failure is an action that reached GitHub and did not do what it was
// asked, in its own words for the agent: an error result.
type failure string

func (f failure) Error() string { return string(f) }

// commentedFirst is a close that failed after posting its comment
// (comment, the comment's address): the agent is told the comment is
// on GitHub, so that it does not post it again.
type commentedFirst struct {
	err     error
	comment string
}

func (e *commentedFirst) Error() string { return e.err.Error() }
func (e *commentedFirst) Unwrap() error { return e.err }

// repoPath is the REST path of owner/name.
func repoPath(repo string) string { return "/repos/" + repo }

// The actions.

func listRepositories(ctx context.Context, c *call) (any, error) {
	tok, err := c.token(ctx, "", perms())
	if err != nil {
		return nil, err
	}
	var lines []string
	err = c.g.pages(ctx, tok, "/installation/repositories", maxPages, func(b []byte) (int, bool, error) {
		var page struct {
			Repositories []struct {
				FullName    string `json:"full_name"`
				Private     bool   `json:"private"`
				UpdatedAt   string `json:"updated_at"`
				Description string `json:"description"`
			} `json:"repositories"`
		}
		if err := decode(b, &page); err != nil {
			return 0, true, err
		}
		for _, r := range page.Repositories {
			if !c.access.includes(r.FullName) {
				continue
			}
			visibility := "public"
			if r.Private {
				visibility = "private"
			}
			line := fmt.Sprintf("%s (%s, updated %s)", r.FullName, visibility, cmp.Or(r.UpdatedAt, "never"))
			if r.Description != "" {
				line += ": " + firstLine(r.Description)
			}
			lines = append(lines, line)
		}
		return len(page.Repositories), false, nil
	})
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return "No repositories found.", nil
	}
	return strings.Join(lines, "\n"), nil
}

func getRepository(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	tok, err := c.token(ctx, repo, perms())
	if err != nil {
		return nil, err
	}
	var r struct {
		FullName      string `json:"full_name"`
		Description   string `json:"description"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
		HTMLURL       string `json:"html_url"`
		UpdatedAt     string `json:"updated_at"`
	}
	if err := c.g.getJSON(ctx, tok, repoPath(repo), &r); err != nil {
		return nil, err
	}
	return struct {
		Repository    string `json:"repository"`
		Description   string `json:"description"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
		URL           string `json:"url"`
		UpdatedAt     string `json:"updated_at"`
	}{r.FullName, r.Description, r.DefaultBranch, r.Private, r.HTMLURL, r.UpdatedAt}, nil
}

// issueSummary is one issue in a list.
type issueSummary struct {
	Number    int64    `json:"number"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Author    string   `json:"author"`
	Labels    []string `json:"labels"`
	UpdatedAt string   `json:"updated_at"`
	URL       string   `json:"url"`
}

func summarizeIssue(i ghIssue) issueSummary {
	return issueSummary{i.Number, i.Title, i.State, i.User.Login, nonNil(i.Labels), i.UpdatedAt, i.HTMLURL}
}

func listIssues(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	state, err := c.args.choice("state", []string{"open", "closed", "all"}, "open")
	if err != nil {
		return nil, err
	}
	limit, err := c.args.limit(30, 50)
	if err != nil {
		return nil, err
	}
	labels, err := c.args.labels()
	if err != nil {
		return nil, err
	}
	search, err := c.args.text("search", false, true, searchMax)
	if err != nil {
		return nil, err
	}
	if search != "" {
		if whereQualifier.MatchString(search) {
			return nil, badArgument("search cannot name repositories, organizations or users; the repository argument does that.")
		}
		for _, l := range labels {
			if strings.Contains(l, `"`) {
				return nil, badArgument("A label in a search cannot hold a double quote.")
			}
		}
	}
	tok, err := c.token(ctx, repo, perms("issues", "read"))
	if err != nil {
		return nil, err
	}
	// Only the repository's own issues, whatever GitHub found: an
	// issue's address is under the repository's /issues/, a pull
	// request's under /pull/.
	prefix := strings.ToLower("https://github.com/" + repo + "/issues/")
	keep := func(i ghIssue) bool {
		return !i.isPullRequest() && strings.HasPrefix(strings.ToLower(i.HTMLURL), prefix)
	}
	out := []issueSummary{}
	if search != "" {
		q := "repo:" + repo + " is:issue"
		if state != "all" {
			q += " is:" + state
		}
		for _, l := range labels {
			q += ` label:"` + l + `"`
		}
		q += " " + search
		v := url.Values{"q": {q}, "sort": {"created"}, "order": {"desc"}, "per_page": {strconv.Itoa(limit)}}
		var found struct {
			Items []ghIssue `json:"items"`
		}
		if err := c.g.getJSON(ctx, tok, "/search/issues?"+v.Encode(), &found); err != nil {
			return nil, err
		}
		for _, i := range found.Items {
			if keep(i) && len(out) < limit {
				out = append(out, summarizeIssue(i))
			}
		}
		return out, nil
	}
	v := url.Values{"state": {state}, "sort": {"created"}, "direction": {"desc"}}
	if len(labels) > 0 {
		v.Set("labels", strings.Join(labels, ","))
	}
	err = c.g.pages(ctx, tok, repoPath(repo)+"/issues?"+v.Encode(), maxListPages, func(b []byte) (int, bool, error) {
		var page []ghIssue
		if err := decode(b, &page); err != nil {
			return 0, true, err
		}
		for _, i := range page {
			if keep(i) {
				out = append(out, summarizeIssue(i))
				if len(out) == limit {
					return len(page), true, nil
				}
			}
		}
		return len(page), false, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// commentOut is one comment as an answer gives it.
type commentOut struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	URL       string `json:"url"`
}

func getIssue(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	n, err := c.args.number("number")
	if err != nil {
		return nil, err
	}
	tok, err := c.token(ctx, repo, perms("issues", "read", "pull_requests", "read"))
	if err != nil {
		return nil, err
	}
	path := repoPath(repo) + "/issues/" + strconv.FormatInt(n, 10)
	var issue ghIssue
	if err := c.g.getJSON(ctx, tok, path, &issue); err != nil {
		return nil, err
	}
	comments, err := latestComments(ctx, c.g, tok, path+"/comments", issue.Comments)
	if err != nil {
		return nil, err
	}
	var notes []string
	body, clipped := clip(issue.Body, bodyClip)
	if clipped {
		notes = append(notes, fmt.Sprintf("The body is cut to its first %d characters.", bodyClip))
	}
	if issue.Comments > commentsKept {
		notes = append(notes, fmt.Sprintf("Only the latest %d of its %d comments are here.", commentsKept, issue.Comments))
	}
	outComments := make([]commentOut, 0, len(comments))
	cut := false
	for _, cm := range comments {
		b, clipped := clip(cm.Body, bodyClip)
		cut = cut || clipped
		outComments = append(outComments, commentOut{cm.User.Login, b, cm.CreatedAt, cm.HTMLURL})
	}
	if cut {
		notes = append(notes, fmt.Sprintf("A comment's body is cut to its first %d characters.", bodyClip))
	}
	return struct {
		Number      int64        `json:"number"`
		Title       string       `json:"title"`
		State       string       `json:"state"`
		PullRequest bool         `json:"pull_request,omitempty"`
		Author      string       `json:"author"`
		Labels      []string     `json:"labels"`
		Body        string       `json:"body"`
		Comments    []commentOut `json:"comments"`
		URL         string       `json:"url"`
		Note        string       `json:"note,omitempty"`
	}{issue.Number, issue.Title, issue.State, issue.isPullRequest(), issue.User.Login, nonNil(issue.Labels), body, outComments, issue.HTMLURL, strings.Join(notes, " ")}, nil
}

// latestComments is the latest commentsKept of an issue's total
// comments, oldest first. GitHub lists them oldest first, 100 a page,
// so they are on the last page and, when it holds fewer, the one
// before.
func latestComments(ctx context.Context, g *GitHub, tok, path string, total int) ([]ghComment, error) {
	if total <= 0 {
		return nil, nil
	}
	last := (total + 99) / 100
	read := func(page int) ([]ghComment, error) {
		var out []ghComment
		err := g.getJSON(ctx, tok, path+"?per_page=100&page="+strconv.Itoa(page), &out)
		return out, err
	}
	got, err := read(last)
	if err != nil {
		return nil, err
	}
	if len(got) < commentsKept && last > 1 {
		before, err := read(last - 1)
		if err != nil {
			return nil, err
		}
		got = append(before, got...)
	}
	if len(got) > commentsKept {
		got = got[len(got)-commentsKept:]
	}
	return got, nil
}

func createIssue(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	title, err := c.args.text("title", true, true, titleMax)
	if err != nil {
		return nil, err
	}
	body, err := c.args.text("body", false, false, maxText)
	if err != nil {
		return nil, err
	}
	labels, err := c.args.labels()
	if err != nil {
		return nil, err
	}
	tok, err := c.token(ctx, repo, perms("issues", "write"))
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		payload["labels"] = labels
	}
	var out ghCreated
	if err := c.g.sendJSON(ctx, request{method: http.MethodPost, path: repoPath(repo) + "/issues", auth: tok, body: payload}, &out); err != nil {
		return nil, err
	}
	return created(out.HTMLURL), nil
}

// comment comments on an issue or a pull request. They share their
// numbers, and GitHub comments on both through the issue's comments;
// the number is looked up first, so the token that writes is narrowed
// to issues or to pull requests, whichever it is.
func comment(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	n, err := c.args.number("number")
	if err != nil {
		return nil, err
	}
	body, err := c.args.text("body", true, false, maxText)
	if err != nil {
		return nil, err
	}
	path := repoPath(repo) + "/issues/" + strconv.FormatInt(n, 10)
	read, err := c.token(ctx, repo, perms("issues", "read", "pull_requests", "read"))
	if err != nil {
		return nil, err
	}
	var issue ghIssue
	if err := c.g.getJSON(ctx, read, path, &issue); err != nil {
		return nil, err
	}
	p := perms("issues", "write")
	if issue.isPullRequest() {
		p = perms("pull_requests", "write")
	}
	write, err := c.token(ctx, repo, p)
	if err != nil {
		return nil, err
	}
	var out ghCreated
	if err := c.g.sendJSON(ctx, request{method: http.MethodPost, path: path + "/comments", auth: write, body: map[string]any{"body": body}}, &out); err != nil {
		return nil, err
	}
	return created(out.HTMLURL), nil
}

// isAppBot reports whether u, a user GitHub names (the author of an
// issue or a pull request, or the user it resolved a commit's author or
// committer to), is the app's bot user: of type Bot, its login
// <slug>[bot]. The slug is the connection's. A Bot whose
// login is not that, or any Bot on a connection that keeps no slug
// (one set up before it was kept), is held to the slug GitHub gives
// the app now (GET /app, with the app's JWT), read once a call, so an
// app renamed since setup still knows its own bot. A user of another
// type is never the app, and costs no request.
func (c *call) isAppBot(ctx context.Context, u ghUser) (bool, error) {
	if u.Type != "Bot" {
		return false, nil
	}
	kept := appSlug.MatchString(c.conn.slug)
	if kept && u.Login == c.conn.slug+"[bot]" {
		return true, nil
	}
	if !c.asked {
		app, err := c.g.appOf(ctx, c.conn)
		if err != nil {
			return false, err
		}
		c.asked = true
		if appSlug.MatchString(app.Slug) {
			c.current = app.Slug + "[bot]"
		}
	}
	if c.current == "" && !kept {
		return false, refuse(notSetUp, "GitHub names no slug for the app")
	}
	return c.current != "" && u.Login == c.current, nil
}

// ghCommit is one commit of a comparison: the author and committer git
// records (their emails), and the users GitHub resolved those emails
// to, nil (or no login) where it resolved none.
type ghCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Author struct {
			Email string `json:"email"`
		} `json:"author"`
		Committer struct {
			Email string `json:"email"`
		} `json:"committer"`
	} `json:"commit"`
	Author    *ghUser `json:"author"`
	Committer *ghUser `json:"committer"`
}

// unresolved reports whether GitHub resolved no user for u.
func unresolved(u *ghUser) bool { return u == nil || u.Login == "" }

// commitByBot reports whether cm was authored and committed by the
// app's bot, by what git records: each of its author and its committer
// either resolved by GitHub to the bot user (isAppBot), or, where
// GitHub resolved no user, an email that is the bot's no-reply address
// exactly (noreply; "" when it is not known, which no email matches).
func (c *call) commitByBot(ctx context.Context, cm ghCommit, noreply string) (bool, error) {
	for _, side := range []struct {
		user  *ghUser
		email string
	}{{cm.Author, cm.Commit.Author.Email}, {cm.Committer, cm.Commit.Committer.Email}} {
		if unresolved(side.user) {
			if noreply == "" || side.email != noreply {
				return false, nil
			}
			continue
		}
		mine, err := c.isAppBot(ctx, *side.user)
		if err != nil || !mine {
			return false, err
		}
	}
	return true, nil
}

// closingComment is the comment argument of a close: Markdown of at
// most maxText characters, "" when it is absent or blank.
func (a args) closingComment() (string, error) {
	s, err := a.text("comment", false, false, maxText)
	if err != nil || strings.TrimSpace(s) == "" {
		return "", err
	}
	return s, nil
}

// closeItem closes the issue or pull request at path with write, a
// token that may write it, after posting note on it through
// commentsPath when note is not empty. Its answer names what was
// closed, GitHub's address for it (address when GitHub's answer names
// none), and the comment's.
func (c *call) closeItem(ctx context.Context, write, path, commentsPath, note, address string) (any, error) {
	commented, comment := false, ""
	if note != "" {
		var out ghCreated
		if err := c.g.sendJSON(ctx, request{method: http.MethodPost, path: commentsPath, auth: write, body: map[string]any{"body": note}}, &out); err != nil {
			return nil, err
		}
		commented, comment = true, out.HTMLURL
	}
	var out ghCreated
	if err := c.g.sendJSON(ctx, request{method: http.MethodPatch, path: path, auth: write, body: map[string]any{"state": "closed"}}, &out); err != nil {
		if commented {
			return nil, &commentedFirst{err: err, comment: comment}
		}
		return nil, err
	}
	what := "closed " + cmp.Or(out.HTMLURL, address)
	if commented {
		what += ", with the comment " + comment
	}
	return done(what), nil
}

// closeIssue closes an issue the app opened, after posting the comment
// when one is given. The issue is read first with a token that only
// reads: a pull request (the issues endpoint serves those too) and an
// issue anyone else opened are refused, one already closed fails, and
// none of them gets a token that writes.
func closeIssue(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	n, err := c.args.number("number")
	if err != nil {
		return nil, err
	}
	note, err := c.args.closingComment()
	if err != nil {
		return nil, err
	}
	read, err := c.token(ctx, repo, perms("issues", "read", "pull_requests", "read"))
	if err != nil {
		return nil, err
	}
	path := repoPath(repo) + "/issues/" + strconv.FormatInt(n, 10)
	var issue ghIssue
	if err := c.g.getJSON(ctx, read, path, &issue); err != nil {
		return nil, err
	}
	if issue.isPullRequest() {
		return nil, refuse(fmt.Sprintf("%s#%d is a pull request; github_close_pull_request closes it.", repo, n), "the number is a pull request")
	}
	mine, err := c.isAppBot(ctx, issue.User)
	switch {
	case err != nil:
		return nil, err
	case !mine:
		return nil, refuse("Your access lets you close only issues the GitHub App opened; ask the owner.", fmt.Sprintf("the GitHub App did not open %s#%d", repo, n))
	case issue.State == "closed":
		return nil, failure(fmt.Sprintf("Issue %s#%d is closed already.", repo, n))
	}
	write, err := c.token(ctx, repo, perms("issues", "write"))
	if err != nil {
		return nil, err
	}
	return c.closeItem(ctx, write, path, path+"/comments", note, issue.HTMLURL)
}

// pullSummary is one pull request in a list.
type pullSummary struct {
	Number    int64  `json:"number"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Author    string `json:"author"`
	Head      string `json:"head"`
	Base      string `json:"base"`
	Draft     bool   `json:"draft"`
	UpdatedAt string `json:"updated_at"`
	URL       string `json:"url"`
}

// pullState is a pull request's state, merged for a closed one that
// was merged.
func pullState(p ghPull) string {
	if p.MergedAt != nil && *p.MergedAt != "" {
		return "merged"
	}
	return p.State
}

func listPullRequests(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	state, err := c.args.choice("state", []string{"open", "closed", "merged", "all"}, "open")
	if err != nil {
		return nil, err
	}
	limit, err := c.args.limit(30, 50)
	if err != nil {
		return nil, err
	}
	base, err := c.args.branch("base", false)
	if err != nil {
		return nil, err
	}
	tok, err := c.token(ctx, repo, perms("pull_requests", "read"))
	if err != nil {
		return nil, err
	}
	v := url.Values{"state": {state}}
	if state == "merged" {
		v.Set("state", "closed")
	}
	if base != "" {
		v.Set("base", base)
	}
	out := []pullSummary{}
	err = c.g.pages(ctx, tok, repoPath(repo)+"/pulls?"+v.Encode(), maxListPages, func(b []byte) (int, bool, error) {
		var page []ghPull
		if err := decode(b, &page); err != nil {
			return 0, true, err
		}
		for _, p := range page {
			if state == "merged" && pullState(p) != "merged" {
				continue
			}
			out = append(out, pullSummary{p.Number, p.Title, pullState(p), p.User.Login, p.Head.Ref, p.Base.Ref, p.Draft, p.UpdatedAt, p.HTMLURL})
			if len(out) == limit {
				return len(page), true, nil
			}
		}
		return len(page), false, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func getPullRequest(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	n, err := c.args.number("number")
	if err != nil {
		return nil, err
	}
	withDiff, err := c.args.boolean("diff")
	if err != nil {
		return nil, err
	}
	p := perms("pull_requests", "read")
	if withDiff {
		p = perms("pull_requests", "read", "contents", "read")
	}
	tok, err := c.token(ctx, repo, p)
	if err != nil {
		return nil, err
	}
	path := repoPath(repo) + "/pulls/" + strconv.FormatInt(n, 10)
	var pr ghPull
	if err := c.g.getJSON(ctx, tok, path, &pr); err != nil {
		return nil, err
	}
	var notes []string
	body, clipped := clip(pr.Body, bodyClip)
	if clipped {
		notes = append(notes, fmt.Sprintf("The body is cut to its first %d characters.", bodyClip))
	}
	var diff *string
	if withDiff {
		b, _, err := c.g.do(ctx, request{method: http.MethodGet, path: path, auth: tok, accept: mediaDiff})
		var ae *apiError
		switch {
		case errors.As(err, &ae) && ae.Status == http.StatusNotAcceptable:
			// GitHub gives no diff past its own size limits.
			notes = append(notes, "GitHub gives no diff for this pull request ("+sentence(ae.Message)+"); compare lists every file changed.")
		case err != nil:
			return nil, err
		default:
			text, clipped := clip(string(b), diffClip)
			diff = &text
			if clipped {
				notes = append(notes, fmt.Sprintf("The diff is cut to its first %d characters; compare lists every file changed.", diffClip))
			}
		}
	}
	return struct {
		Number       int64    `json:"number"`
		Title        string   `json:"title"`
		State        string   `json:"state"`
		Author       string   `json:"author"`
		Head         string   `json:"head"`
		Base         string   `json:"base"`
		Draft        bool     `json:"draft"`
		Mergeable    *bool    `json:"mergeable"`
		Additions    int      `json:"additions"`
		Deletions    int      `json:"deletions"`
		ChangedFiles int      `json:"changed_files"`
		Labels       []string `json:"labels"`
		Body         string   `json:"body"`
		UpdatedAt    string   `json:"updated_at"`
		URL          string   `json:"url"`
		Diff         *string  `json:"diff,omitempty"`
		Note         string   `json:"note,omitempty"`
	}{pr.Number, pr.Title, pullState(pr), pr.User.Login, pr.Head.Ref, pr.Base.Ref, pr.Draft, pr.Mergeable,
		pr.Additions, pr.Deletions, pr.ChangedFiles, nonNil(pr.Labels), body, pr.UpdatedAt, pr.HTMLURL, diff, strings.Join(notes, " ")}, nil
}

// createPullRequest opens a pull request from a head branch already on
// GitHub; it moves no commits.
func createPullRequest(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	title, err := c.args.text("title", true, true, titleMax)
	if err != nil {
		return nil, err
	}
	head, err := c.args.branch("head", true)
	if err != nil {
		return nil, err
	}
	base, err := c.args.branch("base", true)
	if err != nil {
		return nil, err
	}
	body, err := c.args.text("body", false, false, maxText)
	if err != nil {
		return nil, err
	}
	draft, err := c.args.boolean("draft")
	if err != nil {
		return nil, err
	}
	// contents read besides pull_requests write: GitHub resolves the
	// head and base branches with the token, and refuses a pull request
	// (422, "not all refs are readable") from one that may not read the
	// repository's contents.
	tok, err := c.token(ctx, repo, perms("pull_requests", "write", "contents", "read"))
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"title": title, "head": head, "base": base, "body": body, "draft": draft}
	var out ghCreated
	if err := c.g.sendJSON(ctx, request{method: http.MethodPost, path: repoPath(repo) + "/pulls", auth: tok, body: payload}, &out); err != nil {
		return nil, err
	}
	return created(out.HTMLURL), nil
}

// closePullRequest closes a pull request the app opened, unmerged,
// after posting the comment when one is given (through the issue's
// comments, as comment does). It is read first with a token that only
// reads: one anyone else opened is refused, a merged or closed one
// fails, and none of them gets a token that writes.
func closePullRequest(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	n, err := c.args.number("number")
	if err != nil {
		return nil, err
	}
	note, err := c.args.closingComment()
	if err != nil {
		return nil, err
	}
	read, err := c.token(ctx, repo, perms("pull_requests", "read"))
	if err != nil {
		return nil, err
	}
	num := strconv.FormatInt(n, 10)
	path := repoPath(repo) + "/pulls/" + num
	var pr ghPull
	if err := c.g.getJSON(ctx, read, path, &pr); err != nil {
		return nil, err
	}
	mine, err := c.isAppBot(ctx, pr.User)
	switch {
	case err != nil:
		return nil, err
	case !mine:
		return nil, refuse("Your access lets you close only pull requests the GitHub App opened; ask the owner.", fmt.Sprintf("the GitHub App did not open %s#%d", repo, n))
	case pullState(pr) == "merged":
		return nil, failure(fmt.Sprintf("Pull request %s#%d is merged, and a merged pull request cannot be closed.", repo, n))
	case pr.State == "closed":
		return nil, failure(fmt.Sprintf("Pull request %s#%d is closed already.", repo, n))
	}
	write, err := c.token(ctx, repo, perms("pull_requests", "write"))
	if err != nil {
		return nil, err
	}
	return c.closeItem(ctx, write, path, repoPath(repo)+"/issues/"+num+"/comments", note, pr.HTMLURL)
}

func listBranches(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	n, err := c.args.limit(30, 100)
	if err != nil {
		return nil, err
	}
	tok, err := c.token(ctx, repo, perms("contents", "read"))
	if err != nil {
		return nil, err
	}
	var branches []struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.g.getJSON(ctx, tok, repoPath(repo)+"/branches?per_page="+strconv.Itoa(n), &branches); err != nil {
		return nil, err
	}
	if len(branches) == 0 {
		return "No branches.", nil
	}
	lines := make([]string, 0, len(branches))
	for _, b := range branches {
		lines = append(lines, b.Name+" "+b.Commit.SHA)
	}
	return strings.Join(lines, "\n"), nil
}

// openPullRefusal is the words of a branch an open pull request in the
// repository has as its head or its base (role), n its number.
func openPullRefusal(branch, role string, n int64) string {
	return fmt.Sprintf("%s is the %s of pull request #%d, which is open; a branch is deleted only once no open pull request has it as its head or its base; ask the owner.", branch, role, n)
}

// pullsCut is the refusal of a branch whose pull requests GitHub lists
// past maxListPages pages: some were not read, so none of the checks
// can be said to hold.
func pullsCut(branch, repo string) error {
	return refuse(fmt.Sprintf("The pull requests of %s in %s could not all be read, so it is not deleted; ask the owner.", branch, repo),
		"the branch's pull requests could not all be read")
}

// allPulls is every pull request GitHub lists at path, and whether the
// listing was cut: maxListPages pages read, the last of them full, so
// that more may follow.
func (c *call) allPulls(ctx context.Context, tok, path string) ([]ghPull, bool, error) {
	var out []ghPull
	read, last := 0, 0
	err := c.g.pages(ctx, tok, path, maxListPages, func(b []byte) (int, bool, error) {
		var page []ghPull
		if err := decode(b, &page); err != nil {
			return 0, true, err
		}
		out = append(out, page...)
		read, last = read+1, len(page)
		return len(page), false, nil
	})
	return out, read == maxListPages && last >= 100, err
}

// refPath is a branch name in a REST path: each segment escaped, the
// slashes between them kept.
func refPath(name string) string {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// gitBranch reports whether name is one git takes as a branch's, past
// what args.branch holds a name to: not HEAD, and no segment empty,
// starting with a dot or ending with .lock.
func gitBranch(name string) bool {
	if name == "HEAD" {
		return false
	}
	for _, s := range strings.Split(name, "/") {
		if s == "" || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".lock") {
			return false
		}
	}
	return true
}

// commitID matches a commit's id as GitHub gives it: SHA-1 or SHA-256,
// in hex.
var commitID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// branchTip is the commit the branch name points at (GET
// /repos/{r}/branches/{name}, which names branches alone, never a tag
// of the same name), and false when the repository has no such branch.
// An answer for another branch (GitHub redirects a renamed branch's
// name to the new one) is no such branch.
func (c *call) branchTip(ctx context.Context, tok, repo, name string) (string, bool, error) {
	var b struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	err := c.g.getJSON(ctx, tok, repoPath(repo)+"/branches/"+refPath(name), &b)
	var ae *apiError
	switch {
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		return "", false, nil
	case err != nil:
		return "", false, err
	case b.Name != name:
		return "", false, nil
	case !commitID.MatchString(b.Commit.SHA):
		return "", false, failure(fmt.Sprintf("GitHub names no commit for the branch %s of %s; ask the owner.", name, repo))
	}
	return b.Commit.SHA, true, nil
}

// branchByBot is the rule that lets a branch go once nothing open uses
// it: every commit the branch's tip (head, a commit id) has beyond the
// default branch's (base) was authored and committed by the app's bot
// (commitByBot), or it has none (GET /repos/{r}/compare/{base}...{head},
// by commit, so no tag of either name is compared in a branch's
// place). A comparison GitHub cannot make (404: no common history), or
// one that lists fewer commits than it counts, refuses the branch; so
// does the first commit that is not the bot's. branch and def name the
// branch and the default branch in its words.
func (c *call) branchByBot(ctx context.Context, tok, repo, branch, def, base, head string) error {
	var cmpd struct {
		TotalCommits int        `json:"total_commits"`
		Commits      []ghCommit `json:"commits"`
	}
	err := c.g.getJSON(ctx, tok, repoPath(repo)+"/compare/"+base+"..."+head, &cmpd)
	var ae *apiError
	switch {
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		return refuse(fmt.Sprintf("GitHub cannot compare %s with %s (HTTP 404: %s), so it is not deleted; ask the owner.", branch, def, sentence(ae.Message)),
			"the branch cannot be compared with the default branch")
	case err != nil:
		return err
	case cmpd.TotalCommits > len(cmpd.Commits):
		return refuse(fmt.Sprintf("%s has too many commits beyond %s to check (%d), so it is not deleted; ask the owner.", branch, def, cmpd.TotalCommits),
			"the branch has too many commits to check")
	}
	// The bot's no-reply address is read only for a commit GitHub
	// resolved no user for.
	noreply := ""
	for _, cm := range cmpd.Commits {
		if unresolved(cm.Author) || unresolved(cm.Committer) {
			b, err := c.g.bot(ctx, c.conn, repo)
			if err != nil {
				return err
			}
			noreply = b.noreply()
			break
		}
	}
	for _, cm := range cmpd.Commits {
		mine, err := c.commitByBot(ctx, cm, noreply)
		switch {
		case err != nil:
			return err
		case !mine:
			sha, _ := clip(cm.SHA, 12)
			return refuse(fmt.Sprintf("%s has a commit beyond %s that the GitHub App's bot did not both author and commit (%s); your access lets you delete only a branch whose own commits are all the bot's; ask the owner.", branch, def, sha),
				fmt.Sprintf("commit %s of the branch is not the app's bot's", sha))
		}
	}
	return nil
}

// deleteBranch deletes a branch whose work is done (branchByBot). With
// a token that only reads, it reads the repository's default branch,
// which it never deletes, the commit the branch points at (as a
// branch), and the repository's open pull requests from the branch
// (head owner:branch, the owner GitHub names the repository's) and into
// it (base), refusing while there is one or either listing is cut; then
// it compares the branch's commit with the default branch's, and reads
// the branch again, so one that moved meanwhile is not deleted. Only
// then is a token that writes the repository's contents minted, for the
// deletion.
func deleteBranch(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	branch, err := c.args.branch("branch", true)
	if err != nil {
		return nil, err
	}
	if !gitBranch(branch) {
		return nil, badArgument("branch is not a branch name.")
	}
	read, err := c.token(ctx, repo, perms("contents", "read", "pull_requests", "read"))
	if err != nil {
		return nil, err
	}
	var r struct {
		DefaultBranch string `json:"default_branch"`
		Owner         ghUser `json:"owner"`
	}
	if err := c.g.getJSON(ctx, read, repoPath(repo), &r); err != nil {
		return nil, err
	}
	switch {
	case r.DefaultBranch == "":
		return nil, failure(fmt.Sprintf("GitHub names no default branch for %s; ask the owner.", repo))
	case strings.EqualFold(branch, r.DefaultBranch):
		// Capitals aside: a name that differs only in them is refused
		// too.
		return nil, refuse(fmt.Sprintf("%s is the default branch of %s, which your access does not let you delete; ask the owner.", branch, repo), "the branch is the repository's default branch")
	}
	tip, ok, err := c.branchTip(ctx, read, repo, branch)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, failure(fmt.Sprintf("%s has no branch named %s.", repo, branch))
	}
	owner, _, _ := strings.Cut(repo, "/")

	head := url.Values{"head": {cmp.Or(r.Owner.Login, owner) + ":" + branch}, "state": {"open"}}
	from, cut, err := c.allPulls(ctx, read, repoPath(repo)+"/pulls?"+head.Encode())
	switch {
	case err != nil:
		return nil, err
	case cut:
		return nil, pullsCut(branch, repo)
	}
	// An open one holds the branch capitals aside, so a name that
	// differs only in them is held too.
	for _, p := range from {
		if p.State == "open" && strings.EqualFold(p.Head.Ref, branch) {
			return nil, refuse(openPullRefusal(branch, "head", p.Number), fmt.Sprintf("pull request #%d is open from the branch", p.Number))
		}
	}
	// Deleting a branch open pull requests are into closes or retargets
	// them.
	base := url.Values{"base": {branch}, "state": {"open"}}
	into, cut, err := c.allPulls(ctx, read, repoPath(repo)+"/pulls?"+base.Encode())
	switch {
	case err != nil:
		return nil, err
	case cut:
		return nil, pullsCut(branch, repo)
	}
	for _, p := range into {
		if p.State == "open" && strings.EqualFold(p.Base.Ref, branch) {
			return nil, refuse(openPullRefusal(branch, "base", p.Number), fmt.Sprintf("pull request #%d is open into the branch", p.Number))
		}
	}

	defTip, ok, err := c.branchTip(ctx, read, repo, r.DefaultBranch)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, failure(fmt.Sprintf("GitHub has no branch %s, the default branch of %s; ask the owner.", r.DefaultBranch, repo))
	}
	if err := c.branchByBot(ctx, read, repo, branch, r.DefaultBranch, defTip, tip); err != nil {
		return nil, err
	}
	// The checks and the deletion are separate requests: a branch that
	// moved since its commit was compared is not deleted.
	now, ok, err := c.branchTip(ctx, read, repo, branch)
	switch {
	case err != nil:
		return nil, err
	case !ok || now != tip:
		return nil, failure(fmt.Sprintf("%s changed while it was checked, so it is not deleted; try again.", branch))
	}
	write, err := c.token(ctx, repo, perms("contents", "write"))
	if err != nil {
		return nil, err
	}
	if _, _, err := c.g.do(ctx, request{method: http.MethodDelete, path: repoPath(repo) + "/git/refs/heads/" + branch, auth: write}); err != nil {
		return nil, err
	}
	return done("deleted the branch " + branch + " of " + repo), nil
}

func compare(ctx context.Context, c *call) (any, error) {
	repo, err := c.args.repository(c.access)
	if err != nil {
		return nil, err
	}
	base, err := c.args.branch("base", true)
	if err != nil {
		return nil, err
	}
	head, err := c.args.branch("head", true)
	if err != nil {
		return nil, err
	}
	tok, err := c.token(ctx, repo, perms("contents", "read"))
	if err != nil {
		return nil, err
	}
	var data struct {
		Status   string `json:"status"`
		AheadBy  int    `json:"ahead_by"`
		BehindBy int    `json:"behind_by"`
		Commits  []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Name string `json:"name"`
				} `json:"author"`
			} `json:"commit"`
			Author *ghUser `json:"author"`
		} `json:"commits"`
		Files []struct {
			Filename  string `json:"filename"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		} `json:"files"`
	}
	if err := c.g.getJSON(ctx, tok, repoPath(repo)+"/compare/"+base+"..."+head, &data); err != nil {
		return nil, err
	}
	type commitOut struct {
		SHA    string `json:"sha"`
		Title  string `json:"title"`
		Author string `json:"author"`
	}
	type fileOut struct {
		Name      string `json:"name"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	}
	commits := make([]commitOut, 0, len(data.Commits))
	for _, cm := range data.Commits {
		author := cm.Commit.Author.Name
		if cm.Author != nil && cm.Author.Login != "" {
			author = cm.Author.Login
		}
		commits = append(commits, commitOut{cm.SHA, firstLine(cm.Commit.Message), author})
	}
	files := make([]fileOut, 0, len(data.Files))
	for _, f := range data.Files {
		files = append(files, fileOut{f.Filename, f.Status, f.Additions, f.Deletions})
	}
	return struct {
		Status   string      `json:"status"`
		AheadBy  int         `json:"ahead_by"`
		BehindBy int         `json:"behind_by"`
		Commits  []commitOut `json:"commits"`
		Files    []fileOut   `json:"files"`
	}{data.Status, data.AheadBy, data.BehindBy, commits, files}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
