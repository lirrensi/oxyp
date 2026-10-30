#!/usr/bin/env python3
"""zoo needy: server that REQUESTS sampling/elicitation/roots/list_changed.

On tools/call to `needy_tool`, it sends (in order) roots/list, sampling/createMessage,
elicitation/create to the client, then returns what it got. Tests that oxyp's
mcp-go client declines/handles without hanging.
"""
import json
import sys

TOOLS = [{"name": "needy_tool", "description": "requests things from client",
          "inputSchema": {"type": "object", "properties": {}}}]


def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()


def main():
    pending = {}
    next_id = 100
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError:
            continue
        # client responding to OUR request?
        if "method" not in req and "id" in req and req["id"] in pending:
            pending[req["id"]] = req.get("result", req.get("error"))
            if len([v for v in pending.values() if v is not None]) == 3:
                send({"jsonrpc": "2.0", "id": pending["call_id"],
                      "result": {"content": [{"type": "text",
                                              "text": "got: " + json.dumps(
                                                  {k: v for k, v in pending.items() if k != "call_id"})}]}})
            continue
        method = req.get("method", "")
        id_ = req.get("id")
        if method == "initialize":
            send({"jsonrpc": "2.0", "id": id_,
                  "result": {"protocolVersion": "2024-11-05",
                             "serverInfo": {"name": "zoo-needy", "version": "0.0.1"},
                             "capabilities": {}}})
        elif method == "notifications/initialized":
            pass
        elif method == "tools/list":
            send({"jsonrpc": "2.0", "id": id_, "result": {"tools": TOOLS}})
        elif method == "tools/call":
            pending = {"call_id": id_}
            for m, p in [("roots/list", {}),
                          ("sampling/createMessage",
                           {"messages": [{"role": "user", "content": {"type": "text", "text": "hi"}}],
                            "maxTokens": 10}),
                          ("elicitation/create",
                           {"message": "pick one", "requestedSchema": {"type": "object"}})]:
                next_id += 1
                pending[next_id] = None
                send({"jsonrpc": "2.0", "id": next_id, "method": m, "params": p})
        elif method == "ping":
            send({"jsonrpc": "2.0", "id": id_, "result": {}})
        elif id_ is not None:
            send({"jsonrpc": "2.0", "id": id_,
                  "error": {"code": -32601, "message": "not found"}})


if __name__ == "__main__":
    main()
