# Anubis Review — oxyp federation subsystem

Mode: Focused module review (federation + immediate contract neighbors).
Scope: `internal/peerfederation/`, `internal/daemon/peers.go`, federation touchpoints in `internal/daemon/daemon.go`, `internal/meta/federation.go` + federation paths in `internal/meta/meta.go`/`mcp.go`, `internal/config/peers.go` + `[daemon.peer_sharing]`/`peer_export` in `internal/config/config.go`, `cmd/oxyp/peers.go` + doctor peer rows, `doc/spec-federation.md`.
Constraint: read-only. No edits, builds, tests, linters, or formatters were run (assignment forbids).

## Threat Model Snapshot

- **Profile**: local-first Go daemon + CLI. HTTP REST on loopback (or wildcard bind), bearer token, bearer-authenticated daemon-to-daemon federation, downstream MCP tool execution (stdio + Streamable HTTP/SSE), owner-side OAuth token stores (`0600`).
- **Assets**: ability to execute downstream tools (shell/file mutations, external side effects); owner OAuth tokens (must never travel); tool output and resource data; the export surface itself (`[daemon.peer_sharing]`, `[servers.*.peer_export]`); peer bearer tokens and the persistent daemon identity.
- **Entry points**: REST (`/servers`, `/servers/{name}/tools/{tool}`, `/resources`, `/prompts`, `/health`, `/peer`, `/federation/v1/*`, `/mcp`, `/reload`, `/servers/{name}/refresh`); CLI (`peers add|remove|list|sync`, `doctor`, `login`); TOML; peer-supplied headers (`X-Oxyp-Instance`, `X-Oxyp-Visited`) and peer JSON (catalog rows, tool arguments/results).
- **Trust boundaries**: local CLI↔daemon (bearer); peer daemon↔daemon (same operator bearer — peers are trusted by design, so `peer_sharing` controls automation, not access); owner↔downstream MCP servers (owner-held tokens/headers); import chain across daemons (visited + instance headers); the sharing filter is keyed on header presence and is bypassable by a modified client (documented).
- **What can execute or mutate**: proxied tool/resource/prompt/completion calls across daemons, config file rewrites by CLI, health/cache/alias state, `identity.json`.
- **Snapshot verdict**: no authentication bypass or cross-tenant-style authorization break found in the federation plane; the serious defects are in freshness/convergence and in synchronization of shared state.

---

## Findings (CRITICAL → HIGH → MEDIUM → LOW)

No CRITICAL findings.

---

### [HIGH] — `/rev` hashes a different catalog view than `/catalog`, driving perpetual resync loops

**Rule**: QUAL-STATE-01
**Location**: `internal/meta/federation.go:172-181` (rev-mode branch), with `internal/meta/federation.go:129` and `internal/meta/federation.go:191-231` (`hashServerRows`)
**Evidence**: CONFIRMED — traced `handleFederationCatalog` → `catalogRows(full=true)` and `handleFederationRev` → `catalogRows(full=false)` into the shared `hashServerRows`. Full mode (a) filters tools through `perms.Resolve(e, t.Name)` at :129 and (b) populates `sf.Resources`/`sf.Templates`/`sf.Prompts` at :147-155. Rev mode appends raw `CachedToolNames(name)` unfiltered (:177) and never populates resources/templates/prompts. `hashServerRows` hashes tool names at :205-213 and resource URIs/template/prompt names at :214-237. Therefore, for any shared server with (i) a tools policy that actually denies a tool, or (ii) at least one resource/template/prompt, `catalog.rev != /rev.rev` for the same state.
**Confidence**: High — both builders and the hash are fully traced; the only convergence the code guarded (health, :156-167) is not the only divergent input.
**Problem**: The importer stores `cat.Rev` from the catalog response (`internal/daemon/peers.go:572-578`). The next ping tick compares the peer's `/rev` hash (`peers.go:143-146`); the hashes cannot match under the conditions above, so every tick adopts the new rev and schedules another sync.
**Impact**: A v1 peer whose owner shares one resource-bearing or policy-filtered server triggers a full catalog fetch every ~10 s, forever: per server that is `ListTools` + `ListResources` + `ListResourceTemplates` + `ListPrompts`, log spam ("peer catalog changed, resyncing"), and repeated spawn attempts for cold servers. The freshness mechanism that is supposed to converge never does. It also masks real changes: the rev is constantly moving for a state that did not change.
**Fix**: Compute both revs from one canonical row builder: make rev mode apply the same `perms.Resolve` filter and read the same cached resource/template/prompt sets (or remove resource/template/prompt names from the hash if they are not meant to be part of the revision contract). Add a test that asserts the `rev` in a `/federation/v1/catalog` response equals the `rev` returned by `/federation/v1/rev` for a server with one resource, one prompt, and a denied tool.
**Scope**: M — Module (two row-building modes + accessor for cached lists, plus test)
**Scheduling**: this change

---

### [HIGH] — Single-flight `SyncPeers` drops requested syncs while committing the rev that requested them

**Rule**: QUAL-STATE-02
**Location**: `internal/daemon/peers.go:400-410` (try-lock drop), `internal/daemon/peers.go:143-156` (rev adopted before the async sync)
**Evidence**: CONFIRMED — `SyncPeers` returns immediately when `d.syncing` is true (:404-408) instead of flagging a rerun. Reload (`cmd/oxyp/main.go:206`) and rev changes (`peers.go:143-156`) both call it asynchronously and discard the result. In the rev path the code commits `cur.rev = rev` *before* scheduling the sync, and `syncOnePeer` only stores the catalog rev when `cur.rev == revStart` (`peers.go:572-578`), so a dropped sync leaves `rev` describing a catalog that was never applied.
**Confidence**: High — control flow and state writes are explicit; only the interleaving is timing-dependent.
**Problem**: Sync requests are not coalesced. A sync requested while another runs is lost, not deferred. Two concrete losses: (1) `POST /reload` right after `peers add` (or at startup, while the initial sync is still working through peers serially, up to 15 s each) returns "reloaded: peer mounts now" but the new peer is not in the running sync's `names` snapshot and no later sync is requested; (2) a tick that observes a changed rev but hits an in-flight sync has already adopted the new rev, so the in-flight sync applies the old catalog (or fails) and no subsequent tick sees a mismatch.
**Impact**: A newly added or changed peer can remain un-mounted/stale until the next manual reload, while its rev claims it is current. For `peers remove`, the reverse: virtuals of the removed peer stay callable (qualified names still resolve in `d.servers`) until a sync finally runs `dropOrphanVirtuals`.
**Fix**: Replace drop-on-contention with coalescing: set a `syncRequested` flag under `syncMu`; the running sync re-runs once more before clearing `syncing`. Keep `rev` as three values (last observed, last applied, pending) and only commit "applied" after `expandBulk`/`expandThin` succeeds. Add a test that starts a slow sync, registers a new peer + reload mid-flight, and asserts the peer mounts after the in-flight sync returns.
**Scope**: M — Module (scheduler state + rev commit ordering)
**Scheduling**: this change

---

### [HIGH] — Peers that fail their first sync never retry: no recovery path to mount

**Rule**: QUAL-STATE-03
**Location**: `internal/daemon/peers.go:116-129` (`peerNamesMissing` skips `pr.down != ""`), `internal/daemon/peers.go:143-146` (resync only when a rev is already known)
**Evidence**: CONFIRMED — `syncOnePeer` calls `markPeerDown` on failure (`peers.go:552`, `peers.go:922-936`) and leaves `pr.rev` empty for a peer that never synced. The only periodic probe is `PingLoop` → `pingConnected`; it fans out peers either via `peerVirt` (connected virtuals only) or `peerNamesMissing`, which skips any peer with `pr.down != ""` and any peer with no `pr.client`. `pingPeer` only calls `Rev` when `wantRev != ""`; a never-synced peer has none, so even if it were pinged there is no sync trigger. No other periodic sync exists (only startup, reload, and rev-change).
**Confidence**: High — every trigger site was traced; no retry timer exists.
**Problem**: A peer that is down/slow when the daemon starts (or when `peers add` runs) is marked down and then never probed again. A peer that was previously mounted but whose virtuals are all cold (no live `m.client`) is likewise skipped after a failed sync. `PingLoop` will not revisit either.
**Impact**: Boot-order-dependent federation failure: if the peer daemon starts after this one, the mount is silently absent forever (until `oxyp peers sync`/restart). The user story in `doc/spec-federation.md` ("a dead/renamed/rotated peer to degrade honestly and recover") is not met for the never-mounted and cold-virtual cases.
**Fix**: (a) include down peers in the ping tick with a backoff (or probe `/health` for them without the rev compare); (b) when a peer's `/health` succeeds and it has never produced a catalog (explicit `synced` flag, not `rev == ""`, since old/thin peers legitimately have no rev), request a sync; (c) add a test where the first sync returns 500 and a later tick with the peer healthy mounts its virtuals.
**Scope**: S — Spot (skip condition + one trigger) — escalates to M if a backoff/retry policy is added.
**Scheduling**: this change

---

### [HIGH] — Published health records are mutated in place, racing every health reader

**Rule**: QUAL-STATE-04
**Location**: `internal/daemon/peers.go:899-917` (`mergeOwnerHealth`), also `peers.go:105-106`, `peers.go:202-207`; reader contract `internal/daemon/daemon.go:424-432` (`HealthOf`)
**Evidence**: CONFIRMED — `HealthOf` returns the live `*Health` pointer after releasing `m.healthMu` (`daemon.go:429-431`). Callers then read fields unlocked and some publish `&h.Healthy` into responses (`internal/meta/meta.go:272`, `internal/meta/federation.go:98-101`, `161-165`; `internal/meta/meta.go:681`). The federation patch adds in-place writers under `healthMu`: `mergeOwnerHealth` assigns `m.health.Error/AuthRequired/Healthy/LastChecked` (:906-917), `pingPeer` assigns `Healthy/LastChecked/Latency` (:202-207), `spawnPeer` assigns `Healthy/LastChecked` (:105-106). These run from `SyncPeers`/`mergeVirtuals` and from the 10 s ping tick, i.e. concurrently with HTTP reads.
**Confidence**: High — Go memory model: unsynchronized read and write of the same `bool`/`string` fields is a data race; `string` headers can tear.
**Problem**: The daemon publishes pointers to mutable health records. The established pattern elsewhere (`recordFailure`, `setHealth`, `Reload`) *replaces* the pointer under the lock; the new code mutates the already-published object. `catalogRows`' quarantine (`savedHealth := HealthOf(...)` then `RestoreHealth`) also assumes the saved pointer is an immutable snapshot, which these writers break.
**Impact**: Data race under normal operation (ping every 10 s + syncs + any `/servers`, `/health`, or `/federation/v1/catalog` request). Outcomes range from inconsistent health JSON (e.g. a new `Error` with an old `Healthy`) to a torn string header and a panic/garbage read. Race detector will flag the daemon immediately; `go test -race` on the federation tests may already do so under load.
**Fix**: Restore the invariant "a published `*Health` is immutable": have `mergeOwnerHealth`, `pingPeer`, and `spawnPeer` build a copy and call `setHealth`; make `HealthOf` return a copy; make `RestoreHealth` store a copy. Any one of the first two removes the race; doing both makes the snapshot semantics explicit.
**Scope**: S — Spot (three write sites + copy on read)
**Scheduling**: this change

---

### [MEDIUM] — Catalog rows advertise stale pre-fill health when a server cannot be listed

**Rule**: QUAL-ERR-01
**Location**: `internal/meta/federation.go:114-127`
**Evidence**: CONFIRMED — `sf.Healthy/Error/AuthRequired` are copied at :98-101 before the fill. `savedHealth` is captured at :114. If `ListTools` fails, the code appends `sf` and `continue`s at :121-126, bypassing both `fillDone()` and the post-fill re-read at :161-165. The comment claims it keeps "the failure's health (real signal)", but the failure was recorded on the daemon's record by `ListTools`, not copied into `sf`.
**Confidence**: High — the error path never reads `HealthOf` again.
**Problem**: For an unlistable server the catalog row reports the pre-fill state (often "healthy" or a nil/unknown health) instead of the just-recorded failure. `ownerHealth` on the importer then clears the virtual's owner error/auth state instead of surfacing it, and the catalog rev hashes the stale health, feeding QUAL-STATE-01's mismatch.
**Impact**: An owner-side failure (spawn failure, upstream list error) is advertised as healthy to every importer; importers drop their last-good error and keep serving last-good tools with a green row. The "stale-honest" guarantee in `doc/spec-federation.md` (G4) is violated on exactly the path that reports failures.
**Fix**: On the `ListTools` error path, re-read `HealthOf(name)` and copy `Healthy/Error/AuthRequired` into `sf` before appending (that is what the comment intends), or compute `sf` health through the same `ownerHealth` helper after the fill in both modes.
**Scope**: S — Spot
**Scheduling**: opportunistic

---

### [MEDIUM] — Empty discovery results never clear importer caches, so last-item removals never propagate

**Rule**: QUAL-STATE-05
**Location**: `internal/daemon/peers.go:649-654` (tools only seeded when non-empty), `internal/daemon/peers.go:782-801` (seed only when key present); resources/templates/prompts/instructions gates at `peers.go:657-675`
**Evidence**: CONFIRMED — `expandBulk` records `metas[q]` only `if len(s.Tools) != 0`, and `instrs`/`bulkRes`/`bulkTmpl`/`bulkPrompts` only when non-empty/non-nil. `seedBulkMeta` applies a value only when the map key exists. A catalog row with `tools: []` (the owner always emits the key), or with omitted `resources`/`templates`/`prompts`/`instructions` (`omitempty`), therefore leaves `m.tools`, `m.res`, `m.templates`, `m.prompts`, and `m.instructions` untouched.
**Confidence**: High — traced both producers and consumers; cache read paths (`ListTools` memory-first, `CachedToolNames`) never revalidate peer virtuals by TTL.
**Problem**: The wire contract cannot distinguish "this server now has zero tools/resources/prompts" from "not reported", and the importer resolves the ambiguity as "keep last-good". Removing the last tool/resource/prompt, or clearing instructions, changes the owner's rev (names disappear), triggers a resync, and is then ignored on apply.
**Impact**: `list_tools`/`describe`/search keep advertising a tool the owner no longer has (subsequent calls fail or are denied); stale resources/prompts/instructions stay served from memory indefinitely (no TTL for peer virtuals). The revision machinery detects the change but the apply step discards it.
**Fix**: Make presence explicit: always seed the tool set (including empty when the owner sent a row that listed successfully), add a `discovery_known`/`complete` marker for resources/templates/prompts, and clear the cached slice/instructions when the owner reports an authoritative empty set. Distinguish the ListTools-failure row (unknown) from a successful empty list. Add a test that removes the last tool on the owner across a sync.
**Scope**: M — Module (catalog contract + seeding semantics)
**Scheduling**: backlog (with a plan)

---

### [MEDIUM] — `peers remove` silently no-ops on a table header with a trailing comment

**Rule**: QUAL-ERR-02
**Location**: `cmd/oxyp/peers.go:310-321` (line scan), `cmd/oxyp/peers.go:335-345` (`isPeerHeader`), call site `cmd/oxyp/peers.go:287-302`
**Evidence**: CONFIRMED — for each line starting with `[`, `isPeerHeader` trims only `[` and a *trailing* `]` (`strings.TrimSuffix(strings.TrimPrefix(header, "["), "]")`). For `[peers.wsl] # office box`, the suffix is not `]`, so `inner` becomes `peers.wsl] # office box`; `cutTomlSeg` yields segment 2 as `wsl] # office box`, which does not equal `wsl`. The header therefore does not start a cut, and the entire peer table (header + body) is preserved.
**Confidence**: High — string handling is explicit; TOML permits comments after table headers.
**Problem**: `oxyp peers remove wsl` prints `removed [peers.wsl] from ...`, reloads the daemon, and returns success, but the peer block survives. No check exists that a block was actually removed. The command's contract (`doc/spec-federation.md`: "cut the table byte-precisely") is broken for hand-edited configs, which are exactly the configs likely to carry comments.
**Impact**: Operators believe a peer is unmounted and its tools removed; the peer keeps mounting and its qualified names keep resolving. Removal appears to require editing the file by hand.
**Fix**: Strip a trailing TOML comment before parsing the header (quote-aware), or parse headers with the TOML scanner; make `removePeerBlock` return an error when no matching header was found. Add a test with `[peers.wsl] # comment` and a commented `[peers.wsl.servers.g]`.
**Scope**: S — Spot
**Scheduling**: opportunistic

---

### [MEDIUM] — Merge alias tie-break is not total; bare winner is nondeterministic when one peer advertises two routes with the same leaf

**Rule**: QUAL-STATE-06
**Location**: `internal/daemon/peers.go:989-999` (comparator), candidates built at `internal/daemon/peers.go:962-976`
**Evidence**: CONFIRMED — `byBare` is built by ranging the Go map `d.servers` (random iteration order). Candidates from the same peer are `{rank: pr.cfg.ServerPriority(cfg.PeerOwner), peer: cfg.Peer}`. Two distinct routes from one peer with the same leaf (e.g. the owner advertises local `db` and pinned `c:db`, giving importer keys `p:db` and `p:c:db`) get equal rank (same peer default) and equal peer name. The comparator returns false in both directions, and `sort.Slice` is not stable, so `cs[0]` — the bare winner — depends on map iteration order.
**Confidence**: High — language semantics; no final comparison on `qualified` exists.
**Problem**: For candidates the comparator treats as equal, the winner is arbitrary and can flip across restarts or syncs. The comment claims "Deterministic tie-break … peers order lexically", but that only orders different peers.
**Impact**: `call(server="db")` can route to a different owner server between daemon runs, violating G2 ("I never wonder which db I got"). The same flapping applies to the hidden/shadowed listing and to which qualified key is hidden by `Names()`.
**Fix**: Make the comparator a total order: after `rank` and `peer`, compare `qualified` (and/or `PeerOwner`) lexically so equal-comparing candidates cannot exist. Add a test with two same-peer routes sharing a leaf over repeated syncs.
**Scope**: S — Spot
**Scheduling**: opportunistic

---

### [MEDIUM] — `pingPeer` reads the peer client outside the daemon lock while reloads swap it

**Rule**: QUAL-STATE-07
**Location**: `internal/daemon/peers.go:131-161`; writer `internal/daemon/peers.go:340-347` (`old.client = c` under `d.mu`)
**Evidence**: CONFIRMED — `pingPeer` reads `pr.client` at :136 and uses `pr.client.Rev`/`Health` at :146/:161 after the initial `d.mu.RUnlock()`. `rebuildPeers` replaces `old.client` under `d.mu.Lock()` when URL/token/TLS change (:340-343). `syncOnePeer` (:518-523) and `spawnPeer` (:70-75) both correctly snapshot `pr.client` under the read lock; `pingPeer` is the outlier.
**Confidence**: High — unsynchronized pointer read vs locked write; the rest of the file establishes the locking pattern.
**Problem**: A ping tick concurrent with a connection rebind can read a torn/stale client pointer and issue `/rev` and `/health` against the old URL/token for that tick. This contradicts the spec's "atomic client swap, no split brain" claim at the health plane, and it is a Go data race.
**Impact**: Transient health attributed to the old owner, spurious rev-mismatch resyncs from the old catalog, and a race-detector failure. Practically bounded to one tick, but it is real shared-mutable-state access.
**Fix**: Snapshot `client`, `rev`, and `instance` under the initial `d.mu.RLock()` into locals (as `syncOnePeer` does) and use them for the whole tick.
**Scope**: S — Spot
**Scheduling**: opportunistic

---

### [MEDIUM] — Transitive-export consent changes never propagate; revocation is not re-checked at the origin

**Rule**: SEC-ABUSE-01
**Location**: `internal/meta/federation.go:86-92` (Transitive set per row), `internal/meta/federation.go:191-231` (`hashServerRows` omits `Transitive`), `internal/daemon/peers.go:1041-1061` (stored `PeerTransitive` drives re-export)
**Evidence**: CONFIRMED — the catalog row carries `transitive`, and importers store it as `PeerTransitive` (`peers.go:621`, `:710`). `/rev` is the only resync trigger for a live peer, and `hashServerRows` hashes name/description/health/tool/resource/template/prompt names only — not `Transitive`. Flipping `[servers.x.peer_export] transitive = false` on the origin does not change the hash, so no middle resyncs; `SharedWithPeer` keeps authorizing re-export from the stored flag (`peers.go:1049-1056`). The origin's own `SharedWithPeer` treats an owner-local server as non-peer-derived, so it never re-evaluates the flag when serving a transitively-originated call.
**Confidence**: High — all three enforcement points were traced; only the "operator expects revocation" intent is inferred from the spec's "both sides veto" / "flipping show never leaks what the origin didn't consent to".
**Problem**: `peer_export.transitive` is enforced once, at import time. There is no revocation channel: the rev hash does not observe the flag, and no call-time check exists at the origin.
**Impact**: An operator who revokes re-export consent keeps leaking the server through every middle that already imported it: the automation surface stays open indefinitely (until an unrelated catalog change or reload triggers a resync). In a show-mode mesh this is the primary guard for transitive exposure.
**Fix**: Include the effective `Transitive` value in the rev hash so consent changes force a resync (and therefore re-evaluation by every middle), and consider re-checking the requesting chain against per-server consent at the origin for defense in depth. If delayed revocation is intentional, document it explicitly and give operators a reload-driven invalidation path in the spec.
**Scope**: M — Module (hash input + optional origin-side check)
**Scheduling**: backlog (with a plan); the hash change alone is S and can ship with the sync fixes.

---

### [MEDIUM] — Peer `timeout` is ignored on most peer-facing RPCs; `Complete` and `GetPrompt` resolve deadlines differently

**Rule**: CONSIST-01
**Location**: `internal/daemon/daemon.go:1751` (`completeOnce`), also `daemon.go:1280`, `1496`, `1550`, `1602`, `1659`; correct sibling `daemon.go:1706` (`getPromptOnce`); spec `doc/spec-federation.md:96-97`
**Evidence**: CONFIRMED — `callToolOnce` (:1984) and `getPromptOnce` (:1706) use `effectiveTimeout(m.getCfg(), d.reqTO)`. `completeOnce`, `listToolsOnce`, `listResourcesOnce`, `readResourceOnce`, `listPromptsOnce`, and `listTemplatesOnce` use `d.reqTO` directly. For peer virtuals the per-server/peer `timeout` is materialized into `ServerEntry.ToolTimeout` (`peers.go:627-630`, `712-715`), so those paths silently ignore it. `doc/spec-federation.md` states peer HTTP clients must run with ctx deadlines from the virtual's materialized `ToolTimeout`.
**Confidence**: High — direct call-site comparison; the spec sentence is explicit.
**Problem**: Two sibling methods added by this patch (GetPrompt and Complete) resolve the virtual's deadline differently, and the list/read/complete family contradicts the spec MUST. Only tools/call and prompts/get honor a configured peer timeout.
**Impact**: A slow owner prompt-completion or resource read fails at the daemon default (10 s) even though the operator configured a larger per-peer timeout for that server; behavior differs across RPC types with no documented reason.
**Fix**: Use `effectiveTimeout(m.getCfg(), d.reqTO)` in every peer-capable RPC path (the five list/read calls plus `completeOnce`), or amend the spec to scope `ToolTimeout` to tool calls only and document the 10 s cap for the rest.
**Scope**: S — Spot
**Scheduling**: opportunistic

---

### [MEDIUM] — `Proxy.ListToolsByPage` does not use the v1 catalog: one thin list plus N describes on every cold path

**Rule**: CONSIST-02
**Location**: `internal/peerfederation/proxy.go:102-121`
**Evidence**: CONFIRMED — `ListToolsByPage` always calls `Rest().Tools` then `Rest().Describe` per tool, ignoring `p.v1`. Sibling methods use the bulk catalog: `ListResourceTemplatesByPage` (`proxy.go:192-210`) and `ListPromptsByPage` (`proxy.go:249-267`) call `Catalog` when v1. The daemon's cache seeding (`seedBulkMeta`) normally covers this, but every config change clears `m.tools` (`peers.go:854-859`), and `RefreshTools` always calls `listToolsOnce`, so the N+1 path is reached in normal operations.
**Confidence**: High — call graph and the sibling pattern are explicit.
**Problem**: A v1 peer exposes the full tool set (with schemas) in one catalog response, but a cold `tools/list` re-fetches the thin list and then performs one describe per tool: 1 + N HTTP round trips against the owner per cold list.
**Impact**: Latency and owner load scale with tool count; a 50-tool peer costs 51 requests where one suffices, after every config change or refresh. This contradicts the "bulk catalog / no N+1" design the rest of the subsystem is built around.
**Fix**: When `p.v1.Load()`, serve `ListToolsByPage` from `Catalog` (falling back to thin + describe on 404), matching the prompt/template siblings.
**Scope**: M — Module (rework one method + fallback)
**Scheduling**: backlog (with a plan)

---

### [MEDIUM] — `doctor --live` omits config-broken and disabled peers, so it can exit green on a broken mount

**Rule**: CONSIST-03
**Location**: `cmd/oxyp/doctor.go:221-231`; data source `internal/daemon/peers.go:324-326` and `internal/daemon/peers.go:292-312`
**Evidence**: CONFIRMED — `rebuildPeers` skips peers with `!p.IsEnabled() || p.ConfigError != ""` (`peers.go:324-326`), so they never enter `d.peersMap`. `PeerStatuses` iterates `d.peersMap` only (`peers.go:302-309`) and hardcodes `Enabled: true`. `runDoctorLive` reports only `PeerStatuses()`. The offline path (`doctor.go:81-92`) iterates `cfg.Peers` and reports disabled/config-error peers.
**Confidence**: High — both doctor paths and the peer map population were traced.
**Problem**: The live doctor's peer rows cover exactly the peers that already made it into the runtime map; a peer with an unreadable TLS file, an invalid field, or `enabled = false` is absent from `--live` output, while the same peer is reported by offline doctor.
**Impact**: `oxyp doctor --live` can print only OK rows and exit 0 while a peer is misconfigured and unmounted. The diagnostic tool presents a false green in the mode operators use to verify a live fleet.
**Fix**: Build the live peer row set from `cfg.Peers` (as offline does) and overlay live state from `PeerStatuses`; report disabled/config-error peers identically in both modes.
**Scope**: S — Spot
**Scheduling**: opportunistic

---

### [LOW] — `/federation/v1/call` accepts `_meta` and silently discards it

**Rule**: ARCH-DEP-01
**Location**: `internal/meta/federation.go:276` (decode), `internal/meta/federation.go:303` (call without meta); producer `internal/peerfederation/proxy.go:150-160`
**Evidence**: CONFIRMED — `Proxy.CallTool` marshals `req.Params.Meta` into `VCall`'s body as `_meta`; `handleFederationCall` decodes it into `body.Meta` and never uses it. `daemon.CallTool` has no meta parameter, so the value cannot reach the downstream `mcp.CallToolRequest`.
**Confidence**: High — the field is decoded and then unused; no other write path exists.
**Problem**: The v1 wire contract advertises request `_meta` forwarding (the client sends it), but the consuming side drops it without error or log. A peer that sends progress tokens or tracing metadata gets no indication it was ignored.
**Impact**: Silent protocol loss for third-party/older clients that populate `_meta`; oxyp's own daemon currently never sets it, so the producer path is dead today and the defect is latent.
**Fix**: Thread `_meta` through `daemon.CallTool` into `mcp.CallToolParams.Meta`, or reject/omit the field in the v1 contract and document that request metadata is not forwarded.
**Scope**: S — Spot
**Scheduling**: whenever touched

---

### [LOW] — `peers remove` rewrites unrelated bytes, violating its byte-precise contract

**Rule**: CONSIST-04
**Location**: `cmd/oxyp/peers.go:322-329`
**Evidence**: CONFIRMED — after cutting lines, the function collapses every run of three or more newlines anywhere in the file (`strings.ReplaceAll(joined, "\n\n\n", "\n\n")` in a loop) and strips all leading newlines (`strings.TrimLeft(joined, "\n")`), then rewrites the file unconditionally at `0600`.
**Confidence**: High — code is explicit.
**Problem**: The spec requires the peer table to be cut "byte-precisely"; unrelated blank-line runs and leading blank lines outside the peer block are modified on every removal.
**Impact**: Cosmetic file churn and diff noise in user configs; a rewritten mode may surprise operators. No functional config change.
**Fix**: Delete only the matched block's line range; do not post-process the rest of the document; preserve the original file mode.
**Scope**: S — Spot
**Scheduling**: whenever touched

---

### [LOW] — Dead/speculative federation API surface

**Rule**: ARCH-DUP-01
**Location**: `internal/peerfederation/proxy.go:43-66`, `internal/daemon/peers.go:245-253`, `internal/peerfederation/identity.go:66-68`, `cmd/oxyp/peers.go:190`
**Evidence**: CONFIRMED — `Proxy.SetV1` has no callers (repo-wide search over `internal/`, `cmd/` shows only the definition); `Proxy.Peer`/`Proxy.Owner` have no callers; `Daemon.peers()` has no callers; `IdentityPath` has no callers; `findDuplicateInstance`'s `selfID` parameter is unused.
**Confidence**: High — repo-wide search; test files do not use them either.
**Problem**: The patch ships exported methods and helpers that nothing calls. `SetV1` in particular documents behavior ("daemon stamps from sync capability") that the daemon does not implement — it relies on `mergeVirtuals`/`serverChanged` to respawn with the new flag instead.
**Impact**: Dead weight and a misleading API contract for future maintainers; `SetV1`'s comment claims a call path that does not exist.
**Fix**: Delete the unused methods/helper/parameter, or wire `SetV1` in `mergeVirtuals` if flipping the route family without a respawn is intended (and document why).
**Scope**: S — Spot
**Scheduling**: whenever touched

---

### [LOW] — Federation spec omits the rev contract and the v1 mutation endpoints

**Rule**: VERIFY-TEST-01
**Location**: `doc/spec-federation.md:128-155` (Data Contracts)
**Evidence**: CONFIRMED — the spec documents `/peer` and `/federation/v1/catalog` only. `GET /federation/v1/rev` (the entire freshness/resync mechanism, including what the hash covers) and `POST /federation/v1/{call,resource,prompt,complete}` (the invocation surface) have no contract; the catalog table omits the `rev` response field and the row fields `resources`/`templates`/`prompts` that the proxy consumes; doctor peer rows (`cmd/oxyp/doctor.go`, added by this patch) have no clause in this spec or `doc/spec-cli.md`.
**Confidence**: High — section read in full; endpoints absent.
**Problem**: The behavior most likely to regress (what the revision hash means and when importers resync) is specified only in code comments. QUAL-STATE-01 and CONSIST-03 are direct consequences of undocumented semantics.
**Impact**: No upstream requirement to test against; reviewers and future implementers cannot tell whether the hash inputs are intentional, and the doctor surface is unpinned.
**Fix**: Add a `/federation/v1/rev` contract (hash inputs, empty-vs-absent semantics, convergence requirement with `/catalog`), the four POST envelopes, the `rev` field and extra catalog row fields, and a doctor peer-row requirement; add the corresponding rows to the spec's verification trace.
**Scope**: M — Module (documentation)
**Scheduling**: with the QUAL-STATE-01/02 fixes

---

### [LOW] — Thin-peer instructions are seeded before the virtual exists, so the first sync drops them

**Rule**: QUAL-STATE-08
**Location**: `internal/daemon/peers.go:733-745`
**Evidence**: CONFIRMED — `expandThin` looks up `d.servers[q]` and writes `m.instructions` inside the per-row loop, then calls `d.mergeVirtuals(...)` which creates missing virtuals afterwards. For a fresh virtual the lookup misses and the instructions are discarded; only a later sync (with an already-existing virtual) can seed them.
**Confidence**: High — ordering is explicit; the bulk path seeds after `mergeVirtuals` via `seedBulkMeta`.
**Problem**: The two expansion paths order seeding differently; the thin path (old peers) loses instructions on the sync that actually mounts the server. Peer proxies never fetch instructions at spawn (`Proxy.Initialize` discards the health result), so there is no second chance until the next sync.
**Impact**: Servers mounted from old peers advertise no instructions in `describe`/listings on first mount, contrary to the thin-path comment ("seed them so describe works cold").
**Fix**: Seed instructions after `mergeVirtuals` (mirror `seedBulkMeta`), or pass them through the `want`/metadata maps like the bulk path.
**Scope**: S — Spot
**Scheduling**: whenever touched

---

### [LOW] — `doctor --live` gives the whole peer set one 15 s budget instead of one per peer

**Rule**: CONSIST-05
**Location**: `cmd/oxyp/doctor.go:220-222`
**Evidence**: CONFIRMED — `lctx` is a single `context.WithTimeout(..., liveBudget)` (15 s) passed to `d.SyncPeers`, which processes peers serially; each `syncOnePeer` derives its own 15 s budget from that parent, so the total is capped at 15 s. `doc/spec-federation.md` (Limits) specifies a per-peer sync budget of 15 s.
**Confidence**: High — context derivation and sequential loop are explicit.
**Problem**: When the aggregate sync exceeds 15 s (several peers, or one slow peer), the remaining peers fail on the expired parent context and are reported as `failed` even though they were never probed.
**Impact**: False negatives in `doctor --live` for healthy peers; an operator may chase an unreachable-peer error that is an artifact of the doctor's budget.
**Fix**: Give each peer its own 15 s budget (fresh `context.WithTimeout` per sync, or per-peer sync calls), or size the parent to `len(syncable peers) * peerSyncTimeout` as a safety net.
**Scope**: S — Spot
**Scheduling**: whenever touched

---

## Consistency Sweep

Majority patterns and the deviants that produced findings:

- **Timeout resolution**: the pre-existing `*Once` helpers use `d.reqTO`; the new `getPromptOnce` (`daemon.go:1706`) and `callToolOnce` use `effectiveTimeout`. Deviant: `completeOnce` plus the list/read helpers ignore the materialized peer timeout (CONSIST-01). No comment states why.
- **v1 discovery**: `ListResourceTemplatesByPage` and `ListPromptsByPage` fetch the bulk `Catalog`; `ListToolsByPage` uses thin list + per-tool describes (CONSIST-02).
- **Doctor peer reporting**: offline iterates `cfg.Peers` (disabled/config-error/ok); live iterates `peersMap` (only healthy-shaped entries), and the live budget is fleet-wide (CONSIST-03, CONSIST-05).
- **Error unwrapping**: `IsNotFound` and `meta.writeDownstreamErr` use `errors.As`; `Proxy.wrap`/`wrapCall` type-assert `*PeerError`. All current call sites pass raw `*PeerError`, so no live defect; not filed, but it will mis-map the first wrapped peer error.
- **Health publication**: `recordFailure`/`setHealth`/`Reload` replace the `*Health` under lock; new federation writers mutate the published object in place (QUAL-STATE-04).

## Systemic Risk

- **Biggest risk**: the federation control plane has no convergence guarantee. The revision hash is computed over a different view than the catalog (QUAL-STATE-01), a rev is committed before its catalog is applied and the sync that would apply it can be dropped (QUAL-STATE-02), and a peer that fails its first sync has no retry path (QUAL-STATE-03). Blast radius: every v1 peer with resources/prompts/denied tools drives a full catalog + downstream RPC fan-out every ~10 s, while genuinely changed catalogs can be missed indefinitely; reload and removal semantics also depend on the droppable sync, so the mounted topology can diverge from the config file.
- **Compounds**: QUAL-STATE-01 + QUAL-STATE-02 + QUAL-STATE-03 + QUAL-ERR-01 + CONSIST-03. The health race (QUAL-STATE-04) and the stale-catalog apply path (QUAL-STATE-05) compound by making the served state inconsistent with both the owner and the daemon's own rev.

## Coverage

- **Analyzed**: `internal/peerfederation/` (client, proxy, visited, identity, errors) in full; `internal/daemon/peers.go` in full; federation touchpoints in `daemon.go` (`Names`/`PeerNames`/`Entry`/`managed`/`Reload`/`spawn`/`clientFor`/health tick/`SweepLoop`/timeouts/`WithPassthrough`); `internal/meta/federation.go` in full; peer gates in `meta.go`/`mcp.go`; `internal/config/peers.go` in full; peer config/validation/deepcopy/secret expansion in `config.go`/`expand.go`; `cmd/oxyp/peers.go`; doctor peer rows; `doc/spec-federation.md` traced against code.
- **Leaves loaded**: security/core, security/identity, security/remote_surfaces, security/execution_and_supply_chain; quality/core, quality/architecture_and_maintainability, quality/verification_and_tooling. Perf leaves were not loaded: performance is not the review focus, and the only latency finding (CONSIST-02) is an N+1 round-trip pattern visible from the code.
- **Not analyzed**: test execution, lint/typecheck/SCA/race detector (assignment forbids builds/tests; none of this is recorded as a pass); OAuth/login internals beyond the peer hint path; shim/CLI command table outside peer rows; live multi-daemon mesh behavior.
- **Unverified (HYPOTHESIS-class)**: operational frequency of the QUAL-STATE-02/03 scheduling windows (structurally proven, not executed); QUAL-STATE-01 observed loop on a live owner (both builders traced; not run); QUAL-STATE-06 observed flip (language semantics; not observed across restarts).
- **Scheduling**: QUAL-STATE-01..04 now (ship with this change); QUAL-ERR-01/02, QUAL-STATE-06/07, CONSIST-01/03 opportunistic; QUAL-STATE-05, SEC-ABUSE-01, CONSIST-02 backlog with a plan; LOW items whenever touched.
- **Confidence**: High on code-path conclusions; Medium on operational likelihood of the two scheduling-dependent races and on the intent behind consent revocation (inferred from spec language).

## Cross-Cutting

- **Merged duplicates**: the two rev-hash divergences (unfiltered tools, unfilled resources/templates/prompts) are one finding — same deviant rev-mode row. Sync-drop and rev-adopt-before-apply are one finding — same commit-before-apply root cause.
- **Conflicting decisions**: `getPromptOnce` vs `completeOnce` deadline resolution; offline vs live doctor peer coverage; bulk catalog in prompt/template proxy methods vs thin list+describe in the tools method.
- **Blind spots**: multi-daemon end-to-end behavior (not executable here); whether request `_meta` forwarding is a deliberate v1 omission (undocumented); whether advertising a merge-shadowed local only via the unlisted `local:` hatch is intended (the code comment explicitly says the local hides and "never listed", so no finding was filed); lock-window interleaving in `rebuildAliases` (snapshot-then-install window was reviewed but not filed — the observable outcome is a transient stale alias repaired by the next sync).
