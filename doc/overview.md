---
node_type: overview
title: oxyp Overview and Architecture
status: active
updated: 2026-11-09
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

```text
MCP clients (hermes / omp / opencode) — URL-only clients via POST /mcp
  │  stdio JSON-RPC (shim) or Streamable HTTP (/mcp): initialize / tools/list / tools/call / ping
  ▼
oxyp shim (cmd/oxyp/shim.go) — 5 static meta-tools, stdout pure JSONL
  │  HTTP + bearer (60 s client), autostarts daemon if needed; --group/--servers/--exclude scopes
  │  (flags over OXYP_GROUP/OXYP_SERVERS/OXYP_EXCLUDE; one view per client, agent can't escape)
  ▼
daemon HTTP API (internal/meta) — 11 routes + auth gate
  │  bearer → Host → Origin → Content-Type
  │  per-request scope: ?group=&servers=&exclude= (deny wins, unknown group 404, outside 403)
  │  POST /mcp = same 5 meta-tools over Streamable HTTP (internal/meta/mcp.go)
  │  (tool args group/servers/exclude over ?query over env; InScope ANDs with sharing)
  ▼
Daemon (internal/daemon) — lifecycle, spawn, health, cache, OAuth, probe, groups+scope
  ├── local stdio children (mcp-go, 30 s init / 10 s RPC / 3 s ping; per-server tool_timeout)
  ├── remote Streamable HTTP + legacy SSE fallback (remhttp client; token_header remap)
  ├── metacache (<dir>/cache.json) — tool metadata, fingerprint-keyed; pinned / new_tools=hide
  ├── OAuth store (<dir>/oauth/) — daemon-held tokens + DCR sidecar; authorization_url override
  ├── scope (internal/daemon/scope.go) — group + allow/exclude globs (*, ?, case-insensitive); deny wins
  ├── params gates ([servers.x.params] → 403 param-blocked)
  └── spill files (os.TempDir()/oxyp-output-*.txt, 0600, pruned)
Config: <dir>/oxyp.toml (+ [groups.*]) + <dir>/connectors/  Endpoint: <dir>/daemon.json
Dir precedence: --dir > OXYP_DIR > XDG_CONFIG_HOME/oxyp > ~/.oxyp
```

### Components

| Component | Code | Spec |
|---|---|---|
| Daemon lifecycle, health, reload, idle sweep, endpoint, per-client scope | `internal/daemon` (+ `scope.go`), `cmd/oxyp/main.go` | [/spec-daemon.md](/spec-daemon.md) |
| Tool metadata cache, ranked search, permissions + scope filtering | `internal/metacache`, `internal/search`, `internal/perms` | [/spec-discovery.md](/spec-discovery.md) |
| Daemon-held OAuth (4 flows), DCR sidecar, auto-detect | `internal/oauth`, `internal/daemon/login.go` | [/spec-oauth.md](/spec-oauth.md) |
| HTTP API, CSRF/token policy, shim, per-client views, media passthrough | `internal/meta`, `cmd/oxyp/shim.go` | [/spec-http-shim.md](/spec-http-shim.md) |
| Output guard/spill, trace, redaction, HTTP transport, secret expansion, config schema | `internal/outguard`, `internal/ptrace`, `internal/redact`, `internal/remhttp`, `internal/config` | [/spec-guard.md](/spec-guard.md) |
| CLI (16 commands, scope flags), doctor, add, import, connectors | `cmd/oxyp`, `internal/importcfg` | [/spec-cli.md](/spec-cli.md) |
| Federation (peer daemons, virtual servers, sharing) + scope ANDs with sharing | `internal/daemon/peers.go`, `internal/peerfederation/` | [/spec-federation.md](/spec-federation.md) |
| Compat batteries (stdio/HTTP/OAuth/TLS zoo servers) | `zoo/` | [/ZOO.md](/ZOO.md), [/verification/strategy.md](/verification/strategy.md) |
| Client attach shapes (opencode/omp/hermes) + per-folder views | — (docs) | [/CLIENTS.md](/CLIENTS.md) |
| Stances (secrets, portability, no approval prompts, views-not-tenants) | — (docs) | [/PHILOSOPHY.md](/PHILOSOPHY.md) |

## Why oxyp vs a plain multiplexer

> Plain multiplexers fan out stdio. oxyp is a **lazy gateway + search engine + OAuth vault + federated mesh + per-client views** — see `README.md#features--more-than-a-multiplexer` for the full table. This doc stays code-accurate; the README stays persuasive.

## Key facts (code-accurate as of 2026-11-09)

- Default bind `127.0.0.1:8379`; empty token means loopback trust ONLY —
  a non-loopback addr with an empty token fails `config.Load`.
- Timeouts: initialize 30 s, per-RPC 10 s (per-server `tool_timeout` overrides),
  health ping 3 s every 10 s, idle sweep every 30 s with 10 min default,
  remote metadata TTL 5 min, CLI API 60 s, autostart wait 5 s, doctor/add live
  probe 15 s per server.
- The shim AND `POST /mcp` expose exactly **5** meta-tools (`list_servers`,
  `list_tools`, `describe_tool`, `call`, `search_tools`).
- Per-client scope travels per request, never daemon-global: `--group` /
  `--servers` / `--exclude` flags over `OXYP_GROUP` / `OXYP_SERVERS` /
  `OXYP_EXCLUDE` env (CLI + shim, one view per client), `?group=&servers=&exclude=`
  query (REST, comma-separated + repeatable, `*`/`?` globs, deny wins), and
  `group` / `servers` / `exclude` tool args over `?query` over env (`POST /mcp`).
  Unknown group → 404, outside scope → 403 (`server not in scope`; legacy
  `server not in group` when only groups exclude). `/health` stays global.
- Redirects: up to **4** redirects followed (5 requests total); cross-origin
  keeps only `Accept`, `Content-Type`, `Mcp-Session-Id`,
  `MCP-Protocol-Version`.
- Toolchains: Go 1.26.8; deps `mcp-go v1.4.0`, `cobra v1.10.2`,
  `BurntSushi/toml v1.6.0`.
