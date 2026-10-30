#!/usr/bin/env python3
"""Throwaway MCP server for oxyp smoke tests: 3 tools, stdio."""
import json
import sys

TOOLS = [
    {"name": "get_time", "description": "current UTC time",
     "inputSchema": {"type": "object", "properties": {}}},
    {"name": "echo_msg", "description": "echo a message back",
     "inputSchema": {"type": "object", "properties": {"msg": {"type": "string"}}, "required": ["msg"]}},
    {"name": "delete_all", "description": "dangerous tool for C1 tests",
     "inputSchema": {"type": "object", "properties": {}}},
]

def respond(id_, result=None, error=None):
    msg = {"jsonrpc": "2.0", "id": id_}
    if error is not None:
        msg["error"] = error
    else:
        msg["result"] = result
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()

def main():
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
        if method == "initialize":
            respond(id_, {"protocolVersion": "2024-11-05",
                           "serverInfo": {"name": "smoke", "version": "0.0.1"},
                           "capabilities": {"tools": {}}})
        elif method == "notifications/initialized":
            pass
        elif method == "tools/list":
            respond(id_, {"tools": TOOLS})
        elif method == "tools/call":
            p = req.get("params", {})
            name = p.get("name", "")
            args = p.get("arguments", {}) or {}
            if name == "get_time":
                respond(id_, {"content": [{"type": "text", "text": "2026-10-28T00:00:00Z"}]})
            elif name == "echo_msg":
                respond(id_, {"content": [{"type": "text", "text": "echo:" + str(args.get("msg", ""))}]})
            elif name == "delete_all":
                respond(id_, {"content": [{"type": "text", "text": "NUKED"}]})
            else:
                respond(id_, error={"code": -32602, "message": "unknown tool " + name})
        elif method == "ping":
            respond(id_, {})
        else:
            if id_ is not None:
                respond(id_, error={"code": -32601, "message": "not found: " + method})

if __name__ == "__main__":
    main()
