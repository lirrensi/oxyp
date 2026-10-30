---
node_type: spec
title: Daemon-Held OAuth and Auto-Detect
status: active
updated: 2026-10-30
tags: [oauth, dcr, device, auth]
links:
  depends_on: [/overview.md, /spec-daemon.md]
  documents: [/internal/oauth/, /internal/daemon/login.go]
  verified_by: [/verification/strategy.md]
---

# OAuth Behavior (Daemon-Held)

## Purpose

This spec covers how oxyp owns OAuth for remote MCP servers: four login
flows, Dynamic Client Registration persistence, token storage, refresh, and
automatic detection of OAuth-gated remotes. Tokens live in
`<dir>/oauth/` (0600), owned by the daemon; callers never see them. A
different identity is a second daemon (`--dir`).

## Boundaries

**Handles:** browser (default + `--complete`), device (RFC 8628), token
(pre-seeded verify), client_credentials flows; DCR sidecar + pending carry;
refresh; 401 auto-detect vs explicit `auth = "oauth"`.
**Explicitly does NOT:** static header auth shapes without OAuth (see
[/spec-guard.md](/spec-guard.md) for header forwarding), HTTP envelopes
(see [/spec-http-shim.md](/spec-http-shim.md)).
**If it happens anyway:** non-OAuth header auth is plain config, not this
spec.

## Goals

- **G1:** Default browser login works against DCR-capable IdPs with zero
  client configuration.
- **G2:** Headless boxes can finish login via device flow or pasted-callback
  `--complete`, with DCR credentials surviving cross-process and restart.
- **G3:** Tokens refresh unattended; expiry surfaces `auth_required`, never
  an opaque 502.
- **G4:** Any remote that 401s with `resource_metadata` is treated as OAuth
  even without `auth = "oauth"`.

## Requirements

### Requirement: Common login substrate

`oxyp login` MUST build a throwaway daemon on `<config-dir>/oauth`, run the
flow with a 5-minute context, write token files, and reset the cached
client. Storage keys are the first 32 hex chars of
`sha256(name + "\x00" + url)`. Files: `<key>.json` (token),
`<key>.client.json` (DCR sidecar), `<key>.pending.json` (pending browser
flow). Dir 0700, files 0600. Login requires a remote server whose `auth`
is empty or `"oauth"`. `grant_type = "client_credentials"` takes precedence
over `flow`.

#### Scenario: Login writes daemon-owned token

- **GIVEN** a remote with `auth = "oauth"`
- **WHEN** `oxyp login <name>` succeeds
- **THEN** `<key>.json` exists at 0600 and no token ever prints to stdout.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-OAUTH-001` (zoo IdP battery).

### Requirement: Browser flow (default)

The system MUST offer authorization-code + PKCE login with S256 challenge,
128-bit state, DCR (`client_name: "oxyp"`) whenever config `client_id` is
empty, and a loopback callback server. Default bind is OS-assigned
ephemeral; `callback_port` pins it; a `redirect_uri` WITHOUT `{port}`
names its own fixed port (a `callback_port` mismatch is an immediate
error, not a hang); `{port}` in the template is replaced with the bound
port; empty template means `http://localhost:<port>/callback`. The waiter
MUST register before the URL is shown (no lost-callback race); unknown
states answer 400, duplicates 409. Exchange uses code + state + verifier;
the saved token is issuer-stamped; pending state is cleared and the client
reset.

**As a** laptop user
**I want** one-command browser login
**So that** remote tools light up without hand-editing client secrets.

`client_id` in config skips DCR; configured values always win over
sidecar/pending on refill. DCR request asks
`grant_types: [authorization_code, refresh_token]`,
`response_types: [code]`, `token_endpoint_auth_method: none` unless a
secret exists, plus config scope and optional `resource` (only when
protected-resource discovery yields one).

#### Scenario: Headless finish via --complete

- **GIVEN** a browser login started on a box with no loopback path
- **WHEN** the operator runs
  `oxyp login --complete '<pasted-callback-url>' <name>` in a second
  process
- **THEN** the saved pending (state/verifier/redirect + DCR credentials)
  is loaded, the code exchanges, the token saves, and the pending file is
  cleared.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-OAUTH-002`.

### Requirement: Device flow (RFC 8628)

The system MUST offer `flow = "device"`: discover
`<origin>/.well-known/oauth-authorization-server` (requiring
`device_authorization_endpoint`, `issuer`, `token_endpoint`), use config
credentials or DCR (`grant_types: [device-code, refresh_token]`,
`token_endpoint_auth_method: client_secret_post`), request a device code
with `client_id` + optional `scope` + always `resource=<mcp URL>`, display
verification URI + user code once, and poll (`authorization_pending`
continues, `slow_down` adds 5 s, denied/expired fail; transport errors
double the interval) until `min(expires_in, timeout)` (defaults: interval
5 s, expiry/timeout 300 s).

**As a** headless-box user
**I want** type-the-code login with no browser, no callback socket
**So that** SSH-only hosts can authorize.

Device tokens MUST be issuer-stamped so refresh can bind.

#### Scenario: Device approve lights up tools

- **GIVEN** a server with `flow = "device"`
- **WHEN** the user opens the verification URI, types the code, and the
  poll succeeds
- **THEN** an issuer-stamped token saves and authed list + call work.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-OAUTH-003`.

### Requirement: Token flow (pre-seeded verify)

The system MUST offer `flow = "token"`: require an `Authorization` header
in config (case-insensitive), verify with one probe (ping, else
`tools/list`), report ok vs clear failure, and perform no dance, no token
write, and no refresh. `remoteStack` MUST attach no OAuth handler for this
flow even with `auth = "oauth"` or stored tokens.

**As a** operator with a PAT
**I want** "use this static token" verified in one step
**So that** service setups stay boring.

#### Scenario: Good token verifies, bad token fails clear

- **GIVEN** a server with `flow = "token"` and a static bearer
- **WHEN** `oxyp login <name>` runs
- **THEN** a working header reports ok (no files written) and a dead one
  reports failure without hanging.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-OAUTH-004`.

### Requirement: Client-credentials grant

The system MUST offer `grant_type = "client_credentials"` for service
accounts: require config `client_id` + `client_secret`, discover origin
metadata (only `token_endpoint` required), POST
`grant_type=client_credentials` + optional `scope` + always
`resource=<mcp URL>` with `client_id`, choosing `client_secret_basic`
(RFC 3986 Basic encoding) only when the AS advertises Basic without POST,
else `client_secret_post`; any 2xx with `access_token` saves an
issuer-stamped token (refresh/expiry captured) and resets the client.

**As a** service owner
**I want** no-browser machine login
**So that** daemons authorize themselves.

#### Scenario: Service login without browser

- **GIVEN** a server with `grant_type = "client_credentials"` + id/secret
- **WHEN** `oxyp login <name>` runs
- **THEN** the token endpoint is POSTed with resource binding and the
  token saves.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-OAUTH-005`.

### Requirement: DCR sidecar + pending carry

The system MUST persist DCR registration (`client_id`, `client_secret`,
token-endpoint auth method, issuer) per server as `<key>.client.json`
alongside the token, and MUST carry the same credentials in
`<key>.pending.json` — so `login --complete` works cross-process and
refresh survives daemon restarts even against IdPs that validate
`client_id`.

**As a** headless user
**I want** registration to survive process boundaries
**So that** `--complete` and refresh never fail with `invalid_client`.

Sidecar saves only when `client_id` is non-empty; loads treat
missing/corrupt/empty-ID as absent; refill fills only empty handler
fields (config wins). Cross-process complete prefers the saved
`redirect_uri` (exact bound port) over config.

#### Scenario: Restart refresh keeps client_id

- **GIVEN** a DCR-issued token after daemon restart
- **WHEN** the access token expires
- **THEN** refresh sends the persisted `client_id` and succeeds without
  re-login.

*Definition of Done: If this scenario fails with `invalid_client`, the
system is broken.*

Verification: `V-OAUTH-006`.

### Requirement: Auto-detect vs explicit auth

The system MUST attach an OAuth handler for explicit `auth = "oauth"`,
MUST attach one for `auth = ""` once a non-empty stored access token
exists, MUST attach none for `flow = "token"`, and MUST treat any remote
that 401s with `resource_metadata` as OAuth even without `auth = "oauth"`
(explicit `auth` forces the mapping without metadata).

**As a** operator
**I want** zero-config OAuth discovery
**So that** I only write `auth = "oauth"` when I want to force it.

Spawn-time 401s map (`ErrOAuthAuthorizationRequired` /
`ErrAuthorizationRequired` + metadata-or-forced) to `AuthNeededError`;
mid-session OAuth sentinels map in EVERY RPC path (list, resources, read,
prompts, call) with client reset. All set `auth_required` health and HTTP
401; the stale token file is kept (no auto-delete).

#### Scenario: Undocumented OAuth remote still prompts login

- **GIVEN** a remote with no `auth` key that 401s with `resource_metadata`
- **WHEN** any call runs
- **THEN** health shows `auth_required` and the CLI hint reads
  `run: oxyp login <name>`.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-OAUTH-007`.

## Data Contracts

### Token file (`<key>.json`)

| Field | Type | Required | Notes |
|---|---|---|---|
| issuer | string | yes (except CC w/o metadata) | refresh binding |
| access_token | string | yes | never logged |
| token_type | string | yes | `Bearer` |
| refresh_token | string | no | retained if refresh omits |
| expires_in / expires_at | int / time | no | zero `expires_at` = non-expiring |
| scope | string | no | granted scope |

### DCR sidecar (`<key>.client.json`)

| Field | Type | Required | Notes |
|---|---|---|---|
| client_id | string | yes | refill only when handler empty |
| client_secret | string | no | |
| auth_method | string | no | token-endpoint auth method |
| issuer | string | no | credential issuer |

## State Transitions

| From State | Event | To State | Side Effects |
|---|---|---|---|
| no token | login success | token saved | client reset; handler attaches next spawn |
| token fresh | RPC | authed RPC | bearer/refresh via handler |
| token expired + refresh | RPC | refreshed | new access saved, old refresh retained if omitted |
| token dead / revoked | RPC | auth_required | 401 + hint; files kept |
| browser pending | callback / `--complete` | exchanged | pending cleared, token saved |

## Error Handling

| Error | HTTP | When | Response |
|---|---|---|---|
| OAuth needed | 401 | 401+metadata or mid-session sentinel | `auth_required: run: oxyp login <name>` |
| device denied/expired | CLI fail | `access_denied` / `expired_token` | clear failure, no hang |
| CC missing id/secret | CLI fail | config validation + login check | actionable error |
| callback unknown state | 400 (callback srv) | early/foreign callback | `unknown oauth state` |
| callback duplicate | 409 | second delivery | duplicate rejected |

## Security Considerations

- Tokens and DCR secrets are files at 0600 under a 0700 dir; the daemon
  NEVER prints them; `auth_required` carries only the login hint.
- `redirect_uri` fixed ports MUST conflict-error with `callback_port`
  instead of hanging.
- Refresh MUST bind issuer; empty-issuer tokens MUST NOT attempt refresh.

## Limits

| Constraint | Value | Reason |
|---|---|---|
| Login context | 5 min | bound approval wait |
| Device defaults | interval 5 s, expiry/timeout 300 s | RFC 8628 usability |
| Storage key | 32 hex (sha256 prefix) | stable per name+url |
| File modes | dir 0700, files 0600 | token secrecy |
