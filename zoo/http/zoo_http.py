#!/usr/bin/env python3
"""zoo http: Streamable-HTTP MCP server with header/behavior matrix.

Modes (pick one --mode):
  clean          POST /mcp JSON-RPC, no auth, JSON responses
  bearer         requires Authorization: Bearer s3cret (401 otherwise)
  custom-header  requires X-Tenant: acme
  sse-stream     POST /mcp returns text/event-stream instead of JSON
  redirect       302 to /mcp (same-origin) once, then clean
  xredirect      302 to http://127.0.0.1:{other-port}/mcp (cross-origin)
  slow           sleeps 15s before responding (timeout test)
  hang           never responds
  broken-json    returns 200 with garbage body
  wrong-ct       returns text/plain body with tools payload
  paginated      tools/list returns 150 tools with cursors
  oauth-sim      401 + WWW-Authenticate, then accepts Bearer issued-token
                 (documents the flow; full OAuth needs browser dance)
  session        issues Mcp-Session-Id on initialize, requires it after
  session-expire like session, but 404s the 2nd tools/list (tests re-init)
  legacy-sse     POST /mcp → 404 (forces ErrLegacySSEServer); real SSE at
                 GET /sse + POST /messages/
"""
import argparse
import json
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import urlparse

TOOLS = [
    {"name": "http_ping", "description": "http pong",
     "inputSchema": {"type": "object", "properties": {}}},
    {"name": "http_echo", "description": "http echo",
     "inputSchema": {"type": "object",
                     "properties": {"msg": {"type": "string"}},
                     "required": ["msg"]}},
]

MODE = "clean"
OTHER_PORT = 18999
SEEN = {"redirected": False, "lists": 0, "session": "zoo-session-1"}
SSE_QUEUES = {}  # sid -> list of pending SSE data strings


def handle_rpc(req):
    method = req.get("method", "")
    id_ = req.get("id")
    if method == "initialize":
        return {"protocolVersion": "2024-11-05",
                "serverInfo": {"name": "zoo-http", "version": "0.0.1"},
                "capabilities": {}}
    if method == "tools/list":
        tools = TOOLS
        if MODE == "paginated":
            tools = [{"name": f"htool_{i:03d}", "description": f"tool {i}",
                      "inputSchema": {"type": "object", "properties": {}}}
                     for i in range(150)]
        return {"tools": tools}
    if method == "tools/call":
        p = req.get("params", {})
        args = p.get("arguments", {}) or {}
        if p.get("name") == "http_echo":
            return {"content": [{"type": "text", "text": "http-echo:" + str(args.get("msg", ""))}]}
        return {"content": [{"type": "text", "text": "http-pong"}]}
    if method == "ping":
        return {}
    return {"_error": {"code": -32601, "message": "not found: " + method}}


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _auth_fail(self):
        body = b'{"error":"unauthorized"}'
        self.send_response(401)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        if MODE == "oauth-sim":
            self.send_header("WWW-Authenticate",
                             'Bearer resource_metadata="http://127.0.0.1:PORT/.well-known/oauth-protected-resource"')
        self.end_headers()
        self.wfile.write(body)

    def _check_auth(self):
        if MODE == "bearer" or MODE == "oauth-sim":
            want = "Bearer issued-token" if MODE == "oauth-sim" else "Bearer s3cret"
            if self.headers.get("Authorization") != want:
                self._auth_fail()
                return False
        if MODE == "custom-header":
            if self.headers.get("X-Tenant") != "acme":
                body = b'{"error":"missing tenant"}'
                self.send_response(403)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return False
        return True

    def do_POST(self):
        path = urlparse(self.path).path
        if MODE == "legacy-sse":
            if path == "/mcp":
                # force ErrLegacySSEServer on streamable attempt
                self.send_response(404)
                self.end_headers()
                return
            if path.startswith("/messages/"):
                return self._sse_message()
            self.send_response(404)
            self.end_headers()
            return
        if path != "/mcp":
            self.send_response(404)
            self.end_headers()
            return
        if MODE == "redirect" and not SEEN["redirected"]:
            SEEN["redirected"] = True
            self.send_response(302)
            self.send_header("Location", "/mcp")
            self.end_headers()
            return
        if MODE == "xredirect" and not SEEN["redirected"]:
            SEEN["redirected"] = True
            self.send_response(302)
            self.send_header("Location", f"http://127.0.0.1:{OTHER_PORT}/mcp")
            self.end_headers()
            return
        if not self._check_auth():
            return
        if MODE == "slow":
            time.sleep(15)
        if MODE == "hang":
            time.sleep(3600)
        length = int(self.headers.get("Content-Length", 0))
        try:
            req = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            self.send_response(400)
            self.end_headers()
            return
        result = handle_rpc(req)
        is_session_mode = MODE in ("session", "session-expire")
        if is_session_mode:
            sid = SEEN["session"]
            if req.get("method") != "initialize":
                if self.headers.get("Mcp-Session-Id") != sid:
                    self.send_response(404)
                    self.end_headers()
                    return
                if req.get("method") == "tools/list":
                    SEEN["lists"] += 1
                    if MODE == "session-expire" and SEEN["lists"] == 2:
                        SEEN["session"] = "zoo-session-2"
                        self.send_response(404)
                        self.end_headers()
                        return
        if MODE == "broken-json":
            body = b"{nope this is broken"
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if "_error" in result:
            payload = {"jsonrpc": "2.0", "id": req.get("id"), "error": result["_error"]}
        else:
            payload = {"jsonrpc": "2.0", "id": req.get("id"), "result": result}
        body = json.dumps(payload).encode()
        if MODE == "sse-stream":
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Content-Length", str(len(body) + 8))
            self.end_headers()
            self.wfile.write(b"data: " + body + b"\n\n")
            return
        if MODE == "wrong-ct":
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        if is_session_mode:
            self.send_header("Mcp-Session-Id", SEEN["session"])
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _sse_message(self):
        # legacy SSE: POST JSON-RPC to /messages/<sid>; queue reply for GET stream
        sid = urlparse(self.path).path.split("/")[-1]
        length = int(self.headers.get("Content-Length", 0))
        try:
            req = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            self.send_response(400)
            self.end_headers()
            return
        result = handle_rpc(req)
        if "_error" in result:
            payload = {"jsonrpc": "2.0", "id": req.get("id"), "error": result["_error"]}
        else:
            payload = {"jsonrpc": "2.0", "id": req.get("id"), "result": result}
        SSE_QUEUES.setdefault(sid, []).append(json.dumps(payload))
        self.send_response(202)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):
        path = urlparse(self.path).path
        if path == "/sse":
            if MODE == "legacy-sse":
                return self._sse_stream()
            body = b"event: endpoint\ndata: /messages/1\n\n"
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        self.send_response(405)
        self.end_headers()

    def _sse_stream(self):
        # hold the stream open: endpoint event, then queued messages
        sid = "1"
        SSE_QUEUES.setdefault(sid, [])
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "keep-alive")
        self.end_headers()
        try:
            self.wfile.write(b"event: endpoint\ndata: /messages/1\n\n")
            self.wfile.flush()
            for _ in range(600):  # up to ~60s stream
                time.sleep(0.1)
                while SSE_QUEUES.get(sid):
                    data = SSE_QUEUES[sid].pop(0)
                    self.wfile.write(("event: message\ndata: " + data + "\n\n").encode())
                    self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass


def main():
    global MODE, OTHER_PORT
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", default="clean")
    ap.add_argument("--port", type=int, default=18881)
    ap.add_argument("--other-port", type=int, default=18999)
    a = ap.parse_args()
    MODE = a.mode
    OTHER_PORT = a.other_port
    print(f"zoo-http mode={MODE} port={a.port}", flush=True)
    # threaded: legacy-sse holds GET open while POSTs arrive
    from http.server import ThreadingHTTPServer
    ThreadingHTTPServer(("127.0.0.1", a.port), H).serve_forever()


if __name__ == "__main__":
    main()
