"""Run an integration bundle's server the way Kivali runs it, and call
it the way Kivali calls it: as the owner (the list at every start, Test
connection, an access field's options) or as an agent with an access.

usage: python3 try.py [options] BUNDLE REQUEST...

BUNDLE is the bundle's directory, the one holding manifest.json.

Each REQUEST, in the order given:
  list                 the actions, as Kivali lists them
  call ACTION [JSON]   one action, with JSON as its arguments ({} when left out)
  verify               the kivali.verify action, as Test connection calls it
  choices FIELD        the action kivali.access.choices names for FIELD, as
                       the access form calls it

options:
  --field KEY=VALUE    a connection field's value, by user_config key; repeat
                       it. With none, the server starts as it does before the
                       owner connects it: every field empty.
  --env NAME=VALUE     an environment variable over the manifest's, such as the
                       stub's address; repeat it.
  --agent SLUG         make the requests as this agent; without it, they are
                       the owner's.
  --access JSON        the agent's access, as the access form fills it in
                       (default {}).
  --stub COMMAND       start COMMAND (a stub of the service) first, wait until
                       the 127.0.0.1 and localhost ports the --env values name
                       accept connections, and stop it at the end.

The server gets what the pod gives it (PATH, a HOME of its own and the
manifest's env with the connection's values put in) but no proxy
variable, so it reaches this machine and nothing else: a stub answers,
the real service does not. Lines starting "problem:" are what fails in
Kivali; the exit status is 1 when there is one.
"""

import argparse
import json
import os
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.parse

PROTOCOL_VERSION = "2025-06-18"
MAX_LINE = 16 << 20
STDERR_LINE = 8 << 10
HANDSHAKE_SECONDS = 20
CALL_SECONDS = 60
OTHER_SECONDS = 20
VERIFY_MESSAGE = 1000
# The shortest secret value Kivali replaces with [secret].
SCRUB_MIN = 6
PROXY_VARS = {"http_proxy", "https_proxy", "all_proxy", "no_proxy", "ftp_proxy"}
PLACEHOLDER = re.compile(r"\$\{([^}]*)\}")

out_lock = threading.Lock()
problems = []


def say(line=""):
    with out_lock:
        print(line, flush=True)


def problem(text):
    problems.append(text)
    say("problem: " + text)


def note(text):
    say("note: " + text)


def fail(text):
    say("problem: " + text)
    sys.exit(1)


def parse_pairs(pairs, what):
    out = {}
    for p in pairs:
        k, sep, v = p.partition("=")
        if not sep or not k:
            fail("%s %r is not NAME=VALUE" % (what, p))
        out[k] = v
    return out


def parse_requests(words):
    reqs, i = [], 0
    while i < len(words):
        w = words[i]
        if w in ("list", "verify"):
            reqs.append((w, None, None))
            i += 1
        elif w == "choices":
            if i + 1 >= len(words):
                fail("choices needs the field's key")
            reqs.append((w, words[i + 1], None))
            i += 2
        elif w == "call":
            if i + 1 >= len(words):
                fail("call needs the action's name")
            action, args = words[i + 1], {}
            i += 2
            if i < len(words) and words[i].lstrip().startswith("{"):
                try:
                    args = json.loads(words[i])
                except ValueError as e:
                    fail("the arguments of call %s are not JSON: %s" % (action, e))
                i += 1
            reqs.append(("call", action, args))
        else:
            fail("%r is not a request: list, call ACTION [JSON], verify or choices FIELD" % w)
    return reqs


def check_access(manifest, access):
    """The access as the owner's form would send it: say what the form
    would never send, since no agent can have it."""
    schema = ((manifest.get("kivali") or {}).get("access") or {}).get("schema")
    if schema is None:
        if access:
            problem("the manifest has no access form, so every agent's access is {}; this access is one no agent has")
        return
    props = schema.get("properties") or {}
    for key, value in access.items():
        p = props.get(key)
        if p is None:
            problem("the access form has no field %s; no agent's access holds it" % key)
            continue
        typ = p.get("type")
        if typ == "boolean" and not isinstance(value, bool):
            problem("%s is a switch: true or false" % key)
        elif typ == "string" and not isinstance(value, str):
            problem("%s is text" % key)
        elif typ == "array" and not (isinstance(value, list) and all(isinstance(x, str) for x in value)):
            problem("%s is a list of strings" % key)
        elif value in ("", []):
            note("the access form leaves %s out when it is empty; leave it out here to see what an agent gets" % key)
    for key, p in props.items():
        if p.get("type") == "boolean" and key not in access:
            note("the access form always sends the switch %s; an agent whose access was set before a version added it is the only one without it" % key)
    for key in schema.get("required") or []:
        if access.get(key) in (None, "", []):
            note("the access form refuses access without %s, so no agent holds this access" % key)


class Server:
    def __init__(self, manifest, bundle, env, fields, sensitive, display):
        self.manifest, self.fields, self.display = manifest, fields, display
        self.secrets = [v for k, v in fields.items() if k in sensitive and len(v) >= SCRUB_MIN]
        mcp = (manifest.get("server") or {}).get("mcp_config") or {}
        self.home = tempfile.mkdtemp(prefix="kivali-try-")
        env = dict(env, HOME=self.home)
        self.env = env
        self.next_id = 0
        self.waiting = {}
        self.cond = threading.Condition()
        self.dropped = set()
        self.stopped = False
        self.bad_line = False
        # flagged: the request in flight already has its problem said.
        self.flagged = False
        try:
            self.proc = subprocess.Popen(
                [mcp.get("command") or "python3"] + list(mcp.get("args") or []),
                cwd=bundle,
                env=env,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
            )
        except OSError as e:
            shutil.rmtree(self.home, ignore_errors=True)
            fail("the server does not start: %s" % e)
        self.wlock = threading.Lock()
        threading.Thread(target=self.read_out, daemon=True).start()
        threading.Thread(target=self.read_err, daemon=True).start()

    def scrub(self, s):
        for v in self.secrets:
            s = s.replace(v, "[secret]")
        return s

    def write(self, msg):
        try:
            with self.wlock:
                self.proc.stdin.write((json.dumps(msg) + "\n").encode())
                self.proc.stdin.flush()
        except (BrokenPipeError, OSError):
            pass

    def read_err(self):
        for raw in iter(self.proc.stderr.readline, b""):
            line = raw[:STDERR_LINE].decode("utf-8", "replace").rstrip("\r\n")
            if line:
                say("  log stderr: " + self.scrub(line))

    def read_out(self):
        for raw in iter(self.proc.stdout.readline, b""):
            if len(raw) > MAX_LINE:
                problem("the server wrote a line over 16 MiB; Kivali stops the server there")
                self.stop_for_bad_line()
                return
            line = raw.decode("utf-8", "replace").strip()
            if not line:
                continue
            msg = self.as_jsonrpc(line)
            if msg is None:
                problem("the server wrote a line that is not JSON-RPC; Kivali stops the server there. The line: " + self.scrub(line[:300]))
                self.stop_for_bad_line()
                return
            self.on_message(msg)
        with self.cond:
            self.stopped = True
            self.cond.notify_all()

    def stop_for_bad_line(self):
        self.bad_line = True
        self.proc.kill()
        with self.cond:
            self.stopped = True
            self.cond.notify_all()

    @staticmethod
    def as_jsonrpc(line):
        # What the shim takes for JSON-RPC: an object that is a request
        # (method and id), a notification (method) or a response (id).
        try:
            msg = json.loads(line)
        except ValueError:
            return None
        if not isinstance(msg, dict):
            return None
        method = msg.get("method", "")
        if not isinstance(method, str):
            return None
        if method == "" and "id" not in msg:
            return None
        return msg

    def on_message(self, msg):
        method = msg.get("method", "")
        if method == "":
            # The answer is matched by its id exactly as it was sent:
            # the shim's handshake sends text, Kivali's requests send
            # whole numbers, and Kivali never matches a number that
            # comes back as text or with a decimal point.
            with self.cond:
                key = msg.get("id")
                exact = isinstance(key, str) or (isinstance(key, int) and not isinstance(key, bool))
                if exact and key in self.waiting:
                    self.waiting[key] = msg
                    self.cond.notify_all()
                    return
                sent = None
                if isinstance(key, str) and key.isdigit():
                    sent = int(key)
                elif isinstance(key, float) and key.is_integer():
                    sent = int(key)
                if sent is not None and sent in self.waiting:
                    problem("the server answered request %d with the id %s; Kivali matches the id exactly as it sent it (%d), never matches this answer, and the call fails after %d seconds" % (sent, json.dumps(key), sent, CALL_SECONDS))
                    self.flagged = True
                    self.waiting.pop(sent)
                    self.cond.notify_all()
            return
        if method == "kivali/log":
            params = msg.get("params") or {}
            level = params.get("level") if params.get("level") in ("info", "warning", "error", "debug") else "info"
            say("  log %s: %s" % (level, self.scrub(str(params.get("message", "")))[:8192]))
            if "id" in msg:
                self.write({"jsonrpc": "2.0", "id": msg["id"], "result": {}})
            return
        if "id" not in msg:
            if method not in self.dropped:
                self.dropped.add(method)
                note("the server sent the notification %s; Kivali passes on only kivali/log and drops it" % method)
            return
        if method == "kivali/credential":
            if not self.fields:
                note("the server asked for the connection's values; with no connection Kivali answers -32001 no connection")
                self.write({"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32001, "message": "no connection"}})
                return
            note("the server asked for the connection's values")
            self.write({"jsonrpc": "2.0", "id": msg["id"], "result": {"env": server_env(self.manifest, self.fields), "fields": self.fields}})
            return
        note("the server asked Kivali for %s; Kivali answers that it is a method that does not exist" % method)
        self.write({"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32601, "message": "method not found: " + method}})

    def request(self, method, params, seconds, rid=None):
        """The response to one request, or None when there is none in
        time. rid is the id, a whole number unless given."""
        with self.cond:
            if rid is None:
                self.next_id += 1
                rid = self.next_id
            self.waiting[rid] = None
            self.flagged = False
        self.write({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
        deadline = time.monotonic() + seconds
        start = time.monotonic()
        with self.cond:
            while rid in self.waiting and self.waiting[rid] is None and not self.stopped:
                left = deadline - time.monotonic()
                if left <= 0:
                    break
                self.cond.wait(left)
            resp = self.waiting.pop(rid, None)
        return resp, time.monotonic() - start

    def missing(self, text):
        """Say that an answer did not come, unless its cause is said already."""
        if not self.bad_line and not self.flagged:
            problem(text)

    def stop(self):
        try:
            self.proc.stdin.close()
        except OSError:
            pass
        try:
            self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            note("the server did not exit within 5 seconds of its input closing; Kivali kills it then")
            self.proc.kill()
            self.proc.wait()
        shutil.rmtree(self.home, ignore_errors=True)


def server_env(manifest, fields):
    env = ((manifest.get("server") or {}).get("mcp_config") or {}).get("env") or {}

    def put(m):
        key = m.group(1)
        return fields.get(key[len("user_config."):], "") if key.startswith("user_config.") else ""

    return {k: PLACEHOLDER.sub(put, str(v)) for k, v in env.items()}


def local_ports(env):
    ports = set()
    for v in env.values():
        try:
            u = urllib.parse.urlsplit(v)
            if u.scheme in ("http", "https") and u.hostname in ("127.0.0.1", "localhost") and u.port:
                ports.add(u.port)
        except ValueError:
            pass
    return sorted(ports)


def start_stub(command, ports):
    logf = tempfile.TemporaryFile()
    env = {k: v for k, v in os.environ.items() if k.lower() not in PROXY_VARS}
    proc = subprocess.Popen(command, shell=True, stdin=subprocess.DEVNULL, stdout=logf, stderr=subprocess.STDOUT, env=env, start_new_session=True)
    deadline = time.monotonic() + 10
    waiting = list(ports)
    while waiting and time.monotonic() < deadline:
        if proc.poll() is not None:
            break
        try:
            socket.create_connection(("127.0.0.1", waiting[0]), timeout=0.2).close()
            waiting.pop(0)
        except OSError:
            time.sleep(0.05)
    if proc.poll() is not None:
        stop_stub(proc, logf)
        fail("the stub exited (status %s) before it was ready" % proc.returncode)
    if waiting:
        stop_stub(proc, logf)
        fail("the stub did not accept connections on port %s within 10 seconds" % waiting[0])
    return proc, logf


def stop_stub(proc, logf):
    if proc.poll() is None:
        try:
            os.killpg(proc.pid, signal.SIGTERM)
            proc.wait(timeout=2)
        except (OSError, subprocess.TimeoutExpired):
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except OSError:
                pass
            proc.wait()
    logf.seek(0)
    lines = logf.read().decode("utf-8", "replace").splitlines()
    logf.close()
    if lines:
        say("The stub's output, its last %d lines:" % min(len(lines), 20))
        for line in lines[-20:]:
            say("  " + line)


def answer_texts(display, resp):
    """The answer as Kivali passes it to an agent: the text blocks, else
    the structured content, and whether it is an error."""
    if resp is None:
        return None, True
    if "error" in resp:
        err = resp.get("error") or {}
        return ["The %s integration answered with an error: %s" % (display, err.get("message", ""))], True
    res = resp.get("result")
    if res is None:
        res = {}
    if not isinstance(res, dict):
        return ["The %s integration's answer is not one Kivali reads; tell the owner." % display], True
    texts, other = [], []
    for c in res.get("content") or []:
        if isinstance(c, dict) and c.get("type") == "text":
            texts.append(str(c.get("text", "")))
        else:
            t = (c.get("type") if isinstance(c, dict) else None) or "unnamed"
            if t not in other:
                other.append(t)
    if not texts and res.get("structuredContent") is not None:
        texts.append(json.dumps(res["structuredContent"]))
    if other:
        texts.append("The answer also held content Kivali does not pass on (%s)." % ", ".join(other))
    if not texts:
        texts = ["The %s integration's answer was empty." % display]
    return texts, bool(res.get("isError"))


def first_text(resp):
    res = (resp or {}).get("result") or {}
    for c in res.get("content") or []:
        if isinstance(c, dict) and c.get("type") == "text":
            return str(c.get("text", ""))
    return ""


def main():
    if any(a in ("-h", "--help") for a in sys.argv[1:]):
        print(__doc__.strip())
        return
    ap = argparse.ArgumentParser(prog="try.py", usage="python3 try.py [options] BUNDLE REQUEST... (--help says more)", add_help=False)
    ap.add_argument("--field", action="append", default=[])
    ap.add_argument("--env", action="append", default=[])
    ap.add_argument("--agent")
    ap.add_argument("--access", default="{}")
    ap.add_argument("--stub")
    ap.add_argument("bundle")
    ap.add_argument("requests", nargs="*")
    opts = ap.parse_intermixed_args()

    bundle = os.path.abspath(opts.bundle)
    try:
        with open(os.path.join(bundle, "manifest.json"), encoding="utf-8") as f:
            manifest = json.load(f)
    except (OSError, ValueError) as e:
        fail("read %s/manifest.json: %s" % (opts.bundle, e))
    if not isinstance(manifest, dict):
        fail("manifest.json is not an object")
    name = str(manifest.get("name") or "")
    display = str(manifest.get("display_name") or name)
    prefix = name.replace("-", "_") + "_"
    kivali = manifest.get("kivali") or {}
    user_config = manifest.get("user_config") or {}
    declared = [t.get("name") for t in manifest.get("tools") or [] if isinstance(t, dict)]

    fields = parse_pairs(opts.field, "--field")
    for k in fields:
        if k not in user_config:
            fail("--field %s: user_config has no field %s" % (k, k))
    sensitive = {k for k, u in user_config.items() if isinstance(u, dict) and u.get("sensitive") is True}
    try:
        access = json.loads(opts.access)
    except ValueError as e:
        fail("--access is not JSON: %s" % e)
    if not isinstance(access, dict):
        fail("--access is an object of field values")
    requests = parse_requests(opts.requests)

    extra = parse_pairs(opts.env, "--env")
    env = {
        "PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"),
        # Keeps __pycache__ out of the bundle's directory; the pod's
        # bundle is read-only, so it writes none there either.
        "PYTHONDONTWRITEBYTECODE": "1",
    }
    env.update(server_env(manifest, fields))
    env.update(extra)
    for k in list(env):
        if k.lower() in PROXY_VARS:
            del env[k]

    if opts.agent:
        check_access(manifest, access)
    stub = None
    if opts.stub:
        stub = start_stub(opts.stub, local_ports(extra))

    who = "as %s with access %s" % (opts.agent, json.dumps(access)) if opts.agent else "as the owner"
    conn = "connection fields: " + ", ".join(sorted(fields)) if fields else "no connection, every connection field empty"
    say("Starting the %s server in %s (%s; no proxy, so it reaches this machine only)." % (display, opts.bundle, conn))
    srv = Server(manifest, bundle, env, fields, sensitive, display)
    try:
        run(srv, manifest, display, prefix, kivali, declared, requests, opts.agent, access, who)
    finally:
        srv.stop()
        if srv.proc.returncode not in (0, None) and not srv.bad_line:
            say("The server exited with status %s." % srv.proc.returncode)
        if stub:
            stop_stub(*stub)
    say()
    if problems:
        say("%d problem%s." % (len(problems), "" if len(problems) == 1 else "s"))
        sys.exit(1)
    say("No problems.")


def run(srv, manifest, display, prefix, kivali, declared, requests, agent, access, who):
    params = {"protocolVersion": PROTOCOL_VERSION, "capabilities": {}, "clientInfo": {"name": "kivali", "version": "try"}}
    init, _ = srv.request("initialize", params, HANDSHAKE_SECONDS, rid="kivali-shim-1")
    if init is None:
        srv.missing("the server did not answer initialize within %d seconds, or exited first; Kivali stops it and the pod starts again" % HANDSHAKE_SECONDS)
        return
    if "error" in init:
        problem("the server refused initialize: %s" % (init.get("error") or {}).get("message", ""))
        return
    srv.write({"jsonrpc": "2.0", "method": "notifications/initialized"})

    # Kivali's own list, at every start: every declared action is on it.
    owner_list, _ = srv.request("tools/list", {}, OTHER_SECONDS)
    owner_raw = listed(owner_list)
    if owner_raw is None:
        srv.missing("the server gave no list of actions to Kivali's own tools/list within %d seconds" % OTHER_SECONDS)
        return
    for t in declared:
        if t not in owner_raw:
            problem("the server's list for the owner lacks %s, which the manifest declares; integration_validate refuses it" % t)
    owner_tools = kept(owner_raw, declared, "the owner")

    meta = {"_meta": {"kivali": {"agent": agent, "access": access}}} if agent else {}
    agent_tools = None

    def agent_list():
        nonlocal agent_tools
        if agent_tools is None:
            resp, _ = srv.request("tools/list", dict(meta), OTHER_SECONDS)
            raw = listed(resp)
            if raw is None:
                srv.missing("the server did not answer the agent's tools/list within %d seconds; Kivali leaves the integration out of the agent's actions" % OTHER_SECONDS)
                raw = {}
            agent_tools = kept(raw, declared, "the agent")
        return agent_tools

    for kind, arg, args in requests:
        say()
        if srv.stopped:
            note("the server has stopped; the requests left are not sent")
            return
        if kind == "list":
            tools = agent_list() if agent else owner_tools
            say("list, %s:" % who)
            show_list(tools, prefix, display, bool(agent))
        elif kind == "call":
            say("call %s %s, %s:" % (arg, json.dumps(args), who))
            if arg not in declared:
                say("  Kivali refuses it: No integration has an action named %s%s." % (prefix, arg))
                continue
            if agent and arg not in agent_list():
                say("  Kivali refuses it before it reaches the server: Your access to the %s integration does not include %s. Ask the owner if your work needs it." % (display, arg))
                continue
            resp, took = srv.request("tools/call", dict({"name": arg, "arguments": args}, **meta), CALL_SECONDS)
            show_answer(srv, display, resp, took)
        elif kind == "verify":
            tool = kivali.get("verify")
            say("verify, as Test connection calls it:")
            if not tool:
                problem("the manifest names no kivali.verify, so the owner has no Test connection")
                continue
            resp, took = srv.request("tools/call", {"name": tool, "arguments": {}}, CALL_SECONDS)
            if resp is None:
                srv.missing("%s did not answer within %d seconds; the owner reads that the connection failed" % (tool, CALL_SECONDS))
                continue
            texts, is_error = answer_texts(display, resp)
            message = (first_text(resp) if "error" not in resp else texts[0]).encode("utf-8")[:VERIFY_MESSAGE].decode("utf-8", "ignore")
            say("  %s, in %.1f s. The owner reads: %s" % ("Not verified" if is_error else "Verified", took, srv.scrub(message) or "(nothing)"))
        elif kind == "choices":
            tool = ((kivali.get("access") or {}).get("choices") or {}).get(arg)
            say("choices %s, as the access form calls it:" % arg)
            if not tool:
                problem("kivali.access.choices names no action for %s" % arg)
                continue
            resp, took = srv.request("tools/call", {"name": tool, "arguments": {}}, CALL_SECONDS)
            if resp is None:
                srv.missing("%s did not answer within %d seconds; the owner's form says it could not list the options" % (tool, CALL_SECONDS))
                continue
            texts, is_error = answer_texts(display, resp)
            if is_error:
                problem("%s answered an error, so the owner's form cannot list the options: %s" % (tool, srv.scrub(texts[0])))
                continue
            options = parse_choices(first_text(resp))
            if options is None:
                problem("%s's first text is not a JSON array of strings, or an object whose options is one; the owner's form says the options are not a list of names" % tool)
                continue
            say("  The form offers, in %.1f s: %s" % (took, ", ".join(options) if options else "(no options)"))


def listed(resp):
    """The tools of a tools/list answer by name, or None."""
    if resp is None or not isinstance(resp.get("result"), dict):
        return None
    if resp["result"].get("nextCursor"):
        note("the list has a nextCursor; Kivali reads only the first page, so put every action on it")
    out = {}
    for t in resp["result"].get("tools") or []:
        if isinstance(t, dict) and isinstance(t.get("name"), str):
            out[t["name"]] = t
    return out


def kept(tools, declared, whose):
    """The actions Kivali keeps from a list: those the manifest declares,
    with an object's input schema (none reads as one). It says once what
    it leaves out; a call to an action left out is refused."""
    out = {}
    for name, t in tools.items():
        if name not in declared:
            note("%s's list has %s, which the manifest does not declare; Kivali leaves it out" % (whose, name))
            continue
        schema = t.get("inputSchema")
        if schema is None:
            schema = {"type": "object"}
        if not (isinstance(schema, dict) and schema.get("type") == "object"):
            problem("%s's list gives %s an inputSchema that is not an object's; Kivali leaves the action out, and says so only in its own log" % (whose, name))
            continue
        out[name] = dict(t, inputSchema=schema)
    return out


def show_list(tools, prefix, display, for_agent):
    suffix = "Through the %s integration; the owner decides what you may do with it." % display
    for name, t in tools.items():
        desc = str(t.get("description") or "").strip()
        say("  %s%s" % (prefix if for_agent else "", name))
        say("    %s" % ((desc + " " if desc else "") + suffix if for_agent else desc or "(no description)"))
        say("    input: %s" % json.dumps(t["inputSchema"], separators=(",", ":")))
    if not tools:
        say("  (no actions)" + (": with this access the agent gets nothing from the integration" if for_agent else ""))


def show_answer(srv, display, resp, took):
    if resp is None:
        srv.missing("no answer within %d seconds; the agent reads that the integration did not answer" % CALL_SECONDS)
        return
    texts, is_error = answer_texts(display, resp)
    scrubbed = [srv.scrub(t) for t in texts]
    if scrubbed != texts:
        note("the answer quotes a secret of the connection; the agent reads [secret] in its place")
    say("  %s, in %.1f s. The agent reads:" % ("An error" if is_error else "An answer", took))
    for t in scrubbed:
        for line in t.splitlines() or [""]:
            say("    " + line)


def parse_choices(text):
    try:
        v = json.loads(text)
    except ValueError:
        return None
    if v is None:
        return []
    if isinstance(v, dict):
        v = v.get("options")
    if isinstance(v, list) and all(isinstance(x, str) for x in v):
        return v
    return None


if __name__ == "__main__":
    main()
