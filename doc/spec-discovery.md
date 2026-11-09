---
node_type: spec
title: Discovery — Metacache, Search, Permissions, and Views
status: active
updated: 2026-11-09
tags: [metacache, search, permissions, discovery, scope]
links:
  depends_on: [/overview.md, /spec-daemon.md]
  documents: [/internal/metacache/, /internal/search/, /internal/perms/, /internal/daemon/scope.go]
  verified_by: [/verification/strategy.md]
---

# Discovery Behavior (Metacache + Search + Permissions + Views)

## Purpose

This spec covers how oxyp remembers tool metadata (disk cache), how it ranks
searches without spawning, how allow/deny globs hide tools, and how per-client
views hide servers. Together
they deliver the "cold, searchable, deny-means-invisible, one-daemon-many-views"
promise: an agent
can explore hundreds of tools without waking a single downstream server,
a denied tool is invisible everywhere and blocked at invocation, and one
folder's 5-server view never leaks into another's 10-server view.

## Boundaries

**Handles:** cache file format and validity, invalidation triggers,
word/regex ranking, pagination, glob semantics, invisible+blocked
enforcement, per-client server views (`group`/`servers`/`exclude`),
`enabled = false`.
**Explicitly does NOT:** lifecycle/idle (see
[/spec-daemon.md](/spec-daemon.md)), HTTP/shim envelopes (see
[/spec-http-shim.md](/spec-http-shim.md)).
**If it happens anyway:** presentation details defer to the HTTP/shim spec.

## Goals

- **G1:** `list`/`describe`/`search` work cold and survive restarts.
- **G2:** Remote metadata goes stale no later than TTL + one failed
  revalidation; local metadata invalidates on push.
- **G3:** A denied tool cannot be discovered (list/describe/search) and
  cannot be invoked (403), with no oracle distinguishing "denied" from
  "nonexistent" on describe.
- **G4:** No server-level permission misconfiguration can crash the daemon.

## Requirements

### Requirement: Fingerprint-keyed disk cache

The system MUST persist tool metadata in one compact JSON file
`<oxypdir>/cache.json` as
`{"servers": {name: {fingerprint, cachedAt, tools}}}`, where an entry is
usable ONLY on exact fingerprint match. `cachedAt` is informational; disk
entries have no age expiry.

**As a** agent harness
**I want** listings to survive restarts
**So that** cold boot is instant.

The fingerprint is 32 hex chars (128-bit SHA-256 prefix) over command
(`\x00`-joined), URL, transport, sorted headers/env, cwd, inherit_env
(when non-nil), auth, TLS dump, OAuth
`client_id|scope|callback_port|flow|timeout|authorization_url|token_url|registration_url`,
and `hide` flag + effective per-server `tool_timeout`. Excluded: description,
tools, enabled, lifecycle, idle, guard, trace, name, connector origin, OAuth
secret/redirect/grant, `pinned`, `params`, `passthrough_headers`, `token_header`
(presentation-time, not identity). `Put` happens only after a successful live
`tools/list` and is skipped while `pinned`. Every mutation rewrites the whole
file atomically (unique temp + chmod 0600 + rename; saves serialized; errors
silent). Corrupt, missing, unreadable, or `{"servers": null}` files degrade
to an empty cache — never an error, never a partial salvage. Resources,
prompts, and instructions are NEVER persisted.

#### Scenario: Restart reuses metadata

- **GIVEN** a daemon that listed server `db` (2 tools) and then stopped
- **WHEN** a new daemon with identical config lists `db`
- **THEN** the answer comes from `cache.json` with no child spawned.

*Definition of Done: If this scenario passes, the requirement is met.*

#### Scenario: Command change invalidates

- **GIVEN** a cached server whose `command` changed in config
- **WHEN** the daemon reloads
- **THEN** the stale disk entry is dropped and the next list goes live.

*Definition of Done: If stale tools were served, the system is broken.*

Verification: `V-DISC-001`.

### Requirement: Pinned snapshots and hidden newcomers

`pinned = true` freezes discovery to the cache snapshot: `ListTools` serves
memory/disk only and never goes live to refresh (disk seeds memory once when
empty; live `storeTools` still fills memory but never writes disk). `doctor
--live` diffs the live catalog against disk and reports
`added=[...] removed=[...] (pinned: serving cache)`; empty when clean or when
no cache baseline exists.

`new_tools = "hide"` serves only tools already in the cache snapshot: the first
seed stores the full live list; afterwards disk always wins (cold, no live
fetch), stale-remote revalidation intersects the live result against disk and
never stores newcomers, and the no-baseline live path intersects too. `doctor
--live` reports the same drift shape with `(hidden by new_tools=hide)`.
Default `new_tools = ""`/`expose` preserves the old behavior (newcomers flow).

Both compose with deny-means-invisible (deny applies after pin/hide at every
presentation boundary) and with `Reload` (generation-guarded stores still
apply; fingerprint covers the `hide` flag so flipping it re-seeds).

Refresh is the explicit thaw: `oxyp refresh <server>` (`POST
/servers/{name}/refresh`) forces one live fetch and stores it as the new
baseline — memory AND disk, no hide-intersect, no pin skip — and returns
`{tools, added, removed}` (live vs old baseline). Freezing never self-heals:
enabling the freeze makes the operator responsible for refreshing it. A
failed refresh changes nothing; a successful one that returns fewer tools
narrows the snapshot (the operator's foot-gun: fix upstream, refresh again).

#### Scenario: New tool stays hidden

### Requirement: Invalidation triggers

The system MUST invalidate tool metadata on config fingerprint change /
removal / disable, on `tools/list_changed` push (stdio + SSE remotes),
on client reset (session expiry, OAuth failure), and MUST revalidate
remote memory after a 5-minute TTL with stale-on-failure.

**As a** operator
**I want** metadata that tracks reality without polling
**So that** deploys and upstream changes propagate.

Details: `DropStale` runs at construction and reload (enabled servers
only — disabling drops the entry). `list_changed` clears memory and drops
disk but keeps the connection — EXCEPT pinned servers keep their disk
snapshot (a pin means frozen; only refresh may thaw it, so push clears
memory and the next list goes live, but the frozen baseline survives a
restart). `resources/list_changed` and `prompts/list_changed` clear only
memory resource/prompt caches.
Streamable-HTTP remotes have no push channel in this stack, so the TTL is
their fallback: memory older than 5 min triggers one live revalidation;
success stores (generation-guarded) and marks healthy, failure serves
stale memory and retries next call. A disk hit re-arms a fresh 5-minute
memory window (disk age itself is ignored). `SearchTools` never
revalidates and never spawns — remote search metadata can lag past the
TTL. Idle stop and `Close` never drop caches.

#### Scenario: Upstream push invalidates local tools

- **GIVEN** a connected stdio server that emits `tools/list_changed`
- **WHEN** the notification arrives
- **THEN** memory is cleared and the disk entry dropped (unpinned); pinned
  servers clear memory only — disk survives. The next list goes live while
  the connection stays.

*Definition of Done: If this scenario passes, the requirement is met.*

#### Scenario: Stale remote serves stale, then retries

- **GIVEN** a remote whose memory is 6 min old and whose live revalidation
  fails
- **WHEN** `ListTools` runs
- **THEN** stale tools are served and the next call retries live.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-DISC-002`, `V-ZOO-001` (notify-changed), `V-ZOO-002`
(session-expiry re-init).

### Requirement: Word-ranked search

The system MUST rank cached tools per query word with exclusive tiers:
tool-name substring +3, else server-name/server-description substring +2,
else tool-description substring +1; duplicate words score repeatedly;
zero-score candidates are dropped; ties sort by server then name (stable).

**As a** agent
**I want** "database backup" to surface `db/backup now` above prose
mentions
**So that** I find the right tool in one query.

Matching is case-insensitive substring (`cod` matches `search_code`), OR-ish
across words (any positive sum returns). Search covers memory tools, else
disk hits at the current fingerprint; unreachable/uncached/config-error
servers are skipped; denied tools are removed BEFORE ranking. A named
`server=` filter scopes to one server (unknown → error); empty scopes to
all enabled servers. Empty queries return nothing in word mode.

#### Scenario: Name hit outranks description hit

- **GIVEN** tools `db/backup-now` ("does things") and `misc/x` ("backup
  helper")
- **WHEN** searching `"backup"`
- **THEN** `db/backup-now` (score 3) sorts above `misc/x` (score 1).

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-DISC-003`.

### Requirement: Regex search

The system MAY offer unranked case-sensitive Go-RE2 regex search over
`server/name + description` (server description excluded), all scores 0,
sorted by server then name; invalid patterns MUST error.

**As a** power user
**I want** precise pattern search
**So that** I can audit tool surfaces mechanically.

Empty pattern matches every cached allowed tool.

#### Scenario: Regex filters mechanically

- **GIVEN** cached tools `db/a1` and `web/b2`
- **WHEN** searching `^db/` with regex on
- **THEN** only `db/a1` returns with score 0; a bad pattern errors.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-DISC-004`.

### Requirement: Pagination

`Page(res, limit, offset)` MUST default `limit <= 0` to 12, clamp negative
offsets to 0, return the absolute next offset or `-1` terminal on a
non-empty page reaching the end, and MUST return `next = len(res)` (not
`-1`) when offset is past the end or input is empty.

**As a** client
**I want** stable paging
**So that** I can walk large catalogs without loss or loops.

The HTTP envelope is
`{"tools": [{server, name, description, score}], "total": N,
"nextOffset": X}` with `tools` never null. Limit/offset parse from query
with numeric-prefix semantics (`12abc` → 12, unparsable → default).

#### Scenario: Walk a 5-tool catalog in 2s

- **GIVEN** 5 ranked results
- **WHEN** paging limit 2 from offset 0
- **THEN** pages are `[2, next 2]`, `[2, next 4]`, `[1, next -1]`; offset 5
  returns empty with next 5.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-DISC-005`.

### Requirement: Permissions — deny wins, invisible + blocked

The system MUST resolve tool visibility with anchored case-insensitive
`*`/`?` globs where: nil server or nil tools block → allow; any deny match
→ deny; empty allow → allow (deny-list mode); any allow match → allow;
otherwise → deny (default-deny allow-list mode).

**As a** operator
**I want** one deny rule to beat every allow
**So that** `deny = ["delete_*"]` is airtight.

`*` collapses runs (`**` ≡ `*`), `?` matches one rune, all other
characters are literal, and `*`/`?` do not cross newlines. Enforcement:
list omits denied tools; describe checks deny before touching downstream
and answers 404 indistinguishable from unknown; invoke answers 403
`{"error": "blocked by policy", "details": …, "_meta":
{"oxypPolicy": "blocked"}}`; search excludes denied candidates pre-rank.
Raw daemon caches are unfiltered — filtering applies at each presentation
boundary — so permission edits take effect without cache invalidation.

#### Scenario: Deny beats allow

- **GIVEN** `allow = ["*"]`, `deny = ["delete_*"]`
- **WHEN** listing, describing, searching, or invoking `delete_rows`
- **THEN** it is absent from list/search, describe 404s, and invoke 403s.

*Definition of Done: If this scenario fails to hide or block, the system
is broken.*

#### Scenario: Missing tools block allows all

- **GIVEN** a server with no `[tools]` block
- **WHEN** any tool is listed or called
- **THEN** everything is allowed.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-DISC-006` (incl. mutation: deny-branch removal is caught).

### Requirement: Per-tool parameter allowlists

`[servers.x.params]` maps tool name → allowed argument names. When a tool has
an entry, `CallTool` rejects any call carrying an argument outside the list
with `ParamDeniedError` (exact, case-sensitive match; no entry = all
parameters allowed; empty args always pass). The REST envelope is 403
`{"error": …, "allowed": [...], "_meta": {"oxypPolicy": "param-blocked"}}`;
shim and `/mcp` surface it as an `isError` tool result. Params are
presentation-time (excluded from the fingerprint) so edits take effect
without cache invalidation.

#### Scenario: Extra argument blocked

- **GIVEN** `params.create_issue = ["title", "body"]`
- **WHEN** calling `create_issue` with `{title, body, labels}`
- **THEN** the call 403s naming `labels` and the allowed list.

*Definition of Done: If the call executes, the system is broken.*

Verification: `V-DISC-008` (new: params gate + 403 envelope).

### Requirement: Opt-in per-request header passthrough

`passthrough_headers` (remote-only) allowlists incoming HTTP request headers
forwarded upstream per-request: the meta handler stashes whitelisted values
(`daemon.WithPassthrough`, 8 KiB cap) into the call ctx, and the transport
`headerFunc` injects them on every downstream request. Config `Headers` win
on conflict; `Authorization` is never overridden (OAuth owns it). Applies to
list/describe/call over Streamable HTTP and SSE; the `/mcp` gateway does not
forward (no HTTP headers in its handler ctx). Discovery caching stays
per-server, not per-credential.

#### Scenario: BYO credential reaches upstream

- **GIVEN** `passthrough_headers = ["X-Team-Token"]` and an incoming
  `X-Team-Token: t` on `POST /servers/s/tools/call`
- **WHEN** the downstream call runs
- **THEN** upstream sees `X-Team-Token: t` unless config `headers` sets it.

*Definition of Done: If the header is dropped or overrides config, the
system is broken.*

Verification: `V-DISC-010` (new: passthrough precedence unit).

### Requirement: Per-client server views

`group` + `servers` + `exclude` MUST compose as one ANDed presentation filter
(`daemon.InScope`): denied tools stay hidden (this spec), out-of-scope servers
stay hidden (HTTP spec). Search excludes out-of-scope hits post-page (total
stays pre-filter, same as group); list skips them; single-server routes 403.
Scope is presentation-time only — excluded from the fingerprint, no cache
invalidation needed, no spawn behavior change (a hidden cold server stays
cold; a hidden connected server stays connected, just invisible to this
request).

#### Scenario: Hidden server stays hidden but healthy

- **GIVEN** servers `a` (connected) and `b` (cold) with `?exclude=b`
- **WHEN** listing, searching, or calling `b`
- **THEN** `b` vanishes from list/search and 403s on call, while `a` still
  works and `b`'s connection (if any) is untouched.

*Definition of Done: If scope drops connections or needs a reload, it's broken.*

Verification: `V-DISC-012` (scope is view-only).

## Data Contracts

### Cache entry

| Field | Type | Required | Notes |
|---|---|---|---|
| fingerprint | string | yes | 32 hex, identity hash |
| cachedAt | string | yes | RFC3339Nano, informational |
| tools | array | yes | full `mcp.Tool` records |

### Search result

| Field | Type | Required | Notes |
|---|---|---|---|
| server | string | yes | owning server |
| name | string | yes | tool name, verbatim |
| description | string | yes | tool description |
| score | int | yes | ranked sum, or 0 in regex mode |

## State Transitions

| From State | Event | To State | Side Effects |
|---|---|---|---|
| no cache | successful live list | memory + disk cached | file rewritten atomically |
| cached | fingerprint change / remove / disable / push / reset | uncached | memory nilled, disk dropped |
| cached (remote memory) | age > 5 min on ListTools | revalidating | live list or stale-serve |
| cached | idle stop / close | cached | no invalidation |

## Error Handling

| Error | HTTP | When | Response |
|---|---|---|---|
| unknown named server / bad regex | 400 | `GET /tools` filter/regex invalid | `{"error": …}` |
| denied tool invoke | 403 | policy deny | blocked-by-policy envelope |
| denied/unknown describe | 404 | policy deny or unknown | `tool not found` (no oracle) |

## Security Considerations

- Denied tools MUST be invisible in list/describe/search AND blocked on
  invoke; describe MUST NOT distinguish denied from nonexistent.
- Permission changes MUST NOT require cache invalidation to take effect.
- Cache files hold tool metadata (names, schemas); the file is 0600.

## Limits

| Constraint | Value | Reason |
|---|---|---|
| Remote memory TTL | 5 min | staleness bound without push |
| Search default limit | 12 | bounded pages |
| Fingerprint | 128-bit | collision-safe identity |
| Cache write mode | 0600 | metadata secrecy |
