"""GitHub: the integration that ships with Kivali. A stdio MCP server,
standard library only, that runs GitHub's gh for each action.

Kivali starts it in the integration's pod with the bundle as its
working directory and sends it one JSON-RPC message per line on its
input; it answers one per line on its output. The pod installs gh (the
manifest's kivali.packages) before the server starts.

The connection is made by signing in: Kivali holds the GitHub App's
client secret and the refresh token, and hands this server a current
user access token at the moment of each call, through
kivali/credential. The server asks for it on every call, gives it to
gh in gh's environment alone (never on its command line, never on
disk) and forgets it when the call ends.

An agent's access names the repositories it may use, as owner/name or
patterns with *, and whether it may create issues, comments and pull
requests. The server holds every call to it.
"""

import json
import os
import re
import subprocess
import sys
import threading
import time
import traceback

# Kivali reads only JSON-RPC on the output: any other line stops the
# server. So the protocol writes to a copy of the output taken here,
# and the output itself (descriptor 1, which sys.stdout and every
# command this server runs write to) becomes the error output, which
# Kivali keeps as the integration's log, a line at a time.
PROTOCOL = os.fdopen(os.dup(1), "w", encoding="utf-8")
os.dup2(2, 1)
sys.stdout = sys.stderr

PROTOCOL_VERSION = "2025-06-18"
VERSION = "1.0.0"

# The name kivali.connection.credential_keys gives the access token.
TOKEN_KEY = "GH_TOKEN"

# Kivali waits 60 seconds for an action's answer. A call's own work
# (the credential and every gh run it makes) ends within CALL_BUDGET,
# and one gh run within GH_TIMEOUT, so the agent reads this server's
# words rather than Kivali's timeout.
CALL_BUDGET = 55
GH_TIMEOUT = 50
# How long a call waits for Kivali's answer to kivali/credential,
# which may renew the sign-in with GitHub first.
CREDENTIAL_TIMEOUT = 20

# gh's own settings for every run: github.com only, a configuration
# directory that holds nothing (gh reads the token from GH_TOKEN), and
# no prompts, update checks or colours.
GH_ENV = {
    "GH_HOST": "github.com",
    "GH_CONFIG_DIR": "/tmp/gh",
    "GH_NO_UPDATE_NOTIFIER": "1",
    "GH_PROMPT_DISABLED": "1",
    "NO_COLOR": "1",
}
# The variables passed on to gh from the server's environment: the
# pod's only way out is the proxy they name.
PASSED_ENV = ("PATH", "HOME", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy")

# Bounds on what an answer carries.
BODY_CLIP = 20000
DIFF_CLIP = 50000
COMMENTS_KEPT = 20
STDERR_TAIL = 2000
MAX_TEXT = 60000

_write = threading.Lock()
_asked_lock = threading.Lock()
_asked = {}
_seq = [0]


def send(msg):
    """Write one message, a whole line at a time: calls answer on threads."""
    line = json.dumps(msg, separators=(",", ":"))
    with _write:
        PROTOCOL.write(line + "\n")
        PROTOCOL.flush()


def log(level, message):
    """A line in the integration's log: info, warning, error or debug."""
    send({"jsonrpc": "2.0", "method": "kivali/log", "params": {"level": level, "message": message}})


class Refusal(Exception):
    """An action that cannot be done, in words the agent reads."""


def answer(value):
    if not isinstance(value, str):
        value = json.dumps(value, separators=(",", ":"), ensure_ascii=False)
    return {"content": [{"type": "text", "text": value}], "isError": False}


def refusal(text):
    return {"content": [{"type": "text", "text": text}], "isError": True}


# Kivali's requests and this server's own share the input: the reading
# loop hands a response to the call waiting for its id.


def ask(method, params, timeout):
    """Send a request of this server's own and wait for its response;
    None when none came in time."""
    with _asked_lock:
        _seq[0] += 1
        rid = "github-%d" % _seq[0]
        waiter = {"event": threading.Event(), "response": None}
        _asked[rid] = waiter
    send({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
    waiter["event"].wait(timeout)
    with _asked_lock:
        _asked.pop(rid, None)
    return waiter["response"]


def deliver(msg):
    rid = msg.get("id")
    if not isinstance(rid, str):
        return
    with _asked_lock:
        waiter = _asked.get(rid)
    if waiter is not None:
        waiter["response"] = msg
        waiter["event"].set()


# kivali/credential's error codes.
CODE_NO_CONNECTION = -32001
CODE_SIGN_IN_NEEDED = -32002
CODE_VENDOR_UNAVAILABLE = -32003


def credential(deadline):
    """The access token as it is now, from Kivali; a Refusal in words
    when there is none to use."""
    resp = ask("kivali/credential", {}, max(0.0, min(CREDENTIAL_TIMEOUT, deadline - time.monotonic())))
    if resp is None:
        raise Refusal("Kivali did not hand over GitHub's connection in time; try again later, or ask the owner if it goes on.")
    err = resp.get("error")
    if isinstance(err, dict):
        code = err.get("code")
        if code == CODE_NO_CONNECTION:
            raise Refusal("GitHub is not connected; ask the owner.")
        if code == CODE_SIGN_IN_NEEDED:
            raise Refusal("GitHub's connection needs the owner to sign in again; ask the owner.")
        if code == CODE_VENDOR_UNAVAILABLE:
            raise Refusal("GitHub did not answer when Kivali renewed the connection's sign-in; try again later, or ask the owner if it goes on.")
        raise Refusal("Kivali did not hand over GitHub's connection; try again later, or ask the owner if it goes on.")
    result = resp.get("result")
    env = result.get("env") if isinstance(result, dict) else None
    token = env.get(TOKEN_KEY) if isinstance(env, dict) else None
    if not isinstance(token, str) or not token:
        raise Refusal("GitHub is not connected; ask the owner.")
    return token


class Call:
    """One action's run: its deadline, and the token once asked for."""

    def __init__(self):
        self.deadline = time.monotonic() + CALL_BUDGET
        self.token = None

    def gh(self, args, stdin_text=None):
        """One gh run: args a list, never a shell. Its standard output,
        or a Refusal with the end of its error output."""
        if self.token is None:
            self.token = credential(self.deadline)
        env = {k: os.environ[k] for k in PASSED_ENV if k in os.environ}
        env.update(GH_ENV)
        env[TOKEN_KEY] = self.token
        timeout = min(GH_TIMEOUT, self.deadline - time.monotonic())
        if timeout <= 0:
            raise Refusal("GitHub did not answer in time; try again later.")
        try:
            run = subprocess.run(
                ["gh"] + args,
                env=env,
                input=stdin_text if stdin_text is not None else "",
                capture_output=True,
                text=True,
                encoding="utf-8",
                errors="replace",
                timeout=timeout,
            )
        except subprocess.TimeoutExpired:
            raise Refusal("GitHub did not answer within %d seconds; try again later." % int(timeout))
        except FileNotFoundError:
            raise Refusal("gh is not installed in the integration's pod; tell the owner.")
        if run.returncode != 0:
            tail = (run.stderr or "").strip()[-STDERR_TAIL:] or "no error output"
            raise Refusal("gh exited with status %d: %s" % (run.returncode, tail))
        return run.stdout

    def gh_json(self, args, stdin_text=None):
        out = self.gh(args, stdin_text)
        try:
            return json.loads(out) if out.strip() else None
        except ValueError:
            raise Refusal("gh answered something that is not JSON: %s" % out.strip()[:500])


# Repositories and access patterns, by the Git integration's rules:
# owner/name, letters, digits, '.', '_' and '-'; a pattern's only
# wildcard is *, which stands within one segment and never across a /;
# capitals do not matter.

_NAME_CHARS = re.compile(r"^[A-Za-z0-9._-]+$")


def clean_repository(value):
    """value as owner/name, slashes around it and a .git suffix
    trimmed; a Refusal when it is not one."""
    if not isinstance(value, str) or not value.strip():
        raise Refusal("Name the repository as owner/name.")
    p = value.strip().strip("/")
    if p.endswith(".git"):
        p = p[: -len(".git")]
    p = p.strip("/")
    segs = p.split("/")
    if len(segs) != 2 or len(p) > 200:
        raise Refusal("%s is not owner/name." % value)
    for s in segs:
        if s in ("", ".", "..") or s.startswith("-") or not _NAME_CHARS.match(s):
            raise Refusal("%s is not owner/name." % value)
    return p


def normalize_pattern(p):
    p = p.strip().strip("/")
    if p.endswith(".git"):
        p = p[: -len(".git")]
    return p.strip("/").lower()


def valid_pattern(p):
    n = normalize_pattern(p)
    return n != "" and not any(c in n for c in "?[]\\")


def _pattern_regex(n):
    return re.compile("^" + "[^/]*".join(re.escape(part) for part in n.split("*")) + "$", re.DOTALL)


def match(patterns, key):
    """Whether the repository key (owner/name) is one of patterns."""
    key = key.lower()
    for p in patterns:
        if not isinstance(p, str) or not valid_pattern(p):
            continue
        if _pattern_regex(normalize_pattern(p)).match(key):
            return True
    return False


# The agents' access, as the owner's form fills it in. A missing list is
# no repository; a missing switch is off.


def repositories_of(access):
    return [p for p in access.get("repositories") or [] if isinstance(p, str) and p.strip()]


def may_write(access):
    return access.get("write") is True


def repository_arg(args, access):
    repo = clean_repository(args.get("repository"))
    if access is not None and not match(repositories_of(access), repo):
        raise Refusal("Your access to GitHub does not include %s; ask the owner." % repo)
    return repo


def number_arg(args, name="number"):
    n = args.get(name)
    if isinstance(n, bool) or not isinstance(n, int) or n < 1:
        raise Refusal("%s is a positive whole number." % name)
    return n


def limit_arg(args, default, most):
    n = args.get("limit", default)
    if isinstance(n, bool) or not isinstance(n, int) or n < 1:
        raise Refusal("limit is a positive whole number.")
    return min(n, most)


def choice_arg(args, name, choices, default):
    v = args.get(name, default)
    if v not in choices:
        raise Refusal("%s is one of %s." % (name, ", ".join(choices)))
    return v


def text_arg(args, name, required=False, one_line=False, most=MAX_TEXT):
    v = args.get(name)
    if v is None or v == "":
        if required:
            raise Refusal("Give the %s." % name)
        return None
    if not isinstance(v, str):
        raise Refusal("%s is text." % name)
    if required and not v.strip():
        raise Refusal("Give the %s." % name)
    if one_line and ("\n" in v or "\r" in v):
        raise Refusal("%s is one line." % name)
    if len(v) > most:
        raise Refusal("%s is longer than %d characters." % (name, most))
    return v


_BRANCH = re.compile(r"^[A-Za-z0-9._/+-]+$")


def branch_arg(args, name, required=True):
    v = args.get(name)
    if v is None or v == "":
        if required:
            raise Refusal("Give the %s branch." % name)
        return None
    if (
        not isinstance(v, str)
        or len(v) > 255
        or not _BRANCH.match(v)
        or v.startswith(("-", "/", "."))
        or v.endswith(("/", ".", ".lock"))
        or ".." in v
        or "//" in v
    ):
        raise Refusal("%s is not a branch name." % name)
    return v


def labels_arg(args):
    v = args.get("labels")
    if v is None:
        return []
    if not isinstance(v, list) or not all(isinstance(l, str) and l.strip() and "," not in l and len(l) <= 100 for l in v):
        raise Refusal("labels is a list of label names.")
    return v


def clip(text, most):
    """text cut to most characters, and whether it was."""
    if text is None:
        return text, False
    if len(text) <= most:
        return text, False
    return text[:most], True


def first_line(s):
    return (s or "").split("\n", 1)[0]


# The actions.


def list_repositories(call, args, access):
    fields = "nameWithOwner,description,isPrivate,updatedAt"
    if access is None:
        owners = [None]
    else:
        owners = []
        for p in repositories_of(access):
            if not valid_pattern(p):
                continue
            owner = normalize_pattern(p).split("/", 1)[0]
            if "*" not in owner and owner not in owners:
                owners.append(owner)
    lines = []
    for owner in owners:
        cmd = ["repo", "list"] + ([owner] if owner else []) + ["--limit=100", "--json=" + fields]
        for r in call.gh_json(cmd) or []:
            name = r.get("nameWithOwner") or ""
            if access is not None and not match(repositories_of(access), name):
                continue
            line = "%s (%s, updated %s)" % (name, "private" if r.get("isPrivate") else "public", r.get("updatedAt") or "never")
            if r.get("description"):
                line += ": " + first_line(r["description"])
            lines.append(line)
    if not lines:
        return answer("No repositories found.")
    return answer("\n".join(lines))


def get_repository(call, args, access):
    repo = repository_arg(args, access)
    return answer(call.gh(["repo", "view", repo, "--json=name,owner,description,defaultBranchRef,isPrivate,url,updatedAt"]).strip())


def list_issues(call, args, access):
    repo = repository_arg(args, access)
    cmd = [
        "issue", "list", "--repo=" + repo,
        "--state=" + choice_arg(args, "state", ["open", "closed", "all"], "open"),
        "--limit=%d" % limit_arg(args, 30, 50),
        "--json=number,title,state,author,labels,updatedAt,url",
    ]
    cmd += ["--label=" + l for l in labels_arg(args)]
    search = text_arg(args, "search", one_line=True, most=500)
    if search:
        cmd.append("--search=" + search)
    return answer(call.gh(cmd).strip())


def get_issue(call, args, access):
    repo = repository_arg(args, access)
    n = number_arg(args)
    issue = call.gh_json(["issue", "view", str(n), "--repo=" + repo, "--json=number,title,state,body,author,labels,comments,url"]) or {}
    notes = []
    body, clipped = clip(issue.get("body"), BODY_CLIP)
    if clipped:
        issue["body"] = body
        notes.append("The body is cut to its first %d characters." % BODY_CLIP)
    comments = issue.get("comments") or []
    if len(comments) > COMMENTS_KEPT:
        notes.append("Only the latest %d of its %d comments are here." % (COMMENTS_KEPT, len(comments)))
        comments = comments[-COMMENTS_KEPT:]
    for c in comments:
        if isinstance(c, dict):
            cb, cc = clip(c.get("body"), BODY_CLIP)
            if cc:
                c["body"] = cb
                notes.append("A comment's body is cut to its first %d characters." % BODY_CLIP)
    issue["comments"] = comments
    if notes:
        issue["note"] = " ".join(notes)
    return answer(issue)


def create_issue(call, args, access):
    repo = repository_arg(args, access)
    title = text_arg(args, "title", required=True, one_line=True, most=256)
    body = text_arg(args, "body") or ""
    cmd = ["issue", "create", "--repo=" + repo, "--title=" + title, "--body-file=-"]
    cmd += ["--label=" + l for l in labels_arg(args)]
    return answer(call.gh(cmd, stdin_text=body).strip())


def comment(call, args, access):
    repo = repository_arg(args, access)
    n = number_arg(args)
    body = text_arg(args, "body", required=True)
    issue = call.gh_json(["api", "repos/%s/issues/%d" % (repo, n)]) or {}
    kind = "pr" if issue.get("pull_request") else "issue"
    return answer(call.gh([kind, "comment", str(n), "--repo=" + repo, "--body-file=-"], stdin_text=body).strip())


def list_pull_requests(call, args, access):
    repo = repository_arg(args, access)
    cmd = [
        "pr", "list", "--repo=" + repo,
        "--state=" + choice_arg(args, "state", ["open", "closed", "merged", "all"], "open"),
        "--limit=%d" % limit_arg(args, 30, 50),
        "--json=number,title,state,author,headRefName,baseRefName,isDraft,updatedAt,url",
    ]
    base = branch_arg(args, "base", required=False)
    if base:
        cmd.append("--base=" + base)
    return answer(call.gh(cmd).strip())


def get_pull_request(call, args, access):
    repo = repository_arg(args, access)
    n = number_arg(args)
    diff = args.get("diff", False)
    if not isinstance(diff, bool):
        raise Refusal("diff is true or false.")
    pr = call.gh_json([
        "pr", "view", str(n), "--repo=" + repo,
        "--json=number,title,state,body,author,headRefName,baseRefName,isDraft,mergeable,reviewDecision,additions,deletions,changedFiles,labels,updatedAt,url",
    ]) or {}
    notes = []
    body, clipped = clip(pr.get("body"), BODY_CLIP)
    if clipped:
        pr["body"] = body
        notes.append("The body is cut to its first %d characters." % BODY_CLIP)
    if diff:
        text, clipped = clip(call.gh(["pr", "diff", str(n), "--repo=" + repo, "--color=never"]), DIFF_CLIP)
        pr["diff"] = text
        if clipped:
            notes.append("The diff is cut to its first %d characters; compare lists every file changed." % DIFF_CLIP)
    if notes:
        pr["note"] = " ".join(notes)
    return answer(pr)


def create_pull_request(call, args, access):
    repo = repository_arg(args, access)
    title = text_arg(args, "title", required=True, one_line=True, most=256)
    head = branch_arg(args, "head")
    base = branch_arg(args, "base")
    body = text_arg(args, "body") or ""
    draft = args.get("draft", False)
    if not isinstance(draft, bool):
        raise Refusal("draft is true or false.")
    cmd = ["pr", "create", "--repo=" + repo, "--head=" + head, "--base=" + base, "--title=" + title, "--body-file=-"]
    if draft:
        cmd.append("--draft")
    return answer(call.gh(cmd, stdin_text=body).strip())


def list_branches(call, args, access):
    repo = repository_arg(args, access)
    n = limit_arg(args, 30, 100)
    branches = call.gh_json(["api", "repos/%s/branches?per_page=%d" % (repo, n)]) or []
    lines = ["%s %s" % (b.get("name"), (b.get("commit") or {}).get("sha", "")) for b in branches if isinstance(b, dict)]
    return answer("\n".join(lines) if lines else "No branches.")


def compare(call, args, access):
    repo = repository_arg(args, access)
    base = branch_arg(args, "base")
    head = branch_arg(args, "head")
    data = call.gh_json(["api", "repos/%s/compare/%s...%s" % (repo, base, head)]) or {}
    commits = []
    for c in data.get("commits") or []:
        commit = c.get("commit") or {}
        author = (c.get("author") or {}).get("login") or (commit.get("author") or {}).get("name")
        commits.append({"sha": c.get("sha"), "title": first_line(commit.get("message")), "author": author})
    files = [
        {"name": f.get("filename"), "status": f.get("status"), "additions": f.get("additions"), "deletions": f.get("deletions")}
        for f in data.get("files") or []
    ]
    return answer({
        "status": data.get("status"),
        "ahead_by": data.get("ahead_by"),
        "behind_by": data.get("behind_by"),
        "commits": commits,
        "files": files,
    })


def whoami(call, args, access):
    # Test connection, and the check after a sign-in, show the owner
    # this answer's first text.
    user = call.gh_json(["api", "user"]) or {}
    return answer("Connected as @%s" % (user.get("login") or "an unnamed account"))


# The actions' input schemas.


def repository_property():
    return {
        "type": "string",
        "description": "The repository as owner/name, such as acme/widgets. list_repositories names the ones your access includes.",
    }


def schema(props, required):
    return {"type": "object", "properties": props, "required": required}


NUMBER = {"type": "integer", "description": "The issue's or pull request's number."}
LIMIT_50 = {"type": "integer", "description": "How many to list, at most 50; 30 when left out."}


def schemas():
    repo = repository_property()
    return {
        "list_repositories": schema({}, []),
        "get_repository": schema({"repository": repo}, ["repository"]),
        "list_issues": schema({
            "repository": repo,
            "state": {"type": "string", "enum": ["open", "closed", "all"], "description": "open when left out."},
            "limit": LIMIT_50,
            "labels": {"type": "array", "items": {"type": "string"}, "description": "Only issues with every one of these labels."},
            "search": {"type": "string", "description": "GitHub search words, such as \"crash in:title\"."},
        }, ["repository"]),
        "get_issue": schema({"repository": repo, "number": NUMBER}, ["repository", "number"]),
        "create_issue": schema({
            "repository": repo,
            "title": {"type": "string"},
            "body": {"type": "string", "description": "Markdown."},
            "labels": {"type": "array", "items": {"type": "string"}, "description": "Labels the repository already has."},
        }, ["repository", "title"]),
        "comment": schema({
            "repository": repo,
            "number": NUMBER,
            "body": {"type": "string", "description": "Markdown."},
        }, ["repository", "number", "body"]),
        "list_pull_requests": schema({
            "repository": repo,
            "state": {"type": "string", "enum": ["open", "closed", "merged", "all"], "description": "open when left out."},
            "limit": LIMIT_50,
            "base": {"type": "string", "description": "Only pull requests into this branch."},
        }, ["repository"]),
        "get_pull_request": schema({
            "repository": repo,
            "number": NUMBER,
            "diff": {"type": "boolean", "description": "Also answer the diff, cut at 50,000 characters."},
        }, ["repository", "number"]),
        "create_pull_request": schema({
            "repository": repo,
            "title": {"type": "string"},
            "head": {"type": "string", "description": "The branch with the changes, already on GitHub in this repository."},
            "base": {"type": "string", "description": "The branch to merge into, such as main."},
            "body": {"type": "string", "description": "Markdown."},
            "draft": {"type": "boolean", "description": "Open it as a draft."},
        }, ["repository", "title", "head", "base"]),
        "list_branches": schema({
            "repository": repo,
            "limit": {"type": "integer", "description": "How many to list, at most 100; 30 when left out."},
        }, ["repository"]),
        "compare": schema({
            "repository": repo,
            "base": {"type": "string", "description": "A branch, tag or commit."},
            "head": {"type": "string", "description": "A branch, tag or commit."},
        }, ["repository", "base", "head"]),
        "whoami": schema({}, []),
    }


OWNER_DECIDES = " The owner decides which repositories you may use and whether you may create issues, comments and pull requests."

# Every action, in the manifest's order. description is what the
# agent's model reads (the manifest's is the owner's). write marks an
# action that creates something on GitHub: an agent has it only when
# its access allows writing. owner_only keeps an action for the owner
# (Test connection).
TOOLS = [
    {"name": "list_repositories", "run": list_repositories,
     "description": "List the GitHub repositories your access includes, one per line, with whether each is private, when it was last updated and its description." + OWNER_DECIDES},
    {"name": "get_repository", "run": get_repository,
     "description": "Read a GitHub repository's details: its description, default branch, whether it is private, its address and when it was last updated." + OWNER_DECIDES},
    {"name": "list_issues", "run": list_issues,
     "description": "List a GitHub repository's issues, open ones unless you choose another state, newest first, filtered by labels or search words." + OWNER_DECIDES},
    {"name": "get_issue", "run": get_issue,
     "description": "Read one GitHub issue: its title, state, body (the first 20,000 characters), labels and its latest 20 comments." + OWNER_DECIDES},
    {"name": "create_issue", "run": create_issue, "write": True,
     "description": "Open an issue in a GitHub repository, as the account the owner signed in with. Answers the issue's address." + OWNER_DECIDES},
    {"name": "comment", "run": comment, "write": True,
     "description": "Comment on a GitHub issue or pull request by its number, as the account the owner signed in with. Answers the comment's address." + OWNER_DECIDES},
    {"name": "list_pull_requests", "run": list_pull_requests,
     "description": "List a GitHub repository's pull requests, open ones unless you choose another state, optionally only those into one base branch." + OWNER_DECIDES},
    {"name": "get_pull_request", "run": get_pull_request,
     "description": "Read one GitHub pull request: its title, state, body, branches, review state and size, and its diff when you ask for it." + OWNER_DECIDES},
    {"name": "create_pull_request", "run": create_pull_request, "write": True,
     "description": "Open a pull request in a GitHub repository from the head branch into the base branch. The head branch must already be on GitHub: this action moves no commits, so push it first with the Git integration, or ask whoever owns the branch. Answers the pull request's address." + OWNER_DECIDES},
    {"name": "list_branches", "run": list_branches,
     "description": "List a GitHub repository's branches, one per line with the commit each points at." + OWNER_DECIDES},
    {"name": "compare", "run": compare,
     "description": "Compare two branches, tags or commits of a GitHub repository: how far head is ahead of and behind base, its commits, and the files changed with lines added and removed (no patches)." + OWNER_DECIDES},
    {"name": "whoami", "run": whoami, "owner_only": True,
     "description": "Name the GitHub account the connection signs in as."},
]


def caller_access(params):
    """The calling agent's access, or None for the owner's own request,
    which carries no _meta.kivali."""
    kivali = (params.get("_meta") or {}).get("kivali")
    if not isinstance(kivali, dict):
        return None
    access = kivali.get("access")
    return access if isinstance(access, dict) else {}


def may(tool, access):
    if access is None:
        return True
    if tool.get("owner_only"):
        return False
    if tool.get("write") and not may_write(access):
        return False
    return True


def tools_list(params):
    # Kivali keeps an agent's list until its access, the connection or
    # the pod changes, so it depends on the access alone.
    access = caller_access(params)
    s = schemas()
    return {
        "tools": [
            {"name": t["name"], "description": t["description"], "inputSchema": s[t["name"]]}
            for t in TOOLS
            if may(t, access)
        ]
    }


def tools_call(params):
    access = caller_access(params)
    name = params.get("name")
    tool = next((t for t in TOOLS if t["name"] == name), None)
    if tool is None:
        return refusal("There is no action named %s." % name)
    if not may(tool, access):
        if tool.get("write"):
            return refusal("Your access to GitHub does not include creating issues, comments or pull requests; ask the owner.")
        return refusal("Your access to GitHub does not include %s; ask the owner." % name)
    args = params.get("arguments") or {}
    if not isinstance(args, dict):
        return refusal("The arguments are an object.")
    try:
        return tool["run"](Call(), args, access)
    except Refusal as e:
        return refusal(str(e))
    except Exception:
        log("error", "%s failed:\n%s" % (name, traceback.format_exc()))
        return refusal("The %s action failed inside the integration; its log has the details. Tell the owner." % name)


def initialize(params):
    return {
        "protocolVersion": params.get("protocolVersion") or PROTOCOL_VERSION,
        "capabilities": {"tools": {}},
        "serverInfo": {"name": "github", "version": VERSION},
    }


METHODS = {
    "initialize": initialize,
    "ping": lambda params: {},
    "tools/list": tools_list,
    "tools/call": tools_call,
}


def handle(msg):
    # The answer carries the request's id exactly as it came.
    out = {"jsonrpc": "2.0", "id": msg["id"]}
    method = METHODS.get(msg["method"])
    params = msg.get("params") if isinstance(msg.get("params"), dict) else {}
    if method is None:
        out["error"] = {"code": -32601, "message": "method not found: %s" % msg["method"]}
    else:
        try:
            out["result"] = method(params)
        except Exception:
            log("error", "%s failed:\n%s" % (msg["method"], traceback.format_exc()))
            out["error"] = {"code": -32603, "message": "the integration failed; its log has the details"}
    send(out)


def main():
    while True:
        line = sys.stdin.readline()
        if not line:
            return  # Kivali closed the input: stop.
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except ValueError:
            log("warning", "a line that is not JSON came in; ignored")
            continue
        if not isinstance(msg, dict):
            continue
        if "method" not in msg and "id" in msg:
            # The response to one of this server's own requests.
            deliver(msg)
            continue
        if not isinstance(msg.get("method"), str) or "id" not in msg:
            # A notification, such as notifications/initialized.
            continue
        if msg["method"] == "tools/call":
            # Each action on its own thread: a slow gh run holds up
            # neither another agent's call nor Kivali's ping, and the
            # reading loop goes on to read kivali/credential's answer.
            threading.Thread(target=handle, args=(msg,), daemon=True).start()
        else:
            handle(msg)


if __name__ == "__main__":
    main()
