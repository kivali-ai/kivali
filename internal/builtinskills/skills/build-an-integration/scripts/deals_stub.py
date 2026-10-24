"""A stub of Deals, the imaginary service template-setup builds an app
in, for try.py: run it with --stub, and point the server at it with
--env DEALS_API=http://127.0.0.1:PORT.

usage: python3 deals_stub.py PORT [--installed] [--token-on-page] [--scopes SCOPES]

  --installed       the account already holds the app setup makes, app_1,
                    installed: for the requirements, a Fix and uninstall
                    of an integration installed before this run.
  --token-on-page   Deals keeps the app's token to the app's page: the
                    token endpoint answers 404, and the app reads as
                    shown there from the second look on, as when the
                    owner chose Show token meanwhile.
  --scopes SCOPES   the scopes the installed app has, comma-separated
                    (default deals.read); empty for an app built before
                    a version needed them.

It answers as Deals's documentation says, and writes each request it
answers to its output, which try.py shows at the end:

  POST   /v1/setup-keys/exchange   {"key"} -> {"access_token"}; a key
                                   used before, or not starting sk-, 401
  GET    /v1/apps?name=            the session's apps of that name
  POST   /v1/apps                  {"name", "scopes"} -> {"id"}
  GET    /v1/apps/{id}             {"id", "name", "token_shown"}
  PUT    /v1/apps/{id}/scopes      {"scopes"}
  POST   /v1/apps/{id}/install
  GET    /v1/apps/{id}/token       {"token"}, or 404 with --token-on-page
  GET    /v1/me                    as the app's token: {"name", "scopes"}
  DELETE /v1/apps/self             as the app's token: the app removed, and
                                   its token refused from then on
  GET    /v1/deals?q=              as the app's token: {"deals": [...]}

The session a setup key is traded for is "session-<key>"; the app's
token is "app-token-0001".
"""

import json
import sys
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

APP_TOKEN = "app-token-0001"
DEALS = [
    {"id": "d1", "name": "Acme renewal", "stage": "proposal", "amount": 12000},
    {"id": "d2", "name": "Globex pilot", "stage": "won", "amount": 4000},
]

lock = threading.Lock()
used = set()
apps = {}
looks = {}
token_on_page = False
removed = []
installed_scopes = ["deals.read"]


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def reply(self, code, body=None):
        raw = b"" if body is None else json.dumps(body).encode()
        self.send_response(code)
        if raw:
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)
        print("%s %s -> %d" % (self.command, self.path.split("?")[0], code), flush=True)

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        if not n:
            return {}
        try:
            return json.loads(self.rfile.read(n))
        except ValueError:
            return {}

    def bearer(self):
        auth = self.headers.get("Authorization") or ""
        return auth[len("Bearer "):] if auth.startswith("Bearer ") else ""

    def route(self):
        u = urllib.parse.urlsplit(self.path)
        parts = [p for p in u.path.split("/") if p]
        query = urllib.parse.parse_qs(u.query)
        auth = self.bearer()
        session = auth.startswith("session-")
        body = self.body()
        m = self.command
        with lock:
            if m == "POST" and parts == ["v1", "setup-keys", "exchange"]:
                key = str(body.get("key") or "")
                if not key.startswith("sk-") or key in used:
                    return self.reply(401, {"error": "that setup key is not one Deals takes, or was used"})
                used.add(key)
                return self.reply(200, {"access_token": "session-" + key})
            if session and parts == ["v1", "apps"]:
                if m == "GET":
                    name = (query.get("name") or [""])[0]
                    return self.reply(200, {"apps": [{"id": i, "name": a["name"]} for i, a in sorted(apps.items()) if a["name"] == name]})
                if m == "POST":
                    app_id = "app_%d" % (len(apps) + 1)
                    apps[app_id] = {"name": str(body.get("name") or ""), "scopes": list(body.get("scopes") or [])}
                    return self.reply(201, {"id": app_id})
            if session and len(parts) >= 3 and parts[:2] == ["v1", "apps"] and parts[2] in apps:
                app_id, rest = parts[2], parts[3:]
                if m == "GET" and rest == []:
                    looks[app_id] = looks.get(app_id, 0) + 1
                    return self.reply(200, {"id": app_id, "name": apps[app_id]["name"], "token_shown": looks[app_id] >= 2})
                if m == "PUT" and rest == ["scopes"]:
                    apps[app_id]["scopes"] = list(body.get("scopes") or [])
                    installed_scopes[:] = apps[app_id]["scopes"]
                    return self.reply(200, {})
                if m == "POST" and rest == ["install"]:
                    installed_scopes[:] = apps[app_id]["scopes"]
                    return self.reply(200, {})
                if m == "GET" and rest == ["token"]:
                    if token_on_page:
                        return self.reply(404, {"error": "the token is on the app's page"})
                    return self.reply(200, {"token": APP_TOKEN})
            if auth == APP_TOKEN and not removed:
                if m == "GET" and parts == ["v1", "me"]:
                    return self.reply(200, {"name": "Acme", "scopes": installed_scopes})
                if m == "DELETE" and parts == ["v1", "apps", "self"]:
                    removed.append(True)
                    print("the app was removed", flush=True)
                    return self.reply(204)
                if m == "GET" and parts == ["v1", "deals"]:
                    q = (query.get("q") or [""])[0].lower()
                    return self.reply(200, {"deals": [d for d in DEALS if q in d["name"].lower()]})
            return self.reply(401, {"error": "not with that token"})

    do_GET = do_POST = do_PUT = do_DELETE = route


def main():
    global token_on_page
    args = sys.argv[1:]
    if not args or not args[0].isdigit():
        sys.exit(__doc__.strip())
    port = int(args.pop(0))
    while args:
        a = args.pop(0)
        if a == "--installed":
            apps["app_1"] = {"name": "Kivali", "scopes": installed_scopes}
        elif a == "--token-on-page":
            token_on_page = True
        elif a == "--scopes" and args:
            installed_scopes[:] = [s for s in args.pop(0).split(",") if s]
        else:
            sys.exit("deals_stub.py: %r is not an option it takes" % a)
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
