"""Echo: a stdio MCP server for Kivali's tests, standard library only.

It reads one JSON-RPC message per line and writes one per line, and
answers initialize, ping, tools/list and tools/call:

- echo returns its arguments and the _meta.kivali its call carried,
  and refuses, as an error result, a color the caller's access does
  not include when the access names its colors;
- whoami names the connected account: "echo as " and ECHO_TOKEN;
- list_colors lists the colors an access form offers, as JSON text.
"""

import json
import os
import sys

PROTOCOL_VERSION = "2025-06-18"

COLORS = ["red", "green", "blue"]

TOOLS = [
    {
        "name": "echo",
        "description": "Return the arguments and who called.",
        "inputSchema": {
            "type": "object",
            "properties": {"color": {"type": "string"}, "text": {"type": "string"}},
        },
    },
    {"name": "whoami", "description": "Name the connected account.", "inputSchema": {"type": "object"}},
    {"name": "list_colors", "description": "List the colors.", "inputSchema": {"type": "object"}},
]


def text(s, is_error=False):
    return {"content": [{"type": "text", "text": s}], "isError": is_error}


def call(name, args, kivali):
    if name == "echo":
        access = (kivali or {}).get("access") or {}
        colors = access.get("colors")
        color = args.get("color")
        if colors is not None and color not in colors:
            allowed = ", ".join(colors) or "none"
            return text("%s is not one of your colors (%s)" % (color or "no color", allowed), True)
        return text(json.dumps({"arguments": args, "kivali": kivali}, sort_keys=True))
    if name == "whoami":
        return text("echo as " + os.environ.get("ECHO_TOKEN", "nobody"))
    if name == "list_colors":
        return text(json.dumps(COLORS))
    return text("unknown tool %s" % name, True)


def answer(msg):
    method = msg.get("method")
    params = msg.get("params") or {}
    if method == "initialize":
        return {
            "protocolVersion": params.get("protocolVersion", PROTOCOL_VERSION),
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "echo", "version": "1.0.0"},
        }, None
    if method == "ping":
        return {}, None
    if method == "tools/list":
        return {"tools": TOOLS}, None
    if method == "tools/call":
        meta = params.get("_meta") or {}
        return call(params.get("name"), params.get("arguments") or {}, meta.get("kivali")), None
    return None, {"code": -32601, "message": "method not found: %s" % method}


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
            sys.stderr.write("echo: a line that is not JSON\n")
            continue
        if not isinstance(msg, dict) or "method" not in msg or "id" not in msg:
            # A notification, or a response to a request this server
            # never sends.
            continue
        result, error = answer(msg)
        out = {"jsonrpc": "2.0", "id": msg["id"]}
        if error is not None:
            out["error"] = error
        else:
            out["result"] = result
        sys.stdout.write(json.dumps(out) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
