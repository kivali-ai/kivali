# The server smoke test's driver (smoke.go). It runs in the bundle's
# author's shell with the bundle directory as its working directory,
# starts the server the way an integration's pod starts it, and asks it
# what Kivali asks at every start: initialize, then tools/list, and for
# a bundle whose connection is made by its own setup flow,
# kivali/requirements with no connection. It prints one JSON line with
# what it found, and nothing else.
#
# argv[1] is the spec as JSON: command, args, env (the manifest's, with
# every connection field empty), timeout (seconds, for the whole check),
# protocol_version and requirements (whether to ask for them). The server's environment is PATH, a HOME of its
# own, LANG and the manifest's env, with no proxy variable: it has no
# network while it is checked.

import json
import os
import queue
import shutil
import subprocess
import sys
import tempfile
import threading
import time

PROXY_VARS = {"http_proxy", "https_proxy", "all_proxy", "no_proxy", "ftp_proxy"}
STDERR_KEEP = 4096


def main():
    spec = json.loads(sys.argv[1])
    deadline = time.monotonic() + float(spec["timeout"])
    out = {
        "started": False,
        "start_error": None,
        "initialize": None,
        "tools_list": None,
        "requirements": None,
        "not_json": None,
        "ended": False,
        "timed_out": False,
        "exit_code": None,
        "stderr": "",
    }
    home = tempfile.mkdtemp(prefix="kivali-check-")
    env = {
        "PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"),
        "HOME": home,
        "LANG": "C.UTF-8",
        "PYTHONDONTWRITEBYTECODE": "1",
    }
    env.update(spec.get("env") or {})
    for k in list(env):
        if k.lower() in PROXY_VARS:
            del env[k]
    try:
        proc = subprocess.Popen(
            [spec["command"]] + list(spec.get("args") or []),
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
    except OSError as e:
        out["start_error"] = str(e)
        print(json.dumps(out))
        shutil.rmtree(home, ignore_errors=True)
        return
    out["started"] = True

    lines = queue.Queue()

    def read_out():
        for raw in iter(proc.stdout.readline, b""):
            lines.put(raw)
        lines.put(None)

    errbuf = []

    def read_err():
        for raw in iter(proc.stderr.readline, b""):
            errbuf.append(raw)
            while sum(len(b) for b in errbuf) > STDERR_KEEP and len(errbuf) > 1:
                errbuf.pop(0)

    threading.Thread(target=read_out, daemon=True).start()
    err_thread = threading.Thread(target=read_err, daemon=True)
    err_thread.start()

    def send(msg):
        try:
            proc.stdin.write((json.dumps(msg) + "\n").encode())
            proc.stdin.flush()
            return True
        except (BrokenPipeError, OSError):
            return False

    def wait_for(want):
        # The response to request want, or None when the server ends,
        # writes a line that is not JSON-RPC, or the time is up. A
        # request of the server's own is answered as the shim answers
        # it with no connection.
        while True:
            left = deadline - time.monotonic()
            if left <= 0:
                out["timed_out"] = True
                return None
            try:
                raw = lines.get(timeout=left)
            except queue.Empty:
                out["timed_out"] = True
                return None
            if raw is None:
                out["ended"] = True
                return None
            line = raw.decode("utf-8", "replace").strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except ValueError:
                msg = None
            # What the shim takes for JSON-RPC: an object with a method
            # or an id. It stops a server that writes anything else.
            if not isinstance(msg, dict) or ("method" not in msg and "id" not in msg):
                out["not_json"] = line[:400]
                return None
            if "method" in msg:
                if "id" in msg:
                    if msg["method"] == "kivali/credential":
                        err = {"code": -32001, "message": "no connection"}
                        send({"jsonrpc": "2.0", "id": msg["id"], "error": err})
                    elif msg["method"] == "kivali/log":
                        send({"jsonrpc": "2.0", "id": msg["id"], "result": {}})
                    else:
                        err = {"code": -32601, "message": "method not found"}
                        send({"jsonrpc": "2.0", "id": msg["id"], "error": err})
                continue
            if msg.get("id") == want and ("result" in msg or "error" in msg):
                return msg

    init = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "initialize",
        "params": {
            "protocolVersion": spec["protocol_version"],
            "capabilities": {},
            "clientInfo": {"name": "kivali", "version": "check"},
        },
    }
    if send(init):
        out["initialize"] = wait_for(1)
    if out["initialize"] is not None and "result" in out["initialize"]:
        send({"jsonrpc": "2.0", "method": "notifications/initialized"})
        if send({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}}):
            out["tools_list"] = wait_for(2)
    if spec.get("requirements") and out["tools_list"] is not None and "result" in out["tools_list"]:
        if send({"jsonrpc": "2.0", "id": 3, "method": "kivali/requirements", "params": {}}):
            out["requirements"] = wait_for(3)

    try:
        proc.stdin.close()
    except OSError:
        pass
    try:
        proc.wait(timeout=2)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait()
    else:
        out["exit_code"] = proc.returncode
    err_thread.join(timeout=1)
    out["stderr"] = b"".join(errbuf).decode("utf-8", "replace")[-STDERR_KEEP:]
    shutil.rmtree(home, ignore_errors=True)
    print(json.dumps(out))


main()
