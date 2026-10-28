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

and, for a bundle whose connection's method is setup:
  setup [FLOW]         the setup flow (setup, or the flow a requirement's Fix
                       names), kivali/setup call after call, as Kivali runs it
  requirements         kivali/requirements, as the integration's page reads it
  uninstall            kivali/uninstall, as Uninstall sends it

options:
  --field KEY=VALUE    a connection field's value, by user_config key or, for a
                       setup connection, by a key setup keeps; repeat it. With
                       none, the server starts as it does before the owner
                       installs it: every field empty.
  --answer KEY=VALUE   what the owner enters in a Form's field. Give a key again
                       to answer the next Form that asks for it: each Form takes
                       the next answer, and the last is used for any Form after.
                       A field with no --answer is asked for on the terminal, or
                       left blank when there is none.
  --return QUERY       the query the service sends the browser back with after
                       a Go step, such as 'code=abc' or 'error=access_denied';
                       repeat it, one per Go, in order. With none left, it is
                       asked for on the terminal. Without a state, the state
                       Kivali gave the step's callback is added.
  --checks N           how many times a Wait is checked before setup stops
                       (default 5).
  --every SECONDS      the wait between a Wait's checks (default 3, as Kivali's).
  --callback URL       the callback address Kivali gives each call (default
                       http://127.0.0.1:8080/connect/callback).
  --address URL        another address the team signs in at, such as
                       https://team.example/connect/callback for its
                       external address; repeat it. Each call's addresses
                       are --callback's, then these.
  --team NAME          the team's name each call carries, for naming what
                       setup makes on the service (default Kivali; empty for
                       a team with none).
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
Kivali; the exit status is 1 when there is one. A setup step is held to
the manifest's kivali.connection.setup as Kivali holds it: a field, a
host or a key it does not declare fails the flow, and is a problem
here. What a step keeps becomes the connection's, and the server is
started again with it, as in the pod. An install that fails has Kivali
send kivali/uninstall, with what it kept, then forget the connection. A
flow that stops before it is done, for want of an answer, is one the
owner left, and waits in Kivali with what it kept: a setup after it in
the same run takes that connection over, kept values and all, as a new
start does in Kivali, and an install still left at the end of the run
is undone as Kivali undoes it once the flow is forgotten.
"""

import argparse
import getpass
import json
import os
import re
import secrets as tokens
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
# Kivali's deadlines for a setup bundle's requests: a setup step, the
# requirements (which the owner's page waits on) and an uninstall.
SETUP_SECONDS = 300
REQUIREMENTS_SECONDS = 5
UNINSTALL_SECONDS = 60
# How many calls of one flow try.py makes before it stops.
SETUP_CALLS = 40
DEFAULT_CALLBACK = "http://127.0.0.1:8080/connect/callback"
# Kivali's own requirement rows, whose ids an integration's row may not
# take; and the rest of what core holds a row to.
CORE_REQUIREMENTS = {"version", "connection", "sign_in", "connection_test", "running", "checks"}
REQUIREMENT_ID = re.compile(r"^[A-Za-z0-9_-]{1,64}$")
REQUIREMENT_TITLE = 80
REQUIREMENT_WORDS = 500
REQUIREMENT_ROWS = 20
# The words of a setup flow's failure, its steps and its progress are
# cut to this many characters after their secrets are replaced, and a
# Wait's code, shown as given, to CODE_CHARS, and each of a Form's
# values to copy, shown as given, to COPY_CHARS. A step naming an
# address longer than URL_CHARS, giving more than COPY_VALUES values to
# copy, keeping and carrying more than CARRY_BYTES together, or posting
# more than CARRY_BYTES fails the flow.
FLOW_WORDS = 200
CODE_CHARS = 64
URL_CHARS = 4096
COPY_VALUES = 10
COPY_CHARS = 1024
CARRY_BYTES = 64 << 10
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


def parse_answers(pairs):
    """--answer's values by key, in the order given."""
    out = {}
    for k, v in parse_pairs_list(pairs, "--answer"):
        out.setdefault(k, []).append(v)
    return out


def parse_pairs_list(pairs, what):
    out = []
    for p in pairs:
        k, sep, v = p.partition("=")
        if not sep or not k:
            fail("%s %r is not NAME=VALUE" % (what, p))
        out.append((k, v))
    return out


def parse_pairs(pairs, what):
    """NAME=VALUE pairs by name, the last given winning."""
    return dict(parse_pairs_list(pairs, what))


REQUESTS = ("list", "call", "verify", "choices", "setup", "requirements", "uninstall")


def parse_requests(words):
    reqs, i = [], 0
    while i < len(words):
        w = words[i]
        if w in ("list", "verify", "requirements", "uninstall"):
            reqs.append((w, None, None))
            i += 1
        elif w == "setup":
            flow = "setup"
            if i + 1 < len(words) and words[i + 1] not in REQUESTS:
                flow = words[i + 1]
                i += 1
            reqs.append((w, flow, None))
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
            fail("%r is not a request: list, call ACTION [JSON], verify, choices FIELD, setup [FLOW], requirements or uninstall" % w)
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
    def __init__(self, manifest, bundle, env, fields, secret_values, display):
        self.manifest, self.fields, self.display = manifest, fields, display
        # What Kivali replaces with [secret]: shared with the run, which
        # adds what a setup flow keeps or the owner enters.
        self.secrets = secret_values
        # The progress token of the kivali/setup request in flight, if any.
        self.progress_token = None
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
        for v in sorted((v for v in self.secrets if len(v) >= SCRUB_MIN), key=len, reverse=True):
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
        if method == "notifications/progress" and "id" not in msg:
            self.on_progress(msg.get("params"))
            return
        if "id" not in msg:
            if method not in self.dropped:
                self.dropped.add(method)
                note("the server sent the notification %s; Kivali passes on only kivali/log and a setup step's notifications/progress, and drops it" % method)
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

    def on_progress(self, params):
        """A setup step's stage, as the flow's page shows it."""
        params = params if isinstance(params, dict) else {}
        token = params.get("progressToken")
        if self.progress_token is None or token != self.progress_token:
            note("the server sent a progress notification with the progressToken %s, which no kivali/setup in flight carries; Kivali drops it" % json.dumps(token))
            return
        done, total = params.get("progress"), params.get("total", 0)
        number = lambda v: isinstance(v, (int, float)) and not isinstance(v, bool)
        if not number(done) or not (number(total) or total is None):
            note("the server sent a progress notification whose progress or total is not a number; Kivali drops it")
            return
        words = params.get("message", "")
        if not isinstance(words, str):
            note("the server sent a progress notification whose message is not text; Kivali drops it")
            return
        total = max(int(total or 0), 0)
        done = min(max(int(done), 0), total)
        count = " (%d of %d)" % (done, total) if total else ""
        say("  progress: %s%s" % (clip(self.scrub(words), FLOW_WORDS), count))

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
    ap.add_argument("--answer", action="append", default=[])
    ap.add_argument("--return", dest="returns", action="append", default=[])
    ap.add_argument("--checks", type=int, default=5)
    ap.add_argument("--every", type=float, default=3)
    ap.add_argument("--callback", default=DEFAULT_CALLBACK)
    ap.add_argument("--team", default="Kivali")
    ap.add_argument("--address", dest="addresses", action="append", default=[])
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

    setup = setup_section(manifest)
    keeps = setup.get("keeps") or {}
    fields = parse_pairs(opts.field, "--field")
    for k in fields:
        if k not in user_config and k not in keeps:
            fail("--field %s: user_config has no field %s%s" % (k, k, ", and setup keeps no such key" if setup else ""))
    # A kept value is a secret, as a sensitive field is, unless its keep
    # is declared "secret": false.
    sensitive = {k for k, u in user_config.items() if isinstance(u, dict) and u.get("sensitive") is True} | {k for k in keeps if keep_secret(keeps, k)}
    secret_values = [v for k, v in fields.items() if k in sensitive]
    try:
        access = json.loads(opts.access)
    except ValueError as e:
        fail("--access is not JSON: %s" % e)
    if not isinstance(access, dict):
        fail("--access is an object of field values")
    requests = parse_requests(opts.requests)

    extra = parse_pairs(opts.env, "--env")
    # A run's Forms and Gos take the answers and returns in order, flow
    # after flow.
    opts.answers = parse_answers(opts.answer)

    if opts.agent:
        check_access(manifest, access)
    stub = None
    if opts.stub:
        stub = start_stub(opts.stub, local_ports(extra))

    who = "as %s with access %s" % (opts.agent, json.dumps(access)) if opts.agent else "as the owner"
    conn = "connection fields: " + ", ".join(sorted(fields)) if fields else "no connection, every connection field empty"
    say("Starting the %s server in %s (%s; no proxy, so it reaches this machine only)." % (display, opts.bundle, conn))
    pod = Pod(manifest, bundle, extra, fields, secret_values, display)
    try:
        run(pod, manifest, display, prefix, kivali, declared, requests, opts.agent, access, who, opts)
    finally:
        pod.stop()
        if stub:
            stop_stub(*stub)
    say()
    if problems:
        say("%d problem%s." % (len(problems), "" if len(problems) == 1 else "s"))
        sys.exit(1)
    say("No problems.")


class Pod:
    """The server as the pod runs it: started with the connection's
    values in its env, through the shim's handshake, and started again
    when they change, as a Keep changes them."""

    def __init__(self, manifest, bundle, extra, fields, secret_values, display):
        self.manifest, self.bundle, self.extra, self.display = manifest, bundle, extra, display
        self.fields, self.secrets = fields, secret_values
        self.srv = None
        self.starts = 0
        # left_install: the connection's values are what an install left
        # partway kept, which a setup started again takes over.
        self.left_install = False

    def env(self):
        env = {
            "PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"),
            # Keeps __pycache__ out of the bundle's directory; the pod's
            # bundle is read-only, so it writes none there either.
            "PYTHONDONTWRITEBYTECODE": "1",
        }
        env.update(server_env(self.manifest, self.fields))
        env.update(self.extra)
        for k in list(env):
            if k.lower() in PROXY_VARS:
                del env[k]
        return env

    def start(self):
        """Start the server and run the shim's handshake: True once it
        answered initialize."""
        self.starts += 1
        self.srv = Server(self.manifest, self.bundle, self.env(), self.fields, self.secrets, self.display)
        params = {"protocolVersion": PROTOCOL_VERSION, "capabilities": {}, "clientInfo": {"name": "kivali", "version": "try"}}
        init, _ = self.srv.request("initialize", params, HANDSHAKE_SECONDS, rid="kivali-shim-%d" % self.starts)
        if init is None:
            self.srv.missing("the server did not answer initialize within %d seconds, or exited first; Kivali stops it and the pod starts again" % HANDSHAKE_SECONDS)
            return False
        if "error" in init:
            problem("the server refused initialize: %s" % (init.get("error") or {}).get("message", ""))
            return False
        self.srv.write({"jsonrpc": "2.0", "method": "notifications/initialized"})
        return True

    def restart(self):
        """Stop the server and start it again with the values as they
        are now, as the shim does when the connection changes."""
        self.stop()
        return self.start()

    def stop(self):
        if self.srv is None:
            return
        self.srv.stop()
        if self.srv.proc.returncode not in (0, None) and not self.srv.bad_line:
            say("The server exited with status %s." % self.srv.proc.returncode)


def run(pod, manifest, display, prefix, kivali, declared, requests, agent, access, who, opts):
    if not pod.start():
        return
    srv = pod.srv

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
        srv = pod.srv
        if srv.stopped:
            note("the server has stopped; the requests left are not sent")
            return
        if kind in ("setup", "requirements", "uninstall") and connection_method(manifest) != "setup":
            problem("Kivali sends kivali/%s only to a bundle whose kivali.connection.method is setup; this one's is %s" % (kind, connection_method(manifest) or "missing"))
            continue
        if kind == "setup":
            run_setup(pod, display, arg, opts)
            continue
        if kind == "requirements":
            run_requirements(pod, display)
            continue
        if kind == "uninstall":
            run_uninstall(pod, display)
            continue
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
    if pod.left_install and pod.fields and not pod.srv.stopped:
        say()
        say("The run ends with a setup left partway:")
        undo_install(pod, display, "Kivali forgets that flow ten minutes after its last change or check once nobody watches it")
        pod.left_install = False


def connection_method(manifest):
    return str(((manifest.get("kivali") or {}).get("connection") or {}).get("method") or "")


def setup_section(manifest):
    """kivali.connection.setup, {} for none."""
    s = ((manifest.get("kivali") or {}).get("connection") or {}).get("setup")
    return s if isinstance(s, dict) else {}


def keep_secret(keeps, key):
    """Whether a kept value of key is replaced with [secret], as Kivali
    has it: a keep is its words (secret), or an object of its words and
    "secret", secret unless that is false."""
    k = keeps.get(key)
    return not (isinstance(k, dict) and k.get("secret") is False)


def clip(s, n):
    return s if len(s) <= n else s[:n]


def secret_return_key(k):
    """Whether Kivali replaces a returned value of this key with [secret]."""
    k = k.lower()
    return k in ("code", "state") or any(w in k for w in ("token", "secret", "password", "key"))


# What a kivali/setup result may hold, as Kivali reads it: a key outside
# these, or a value of another type, and Kivali cannot read the step.
STEP_KEYS = {"form", "go", "wait", "done", "keep", "state"}
KIND_KEYS = {
    "form": {"words": "text", "fields": "texts", "url": "text", "page": "text", "copy": "texts"},
    "go": {"words": "text", "url": "text", "post": "post", "no_return": "switch", "stateless": "switch"},
    "wait": {"words": "text", "url": "text", "page": "text", "code": "text"},
}


def read_step(result):
    """The step of a kivali/setup result, and why Kivali cannot read it
    ("" when it can). A result with no step is a step of nothing."""
    if not isinstance(result, dict):
        return None, "the result is not an object"
    for k in result:
        if k not in ("step", "_meta"):
            return None, 'unknown field "%s"' % k
    step = result.get("step")
    if step is None:
        return {}, ""
    if not isinstance(step, dict):
        return None, "step is not an object"
    for k, v in step.items():
        if k not in STEP_KEYS:
            return None, 'step: unknown field "%s"' % k
        if v is None:
            continue
        if k in KIND_KEYS:
            if not isinstance(v, dict):
                return None, "step: %s is not an object" % k
            for kk, vv in v.items():
                want = KIND_KEYS[k].get(kk)
                if want is None:
                    return None, 'step: unknown field "%s"' % kk
                if vv is None:
                    continue
                ok = {
                    "text": isinstance(vv, str),
                    "switch": isinstance(vv, bool),
                    "texts": isinstance(vv, list) and all(isinstance(x, str) for x in vv),
                    "post": isinstance(vv, dict) and all(isinstance(x, str) or (isinstance(x, list) and all(isinstance(y, str) for y in x)) for x in vv.values()),
                }[want]
                if not ok:
                    return None, "step: %s.%s is not %s" % (k, kk, {"text": "text", "switch": "true or false", "texts": "a list of text", "post": "an object of strings or lists of strings"}[want])
        elif k == "done":
            if not isinstance(v, bool):
                return None, "step: done is not true or false"
        elif not (isinstance(v, dict) and all(x is None or isinstance(x, str) for x in v.values())):
            return None, "step: %s is not an object of text" % k
        else:
            # A null reads as empty, as Kivali reads it: a kept empty
            # value forgets the key.
            step[k] = {kk: vv or "" for kk, vv in v.items()}
    return {k: v for k, v in step.items() if v is not None}, ""


def url_host(raw):
    try:
        u = urllib.parse.urlsplit(raw)
    except ValueError:
        return ""
    return u.netloc.rpartition("@")[2].lower()


def url_words(raw):
    return url_host(raw) or "an address that is not one"


def url_allowed(raw, hosts):
    """Whether a step may name raw: https (plain http on loopback alone),
    no user info, on a declared host, with no port."""
    try:
        u = urllib.parse.urlsplit(raw)
        port = u.port
    except ValueError:
        return False
    if not u.netloc or "@" in u.netloc:
        return False
    if u.scheme == "http":
        if u.hostname not in ("localhost", "127.0.0.1", "::1"):
            return False
    elif u.scheme != "https":
        return False
    host = url_host(raw)
    return host in hosts or (port is None and (u.hostname or "") in hosts)


def check_step(step, display, setup):
    """Why Kivali fails the flow on step, in the words the owner reads,
    or "" when it keeps to the manifest's declarations."""
    kinds = [k for k in ("form", "go", "wait") if k in step] + (["done"] if step.get("done") else [])
    if not kinds:
        return "%s's setup gave no next step" % display
    if len(kinds) > 1:
        return "%s's setup gave more than one next step" % display
    hosts = setup.get("hosts") or []
    fields = setup.get("fields") or {}
    keeps = setup.get("keeps") or {}
    for k in sorted(step.get("keep") or {}):
        if k not in keeps:
            return "%s's setup tried to keep %s, which its manifest does not declare" % (display, k)
    size = lambda m: sum(len(k.encode()) + len(v.encode()) for k, v in (m or {}).items())
    if size(step.get("keep")) + size(step.get("state")) > CARRY_BYTES:
        return "%s's setup gave more to keep and carry than Kivali holds for a step (%d KiB)" % (display, CARRY_BYTES >> 10)
    for kind in ("form", "go", "wait"):
        if kind in step and len((step[kind].get("url") or "").encode()) > URL_CHARS:
            return "%s's setup named an address longer than %d characters" % (display, URL_CHARS)
    post = (step.get("go") or {}).get("post") or {}
    if sum(len(k.encode()) + sum(len(x.encode()) for x in ([v] if isinstance(v, str) else v)) for k, v in post.items()) > CARRY_BYTES:
        return "%s's setup gave a form to post larger than Kivali sends (%d KiB)" % (display, CARRY_BYTES >> 10)
    if "form" in step:
        for k in step["form"].get("fields") or []:
            if k not in fields:
                return "%s's setup asked for %s, which its manifest does not declare" % (display, k)
        if len(step["form"].get("copy") or []) > COPY_VALUES:
            return "%s's setup gave more than %d values to copy" % (display, COPY_VALUES)
        if step["form"].get("url") and not url_allowed(step["form"]["url"], hosts):
            return "%s's setup named a page on %s, which its manifest does not declare" % (display, url_words(step["form"]["url"]))
    if "go" in step and not url_allowed(step["go"].get("url", ""), hosts):
        return "%s's setup tried to send your browser to %s, which its manifest does not declare" % (display, url_words(step["go"].get("url", "")))
    if "wait" in step and step["wait"].get("url") and not url_allowed(step["wait"]["url"], hosts):
        return "%s's setup named a page on %s, which its manifest does not declare" % (display, url_words(step["wait"]["url"]))
    return ""


def field_title(key, field):
    title = field.get("title") if isinstance(field, dict) else None
    return title or key.replace("_", " ").capitalize()


def ask_owner(key, field, answers):
    """What the owner enters in a Form's field: the next --answer for
    it (the last one again once they are used), else the terminal, else
    blank."""
    if answers.get(key):
        given = answers[key]
        return (given.pop(0) if len(given) > 1 else given[0]).strip()
    words = "%s (%s): " % (field_title(key, field), field.get("words", ""))
    if sys.stdin.isatty():
        if field.get("secret") or field.get("setup_only"):
            return getpass.getpass("  " + words).strip()
        return input("  " + words).strip()
    note("no --answer for %s, so it is left blank, as the page sends a field left blank" % key)
    return ""


def ask_return():
    if not sys.stdin.isatty():
        return None
    return input("  The query the service sent the browser back with (code=...&state=...): ").strip()


def run_setup(pod, display, flow, opts):
    """The setup flow, as Kivali's runner calls it (setup_calls). An
    install (a setup flow started with no connection, or over the one an
    install left partway made) keeps nothing unless it is done. One that
    fails has Kivali undo it (undo_install). One that stops for want of
    an answer, as when the owner leaves it, waits in Kivali with what it
    kept: a setup after it in the same run takes that connection over,
    kept values and all, as a new start in Kivali does, and at the end
    of the run it is undone as Kivali undoes it once forgotten."""
    install = flow == "setup" and (not pod.fields or pod.left_install)
    if pod.left_install and install:
        say("  Kivali ends the setup left partway, and this one takes over the connection it made (%s): what it kept stays." % ", ".join(sorted(pod.fields)))
    pod.left_install = False
    end = setup_calls(pod, display, flow, opts)
    if not install or end == "done" or not pod.fields:
        return
    if end == "left":
        pod.left_install = True
        say("  The setup waits partway, as when the owner leaves it. Kivali keeps what it kept (%s) while the flow lives: a setup started again takes it over, and once the flow is forgotten, Kivali undoes it." % ", ".join(sorted(pod.fields)))
        return
    undo_install(pod, display, "The setup did not finish")


def undo_install(pod, display, why):
    """What Kivali does with the connection an install made that ended
    without being done: it asks kivali/uninstall to undo what the install
    made on the service, with what was kept so far, best effort (an
    error or no answer is logged, nothing more), then forgets the
    connection and starts the server again without it."""
    say("  %s: Kivali asks kivali/uninstall to undo on the service what it made, with what it kept (%s), then forgets the connection and starts the server again without it." % (why, ", ".join(sorted(pod.fields))))
    srv = pod.srv
    if not srv.stopped and srv.proc.poll() is None:
        resp, took = srv.request("kivali/uninstall", {}, UNINSTALL_SECONDS)
        if resp is None:
            note("kivali/uninstall did not answer within %d seconds; Kivali logs it and forgets the connection all the same" % UNINSTALL_SECONDS)
        elif "error" in resp:
            err = resp.get("error") or {}
            words = (err.get("message") if isinstance(err, dict) else "") or "%s failed and gave no reason" % display
            say("  kivali/uninstall answered an error, in %.1f s, which Kivali logs, and it forgets the connection all the same: %s" % (took, clip(srv.scrub(words), FLOW_WORDS)))
        else:
            say("  kivali/uninstall answered, in %.1f s." % took)
    pod.fields.clear()
    pod.restart()


def setup_calls(pod, display, flow, opts):
    """The setup flow, call after call, as Kivali's runner calls it: an
    empty answer first, then what each step gave back, each step held to
    the manifest's declarations, until a step is done, a call answers an
    error, or the flow waits for what try.py was not given. "done" once
    a step is done, "failed" when Kivali fails the flow, "left" when it
    waits for the owner."""
    setup = setup_section(pod.manifest)
    fields = setup.get("fields") or {}
    answers = opts.answers
    returns = opts.returns
    say("setup%s, as Kivali runs it:" % ("" if flow == "setup" else " " + flow))
    last_form = None
    state, legs = {}, []
    answer = {"form": {}, "return": {}, "checked": False, "went": False}
    wait, checks = None, 0
    for leg in range(1, SETUP_CALLS + 1):
        if not answer["checked"] or not legs:
            legs.append(tokens.token_hex(16))
            pod.secrets.append(legs[-1])
        callback = opts.callback + "?" + urllib.parse.urlencode({"state": legs[-1]})
        progress = "kivali-progress-%d" % leg
        params = {"flow": flow, "answer": dict(answer, state=dict(state), leg=leg, callback=callback, addresses=addresses(opts), team=opts.team), "_meta": {"progressToken": progress}}
        what = "a check of the Wait" if answer["checked"] else "call %d" % leg
        say("  kivali/setup, %s:" % what)
        srv = pod.srv
        srv.progress_token = progress
        resp, took = srv.request("kivali/setup", params, SETUP_SECONDS)
        srv.progress_token = None
        if resp is None:
            srv.missing("kivali/setup did not answer within %d seconds; Kivali fails the flow: setup did not finish its step within 5 minutes; start it again" % SETUP_SECONDS)
            return "failed"
        if "error" in resp:
            err = resp.get("error") or {}
            words = err.get("message") if isinstance(err, dict) else ""
            say("  Setup fails, in %.1f s. The owner reads: %s" % (took, clip(srv.scrub(words or "%s failed and gave no reason" % display), FLOW_WORDS)))
            return "failed"
        step, why = read_step(resp.get("result"))
        if why:
            problem("Kivali fails the flow: %s's setup answered with a step Kivali cannot read: %s" % (display, why))
            return "failed"
        refused = check_step(step, display, setup)
        if refused:
            problem("Kivali fails the flow: " + clip(srv.scrub(refused), FLOW_WORDS))
            return "failed"
        keep = step.get("keep") or {}
        new_state = step.get("state") or {}
        # A check that finds the same Wait, State and nothing kept is no
        # change, as Kivali has it.
        same = wait is not None and "wait" in step and wait_key(step["wait"]) == wait_key(wait) and new_state == state and not keep
        keeps = setup.get("keeps") or {}
        for v in [v for k, v in keep.items() if keep_secret(keeps, k)] + list(new_state.values()):
            pod.secrets.append(v)
        state = new_state
        if same:
            say("  The same Wait, in %.1f s: Kivali keeps showing it." % took)
        else:
            show_step(srv, step, took)
        if keep:
            changed = {k: v for k, v in keep.items() if pod.fields.get(k, "") != v}
            for k, v in keep.items():
                if v == "":
                    pod.fields.pop(k, None)
                else:
                    pod.fields[k] = v
            say("  Kivali keeps %s in the connection's Secret%s." % (", ".join(sorted(keep)), ", and starts the server again with it, as the pod does" if changed else ""))
            if changed and not pod.restart():
                return "failed"
        if step.get("done"):
            say("  Done, in %.1f s: %s." % (took, "the connection is made, and the integration is installed" if flow == "setup" else "the Fix is made"))
            return "done"
        if "form" in step:
            wait = None
            entered = {}
            for k in step["form"].get("fields") or []:
                f = fields.get(k) or {}
                entered[k] = ask_owner(k, f, answers)
                if f.get("secret") or f.get("setup_only"):
                    pod.secrets.append(entered[k])
            this_form = (step["form"].get("words", ""), tuple(step["form"].get("fields") or []), entered)
            if this_form == last_form:
                note("setup asks for %s again after the same answer; give another --answer to see what follows" % ", ".join(this_form[1]))
                return "left"
            last_form = this_form
            answer = {"form": entered, "return": {}, "checked": False, "went": False}
            continue
        if "go" in step:
            wait = None
            go = step["go"]
            if go.get("no_return"):
                say("  Kivali sends the browser there, and calls setup again at once: nothing comes back.")
                answer = {"form": {}, "return": {}, "checked": False, "went": True}
                continue
            query = returns.pop(0) if returns else ask_return()
            if query is None:
                note("the flow waits for the browser to come back from %s; give --return with the query the service sends back" % url_host(go.get("url", "")))
                return "left"
            back = urllib.parse.parse_qs(query.lstrip("?"), keep_blank_values=True)
            if "state" not in back and not go.get("stateless"):
                back["state"] = [legs[-1]]
                note("the return has no state; the service sends the browser back to the callback the call was given, so try.py added its state")
            elif "state" in back and not any(v in legs for v in back["state"]):
                problem("the return's state is none the flow gave: Kivali finds no flow for it, the browser lands on the integrations list and setup is not called")
                return "left"
            for k, vs in back.items():
                if secret_return_key(k):
                    pod.secrets.extend(vs)
            answer = {"form": {}, "return": back, "checked": False, "went": False}
            continue
        if "wait" in step:
            if not same:
                wait, checks = step["wait"], 0
            if checks >= opts.checks:
                note("the Wait was checked %d time%s; Kivali goes on checking it every 3 seconds while the owner's page is open, up to 24 hours from the flow's start, and forgets it ten minutes after the last check once the page is closed" % (checks, "" if checks == 1 else "s"))
                return "left"
            checks += 1
            if opts.every > 0:
                time.sleep(opts.every)
            answer = {"form": {}, "return": {}, "checked": True, "went": False}
            continue
    note("setup was called %d times and is not done; try.py stops here" % SETUP_CALLS)
    return "left"


def addresses(opts):
    """Every address the team signs in at, as Kivali gives them: the
    --callback address first, then each --address, each once."""
    out = []
    for a in [opts.callback] + opts.addresses:
        if a and a not in out:
            out.append(a)
    return out


def wait_key(w):
    return (w.get("words", ""), w.get("url", ""), w.get("code", ""))


def show_step(srv, step, took):
    """A step as the owner's page shows it, its secrets replaced."""
    def scrub(text):
        return srv.scrub(text)

    def words(text):
        return clip(srv.scrub(text), FLOW_WORDS) or "(no words)"

    if "form" in step:
        f = step["form"]
        say("  A Form, in %.1f s: %s" % (took, words(f.get("words", ""))))
        say("    asks for: %s" % (", ".join(f.get("fields") or []) or "nothing: Continue alone goes on"))
        if f.get("url"):
            say("    page: %s" % scrub(f["url"]))
        for v in f.get("copy") or []:
            say("    to copy there: %s" % clip(v, COPY_CHARS))
    elif "go" in step:
        g = step["go"]
        how = "posts a form to" if g.get("post") is not None else "goes to"
        say("  A Go, in %.1f s: %s" % (took, words(g.get("words", ""))))
        say("    the browser %s %s%s%s" % (how, scrub(g.get("url", "")), ", and nothing comes back" if g.get("no_return") else "", "; the service sends no state back" if g.get("stateless") else ""))
        for k in sorted(g.get("post") or {}):
            v = g["post"][k]
            say("    post %s: %s" % (k, scrub(json.dumps(v) if isinstance(v, list) else v)))
    elif "wait" in step:
        w = step["wait"]
        say("  A Wait, in %.1f s: %s" % (took, words(w.get("words", ""))))
        if w.get("url"):
            say("    page: %s" % scrub(w["url"]))
        if w.get("code"):
            # Shown as given, as Kivali shows it: the step chose to show it.
            say("    code: %s" % clip(w["code"], CODE_CHARS))


def run_requirements(pod, display):
    """kivali/requirements, as the page reads it: the rows after
    Kivali's own, held to what Kivali keeps of a row."""
    srv = pod.srv
    say("requirements, as the integration's page reads them:")
    if not pod.fields:
        note("Kivali asks only once the integration is installed; with no connection, this is what integration_validate sees")
    resp, took = srv.request("kivali/requirements", {}, REQUIREMENTS_SECONDS)
    unmet = "  The page shows one unmet row: %s could not check its requirements: " % display
    if resp is None:
        srv.missing("kivali/requirements did not answer within %d seconds; the owner's page shows one unmet row: %s could not check its requirements: it did not answer within 5 seconds" % (REQUIREMENTS_SECONDS, display))
        return
    if "error" in resp:
        err = resp.get("error") or {}
        say(unmet + clip(srv.scrub((err.get("message") if isinstance(err, dict) else "") or "%s failed and gave no reason" % display), REQUIREMENT_WORDS))
        return
    res = resp.get("result")
    rows = res.get("requirements") if isinstance(res, dict) else None
    shape = "id", "title", "words", "met", "fix"
    if not isinstance(rows, list) or any(k not in ("requirements", "_meta") for k in res) or not all(
        isinstance(r, dict) and all(k in shape for k in r) and isinstance(r.get("id"), str) and isinstance(r.get("title"), str)
        and isinstance(r.get("words"), str) and isinstance(r.get("met"), bool) and isinstance(r.get("fix") or "", str)
        for r in rows
    ):
        problem('kivali/requirements answered rows Kivali cannot read: the answer is {"requirements": [...]}, each row {"id", "title", "words", "met"} and optionally "fix"; the page shows one unmet row in their place')
        return
    seen = set()
    for r in rows:
        rid = r["id"]
        if not REQUIREMENT_ID.match(rid) or rid in CORE_REQUIREMENTS or rid in seen:
            problem("the row %s is left out: an id is letters, digits, hyphens and underscores, 64 at most, not one of Kivali's (%s) and given once" % (json.dumps(rid), ", ".join(sorted(CORE_REQUIREMENTS))))
            continue
        if r.get("fix") and not REQUIREMENT_ID.match(r["fix"]):
            problem("the row %s is left out: its fix %s is not a flow's name, which is letters, digits, hyphens and underscores, 64 at most" % (json.dumps(rid), json.dumps(r["fix"])))
            continue
        if len(seen) == REQUIREMENT_ROWS:
            problem("the integration gave more than %d rows; Kivali shows the first %d and leaves the row %s and those after it out" % (REQUIREMENT_ROWS, REQUIREMENT_ROWS, json.dumps(rid)))
            break
        seen.add(rid)
        title, words = srv.scrub(r.get("title") or rid), srv.scrub(r.get("words", ""))
        if len(title) > REQUIREMENT_TITLE or len(words) > REQUIREMENT_WORDS:
            note("Kivali cuts a row's title to %d characters and its words to %d" % (REQUIREMENT_TITLE, REQUIREMENT_WORDS))
        say("  %s %s: %s" % ("met:" if r.get("met") else "not met:", clip(title, REQUIREMENT_TITLE), clip(words, REQUIREMENT_WORDS)))
        if r.get("fix") and not r.get("met"):
            say("    Fix runs the flow %s: try it with the request setup%s" % (r["fix"], "" if r["fix"] == "setup" else " " + r["fix"]))
    if not rows:
        say("  (no rows of its own)")
    say("  in %.1f s." % took)


def run_uninstall(pod, display):
    srv = pod.srv
    say("uninstall, as Uninstall sends it:")
    resp, took = srv.request("kivali/uninstall", {}, UNINSTALL_SECONDS)
    if resp is None:
        srv.missing("kivali/uninstall did not answer within %d seconds; Kivali refuses the uninstall and changes nothing" % UNINSTALL_SECONDS)
        return
    if "error" in resp:
        err = resp.get("error") or {}
        words = (err.get("message") if isinstance(err, dict) else "") or "%s failed and gave no reason" % display
        say("  Kivali refuses the uninstall, in %.1f s, and changes nothing. The owner reads: %s could not undo its installation: %s; nothing was changed" % (took, display, clip(srv.scrub(words), FLOW_WORDS)))
        return
    say("  Done, in %.1f s: Kivali then forgets the connection's Secret and every agent's access." % took)


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
