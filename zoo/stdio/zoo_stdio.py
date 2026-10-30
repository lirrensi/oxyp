#!/usr/bin/env python3
"""zoo base: minimal stdio MCP server with behavior flags.

Usage: zoo_stdio.py [--junk-stdout] [--blank-stdout] [--stderr-flood]
                    [--slow-init SEC] [--slow-tool SEC] [--hang-tool]
                    [--crash-on N] [--bad-json] [--wrong-version]
                    [--paginated N] [--unicode-names] [--dup-names]
                    [--zero-tools] [--bad-schema] [--giant-result MB]
                    [--request-sampling] [--request-elicitation]
                    [--request-roots] [--notify-changed SEC]
Each flag triggers one breakage axis. Default = clean server.
"""
import argparse
import json
import sys
import time

CLEAN_TOOLS = [
    {"name": "ping_tool", "description": "returns pong",
     "inputSchema": {"type": "object", "properties": {}}},
    {"name": "echo_tool", "description": "echoes msg",
     "inputSchema": {"type": "object",
                     "properties": {"msg": {"type": "string"}},
                     "required": ["msg"]}},
]


def build_tools(a):
    if a.zero_tools:
        return []
    if a.paginated:
        return [{"name": f"tool_{i:03d}", "description": f"tool {i}",
                 "inputSchema": {"type": "object", "properties": {}}}
                for i in range(a.paginated)]
    tools = [dict(t) for t in CLEAN_TOOLS]
    if a.unicode_names:
        tools.append({"name": "héllo-wörld.ü", "description": "ünïcödé",
                      "inputSchema": {"type": "object", "properties": {}}})
    if a.dup_names:
        tools.append(dict(tools[0]))
    if a.bad_schema:
        tools.append({"name": "broken_schema", "description": "bad inputSchema",
                      "inputSchema": {"type": "not-a-type", "properties": "nope"}})
    if a.env_echo:
        tools.append({"name": "env_echo", "description": "reports own env and cwd",
                      "inputSchema": {"type": "object", "properties": {}}})
    return tools


def respond(id_, result=None, error=None):
    msg = {"jsonrpc": "2.0", "id": id_}
    if error is not None:
        msg["error"] = error
    else:
        msg["result"] = result
    sys.stdout.write(json.dumps(msg) + "\n")
def main(a):
    tools = build_tools(a)

    if a.junk_stdout:
        sys.stdout.write("NOT JSON — hello from a rude server\n")
        sys.stdout.flush()
    if a.blank_stdout:
        sys.stdout.write("\n")
        sys.stdout.flush()
    if a.stderr_flood:
        for i in range(5000):
            sys.stderr.write(f"stderr line {i}: " + "x" * 200 + "\n")
        sys.stderr.flush()

    call_count = [0]

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError:
            continue
        method = req.get("method", "")
        id_ = req.get("id")
        call_count[0] += 1
        if a.crash_on and call_count[0] >= a.crash_on:
            sys.stderr.write("zoo: simulated crash\n")
            sys.exit(1)
        if a.bad_json and method == "tools/call":
            # respond with garbage instead of JSON-RPC
            sys.stdout.write("{this is not json\n")
            sys.stdout.flush()
            continue

        if method == "initialize":
            if a.slow_init:
                time.sleep(a.slow_init)
            ver = "1999-01-01" if a.wrong_version else "2024-11-05"
            caps = {}
            if a.notify_changed:
                caps = {"tools": {"listChanged": True}}
            result = {"protocolVersion": ver,
                      "serverInfo": {"name": "zoo-stdio", "version": "0.0.1"},
                      "capabilities": caps}
            if a.with_resource:
                result["instructions"] = "zoo says hi: prefer echo_tool for echoes"
            respond(id_, result)
            if a.notify_changed:
                # fire list_changed shortly after init (in-band: next line)
                pass
        elif method == "notifications/initialized":
            if a.notify_changed:
                time.sleep(min(a.notify_changed, 3))
                sys.stdout.write(json.dumps(
                    {"jsonrpc": "2.0", "method": "notifications/tools/list_changed"}) + "\n")
                sys.stdout.flush()
        elif method == "tools/list":
            # ignore pagination cursor: return all (client must cope)
            respond(id_, {"tools": tools})
        elif method == "resources/list":
            if a.with_resource:
                respond(id_, {"resources": [
                    {"uri": "zoo://greeting", "name": "greeting",
                     "description": "zoo hello text",
                     "mimeType": "text/plain"}]})
            else:
                respond(id_, error={"code": -32601,
                                    "message": "not found: " + method})
        elif method == "resources/read":
            uri = (req.get("params", {}) or {}).get("uri", "")
            if a.with_resource and uri == "zoo://greeting":
                respond(id_, {"contents": [
                    {"uri": uri, "mimeType": "text/plain",
                     "text": "hello from the zoo"}]})
            else:
                respond(id_, error={"code": -32602,
                                    "message": "unknown resource: " + uri})
        elif method == "tools/call":
            p = req.get("params", {})
            name = p.get("name", "")
            args = p.get("arguments", {}) or {}
            if a.slow_tool:
                time.sleep(a.slow_tool)
            if a.hang_tool:
                time.sleep(3600)
            if a.giant_result:
                n = int(a.giant_result * 1024 * 1024)
                respond(id_, {"content": [{"type": "text", "text": "G" * n}]})
            elif a.media:
                respond(id_, {
                    "content": [
                        {"type": "text", "text": "screenshot below"},
                        {"type": "image", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEA",
                         "mimeType": "image/png"},
                        {"type": "resource_link", "uri": "file:///tmp/shot.png",
                         "name": "shot.png", "mimeType": "image/png"},
                    ],
                    "structuredContent": {"format": "png"},
                })
            elif name == "env_echo":
                import os as _os
                respond(id_, {"content": [{"type": "text", "text": json.dumps(
                    {"env": dict(_os.environ), "cwd": _os.getcwd()})} ]})
            elif name == "echo_tool":
                respond(id_, {"content": [{"type": "text",
                                           "text": "echo:" + str(args.get("msg", ""))}]})
            else:
                respond(id_, {"content": [{"type": "text", "text": "pong"}]})
        elif method == "ping":
            respond(id_, {})
        elif method == "roots/list":
            respond(id_, {"roots": []})
        else:
            if id_ is not None:
                respond(id_, error={"code": -32601, "message": "not found: " + method})

    # NOTE: sampling/elicitation/roots REQUESTS from server→client need the
    # server to initiate; this base only answers. Dedicated scripts for those.


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--junk-stdout", action="store_true")
    parser.add_argument("--blank-stdout", action="store_true")
    parser.add_argument("--stderr-flood", action="store_true")
    parser.add_argument("--slow-init", type=float, default=0)
    parser.add_argument("--slow-tool", type=float, default=0)
    parser.add_argument("--hang-tool", action="store_true")
    parser.add_argument("--crash-on", type=int, default=0)
    parser.add_argument("--bad-json", action="store_true")
    parser.add_argument("--wrong-version", action="store_true")
    parser.add_argument("--paginated", type=int, default=0)
    parser.add_argument("--unicode-names", action="store_true")
    parser.add_argument("--dup-names", action="store_true")
    parser.add_argument("--zero-tools", action="store_true")
    parser.add_argument("--bad-schema", action="store_true")
    parser.add_argument("--giant-result", type=float, default=0)
    parser.add_argument("--media", action="store_true")
    parser.add_argument("--env-echo", action="store_true")
    parser.add_argument("--with-resource", action="store_true")
    parser.add_argument("--notify-changed", type=float, default=0)
    main(parser.parse_args())
