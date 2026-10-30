---
node_type: overview
title: oxyp Overview and Architecture
status: active
updated: 2026-10-30
tags: [overview, architecture, gateway, mcp]
links:
  depends_on: [/PHILOSOPHY.md]
---

# oxyp Overview and Architecture

## Purpose

oxyp is a lazy MCP gateway in Go: one static binary plus one TOML file that
fronts N downstream MCP servers (local stdio children and remote
Streamable-HTTP/SSE endpoints) behind a small set of meta-tools. Downstream
connections stay cold until first use, tool metadata survives restarts on
disk, and deny-means-invisible permissions keep the whole permission story in
config. This document is the product identity and the implementation-structure
map; exact behavior lives in the `spec-*` documents, and proof lives in
[/verification/strategy.md](/verification/strategy.md).

## Boundaries

**Handles:** aggregating local + remote MCP servers; cold-start lifecycle with
idle stop; cached tool discovery (list/describe/search) that never spawns;
guarded tool invocation with media passthrough; daemon-held OAuth for remote
servers; config-driven permissions; offline/live doctoring and
probe/append/add/import helpers.

**Explicitly does NOT:** prompt for per-call approval (no allowlist-with-confirm,
no pending queue — see [/PHILOSOPHY.md](/PHILOSOPHY.md)); fetch secrets from
vaults/CLIs (the harness procures, oxyp carries); serve Streamable HTTP to
upstream MCP clients (the daemon HTTP API is oxyp's own REST; MCP clients
attach over stdio via `oxyp shim`); isolate tenants (single user, single
daemon per config dir).

**If it happens anyway:** approval gates belong on the harness side of the
pipe; secret procurement belongs in the harness (env vars / files); remote
MCP access to the daemon stays stdio-shim-over-ssh until a Streamable HTTP
server mode exists (future work).

## Goals

- **G1:** One binary + one config runs an agent anywhere (no keychain, no
  machine-bound state, no install dance).
- **G2:** Cold by default — listing, describing, and searching tools never
  spawns a downstream server, and idle children stop while metadata stays
  searchable.
- **G3:** Fail closed and stay up — bad server entries degrade to unhealthy
  status, never daemon failure; unset secrets never travel literally (remote
  URL/headers); empty tokens never bind non-loopback.
- **G4:** Every shipped capability is documented, code-accurate, and mapped
  to verification evidence or an honest gap.

## System map

```
MCP clients (hermes / omp / opencode)
  │  stdio JSON-RPC: initialize / tools/list / tools/call / ping
  ▼
oxyp shim (cmd/oxyp/shim.go) — 5 static meta-tools, stdout pure JSONL
  │  HTTP + bearer (60 s client), autostarts daemon if needed
  ▼
daemon HTTP API (internal/meta) — 10 routes + auth gate
  │  bearer → Host → Origin → Content-Type
  ▼
Daemon (internal/daemon) — lifecycle, spawn, health, cache, OAuth, probe
  ├── local stdio children (mcp-go, 30 s init / 10 s RPC / 3 s ping)
  ├── remote Streamable HTTP + legacy SSE fallback (remhttp client)
  ├── metacache (<dir>/cache.json) — tool metadata, fingerprint-keyed
  ├── OAuth store (<dir>/oauth/) — daemon-held tokens + DCR sidecar
  └── spill files (os.TempDir()/oxyp-output-*.txt, 0600, pruned)
Config: <dir>/oxyp.toml + <dir>/connectors/  Endpoint: <dir>/daemon.json
Dir precedence: --dir > OXYP_DIR > XDG_CONFIG_HOME/oxyp > ~/.oxyp
```

### Components

| Component | Code | Spec |
|---|---|---|
| Daemon lifecycle, health, reload, idle sweep, endpoint | `internal/daemon`, `cmd/oxyp/main.go` | [/spec-daemon.md](/spec-daemon.md) |
| Tool metadata cache, ranked search, permissions | `internal/metacache`, `internal/search`, `internal/perms` | [/spec-discovery.md](/spec-discovery.md) |
| Daemon-held OAuth (4 flows), DCR sidecar, auto-detect | `internal/oauth`, `internal/daemon/login.go` | [/spec-oauth.md](/spec-oauth.md) |
| HTTP API, CSRF/token policy, shim, media passthrough | `internal/meta`, `cmd/oxyp/shim.go` | [/spec-http-shim.md](/spec-http-shim.md) |
| Output guard/spill, trace, redaction, HTTP transport, secret expansion, config schema | `internal/outguard`, `internal/ptrace`, `internal/redact`, `internal/remhttp`, `internal/config` | [/spec-guard.md](/spec-guard.md) |
| CLI (13 commands), doctor, add, import, connectors | `cmd/oxyp`, `internal/importcfg` | [/spec-cli.md](/spec-cli.md) |
| Compat batteries (stdio/HTTP/OAuth/TLS zoo servers) | `zoo/` | [/ZOO.md](/ZOO.md), [/verification/strategy.md](/verification/strategy.md) |
| Client attach shapes (opencode/omp/hermes) | — (docs) | [/CLIENTS.md](/CLIENTS.md) |
| Stances (secrets, portability, no approval prompts) | — (docs) | [/PHILOSOPHY.md](/PHILOSOPHY.md) |

## Key facts (code-accurate as of 2026-10-30)

- Default bind `127.0.0.1:8379`; empty token means loopback trust ONLY —
  a non-loopback addr with an empty token fails `config.Load`.
- Timeouts: initialize 30 s, per-RPC 10 s, health ping 3 s every 10 s,
  idle sweep every 30 s with 10 min default, remote metadata TTL 5 min,
  CLI API 60 s, autostart wait 5 s, doctor/add live probe 15 s per server.
- The shim exposes exactly **5** meta-tools (`list_servers`, `list_commands`,
  `describe_commands`, `invoke_command`, `search_tools`).
- Redirects: up to **4** redirects followed (5 requests total); cross-origin
  keeps only `Accept`, `Content-Type`, `Mcp-Session-Id`,
  `MCP-Protocol-Version`.
- Toolchains: Go 1.26.8; deps `mcp-go v1.4.0`, `cobra v1.10.2`,
  `BurntSushi/toml v1.6.0`.
