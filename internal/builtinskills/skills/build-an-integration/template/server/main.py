"""Notes: a Kivali integration's server, from the build-an-integration
skill's template. A stdio MCP server, standard library only.

Kivali starts it in the integration's pod with the bundle as its
working directory and sends it one JSON-RPC message per line on its
input; it answers one per line on its output.

To make another integration from it: change SERVICE, VERSION and the
environment names, write one function per action, list each in TOOLS
with the agents' access it needs, and change manifest.json to match.
The plumbing (the output, send, api, handle, main) stays as it is.
"""

import json
import os
import sys
import threading
import traceback
import urllib.error
import urllib.parse
import urllib.request

# Kivali reads only JSON-RPC on the output: any other line, from a
# print, a library or a command this server runs, and it stops the
# server. So the protocol writes to a copy of the output taken here,
# and the output itself (descriptor 1, which sys.stdout and every
# command this server runs write to) becomes the error output, which
# Kivali keeps as the integration's log, a line at a time.
PROTOCOL = os.fdopen(os.dup(1), "w", encoding="utf-8")
os.dup2(2, 1)
sys.stdout = sys.stderr

PROTOCOL_VERSION = "2025-06-18"
SERVICE = "Notes"
VERSION = "0.1.0"

# The connection's values, by the names the manifest's mcp_config.env
# gives them. They are empty until the owner connects the integration,
# and in integration_validate, and the server still starts and lists
# its actions. A changed connection restarts the server, so reading
# them once is enough.
TOKEN = os.environ.get("NOTES_TOKEN", "")
# The API's address. The manifest sets it; a test points it at a stub.
API = os.environ.get("NOTES_API", "https://api.notes.example").rstrip("/")
# urllib's limit on each wait: to connect, and for each read. A service
# that does not answer is given up on well inside the 60 seconds Kivali
# waits for an action, so the agent reads this server's words rather
# than Kivali's timeout.
HTTP_TIMEOUT = 30

_write = threading.Lock()


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
    """An action's answer as the agent reads it: text, or compact JSON."""
    if not isinstance(value, str):
        value = json.dumps(value, separators=(",", ":"), ensure_ascii=False)
    return {"content": [{"type": "text", "text": value}], "isError": False}


def refusal(text):
    return {"content": [{"type": "text", "text": text}], "isError": True}


def api(method, path, body=None, query=None):
    """One request to the service. urllib goes through the proxy that
    HTTPS_PROXY names, the pod's only way out; a client that ignores
    that variable reaches nothing."""
    if not TOKEN:
        raise Refusal("The %s integration is not connected yet; the owner connects it on its page in Kivali." % SERVICE)
    url = API + path
    if query:
        url += "?" + urllib.parse.urlencode(query)
    headers = {
        "Authorization": "Bearer " + TOKEN,
        "Accept": "application/json",
        "User-Agent": "kivali-notes/" + VERSION,
    }
    data = None
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=HTTP_TIMEOUT) as resp:
            raw = resp.read()
    except urllib.error.HTTPError as e:
        detail = e.read(500).decode("utf-8", "replace").strip()
        raise Refusal("%s answered %d %s: %s" % (SERVICE, e.code, e.reason, detail or "no detail"))
    except urllib.error.URLError as e:
        # "Tunnel connection failed: 403" is the proxy refusing a host
        # that kivali.hosts does not list.
        raise Refusal("%s could not be reached: %s" % (SERVICE, e.reason))
    except OSError as e:
        raise Refusal("%s could not be reached: %s" % (SERVICE, e))
    return json.loads(raw) if raw else {}


# The agents' access, as the owner's access form fills it in. A switch
# is always there, true or false; a list or text left empty is left
# out; and a field a later version adds is absent from access the
# owner set before it. Each reads absent as the least it can grant.


def notebooks_of(access):
    return [n for n in access.get("notebooks") or [] if isinstance(n, str) and n]


def may_add(access):
    return access.get("add") is True


def notebook_property(access):
    """The notebook argument. An agent's list of actions is its own, so
    its schema names the notebooks that agent may use."""
    p = {"type": "string", "description": "The notebook, by name."}
    if access is not None:
        p["enum"] = notebooks_of(access)
    return p


def notebook_arg(args, access):
    nb = args.get("notebook")
    if not isinstance(nb, str) or not nb:
        raise Refusal("Name the notebook.")
    if access is not None and nb not in notebooks_of(access):
        raise Refusal("Your access does not include the notebook %s. Ask the owner if your work needs it." % nb)
    return nb


def find_notes(args, access):
    nb = notebook_arg(args, access)
    query = args.get("query") or ""
    if not isinstance(query, str):
        raise Refusal("query is text.")
    data = api("GET", "/v1/notebooks/%s/notes" % urllib.parse.quote(nb, safe=""), query={"q": query, "limit": 20})
    notes = [
        {"id": n.get("id"), "title": n.get("title"), "updated": n.get("updated_at"), "body": (n.get("body") or "")[:500]}
        for n in data.get("notes") or []
    ]
    return answer({"notebook": nb, "notes": notes})


def add_note(args, access):
    if access is not None and not may_add(access):
        raise Refusal("Your access does not include adding notes. Ask the owner if your work needs it.")
    nb = notebook_arg(args, access)
    title, body = args.get("title"), args.get("body") or ""
    if not isinstance(title, str) or not title.strip():
        raise Refusal("Give the note a title.")
    if not isinstance(body, str):
        raise Refusal("body is text.")
    data = api("POST", "/v1/notebooks/%s/notes" % urllib.parse.quote(nb, safe=""), body={"title": title, "body": body})
    return answer({"notebook": nb, "id": data.get("id"), "title": data.get("title", title)})


def list_notebooks(args, access):
    # The access form reads this answer's text as a JSON array of
    # strings, and each one it shows becomes a value in an agent's
    # access: names, as notebook_arg matches them.
    data = api("GET", "/v1/notebooks")
    return answer([nb.get("name") for nb in data.get("notebooks") or [] if nb.get("name")])


def whoami(args, access):
    # Test connection shows the owner this answer's first text.
    data = api("GET", "/v1/me")
    return answer("Signed in as %s (%s)" % (data.get("name") or "an unnamed account", data.get("email") or "no email"))


def find_schema(access):
    return {
        "type": "object",
        "properties": {
            "notebook": notebook_property(access),
            "query": {"type": "string", "description": "Words to look for; leave it out for the latest notes."},
        },
        "required": ["notebook"],
    }


def add_schema(access):
    return {
        "type": "object",
        "properties": {
            "notebook": notebook_property(access),
            "title": {"type": "string"},
            "body": {"type": "string", "description": "The note's text, in Markdown."},
        },
        "required": ["notebook", "title"],
    }


def no_arguments(access):
    return {"type": "object", "properties": {}}


# Every action, in the manifest's order. description is what the
# agent's model reads (the manifest's is the owner's). allowed is the
# access an agent needs for the action to be in its list; None keeps
# it for the owner: Test connection and the access form.
TOOLS = [
    {
        "name": "find_notes",
        "description": "Search the notes in one notebook. Answers each note's id, title, last change and the start of its text.",
        "schema": find_schema,
        "allowed": lambda access: bool(notebooks_of(access)),
        "run": find_notes,
    },
    {
        "name": "add_note",
        "description": "Add a note to a notebook. Answers the new note's id.",
        "schema": add_schema,
        "allowed": lambda access: may_add(access) and bool(notebooks_of(access)),
        "run": add_note,
    },
    {
        "name": "list_notebooks",
        "description": "List the account's notebooks by name.",
        "schema": no_arguments,
        "allowed": None,
        "run": list_notebooks,
    },
    {
        "name": "whoami",
        "description": "Name the account the connection signs in as.",
        "schema": no_arguments,
        "allowed": None,
        "run": whoami,
    },
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
    return access is None or (tool["allowed"] is not None and tool["allowed"](access))


def tools_list(params):
    # Kivali keeps an agent's list until its access, the connection or
    # the pod changes, so it depends on the access alone.
    access = caller_access(params)
    return {
        "tools": [
            {"name": t["name"], "description": t["description"], "inputSchema": t["schema"](access)}
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
        return refusal("Your access does not include %s. Ask the owner if your work needs it." % name)
    args = params.get("arguments") or {}
    if not isinstance(args, dict):
        return refusal("The arguments are an object.")
    try:
        return tool["run"](args, access)
    except Refusal as e:
        return refusal(str(e))
    except Exception:
        log("error", "%s failed:\n%s" % (name, traceback.format_exc()))
        return refusal("The %s action failed inside the integration; its log has the details. Tell the owner." % name)


def initialize(params):
    return {
        "protocolVersion": params.get("protocolVersion") or PROTOCOL_VERSION,
        "capabilities": {"tools": {}},
        "serverInfo": {"name": "notes", "version": VERSION},
    }


# Every other method is answered -32601. Kivali sends kivali/setup,
# kivali/requirements and kivali/uninstall only to a bundle whose
# kivali.connection.method is setup, which answers them (template-setup
# does); this one's is token, so it never gets them.
METHODS = {
    "initialize": initialize,
    "ping": lambda params: {},
    "tools/list": tools_list,
    "tools/call": tools_call,
}


def handle(msg):
    # The answer carries the request's id exactly as it came: Kivali
    # matches it as a number, and never matches one written as text.
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
        if not isinstance(msg, dict) or not isinstance(msg.get("method"), str) or "id" not in msg:
            # A notification, such as notifications/initialized.
            continue
        if msg["method"] == "tools/call":
            # Each action on its own thread: a slow request to the
            # service holds up neither another agent's call nor
            # Kivali's ping, which it sends every 30 seconds.
            threading.Thread(target=handle, args=(msg,), daemon=True).start()
        else:
            handle(msg)


if __name__ == "__main__":
    main()
