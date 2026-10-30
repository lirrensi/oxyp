#!/usr/bin/env python3
"""zoo oauth: fake IdP + protected MCP endpoint for headless OAuth tests.

Endpoints:
  GET /.well-known/oauth-authorization-server  metadata (issuer, auth/token/reg endpoints)
  POST /register                                DCR → {client_id, client_secret?}
  GET /authorize?...                            auto-approves → 302 to redirect_uri?code=zoo-code&state=...
  POST /token                                   code/refresh exchange → tokens
  POST /mcp                                     requires Bearer zoo-access (401 otherwise)

Run: zoo_oauth.py --port P. No browser needed: tests drive the flow with
urllib (GET authorize with redirects, POST token).
"""
import argparse
import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs, urlencode

PORT = 18882
TOOLS = [
    {"name": "secret_tool", "description": "needs oauth",
     "inputSchema": {"type": "object", "properties": {}}},
]
CODES = {}   # code -> {client_id, redirect_uri, challenge}
TOKENS = {}  # access -> {refresh, exp}
REFRESH = {}  # refresh -> access
DEVICE = {"zoo-device-1": {"polls": 0}}  # device_code -> {polls}


def jbody(handler, obj, code=200, extra=None):
    body = json.dumps(obj).encode()
    handler.send_response(code)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(body)))
    for k, v in (extra or {}).items():
        handler.send_header(k, v)
    handler.end_headers()
    handler.wfile.write(body)


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def base(self):
        return f"http://127.0.0.1:{PORT}"

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/.well-known/oauth-authorization-server":
            return jbody(self, {
                "issuer": self.base(),
                "authorization_endpoint": self.base() + "/authorize",
                "token_endpoint": self.base() + "/token",
                "registration_endpoint": self.base() + "/register",
                "device_authorization_endpoint": self.base() + "/device_authorize",
                "response_types_supported": ["code"],
                "grant_types_supported": ["authorization_code", "refresh_token",
                                          "urn:ietf:params:oauth:grant-type:device_code",
                                          "client_credentials"],
                "code_challenge_methods_supported": ["S256"],
                "token_endpoint_auth_methods_supported": ["none", "client_secret_post", "client_secret_basic"],
            })
        if u.path == "/authorize":
            q = parse_qs(u.query)
            code = "zoo-code-1"
            CODES[code] = {
                "client_id": q.get("client_id", [""])[0],
                "redirect_uri": q.get("redirect_uri", [""])[0],
                "challenge": q.get("code_challenge", [""])[0],
            }
            loc = CODES[code]["redirect_uri"] + "?" + urlencode(
                {"code": code, "state": q.get("state", [""])[0]})
            self.send_response(302)
            self.send_header("Location", loc)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if u.path == "/mcp":
            self.send_response(405)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        u = urlparse(self.path)
        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length) or b""
        if u.path == "/register":
            try:
                reg = json.loads(raw or b"{}")
            except json.JSONDecodeError:
                reg = {}
            return jbody(self, {
                "client_id": "zoo-client-1",
                "client_secret": "zoo-secret-1",
                "redirect_uris": reg.get("redirect_uris", []),
                "token_endpoint_auth_method": "client_secret_post",
                "grant_types": ["authorization_code", "refresh_token"],
            }, code=201)
        if u.path == "/token":
            ctype = self.headers.get("Content-Type", "")
            if "json" in ctype:
                try:
                    form = json.loads(raw or b"{}")
                except json.JSONDecodeError:
                    form = {}
            else:
                form = {k: v[0] for k, v in parse_qs(raw.decode()).items()}
            grant = form.get("grant_type")
            if grant == "authorization_code":
                code = form.get("code", "")
                if code not in CODES:
                    return jbody(self, {"error": "invalid_grant"}, code=400)
                # strict client binding: the exchanging client must be the one
                # the code was issued to (catches lost-DCR-credential bugs).
                want = CODES[code].get("client_id", "")
                got = form.get("client_id", "")
                if want and got != want:
                    return jbody(self, {"error": "invalid_client"}, code=401)
                # (skip PKCE verify — the dance shape is what's tested)
                access, refresh = "zoo-access-1", "zoo-refresh-1"
                TOKENS[access] = {"refresh": refresh, "exp": time.time() + 3600}
                REFRESH[refresh] = access
                return jbody(self, {"access_token": access, "token_type": "Bearer",
                                    "expires_in": 3600, "refresh_token": refresh})
            if grant == "refresh_token":
                rt = form.get("refresh_token", "")
                if rt not in REFRESH:
                    return jbody(self, {"error": "invalid_grant"}, code=400)
                access = "zoo-access-2"
                TOKENS[access] = {"refresh": rt, "exp": time.time() + 3600}
                REFRESH[rt] = access
                return jbody(self, {"access_token": access, "token_type": "bearer",
                                    "expires_in": 3600, "refresh_token": rt})
            if grant == "urn:ietf:params:oauth:grant-type:device_code":
                dc = form.get("device_code", "")
                if dc not in DEVICE:
                    return jbody(self, {"error": "invalid_grant"}, code=400)
                DEVICE[dc]["polls"] += 1
                # first poll: pending; second: slow_down (tests backoff); third: approved
                if DEVICE[dc]["polls"] == 1:
                    return jbody(self, {"error": "authorization_pending"}, code=400)
                if DEVICE[dc]["polls"] == 2:
                    return jbody(self, {"error": "slow_down"}, code=400)
                access, refresh = "zoo-device-access", "zoo-device-refresh"
                TOKENS[access] = {"refresh": refresh, "exp": time.time() + 3600}
                REFRESH[refresh] = access
                return jbody(self, {"access_token": access, "token_type": "Bearer",
                                    "expires_in": 3600, "refresh_token": refresh})
            if grant == "client_credentials":
                # pre-registered service client; scope echoed back, no refresh
                if form.get("client_id") != "zoo-svc" or form.get("client_secret") != "zoo-svc-secret":
                    return jbody(self, {"error": "invalid_client"}, code=401)
                access = "zoo-cc-access"
                TOKENS[access] = {"refresh": "", "exp": time.time() + 3600}
                return jbody(self, {"access_token": access, "token_type": "Bearer",
                                    "expires_in": 3600, "scope": form.get("scope", "")})
            return jbody(self, {"error": "unsupported_grant_type"}, code=400)
        if u.path == "/device_authorize":
            form = {k: v[0] for k, v in parse_qs(raw.decode()).items()}
            # record the RFC 8707 resource binding for tests (ignored for auth)
            DEVICE["zoo-device-1"]["resource"] = form.get("resource", "")
            return jbody(self, {
                "device_code": "zoo-device-1",
                "user_code": "ZOO-1234",
                "verification_uri": self.base() + "/device",
                "expires_in": 300,
                "interval": 1,
            })
        if u.path == "/mcp":
            auth = self.headers.get("Authorization", "")
            tok = auth[7:] if auth.startswith("Bearer ") else ""
            if tok not in TOKENS or TOKENS[tok]["exp"] < time.time():
                body = json.dumps({"error": "unauthorized"}).encode()
                self.send_response(401)
                self.send_header("Content-Type", "application/json")
                self.send_header("WWW-Authenticate",
                                 f'Bearer resource_metadata="{self.base()}/.well-known/oauth-protected-resource"')
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            try:
                req = json.loads(raw or b"{}")
            except json.JSONDecodeError:
                self.send_response(400)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            m = req.get("method", "")
            if m == "initialize":
                res = {"protocolVersion": "2024-11-05",
                       "serverInfo": {"name": "zoo-oauth", "version": "0.0.1"},
                       "capabilities": {}}
            elif m == "tools/list":
                res = {"tools": TOOLS}
            elif m == "tools/call":
                res = {"content": [{"type": "text", "text": "oauth-secret-ok"}]}
            elif m == "ping":
                res = {}
            else:
                return jbody(self, {"jsonrpc": "2.0", "id": req.get("id"),
                                    "error": {"code": -32601, "message": "not found"}})
            return jbody(self, {"jsonrpc": "2.0", "id": req.get("id"), "result": res})
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()


def main():
    global PORT
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=18882)
    PORT = ap.parse_args().port
    print(f"zoo-oauth port={PORT}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()


if __name__ == "__main__":
    main()
