---
node_type: spec
title: Federation — Peer Daemons, Virtual Servers, Sharing
status: active
updated: 2026-11-08
tags: [federation, peers, virtual-servers, sharing]
links:
  depends_on: [/overview.md, /spec-daemon.md, /spec-discovery.md, /spec-http-shim.md]
  documents: [/internal/daemon/peers.go, /internal/peerfederation/, /internal/config/peers.go, /internal/meta/federation.go, /cmd/oxyp/peers.go]
  verified_by: [/verification/strategy.md]
---

# Federation Behavior

## Purpose

This spec covers gateway-to-gateway federation: mounting remote oxyp daemons
as virtual servers, proxying discovery and invocation to the owner, merge /
namespace naming, loop guards, export policy, and the `oxyp peers` CLI. It
matters because auth lives where it was earned — a daemon on another host
holds tokens this one can never have — and federation makes those tools
callable without moving secrets.

## Boundaries

**Handles:** `[peers.*]` config, virtual server lifecycle, pinned recursive
naming, merge priority, the visited-chain loop guard, `[daemon.peer_sharing]`
export policy, origin transitive consent, the bulk catalog contract,
duplicate-instance collapse, persistent identity, the peers CLI.
**Explicitly does NOT:** downstream MCP transports (see
[/spec-daemon.md](/spec-daemon.md)), tool ranking (see
[/spec-discovery.md](/spec-discovery.md)), OAuth flows (see
[/spec-oauth.md](/spec-oauth.md)).
**If it happens anyway:** proxy calls ride the same daemon choke points
(`ListTools`/`CallTool`/resources/prompts); transport semantics defer to
their specs.

## Goals

- **G1:** One `[peers.x]` entry mounts N remote servers; calls proxy to the
  owner, which re-runs its own gates; tokens never leave their daemon.
- **G2:** Naming stays predictable: pinned routes compose by prefixing
  (`b:c:gamma`), bare names float to the priority winner, every claimant
  stays reachable qualified.
- **G3:** Loops are impossible by construction (visited chain + hop cap +
  import depth cap + self-mount refusal at handshake and sync).
- **G4:** Sharing defaults closed: locals advertise, virtuals need `show`,
  transitives need origin consent; a dead peer degrades to stale-honest,
  never to a hole.

## Requirements

### Requirement: Peer config and identity

The system MUST support `[peers.<name>]` with `url` (required),
`token` (fail-closed `${VAR}`/`{file:}`), `mode` (`merge` default |
`namespace`), `priority` (lower wins, local = 0), `timeout` (seconds),
`include` (route-or-leaf allowlist), per-server `[peers.x.servers.y]`
(priority/timeout/tools/params), `tls`, and `enabled`. Peer names MUST NOT
contain `:` and MUST NOT be `local`. Each daemon MUST persist a UUIDv4 in
`<dir>/identity.json` (0600, atomic write) and advertise it plus the
optional `daemon.name` on `/health` and `/peer`.

**As a** operator with two daemons
**I want** `oxyp peers add <url>` to handshake and write the entry
**So that** mounting is one command with a preview, not TOML archaeology.

`peers add` MUST refuse self-mount (instance match or loop-508), duplicate
URLs, and duplicate instances (parallel bounded sweep, `--force` overrides);
`peers remove` MUST cut the table byte-precisely; `peers list`/`sync` are
thin views over config/reload.

### Requirement: Virtual servers and naming

The system MUST expand each peer into virtual servers keyed
`peer:route` (route = owner-advertised name, recursively pinned), MUST
resolve bare merge names to the priority winner (local rank 0, lexical
tie-break), MUST hide winner qualified keys from listings, MUST keep every
loser callable qualified, and MUST offer `local:<name>` for peer-beaten
locals. Namespace peers MUST claim no bare name. Import MUST cap qualified
names at 7 colons (ping-pong bound).

**As a** agent caller
**I want** `call(server="db")` to reach the winner and
`call(server="wsl:db")` to reach wsl's, always
**So that** I never wonder which db I got.

### Requirement: Proxying and gates

The system MUST proxy discovery (bulk catalog, thin fallback for old peers)
and invocation (content/structuredContent/_meta preserved) to the owner over
its native REST, MUST re-run owner gates there, MUST enforce importer-local
`tools`/`params` policy here first (either end can deny), MUST carry the
visited chain + instance on every outbound call, and MUST refuse loops (508)
before routing. Peer HTTP clients MUST be `Timeout: 0` with ctx deadlines
from the virtual's materialized `ToolTimeout`.

**As a** security reviewer
**I want** auth to never travel and gates to run on both ends
**So that** a mount is a window, not a key copy.

### Requirement: Sharing and consent

The system MUST advertise locals bare and virtuals pinned (`PeerNames`) to
peers through `[daemon.peer_sharing]`: `hide` (default, locals only) |
`show` (+ virtuals), `allow`/`deny` globs matching route OR leaf (deny
wins), and origin `transitive` consent per leaf (default false — both sides
veto). Hidden servers MUST 404 indistinguishably for header-sending peers
(listings, health, search, direct access).

**As a** daemon owner
**I want** my peers' servers to stay mine unless I opt in twice
**So that** flipping `show` never leaks what the origin didn't consent to.

### Requirement: Liveness and collapse

The system MUST keep last-good virtuals with honest `unreachable` health
when a peer dies, MUST collapse same-instance peers to the lexical winner
(loser `down`, virtuals dropped, revival on winner loss), MUST rebind live
proxies on URL/token/TLS change (atomic client swap, no split brain), MUST
single-flight `SyncPeers`, MUST merge owner-state health per sync (apply or
clear every sync; spawn liveness preserves owner auth/error), and MUST ping
one `/health` per peer per tick fanned out to its virtuals.

**As a** operator
**I want** a dead/renamed/rotated peer to degrade honestly and recover
**So that** I fix the peer, not mysterious ghosts.

## Data Contracts

### `/peer` handshake

| Field | Type | Notes |
|---|---|---|
| instance | string | persistent UUIDv4 |
| name | string | `daemon.name` ("" = anonymous) |
| servers | string[] | shared names from caller's view (pinned for peers) |
| count | int | len(servers) |

### `/federation/v1/catalog`

| Field | Type | Notes |
|---|---|---|
| version | string | `"v1"` |
| instance | string | owner UUID |
| name | string | owner name |
| rev | string | catalog revision (same function as `/rev`; stored by importer) |
| servers | object[] | `{name, description, origin, instructions?, healthy?, error?, auth_required?, connected, tools[], transitive?, resources?, templates?, prompts?}` |
| servers[].tools | object[] | `{name, description, title?, inputSchema, outputSchema?, annotations?, icons?, execution?, _meta?}` (owner-filtered) |

Old peers (404) fall back to thin `/servers` + per-tool describes.

### `/federation/v1/rev`

`{"rev": "<16 hex>"}`: FNV-64a over the caller's view — sorted server rows
(name, description, effective `Transitive`, health markers, tool / resource /
template / prompt names). Tool rows are policy-filtered, matching `/catalog`
exactly; schemas and bodies excluded. INVARIANT: `catalog.rev == /rev.rev`
for the same state (importers resync forever otherwise). Never spawns
(caches only). Consent revocation flips the rev (middles re-evaluate).

### `/federation/v1/{call,resource,prompt,complete}`

| Route | Request | Response |
|---|---|---|
| `POST /federation/v1/call` | `{server, tool, arguments?, _meta?}` | native `{content[], isError, structuredContent?, _meta?}` (blocks verbatim; request `_meta` forwarded downstream) |
| `POST /federation/v1/resource` | `{server, uri}` | native `{contents[]}` |
| `POST /federation/v1/prompt` | `{server, name, arguments?}` | native `{description?, messages[]}` |
| `POST /federation/v1/complete` | `{server, ref, argument, context?}` | native `{completion}` |

Errors reuse the REST mapping (401 auth + `auth_required`, 403 policy/param,
502 downstream). Old peers (404) fall back per-method (thin call/read;
prompts/templates/complete unsupported).

### `/servers` peer rows (additive)

`origin` (`local` | peer name), `shadowed[]` (qualified losers),
`transitive` (re-export consent, omitted false).

### `doctor` peer rows

Both modes iterate config peers: `disabled` / `config: …` identical offline
and live; live overlays `failed` (down) or `ok` (URL, mode, server count,
instance short-id). Live sync budget: one 15s slice per syncable peer.

## State Transitions

| From State | Event | To State | Side Effects |
|---|---|---|---|
| peer configured | sync ok | virtuals mounted | catalog expanded, health applied, aliases rebuilt |
| peer configured | sync fail | virtuals stale + down | last-good kept, `unreachable` health |
| peer removed | sync | virtuals dropped | orphan sweep, aliases rebuilt |
| duplicate instance | sync | loser down | `duplicate of <winner>`, virtuals dropped |
| URL/token/TLS change | reload | proxies rebound | atomic client swap, instance re-probed |
| owner recovers | sync | owner-state cleared | identical catalog still clears stuck error |

## Error Handling

| Error | HTTP / RPC | When | Response |
|---|---|---|---|
| federation loop | 508 | self in visited chain or hop cap | `federation loop refused` |
| unshared server (peer) | 404 | hidden by sharing/consent | `server not found` (no oracle) |
| owner auth needed | 401 | downstream OAuth on owner | `auth_required` + `(on <peer>)` hint; local login refuses |
| owner policy/param deny | 403 | owner gates deny | `blocked` / `param-blocked` envelopes |
| peer down | 502 | proxy dial/call fails | attributed error, stale health kept |

## Security Considerations

- **Trust model: peers are trusted.** Peers hold the same operator bearer
  token, so sharing controls automation (what auto-mounts), not access — a
  modified client can drop `X-Oxyp-Instance` and see the full view. If you
  wouldn't hand a daemon your token, don't peer with it. Scoped peer
  credentials are future work against the first limited-trust peer.
- Auth MUST never travel: owner holds its tokens; only the peer bearer
  token crosses, and only to the peer's own gate.
- `local` and `:` are reserved in daemon/peer/server names; violations fail
  `Load` (daemon/peer) or mark `ConfigError` (servers — see migration note).
- Peer redirects are never followed (headers can't strip mid-hop); peer
  URL paths/queries are stripped (no base-path smuggling).
- `identity.json`, `daemon.json`, `cache.json`, OAuth stores are 0600.

## Limits

| Constraint | Value | Reason |
|---|---|---|
| Visited chain cap | 8 | loop bound (SIP/HTTP Max-Forwards analog) |
| Import depth cap | 7 colons | resync ping-pong bound (one under chain cap) |
| Per-peer sync budget | 15 s | one slow peer's fetch is bounded (peers sync serially, in name order) |
| Sync coalescing | 1 rerun | mid-sync requests defer, never drop |
| Down-peer retry | 30s → 5m backoff | never-mounted peers re-probe on tick via cheap `/health` |
| Peer ping | 1 `/health` + 1 `/rev` per peer per 10 s tick | N virtuals, two probes |
| Peer HTTP client timeout | 0 (ctx governs) | slow tools honor tool_timeout |

## Migration Note (breaking)

Local server names containing `:` are now rejected (`ConfigError`, server
unhealthy but daemon up): `:` is reserved for `peer:server` qualified names.
Rename `[servers."foo:bar"]` to `[servers.foo-bar]` (or similar) and update
callers/groups. `local` is reserved as a peer name and as the `local:`
escape hatch; a `[peers.local]` entry fails validation.
