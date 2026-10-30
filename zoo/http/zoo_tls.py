#!/usr/bin/env python3
"""zoo tls: HTTPS MCP endpoint with configurable TLS personality.

  --mode self-signed   cert CN=127.0.0.1 not in any trust store
  --mode mtls           requires client cert (any cert accepted)

Serves POST /mcp like zoo_http clean mode, over TLS. Certs generated at
first run into the script dir (zoo/http/certs/) via openssl.
"""
import argparse
import json
import os
import ssl
import subprocess
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
CERTS = os.path.join(HERE, "certs")

TOOLS = [{"name": "tls_ping", "description": "tls pong",
          "inputSchema": {"type": "object", "properties": {}}}]


def ensure_certs():
    os.makedirs(CERTS, exist_ok=True)
    srv_crt = os.path.join(CERTS, "server.crt")
    srv_key = os.path.join(CERTS, "server.key")
    ca_crt = os.path.join(CERTS, "ca.crt")
    cli_crt = os.path.join(CERTS, "client.crt")
    cli_key = os.path.join(CERTS, "client.key")
    if all(os.path.exists(p) for p in (srv_crt, srv_key, ca_crt, cli_crt, cli_key)):
        return
    # CA
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                    "-keyout", os.path.join(CERTS, "ca.key"), "-out", ca_crt,
                    "-days", "2", "-subj", "/CN=zoo-ca"],
                   check=True, capture_output=True)
    # server cert signed by CA, SAN 127.0.0.1
    subprocess.run(["openssl", "req", "-newkey", "rsa:2048", "-nodes",
                    "-keyout", srv_key, "-out", os.path.join(CERTS, "server.csr"),
                    "-subj", "/CN=127.0.0.1"], check=True, capture_output=True)
    with open(os.path.join(CERTS, "san.ext"), "w") as f:
        f.write("subjectAltName=IP:127.0.0.1,DNS:localhost\n")
    subprocess.run(["openssl", "x509", "-req",
                    "-in", os.path.join(CERTS, "server.csr"),
                    "-CA", ca_crt, "-CAkey", os.path.join(CERTS, "ca.key"),
                    "-CAcreateserial", "-out", srv_crt, "-days", "2",
                    "-extfile", os.path.join(CERTS, "san.ext")],
                   check=True, capture_output=True)
    # client cert
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                    "-keyout", cli_key, "-out", cli_crt,
                    "-days", "2", "-subj", "/CN=zoo-client"],
                   check=True, capture_output=True)
    print("generated certs", flush=True)


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    MTLS = False

    def log_message(self, *a):
        pass

    def do_POST(self):
        if self.path != "/mcp":
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if H.MTLS and not self.connection.getpeercert():
            body = b'{"error":"client cert required"}'
            self.send_response(403)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        length = int(self.headers.get("Content-Length", 0))
        try:
            req = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            self.send_response(400)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        m = req.get("method", "")
        if m == "initialize":
            res = {"protocolVersion": "2024-11-05",
                   "serverInfo": {"name": "zoo-tls", "version": "0.0.1"},
                   "capabilities": {}}
        elif m == "tools/list":
            res = {"tools": TOOLS}
        elif m == "tools/call":
            res = {"content": [{"type": "text", "text": "tls-ok"}]}
        elif m == "ping":
            res = {}
        else:
            res = {"_error": {"code": -32601, "message": "not found"}}
        if "_error" in res:
            payload = {"jsonrpc": "2.0", "id": req.get("id"), "error": res["_error"]}
        else:
            payload = {"jsonrpc": "2.0", "id": req.get("id"), "result": res}
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", default="self-signed")
    ap.add_argument("--port", type=int, default=18883)
    a = ap.parse_args()
    ensure_certs()
    H.MTLS = (a.mode == "mtls")
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(os.path.join(CERTS, "server.crt"),
                        os.path.join(CERTS, "server.key"))
    if H.MTLS:
        ctx.verify_mode = ssl.CERT_REQUIRED
        ctx.load_verify_locations(os.path.join(CERTS, "client.crt"))
    srv = ThreadingHTTPServer(("127.0.0.1", a.port), H)
    srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    print(f"zoo-tls mode={a.mode} port={a.port}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    if sys.version_info < (3, 7):
        sys.exit("need py3.7+")
    main()
