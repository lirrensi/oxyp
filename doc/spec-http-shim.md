---
node_type: spec
title: HTTP API, CSRF Policy, Shim, and Media Passthrough
status: active
updated: 2026-10-30
tags: [http, csrf, shim, media, api]
links:
  depends_on: [/overview.md, /spec-daemon.md, /spec-discovery.md]
  documents: [/internal/meta/, /cmd/oxyp/shim.go]
  verified_by: [/verification/strategy.md]
---

# HTTP + Shim Behavior

## Purpose

This spec covers the daemon's own REST API (10 routes + auth gate), the
CSRF/token policy that protects a loopback daemon with no token, the stdio
shim that turns that REST into 5 MCP meta-tools, and the media passthrough
that keeps image/audio/resource blocks native end to end. It matters
because every MCP client attaches through the shim, and every agent action
flows through these envelopes.

## Boundaries

**Handles:** route table, status codes, gate order, shim dispatch, error
mapping, media/structuredContent preservation, probe hints.
**Explicitly does NOT:** ranking (see [/spec-discovery.md](/spec-discovery.md)),
OAuth flows (see [/spec-oauth.md](/spec-oauth.md)), guard caps (see
[/spec-guard.md](/spec-guard.md)).
**If it happens anyway:** those specs own the semantics; this spec owns the
envelope.

## Goals

- **G1:** Every route has a fixed method + path + JSON shape + status
  contract a client can code against.
- **G2:** A loopback daemon with an empty token still rejects foreign
  `Host`/`Origin` and non-JSON state changes.
- **G3:** The shim speaks strict MCP (`initialize/2024-11-05`, `tools/list`,
  `tools/call`, `ping`) with pure-JSONL stdout and 5 static tools.
- **G4:** Non-text content blocks and `structuredContent` survive
  daemon → HTTP → shim → client structurally unchanged.

## Requirements

### Requirement: Route table and envelopes

The system MUST serve exactly these routes (Go 1.22 method patterns;
wrong method → 405 plain text; unknown path → 404; JSON always
`Content-Type: application/json`, newline-terminated; errors
`{"error": msg}`).

**As a** client developer
**I want** a fixed contract
**So that** I code once against `curl` and the shim.

| Method + path | Request | Success | Errors |
|---|---|---|---|
| `GET /servers` | `?filter=` substring of name/description | `{servers: [{name, description, instructions?, healthy?, error?, auth_required?, connected}]}`; instructions only when connected + cached; never spawns | gate only |
| `GET /servers/{name}/tools` | — | `{tools: [{name, description}]}`; denied omitted | 404 server; 401 auth; 502 downstream |
| `GET /servers/{name}/tools/{tool}` | — | `{name, description, inputSchema}`; match case-insensitive; deny checked pre-downstream | 404 server/denied/unknown; 401/502 |
| `POST /servers/{name}/tools/{tool}` | `{parameters: {...}}`, body cap 64 MiB, empty ⇒ nil | `{content: [...], isError, structuredContent?, _meta?}` | 404; 403 policy block; 400 bad JSON; 401; 502 |
| `GET /servers/{name}/resources` | — | `{resources: [{uri, name, description, mimeType}], instructions}`; unsupported ⇒ `{resources: [], instructions}` | 404; 401/502 |
| `GET /servers/{name}/resources/{uri…}` | tail or `?uri=`, exactly one | `{contents: [...]}` | 400 both/neither; `..` ⇒ 502; 401/502 |
| `GET /servers/{name}/prompts` | — | `{prompts: [{name, description, arguments}]}`; unsupported ⇒ `{prompts: []}` | 404; 401/502 |
| `GET /tools` | `?q=&server=&regex=&limit=&offset=` (defaults 12/0; `regex` truthy `1`/`true`; numeric-prefix ints) | `{tools: [{server, name, description, score}], total, nextOffset}`; empty q ⇒ `[]` | 400 unknown server/bad regex |
| `GET /health` | — | `{servers: {name: {connected, healthy?, error?, auth_required?}}}`; error redacted; never spawns | gate only |
| `POST /reload` | — | `{added, removed, modified}` | 500 load failure; GET ⇒ 405 |

Resources/prompts are live passthrough (they spawn cold servers; no disk
cache — bodies are live data). Servers without support return `[]`.
Instructions ride the resource list even for tools-only servers.

#### Scenario: Cold list never spawns

- **GIVEN** a cold server with disk metadata
- **WHEN** `GET /servers`, `/health`, or `/tools?q=` runs
- **THEN** the answer serves from memory/disk with no downstream connect.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-HTTP-001`.

### Requirement: CSRF / token gate

The gate MUST run bearer → Host → Origin → Content-Type BEFORE routing, so
even 404/405 targets can yield 401/403/415.

**As a** loopback-daemon user with no token
**I want** browser pages unable to drive my tools
**So that** visiting a page cannot delete my files.

Rules: bearer — when `token != ""`, require exact
`Authorization: Bearer <token>` (failure 401, no `auth_required`); empty
token means loopback trust. Host — enforced only on loopback binds
(malformed addr fails closed); accept only `localhost`/loopback after
split/lower/bracket-strip (failure 403); non-loopback and `:port` binds
accept any Host (token is the defense). Origin — absent passes; present
must hostname-parse to `localhost`/loopback (failure 403).
Content-Type — state-changing methods with a non-zero body MUST start
with `application/json` (failure 415; forces a preflight the daemon never
grants).

#### Scenario: Foreign page cannot invoke

- **GIVEN** a loopback daemon with empty token
- **WHEN** a page posts JSON with `Origin: https://evil.test`
- **THEN** the daemon answers 403 and no tool runs.

*Definition of Done: If this scenario fails to reject, the system is
broken.*

Verification: `V-HTTP-002`.

### Requirement: Error mapping and probe hints

Downstream `AuthNeededError` MUST map to HTTP 401
`{error, auth_required: "run: oxyp login <server>"}`; ALL other downstream
errors MUST map to 502 with `redact.Body` applied. After a remote
initialize failure (non-auth path), the daemon MUST append a probe hint:
one TLS-configured `GET` (15 s cap, configured headers sent) mapped to
DNS/TLS/refused/timeout/404/401-403/status/html-sniff hints with the URL
redacted (appears only for DNS/host-not-found).

**As a** operator
**I want** "needs login" vs "broken" vs "wrong URL" distinguished
**So that** I fix the right thing.

`/health` errors are redacted; `/servers` health errors pass through raw.

#### Scenario: Dead remote explains itself

- **GIVEN** a remote pointing at a refused port
- **WHEN** initialize fails
- **THEN** the 502 carries `connection refused — is the server running
  and is the port right?`.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-HTTP-003`.

### Requirement: Shim transport and tools

The shim MUST speak `initialize` → `{protocolVersion: "2024-11-05",
serverInfo: {name: oxyp, version}, capabilities: {tools: {}}}`,
`tools/list` → 5 static tools (no pagination), `tools/call`, `ping` → `{}`,
unknown → `-32000`, over pure-JSONL stdout (1 MiB–64 MiB lines, blanks
skipped, undecodable lines silently ignored, every response one JSON
object + `\n`), autostarting/reusing the daemon over HTTP (60 s client,
bearer when set). All errors use code `-32000`.

**As a** MCP client (hermes/omp/opencode)
**I want** the strictest-common-denominator server
**So that** one `{command: [oxyp, shim]}` line works everywhere.

The five tools: `list_servers` (optional `filter`), `list_tools`
(`server` required), `describe_tool` (`server` + `tools[]`;
skips empty/non-strings; N sequential GETs, first failure aborts),
`call` (`server` + `tool`, `parameters` non-map ⇒ `{}`),
`search_tools` (`query` required non-empty; optional `server`/`regex`/
`limit`/`offset`, non-positive numbers ignored). Discovery results are
JSON-in-text; only `call` preserves `structuredContent`/`_meta`.
POST failures render `"<error> (<auth_required>)"`; GET failures embed the
raw daemon body. Path segments are `PathEscape`d; queries use
`url.Values`. The shim itself writes nothing to stderr (daemon drains
child stderr into its log, redacted).

#### Scenario: One-line attach lists tools instantly

- **GIVEN** `{command: ["oxyp", "shim"]}` in any client
- **WHEN** the client runs `initialize` then `tools/list`
- **THEN** 5 tools return with no downstream spawned.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-HTTP-004`, `V-ZOO-005` (shim manual matrix — gap).

### Requirement: Media passthrough

`call` MUST forward the daemon's `{content, isError,
structuredContent?, _meta?}` structurally unchanged: the output guard
rewrites ONLY `mcp.TextContent`; image/audio/resource blocks pass through
untouched (never textified), so vision-capable clients get real media.

**As a** vision-capable client
**I want** real image blocks, not `[image]` placeholders
**So that** multimodal tools stay useful through the gateway.

Go re-marshal means structural (not byte-for-byte) identity.

#### Scenario: Image block survives

- **GIVEN** a tool returning text + image + resource_link
- **WHEN** invoked through daemon → shim
- **THEN** the `tools/call` result carries identical blocks plus
  `structuredContent`/`_meta`/`isError`.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-HTTP-005` (zoo media battery).

## Data Contracts

### Invoke response

| Field | Type | Required | Notes |
|---|---|---|---|
| content | array | yes | `mcp.Content` blocks, verbatim |
| isError | bool | yes | downstream error flag |
| structuredContent | any | no | preserved, never guarded |
| _meta | any | no | preserved, never guarded |

### Policy block (403)

| Field | Type | Required | Notes |
|---|---|---|---|
| error | string | yes | `blocked by policy` |
| details | string | yes | `Resolve` message |
| _meta.oxypPolicy | string | yes | `blocked` |

## State Transitions

| From State | Event | To State | Side Effects |
|---|---|---|---|
| shim detached | first message | daemon ensured | spawn + 5 s wait or reuse |
| downstream ok | invoke | 200 + content | guard text-only, media intact |
| downstream OAuth | any RPC | 401 + hint | `auth_required` surfaced in shim error |
| downstream fail | any RPC | 502 + redacted error | probe hint on remote init |

## Error Handling

| Error | HTTP / RPC | When | Response |
|---|---|---|---|
| gate token | 401 | wrong/missing bearer | `missing or wrong bearer token` |
| gate host/origin | 403 | foreign Host/Origin | `foreign Host/Origin rejected` |
| gate content-type | 415 | non-JSON state change | `state-changing requests require application/json` |
| policy deny | 403 | invoke denied tool | blocked envelope |
| downstream auth | 401 | OAuth needed | error + `auth_required` |
| downstream other | 502 | any other failure | redacted error (+ hint) |

## Security Considerations

- Gate order MUST be bearer → Host → Origin → Content-Type, pre-routing.
- Non-loopback binds MUST rely on the bearer token (Host accepts any).
- `..` URI segments MUST be rejected before downstream reads.
- Invoke bodies MUST be capped (64 MiB); the HTTP server MUST carry
  read/write/idle timeouts.

## Limits

| Constraint | Value | Reason |
|---|---|---|
| Invoke body cap | 64 MiB | bound malicious clients |
| Shim line cap | 64 MiB | bound giant results |
| Probe timeout | 15 s | hint without hang |
| Daemon HTTP client (shim/CLI) | 60 s | patient calls |
| Redirects followed | 4 (5 requests) | loop bound |
