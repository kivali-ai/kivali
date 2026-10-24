"""Deals: a Kivali integration whose own setup flow makes its
connection, from the build-an-integration skill's template-setup. A
stdio MCP server, standard library only.

The owner pastes a one-time setup key; the server trades it for a
short-lived session, builds a private app in the owner's Deals account
with the scopes agents need, installs it, and keeps the app's token.
Where Deals does not hand the token back, the owner opens the app's
page, shows the token there, and pastes it.

Besides MCP's own requests, Kivali sends it the three of a setup
connection (method setup in the manifest):

  kivali/setup         one call of the flow: the next step for the owner
  kivali/requirements  the integration's health, as rows met or not
  kivali/uninstall     undo the installation in Deals

README.md, beside manifest.json, says what each part does.
"""

import json
import os
import sys
import threading
import traceback
import urllib.error
import urllib.parse
import urllib.request

# Kivali reads only JSON-RPC on the output: any other line and it stops
# the server. The protocol writes to a copy of the output taken here,
# and the output itself becomes the error output, which Kivali keeps as
# the integration's log.
PROTOCOL = os.fdopen(os.dup(1), "w", encoding="utf-8")
os.dup2(2, 1)
sys.stdout = sys.stderr

PROTOCOL_VERSION = "2025-06-18"
SERVICE = "Deals"
VERSION = "0.1.0"

# The connection's values, by the names the manifest's env gives them:
# what setup kept. Empty until setup keeps them. Each Keep restarts the
# server with what was kept, so reading them once is enough.
TOKEN = os.environ.get("DEALS_TOKEN", "")
APP_ID = os.environ.get("DEALS_APP_ID", "")
# The API, which the server calls through the pod's proxy (kivali.hosts),
# and the pages the owner's browser opens (kivali.connection.setup.hosts).
API = os.environ.get("DEALS_API", "https://api.deals.example").rstrip("/")
APP = os.environ.get("DEALS_APP", "https://app.deals.example").rstrip("/")
# urllib's limit on each wait. Kivali waits five minutes for a setup
# step and a minute for an action, but five seconds for the
# requirements, which the owner's page waits on.
HTTP_TIMEOUT = 30
CHECK_TIMEOUT = 4

# The scopes the app is built with. A version that needs another adds it
# here; the scopes requirement then asks the owner to rebuild the app.
SCOPES = ["deals.read"]

_write = threading.Lock()


def send(msg):
    line = json.dumps(msg, separators=(",", ":"))
    with _write:
        PROTOCOL.write(line + "\n")
        PROTOCOL.flush()


def log(level, message):
    send({"jsonrpc": "2.0", "method": "kivali/log", "params": {"level": level, "message": message}})


class Refusal(Exception):
    """Something that cannot be done, in words the owner or the agent
    reads."""


class Status(Exception):
    """The service answered with an HTTP error."""

    def __init__(self, code, detail):
        super().__init__("%s answered %d: %s" % (SERVICE, code, detail or "no detail"))
        self.code = code


def api(method, path, token, body=None, query=None, timeout=HTTP_TIMEOUT):
    """One request to the service, as token. urllib goes through the
    proxy HTTPS_PROXY names, the pod's only way out."""
    url = API + path
    if query:
        url += "?" + urllib.parse.urlencode(query)
    headers = {"Accept": "application/json", "User-Agent": "kivali-deals/" + VERSION}
    if token:
        headers["Authorization"] = "Bearer " + token
    data = None
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
    except urllib.error.HTTPError as e:
        raise Status(e.code, e.read(500).decode("utf-8", "replace").strip())
    except (urllib.error.URLError, OSError) as e:
        raise Refusal("%s could not be reached: %s" % (SERVICE, getattr(e, "reason", e)))
    return json.loads(raw) if raw else {}


# The setup flow. Kivali calls kivali/setup first with an empty answer,
# then after each step with what the step gave back, until a step is
# done or the call answers an error. Where the flow is lives in the
# step's state, which Kivali carries to the next answer in its memory
# alone: the server may restart between calls (each Keep restarts it),
# so it keeps nothing of the flow itself.


def form(words, fields, state):
    return {"form": {"words": words, "fields": fields}, "state": state}


def ask_key(flow, words=None):
    words = words or (
        "Paste a setup key from your %s account: Settings, Developer, Setup keys. "
        "Kivali uses it once to %s the app, and keeps it nowhere." % (SERVICE, "build" if flow == "setup" else "rebuild")
    )
    return form(words, ["setup_key"], {"at": "key"})


def show_token_wait(app_id, session):
    return {
        "wait": {
            "words": "Open the app's page in %s and choose Show token. Kivali asks for it here once it is shown." % SERVICE,
            "url": APP + "/apps/" + urllib.parse.quote(app_id, safe=""),
        },
        "state": {"at": "show", "app_id": app_id, "session": session},
    }


def build(flow, key, stage):
    """The app built (setup) or rebuilt with this version's scopes
    (rescope), from the setup key, in one call, with its stages."""
    total = 4 if flow == "setup" else 3
    stage("Checking the setup key", 1, total)
    try:
        session = api("POST", "/v1/setup-keys/exchange", None, body={"key": key}).get("access_token") or ""
    except Status as e:
        if e.code in (400, 401, 403):
            return ask_key(flow, "%s did not take that setup key; each one works once. Paste a new one." % SERVICE)
        raise
    if flow == "rescope":
        if not APP_ID:
            raise Refusal("There is no app to rebuild; install %s again." % SERVICE)
        stage("Updating the app's scopes", 2, total)
        api("PUT", "/v1/apps/%s/scopes" % urllib.parse.quote(APP_ID, safe=""), session, body={"scopes": SCOPES})
        stage("Installing the app again", 3, total)
        api("POST", "/v1/apps/%s/install" % urllib.parse.quote(APP_ID, safe=""), session)
        return {"done": True}
    stage("Creating the app", 2, total)
    app_id = str(api("POST", "/v1/apps", session, body={"name": "Kivali", "scopes": SCOPES}).get("id") or "")
    if not app_id:
        raise Refusal("%s made the app but did not say its id." % SERVICE)
    stage("Installing the app", 3, total)
    api("POST", "/v1/apps/%s/install" % urllib.parse.quote(app_id, safe=""), session)
    stage("Reading the app's token", 4, total)
    try:
        token = api("GET", "/v1/apps/%s/token" % urllib.parse.quote(app_id, safe=""), session).get("token") or ""
    except Status as e:
        if e.code != 404:
            raise
        token = ""
    if not token:
        # Deals keeps the token to its own page for some accounts: the
        # owner shows it there and pastes it. The app id is kept now, so
        # the app can be found again.
        step = show_token_wait(app_id, session)
        step["keep"] = {"app_id": app_id}
        return step
    return {"done": True, "keep": {"token": token, "app_id": app_id}}


def setup(params, stage):
    flow = params.get("flow")
    if flow not in ("setup", "rescope"):
        raise Refusal("%s has no flow %s." % (SERVICE, flow))
    answer = params.get("answer") or {}
    state = answer.get("state") or {}
    entered = answer.get("form") or {}
    at = state.get("at", "")
    if at == "":
        return ask_key(flow)
    if at == "key":
        key = entered.get("setup_key", "")
        if not key:
            return ask_key(flow, "Paste a setup key; it cannot be left blank.")
        return build(flow, key, stage)
    if at == "show":
        # A Wait's check: has the owner shown the token on the app's page?
        app = api("GET", "/v1/apps/%s" % urllib.parse.quote(state["app_id"], safe=""), state.get("session"))
        if app.get("token_shown"):
            return form("Paste the app's token, as its page in %s shows it." % SERVICE, ["token"], {"at": "token"})
        return show_token_wait(state["app_id"], state.get("session", ""))
    if at == "token":
        token = entered.get("token", "")
        if token:
            try:
                api("GET", "/v1/me", token)
                return {"done": True, "keep": {"token": token}}
            except Status as e:
                if e.code not in (401, 403):
                    raise
        return form("%s did not take that token. Copy it again from the app's page." % SERVICE, ["token"], {"at": "token"})
    raise Refusal("The setup is at a step this version does not know; start it again.")


def kivali_setup(params, progress_token):
    def stage(words, done, total):
        if progress_token is None:
            return
        send({"jsonrpc": "2.0", "method": "notifications/progress", "params": {
            "progressToken": progress_token, "progress": done, "total": total, "message": words,
        }})

    return {"step": setup(params, stage)}


# The integration's health, read whenever the owner's page is: rows met
# or not, each with the flow its Fix button runs. It answers with no
# connection too, before anything is kept.


def kivali_requirements(params):
    if not TOKEN:
        return {"requirements": [
            {"id": "token", "title": "App token", "words": "No app token is kept yet.", "met": False, "fix": "setup"},
        ]}
    try:
        me = api("GET", "/v1/me", TOKEN, timeout=CHECK_TIMEOUT)
    except Status as e:
        if e.code not in (401, 403):
            return {"requirements": [{"id": "token", "title": "App token", "words": "The app's token could not be checked: %s" % e, "met": False}]}
        return {"requirements": [
            {"id": "token", "title": "App token", "words": "%s no longer takes the app's token. Set it up again to build a new app." % SERVICE, "met": False, "fix": "setup"},
        ]}
    except Refusal as e:
        return {"requirements": [{"id": "token", "title": "App token", "words": "The app's token could not be checked: %s" % e, "met": False}]}
    rows = [{"id": "token", "title": "App token", "words": "The app's token works, in %s." % (me.get("name") or "the account"), "met": True}]
    missing = [s for s in SCOPES if s not in (me.get("scopes") or [])]
    if missing:
        rows.append({
            "id": "scopes", "title": "Scopes", "met": False, "fix": "rescope",
            "words": "This version needs %s, which the app was not built with. Paste a new setup key to rebuild it." % ", ".join(missing),
        })
    else:
        rows.append({"id": "scopes", "title": "Scopes", "words": "The app has every scope this version needs.", "met": True})
    return {"requirements": rows}


def kivali_uninstall(params):
    if not TOKEN:
        return {}
    try:
        api("DELETE", "/v1/apps/self", TOKEN)
    except Status as e:
        if e.code != 404:
            raise Refusal("%s did not remove the app: %s" % (SERVICE, e))
    return {}


# The agents' actions.


def answer(value):
    if not isinstance(value, str):
        value = json.dumps(value, separators=(",", ":"), ensure_ascii=False)
    return {"content": [{"type": "text", "text": value}], "isError": False}


def refusal(text):
    return {"content": [{"type": "text", "text": text}], "isError": True}


def connected():
    if not TOKEN:
        raise Refusal("The %s integration is not set up yet; the owner sets it up on its page in Kivali." % SERVICE)
    return TOKEN


def find_deals(args):
    query = args.get("query") or ""
    if not isinstance(query, str):
        raise Refusal("query is text.")
    data = api("GET", "/v1/deals", connected(), query={"q": query, "limit": 20})
    return answer([{"id": d.get("id"), "name": d.get("name"), "stage": d.get("stage"), "amount": d.get("amount")} for d in data.get("deals") or []])


def whoami(args):
    data = api("GET", "/v1/me", connected())
    return answer("Installed in %s" % (data.get("name") or "an unnamed account"))


TOOLS = [
    {
        "name": "find_deals",
        "description": "Search the account's deals by name. Answers each deal's id, name, stage and amount.",
        "inputSchema": {"type": "object", "properties": {"query": {"type": "string", "description": "Words in the deal's name; leave it out for the latest deals."}}},
        "run": find_deals,
    },
    {
        "name": "whoami",
        "description": "Name the account the app is installed in.",
        "inputSchema": {"type": "object", "properties": {}},
        "run": whoami,
    },
]


def tools_list(params):
    return {"tools": [{k: t[k] for k in ("name", "description", "inputSchema")} for t in TOOLS]}


def tools_call(params):
    tool = next((t for t in TOOLS if t["name"] == params.get("name")), None)
    if tool is None:
        return refusal("There is no action named %s." % params.get("name"))
    args = params.get("arguments") or {}
    if not isinstance(args, dict):
        return refusal("The arguments are an object.")
    try:
        return tool["run"](args)
    except Refusal as e:
        return refusal(str(e))
    except Status as e:
        return refusal(str(e))
    except Exception:
        log("error", "%s failed:\n%s" % (tool["name"], traceback.format_exc()))
        return refusal("The %s action failed inside the integration; its log has the details. Tell the owner." % tool["name"])


def initialize(params):
    return {
        "protocolVersion": params.get("protocolVersion") or PROTOCOL_VERSION,
        "capabilities": {"tools": {}},
        "serverInfo": {"name": "deals", "version": VERSION},
    }


METHODS = {
    "initialize": lambda params, token: initialize(params),
    "ping": lambda params, token: {},
    "tools/list": lambda params, token: tools_list(params),
    "tools/call": lambda params, token: tools_call(params),
    "kivali/setup": kivali_setup,
    "kivali/requirements": lambda params, token: kivali_requirements(params),
    "kivali/uninstall": lambda params, token: kivali_uninstall(params),
}


def handle(msg):
    out = {"jsonrpc": "2.0", "id": msg["id"]}
    method = METHODS.get(msg["method"])
    params = msg.get("params") if isinstance(msg.get("params"), dict) else {}
    progress_token = (params.get("_meta") or {}).get("progressToken")
    if method is None:
        out["error"] = {"code": -32601, "message": "method not found: %s" % msg["method"]}
    else:
        try:
            out["result"] = method(params, progress_token)
        except (Refusal, Status) as e:
            # The owner reads these words: the setup fails with them, or
            # the uninstall is refused with them.
            out["error"] = {"code": -32000, "message": str(e)}
        except Exception:
            log("error", "%s failed:\n%s" % (msg["method"], traceback.format_exc()))
            out["error"] = {"code": -32603, "message": "%s failed inside the integration; its log has the details" % SERVICE}
    send(out)


# Requests that may wait on the service go on threads, so Kivali's ping,
# every 30 seconds, is answered while a setup step builds the app.
SLOW = {"tools/call", "kivali/setup", "kivali/requirements", "kivali/uninstall"}


def main():
    while True:
        line = sys.stdin.readline()
        if not line:
            return
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except ValueError:
            log("warning", "a line that is not JSON came in; ignored")
            continue
        if not isinstance(msg, dict) or not isinstance(msg.get("method"), str) or "id" not in msg:
            continue
        if msg["method"] in SLOW:
            threading.Thread(target=handle, args=(msg,), daemon=True).start()
        else:
            handle(msg)


if __name__ == "__main__":
    main()
