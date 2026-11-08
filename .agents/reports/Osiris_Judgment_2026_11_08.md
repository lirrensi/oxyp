# Osiris Judgment — oxyp federation — 2026-11-08

Scope: `internal/peerfederation/`, `internal/daemon/peers.go`, `internal/meta/federation.go`
(+ peer-view branches of `internal/meta/meta.go`), `internal/config/peers.go`, `internal/config/expand.go`
(peer secrets), `cmd/oxyp/peers.go`, and their `_test.go` files. Contract: read-only judgment +
test plan; **no tests written, no code edited**.

## 0. Method and evidence

### Derivation (blind)
A spec-only scout (`RawPigeon.BlindDeriver`, forbidden from code paths) derived **155 atomic
requirement clauses** from `doc/spec-federation.md` in groups REQ-CFG, REQ-NAME, REQ-PROXY,
REQ-SHARE, REQ-LIVE, REQ-CLI, REQ-SEC, REQ-LIMIT, REQ-DATA, REQ-ERR, REQ-STATE. Clause IDs used
below are that scout's. The main agent then mapped clauses to code and to existing tests
(implementation-aware mapping; every gap below is labeled with the clause it descends from).

### Repo profile (flags)
- `HAS_BACKEND` — Go HTTP daemon + CLI (`internal/meta/meta.go:55-86` route table).
- `HAS_API` — REST contract (`/federation/v1/*`, `/peer`, `/servers` peer rows).
- `HAS_EXTERNAL_SERVICES` — gateway-to-gateway REST calls (`internal/peerfederation/client.go`).
- `HAS_ASYNC` — async `SyncPeers` on reload/start, ping tick (`cmd/oxyp/main.go:206,247`; `internal/daemon/daemon.go:2034-2095`).
- `HAS_AUTH` — peer bearer, owner-side OAuth gates, sharing/consent.
- `HAS_CLI` — `oxyp peers add|list|remove|sync` (`cmd/oxyp/peers.go`).
- `HAS_SECRETS_ROTATION` — peer token rebind, identity persistence.
- `HAS_FILE_STORAGE` — `identity.json`, config rewrite.
- No frontend, no mobile, no database, no multi-tenancy.

### Baselines run (read-only, mutates nothing)
```
go test -count=1 ./internal/peerfederation/ ./internal/config/ ./cmd/oxyp/ \
    -run 'Peer|Federation|Client|Visited|Bad|Qualify|Remove|Same'      -> ok (0.09s / 0.01s / 0.02s)
go test -count=1 ./internal/daemon/ -run 'TestPeer|TestCollapse|TestRebind|TestSync|TestBulk|TestV1|TestOwner|TestShared|TestTransitive|TestImport|TestThin|TestOrigin|TestPeerTimeout|TestPeerSlow|TestPeerLocal|TestPeerMerge|TestPeerNamespace|TestPeerInclude|TestPeerDown|TestPeerSelf|TestPeerReload|TestPeerFingerprint' -> ok (18.33s)
go test -count=1 ./internal/meta/ -run 'TestFederation|TestPeer|TestHealth|TestWriteDownstream|TestServers' -> ok (0.08s)
```
Suite is green at baseline. Federation-scope defenders: **46 test functions**
(peerfederation 4, config 8, daemon 25, CLI 3, meta peer-related 6).

## 1. Verdict

The federation suite proves the **happy shape**: merge expansion, priority-vs-local, namespace
mode, include, pinned transitive routes, bulk catalog + thin fallback, media/`structuredContent`/
`_meta` fidelity on the bulk call path, owner-health apply/clear, duplicate collapse, URL rebind,
down-peer stale retention, and the sharing matrix at the `/servers` surface.

It does **not** defend the paths that make federation *safe and operable*:

- the loop guard is exercised only as pure functions, never through the HTTP/spawn boundary (G3);
- the owner-error taxonomy (401 auth hint / 403 blocked / 403 param-blocked / 502) is untested on
  both producer and consumer sides;
- the visited chain is never proven to cross the daemon boundary;
- the liveness tick (`pingPeer`) has **zero** tests (G4 stale/down honesty);
- the rev contract (`/federation/v1/rev` ↔ catalog `rev`) is untested and, on code reading,
  **diverges for any server with a tool-policy deny** (`[INFERENCE]`, see F6) — the importer's
  resync-on-rev-mismatch would then fire every tick;
- `oxyp peers add` — the "one command" of G1 — is untested end to end;
- importer-local *invoke* gates (tools/params) are proven for discovery only;
- sharing is untested on the search surface and on the federation endpoints.

Clause tally from the 155-clause derivation (mapped): **62 covered (40%)**, **32 partial (21%)**,
**47 uncovered (30%)**, **10 design (6%)**, **4 N/A (3%)**.

Verdict: **not verified against the federation spec**. The naming/merge/mount core is Armed;
the safety, error and liveness surfaces are **Paper Tiger** (mutation probe below: predicted
8/20 = 40% kill on a deliberately suspicious sample). Do not mark federation traceability rows
`verified` until the Critical/High batches land.

## 2. Behavior ↔ test matrix

Clause groups (IDs per REQ group; "full" = asserted in every stated dimension, "partial" = one
dimension asserted, "gap" = no test).

| Clause group | Full | Partial | Gap | Design/N-A | Defending tests |
|---|---|---|---|---|---|
| REQ-CFG (25) | 1,2,3,5,6,8,9,10,11,12,18,19,24 (13) | 7,13,14,23 (4) | 4,15,17,20,22,25 (6) | 16,21 (2) | `TestPeerLoadAndDefaults`, `TestPeerValidation`, `TestPeerSecretsFailClosed`, `TestPeerMergeExpansion`, `TestPeerMergePriority`, `TestPeerNamespaceMode`, `TestPeerIncludeFilter`, `TestTransitivePinnedRoute`, `TestPeerSlowToolRespectsTimeout`, `TestPeerHandshake` |
| REQ-NAME (11) | 1,2,3,6,8 (5) | 4,7,10 (3) | 9,11 (2) | 5 (1) | `TestPeerMergeExpansion` (names/shadow/qualified), `TestPeerMergePriority`, `TestPeerNamespaceMode`, `TestTransitivePinnedRoute`, `TestImportDepthCap` (thin) |
| REQ-PROXY (14) | 1,2,3,4,5,6,11,12,14 (9) | 9,13 (2) | 7,8,10 (3) | — | `TestBulkCatalogSync`, `TestThinFallbackOldPeer`, `TestV1ProxyFidelity`, `TestPeerSlowToolRespectsTimeout`, `TestClientRoundTrip`, `TestPeerSelfMountRefused` |
| REQ-SHARE (18) | 1,2,3,4,5,8,9,10,11,13,14,16,18 (13) | 6,7,12,17 (4) | 15 (1) | — | `TestSharedWithPeerMatrix`, `TestOriginConsentVeto`, `TestPeerNamesAdvertisePinned`, `TestPeerSharingFiltersServers` (meta) |
| REQ-LIVE (16) | 1,2,3,4,5,6,7,13,14 (9) | 10 (1) | 8,11,12,15,16 (5) | 9 (1) | `TestPeerDownKeepsStale`, `TestCollapseDuplicatePeers`, `TestRebindOnURLChange`, `TestOwnerHealthMergeAndClear`, `TestSyncReloadRace` |
| REQ-CLI (11) | — | 9 (1) | 1,2,3,4,5,6,8,10,11 (9) | 7 (1) | `TestRemovePeerBlock`, `TestIsPeerHeader`, `TestSamePeerURL` (helpers only) |
| REQ-SEC (15) | 5,13 (2) | 4,6,7 (3) | 1,2,3,8 (4) | 12,14,15 (3), 9,10,11 N/A (3) | `TestDaemonNameValidation`, `TestPeerSharingFiltersServers` |
| REQ-LIMIT (6) | 6 (1) | 1,2 (2) | 3,4,5 (3) | — | `TestVisitedChain` (pure), `TestImportDepthCap` (thin), `TestPeerSlowToolRespectsTimeout` |
| REQ-DATA (19) | 2,5,6 (3) | 1,3,4,7,9,12,13,15 (8) | 8,11,14,16,17,18 (6) | 10 (1), 19 N/A (1) | `TestPeerHandshake`, `TestFederationCatalog`, `TestFederationV1RoundTrip`, `TestV1ProxyFidelity` |
| REQ-ERR (11) | 3 (1) | 9,10,11 (3) | 1,2,4,5,6,7,8 (7) | — | `TestPeerSharingFiltersServers` (direct 404) |
| REQ-STATE (9) | 1,2,3,4,6,9 (6) | 5 (1) | 7 (1) | 8 (1) | `TestPeerReloadSurvives`, `TestCollapseDuplicatePeers`, `TestOwnerHealthMergeAndClear` |

Uncovered/partial clauses map to findings in §5 (each finding lists its clause IDs).

## 3. The Six Lenses

### Lens 1 — Deletion immunity (what breaks if deleted)
- `internal/daemon/peers.go:131-210 pingPeer` and `:116-128 peerNamesMissing` — deleting either
  changes no test outcome. **Undefended** (F7).
- `internal/peerfederation/identity.go:23-63 LoadOrCreateIdentity` — deleted → green suite (F12).
- `cmd/oxyp/peers.go:30-224 peersAddCmd/findDuplicateInstance`, `:227-260 peersListCmd`,
  `:373-390 peersSyncCmd` — deleted → green suite (F8, F31).
- `internal/meta/federation.go:129-131` (perms filter in catalog), `:191-241` (rev hash body),
  `internal/meta/meta.go:117-125` (inbound loop guard), `:492-495` (search peer filter) —
  each deletable with a green suite (F1, F4, F5, F6).
- `internal/peerfederation/proxy.go:328-347 wrapPeer` — branch-level deletion survives (F2).
- `internal/daemon/peers.go:381-394 peerConnEqual` token/TLS comparison — only URL is defended (F9).
- Trivial accessors (`Peer`, `Owner`, `Close`, `SetLevel`, `ShortID`, `PeerError.Error`) are N/A —
  no branch, no observable decision (skill: do not test trivial accessors).

### Lens 2 — Assumption audit (assumed but never verified)
| Assumption | Where | Status |
|---|---|---|
| A peer that already appears in the caller's chain is never a target | `peers.go:80-89`, `meta.go:117-125` | only pure-function tested (F1) |
| The inbound chain survives `Handler → meta → daemon → proxy → HTTP` | `meta.go:120-129`, `daemon.go:1905`, `client.go:170-180` | untested (F3) |
| Owner 401/403 envelopes decode into typed errors and keep their meaning at the importer | `proxy.go:328-347`, `meta.go:603-661` | untested (F2) |
| The catalog is the owner's *complete, current* tool set | `federation.go:128-170`, `peers.go:649-655,765-810` | fails for emptied list `[INFERENCE]` (F20) |
| Catalog `rev` and `/rev` are the same function of state | `federation.go:70-183,255-259` | untested; diverges under deny `[INFERENCE]` (F6) |
| One `/health` probe per peer per tick, fanned out | `peers.go:131-210`, `daemon.go:2078-2094` | untested (F7) |
| A sync started twice coalesces | `peers.go:400-415` | untested (F18) |
| Peer identity is stable across restarts | `identity.go:23-63` | untested (F12) |
| Removing one advertised server removes its virtual | `peers.go:828-870` | untested (F21) |
| `peers remove` touches only the peer table | `cmd/oxyp/peers.go:305-333` | untested; not byte-precise (F22) |

### Lens 3 — Edge case flood (values the suite never sees)
`""` peer name/URL, unicode peer names (`wsl-🔥`) in TOML keys and `peers add --as`,
`:` inside `--as`, name `local`, include/deny/allow with `:`-only globs, 7/8/9-entry visited
headers with duplicate ids, empty `X-Oxyp-Visited` vs whitespace, priority `math.MinInt`,
timeout `0` vs negative, `tools: []` vs absent in catalog rows, catalogs with 0 servers,
`/peer` with 0 shared servers, identity.json empty/corrupt/0644/readonly, `{file:}` secret pointing
at a directory, URL with userinfo/query/fragment, redirect 301/302/307, 64 MiB+1 body.

### Lens 4 — Death by a thousand users (scale and saturation)
- N peers × 15 s are serialized in `SyncPeers` (`peers.go:400-434`): "one slow peer can't stall
  the fleet" is a spec claim with no test and no concurrency (F11 `[INFERENCE]`).
- 1 probe per peer per tick exists, but fan-out to N virtuals is untested (F7).
- `peers add` duplicate sweep is bounded/parallel (`peers.go:190-224`) — no test (F8).
- 10k-tool catalogs / 64 MiB bodies: untested; cost is a noted N/A (see §8).
- Cache growth: `seedBulkMeta` writes memory + disk each sync; stale-tool retention (F20).

### Lens 5 — Chaos monkey (mid-operation failure)
| Chaos point | Current defender | Gap |
|---|---|---|
| Peer dies after a good sync | `TestPeerDownKeepsStale` | call still returns attributed 502 + stale health — untested (F2) |
| Peer returns 500 on catalog but thin works | none (`peers.go:534-556`) | degraded-mount path untested (F19) |
| Peer returns malformed JSON per endpoint | none | error wrapping untested (F34 → N/A cost) |
| Owner restarts with a new instance mid-session | self-mount test only | instance change / loop guard untested (F1) |
| Token rotated on reload | none | split brain (F9) |
| Identity file corrupt/partial | none | new id semantics untested (F12) |
| Redirect from peer | none | token leak boundary (F23) |
| Owner empties a tool list | none | stale catalog (F20) |
| Owner drops one server | none | ghost virtual (F21) |

### Lens 6 — Mutation chamber
See §6. Predicted survivors are the gap list; every survivor names a finding.

## 4. Two-pass discipline

Every requirement needs a **normal** pass (intended function) and a **robustness** pass (error,
abort, recovery). The suite almost uniformly has normal only:

- Normal proven, robustness missing: loop guard (F1), error taxonomy (F2), chain carry (F3),
  search/federation sharing gates (F4), catalog filtering (F5), rev (F6), liveness (F7),
  CLI (F8), token rebind (F9), invoke gates (F10), sync budget (F11), identity failure modes (F12).
- Both proven: collapse/revive (`TestCollapseDuplicatePeers`), owner health apply/clear
  (`TestOwnerHealthMergeAndClear`), down-peer stale retention (`TestPeerDownKeepsStale`),
  thin fallback (`TestThinFallbackOldPeer`), URL rebind (`TestRebindOnURLChange`),
  importer tools policy for discovery (`TestPeerLocalPolicy`), consent veto (`TestOriginConsentVeto`).

## 5. The Condemned — findings

Each finding: gap, existing coverage, evidence, impact, test-to-write (fixture + one assertion +
oracle), severity, parent clause IDs. "Would fail" findings were reasoned from code, not executed
(read-only contract) and are marked `[INFERENCE]` where the failure needs runtime proof.

---

### F1 — Loop refusal has no end-to-end defender (inbound 508 + spawn guard) — **Critical**
- **Clauses:** REQ-ERR-1, REQ-ERR-2, REQ-LIMIT-1, REQ-PROXY-10, REQ-PROXY-14.
- **Existing:** `TestVisitedChain` covers only `CheckLoop`/`CheckOutbound` pure functions;
  `TestPeerSelfMountRefused` covers sync-time self-mount; **no test sends `X-Oxyp-Visited`**
  (grep across `*_test.go`: only `X-Oxyp-Instance` at `internal/meta/meta_test.go:328,347,378`).
- **Evidence:** inbound guard `internal/meta/meta.go:117-125`; spawn guard
  `internal/daemon/peers.go:80-89`; cap `internal/peerfederation/visited.go:84-91`.
- **Impact:** G3 claims loops are impossible by construction. A regression in header→context
  wiring, route ordering (guard before routing), or spawn validation yields unbounded
  daemon→daemon recursion (request storm) with a fully green suite.
- **Test to write (internal/meta):** daemon with local server `alpha`; `GET /servers/alpha/tools`
  with header `X-Oxyp-Visited: <d.InstanceID()>`; **assert** status 508 and body contains
  `federation loop refused`. Variant: 8 unknown ids → 508. Oracle: spec Error Handling row
  "federation loop | 508".
- **Test to write (internal/daemon):** peer `w` mounted against a call-counting owner; ctx =
  `peerfederation.WithVisited(ctx, []string{d.InstanceID()})`; `d.ListTools(ctx,"w:db")`;
  **assert** error contains `federation loop refused` **and** owner tool-list count == 0.
  Oracle: REQ-PROXY-10 ("refuse … emitting no outbound request").

### F2 — Owner-error taxonomy untested across producer/consumer boundary — **High**
- **Clauses:** REQ-ERR-5, REQ-ERR-7, REQ-ERR-8, REQ-ERR-9, REQ-ERR-10, REQ-ERR-11, REQ-PROXY-7.
- **Existing:** `TestWriteDownstreamErrGeneric` (`internal/meta/meta_security_test.go:227-267`)
  covers `daemon.AuthNeededError` and the generic 502 only. No test constructs
  `peerfederation.AuthNeededError/PolicyDeniedError/ParamDeniedError`.
- **Evidence:** producer `internal/peerfederation/proxy.go:328-347`; consumer
  `internal/meta/meta.go:603-661`; owner envelopes `internal/peerfederation/client.go:186-215`.
- **Impact:** owner-side OAuth stops producing the `(on <peer>)` login hint; owner policy/param
  denials degrade to opaque 502 or lose `oxypPolicy`/`allowed`, so agents cannot tell "blocked"
  from "retry". The Error Handling table is the public contract.
- **Test to write (internal/meta):** fake owner `/federation/v1/call` → 401
  `{"auth_required":"run: oxyp login db"}`; import the virtual; `POST /servers/w:db/tools/q`;
  **assert** 401 and `auth_required` contains `(on w)`. Variants: owner 403
  `{"_meta":{"oxypPolicy":"blocked"}}` → 403 + same marker; 403 `param-blocked` + `allowed` →
  403 + allowed list; dead peer after sync → 502 body contains `peer "w"` and health still
  `unreachable`. Oracle: spec Error Handling rows.
- **Test to write (internal/peerfederation):** table-drive `Proxy.wrapCall` with
  `PeerError{Status:401|403, AuthRequired|Policy|Allowed}` and `errors.As` per row.
  Oracle: same Error table.

### F3 — Visited chain + instance propagation across the daemon boundary untested — **High**
- **Clauses:** REQ-PROXY-9 (and LIMIT-1 robustness).
- **Existing:** `TestClientRoundTrip` asserts headers from a hand-built ctx only
  (`internal/peerfederation/client_test.go:90-96`). Nothing drives
  `meta.Handler → daemon.CallTool → proxy → client.req`.
- **Evidence:** chain install `internal/meta/meta.go:120-129`; daemon stamping
  `internal/daemon/peers.go:213-221` + `daemon.go:1905`; header write
  `internal/peerfederation/client.go:157-184`.
- **Impact:** if the context values are dropped anywhere on that path, the *downstream* owner
  sees an empty chain, so 3+-daemon cycles (A→B→C→A) are no longer detectable by the visited
  check; the call storm continues until hop ctx deadlines.
- **Test to write (internal/daemon):** fake owner records `X-Oxyp-Visited` and
  `X-Oxyp-Instance` on the thin tool POST; ctx = `WithVisited(ctx, []string{"a","b"})`;
  `d.CallTool(ctx,"w:db","query",nil)`; **assert** owner saw `a,b,<selfInstance>` and
  `instance == selfInstance`. Oracle: REQ-PROXY-9.

### F4 — Peer-view sharing gate untested on search and federation routes — **High**
- **Clauses:** REQ-SHARE-15, REQ-SHARE-16, REQ-SHARE-17, REQ-ERR-3, REQ-ERR-4.
- **Existing:** `TestPeerSharingFiltersServers` (meta_test.go:364-420) covers `/servers`, direct
  `/servers/pub/tools` 404, `/health`. No `X-Oxyp-Instance` request hits `/tools` or
  `/federation/v1/*`.
- **Evidence:** search filters `internal/meta/meta.go:473-476,492-495`; federation gates
  `internal/meta/federation.go:289,344,376,409`.
- **Impact:** G4 is "sharing defaults closed … never a hole". Dropping the search hit filter
  exports hidden server + tool names through the discovery surface; dropping the federation gate
  lets a peer invoke a hidden server by name.
- **Test to write (internal/meta):** daemon with local `secret` + `peer_sharing.deny=["secret"]`;
  `GET /tools?q=` with instance header → decode `tools[]`; **assert** no `server=="secret"`;
  same request without header → hit exists. Oracle: REQ-SHARE-15.
  Second: `POST /federation/v1/call {"server":"secret","tool":"x"}` with instance header →
  **assert** 404. Oracle: REQ-SHARE-16.
  Third: compare denied vs never-existing response bodies byte-for-byte (differential).
  Oracle: REQ-ERR-4.

### F5 — Owner tool-policy filtering in the bulk catalog untested — **High**
- **Clauses:** REQ-DATA-14 (and REQ-PROXY "gates run on both ends").
- **Existing:** `TestFederationCatalog` uses a policy-free fixture and asserts one tool.
- **Evidence:** `internal/meta/federation.go:129-131`
  (`if perms.Resolve(e, t.Name).Action != "allow" { continue }`).
- **Impact:** "Denied tools are omitted (owner gates apply before export)" is the catalog's
  security clause; a regression exports denied tool names/descriptions/schemas to every importer.
- **Test to write (internal/meta):** fixture server with `Tools.Deny=["delete_*"]` and one kept
  tool; `GET /federation/v1/catalog`; **assert** the server row's tool names == `["keep"]`.
  Oracle: REQ-DATA-14.

### F6 — Rev contract: hash composition and catalog/rev convergence untested — **High**
- **Clauses:** catalog `rev` + `/federation/v1/rev` (spec-underived; DERIVED-REV — see §8),
  REQ-LIVE-16 (resync trigger), REQ-STATE-6 robustness.
- **Existing:** `TestFederationRev` (meta_test.go:203-235) asserts only non-empty, stable,
  and "never spawns". The comment "New cached tool flips the hash" has **no assertion**
  (`_ = tools`). Nothing compares `/catalog.rev` with `/rev.rev`.
- **Evidence:** `internal/meta/federation.go:70-183` (full vs rev rows), `:191-241`
  (hash inputs; tools from `CachedToolNames` at `internal/daemon/daemon.go:541-562`, **unfiltered**),
  `:255-259`; consumer `internal/daemon/peers.go:140-157` (mismatch → resync).
- **Impact:** a constant hash passes today. Worse, for any server with a tool-policy deny the
  catalog filters tools (`federation.go:129`) while `/rev` hashes all cached names → the two
  never converge, and the importer resyncs on every 10 s tick forever `[INFERENCE: not executed]`.
- **Test to write (internal/meta):** (a) fixture alpha, warm list, `rev1`; add a tool; **assert**
  `rev2 != rev1` (kills constant/partial hash mutants); (b) **assert**
  `catalog.rev == rev.rev` for (i) plain server, (ii) server with `deny=["delete_*"]`,
  (iii) cold cache. Oracle: spec catalog `rev` field + "/rev answers a hash over the caller's
  catalog view".

### F7 — Liveness tick (`pingPeer`) has zero tests — **High**
- **Clauses:** REQ-LIVE-16, REQ-LIMIT-5, plus REQ-LIVE-2 robustness.
- **Existing:** none. `TestColdByDefault` (daemon_test.go:71-86) runs `pingConnected` for a
  *local* only; no test references `pingPeer`/`peerNamesMissing`.
- **Evidence:** `internal/daemon/peers.go:131-210` (rev compare→async resync; one `/health`;
  fan-out; owner auth/error; latency), `:116-128`, wiring `internal/daemon/daemon.go:2078-2094`.
- **Impact:** G4's "a dead peer degrades to stale-honest" depends on this tick; a regression
  leaves virtuals `healthy:true` after the peer dies, or spams resyncs.
- **Test to write (internal/daemon):** peer with two servers, owner counts `/health`; call
  `d.pingPeer(ctx, "w", []string{"w:db","w:gh"})` once; **assert** health count == 1 and both
  `HealthOf(...).Healthy == true`; then owner `/health` → 500, ping again; **assert** both
  errors contain `peer w`. Oracle: Limits row "1 `/health` per peer per 10 s tick" + REQ-LIVE-16.
  Optional: owner `/federation/v1/rev` returns a new value while `w` has zero connected
  virtuals → assert `pr.rev` adopted (sync trigger is async; assert via `PeerStatuses`/rev map
  seam or a counting `/catalog` after a short wait).

### F8 — `oxyp peers add` is untested end to end — **High**
- **Clauses:** REQ-CLI-1, -2, -3, -4, -5, -6, -8 (G1's "one command").
- **Existing:** `cmd/oxyp/peers_test.go` covers only `isPeerHeader`, `removePeerBlock` happy path,
  `samePeerURL`. `peersAddCmd` (`peers.go:30-188`) and `findDuplicateInstance`
  (`:190-224`) have zero tests.
- **Impact:** the primary operator mount path can regress (bad TOML block, missing self-mount
  refusal, broken `--force`, duplicate mounts) with a green suite; G1 is user-facing.
- **Test to write (cmd/oxyp):** temp dir + `withDirFlag`; pre-write `identity.json`
  `{"id":"self-1"}`; httptest owner answers `/peer` with `instance:"self-1"`; run
  `peersAddCmd` with `--write`; **assert** returned error contains `that's you` and
  `config.Load(dir).Peers` is empty. Variants: duplicate URL → "already mounts"; duplicate
  instance under a second URL → "already mounted as"; `--force` writes; 508 handshake →
  "refusing self-mount". Oracle: REQ-CLI-3/-5/-6/-8.
- **Write-path test:** fake daemon endpoint (`daemon.json` with URL of an httptest server that
  answers `/health` + `/reload`, `pid = os.Getpid()`), owner `/peer` `{instance:"fresh",name:"w"}`;
  **assert** `config.Load` has `peers["w"].URL == owner.URL` and stdout contains `[peers.w]`.
  Oracle: REQ-CLI-1/-2.

### F9 — Token-change rebind (and re-probe) untested — **High**
- **Clauses:** REQ-LIVE-8, REQ-LIVE-11, REQ-STATE-5.
- **Existing:** `TestRebindOnURLChange` — URL only. No token or TLS rebind.
- **Evidence:** comparison `internal/daemon/peers.go:381-394` (URL+Token+TLS), swap
  `:317-358`, proxy rebind `:361-379`, instance reset `old.instance = ""` (`:343`).
- **Impact:** rotating a peer token silently keeps the old bearer until restart (split brain:
  in-flight and new calls use stale credentials → 401/denied). Spec's state transition lists
  "URL/token/TLS change".
- **Test to write (internal/daemon):** owner accepts Bearer `old` initially; call succeeds;
  owner flips to require `new` and rejects `old`; `d.Reload` with `Token:"new"`; `SyncPeers`;
  call again; **assert** success and an owner-side auth-header recording equals `new`.
  Oracle: State transitions row + REQ-LIVE-8.
  Re-probe variant: assert the owner receives a `/health` (or `/servers`) request after reload.
  Oracle: REQ-LIVE-11.

### F10 — Importer-local tools/params gates on invoke untested — **High**
- **Clauses:** REQ-CFG-14, REQ-CFG-15, REQ-PROXY-8.
- **Existing:** `TestPeerLocalPolicy` proves materialization + search invisibility only.
- **Evidence:** materialization `internal/daemon/peers.go:625-651`; handler deny
  `internal/meta/meta.go:530-538`; params `internal/daemon/daemon.go:1893-1895`,
  mapping `internal/meta/meta.go:646-659`.
- **Impact:** "both ends enforce, either can deny" is proven for discovery only. A regression
  lets a denied tool or disallowed argument reach the owner, defeating the importer's policy
  (security-reviewer MUST).
- **Test to write (internal/daemon or meta):** peer virtual with `Tools.Deny=["query"]`; owner
  counts tool POSTs; `POST /servers/w:db/tools/query`; **assert** 403 `_meta.oxypPolicy=="blocked"`
  and owner count == 0. Params: `Params{"query":["q"]}`, call `{"q":"x","extra":1}`; **assert**
  403 `param-blocked`, `allowed==["q"]`, owner count == 0. Oracle: REQ-CFG-14/15, PROXY-8.

### F11 — "One slow peer can't stall the fleet" unverified; sync is sequential — **High**
- **Clauses:** REQ-LIMIT-3.
- **Existing:** none (no test measures sync ordering or budget).
- **Evidence:** `SyncPeers` iterates peers serially (`internal/daemon/peers.go:400-434`) and
  each `syncOnePeer` gets a 15 s ctx (`:530`). Spec Limits: "Per-peer sync budget 15 s — one
  slow peer can't stall the fleet".
- **Impact:** with the current serial loop, a hung lexically-first peer delays every later
  peer's mount by up to 15 s at startup/reload; N hung peers stack. `[INFERENCE: behavior by
  code reading, not executed]`
- **Test to write (internal/daemon):** peers `"a"` (catalog handler blocks on a channel) and
  `"b"` (fast); start `SyncPeers` in a goroutine; wait until `a`'s catalog request is in flight;
  **assert** `d.Entry("b:db") != nil` within 500 ms while `a` is still blocked. Oracle: the
  Limits row's stated reason. (If serial is intended, fix the spec reason — flag upward.)

### F12 — `identity.json` persistence/0600/reuse undefended — **Medium**
- **Clauses:** REQ-CFG-20, REQ-CFG-22, REQ-SEC-8 (REQ-CFG-21 design note).
- **Existing:** none (no test references `LoadOrCreateIdentity`/`IdentityPath`).
- **Evidence:** `internal/peerfederation/identity.go:23-63`.
- **Impact:** identity churn across restarts breaks duplicate-instance collapse and the loop
  guard's peer recognition; a mode regression violates the secrets table.
- **Test to write (internal/peerfederation):** `dir := t.TempDir()`; `id1,_ := LoadOrCreateIdentity(dir)`;
  `id2,_ := LoadOrCreateIdentity(dir)`; **assert** `id1==id2`, `uuid.Parse(id1).Version()==4`,
  and `stat(IdentityPath(dir)).Mode().Perm()==0600`; corrupt the file (`"{bad"`) → third call
  returns a *new* valid id. Oracle: REQ-CFG-20/22, SEC-8.

### F13 — Bulk-path import depth cap untested — **Medium**
- **Clauses:** REQ-NAME-10, REQ-LIMIT-2.
- **Existing:** `TestImportDepthCap` exercises the **thin** path only (its owner returns 404 for
  the catalog). The bulk twin is untested.
- **Evidence:** `internal/daemon/peers.go:612-617` (bulk) vs `:701-706` (thin).
- **Impact:** bidirectional show-mode peers can grow `peer:route` by one segment per resync on
  the bulk path; the wire loop cap is the only remaining bound.
- **Test to write (internal/daemon):** v1 catalog advertises
  `strings.Repeat("p:", MaxImportColons+1)+"leaf"`; sync; **assert** `len(d.Names())==0`.
  Oracle: REQ-LIMIT-2.

### F14 — `local:<name>` escape hatch untested — **Medium**
- **Clauses:** REQ-NAME-9.
- **Existing:** `TestPeerMergePriority` asserts peer wins + shadow list; no `local:` lookup.
  `TestSharedWithPeerMatrix` only checks `SharedWithPeer("local:pub")`.
- **Evidence:** `internal/daemon/daemon.go:377-392` (`managed`), `internal/daemon/peers.go:1110-1124`
  (`resolve`), `Names` hiding `internal/daemon/daemon.go:260-287`.
- **Impact:** G2: "every claimant stays reachable qualified" — locals have no qualified form,
  so `local:` is their only address. A regression makes a peer-beaten local unreachable.
- **Test to write (internal/daemon):** local `github` + peer `github` priority `-10`; **assert**
  `Entry("github").Peer=="wsl"` **and** `Entry("local:github") != nil && .Peer==""`.
  Oracle: REQ-NAME-9.

### F15 — Multi-peer priority and lexical tie untested — **Medium**
- **Clauses:** REQ-CFG-7, REQ-NAME-4, REQ-NAME-5, REQ-STATE-8 (design).
- **Existing:** one peer vs local only (`TestPeerMergePriority`).
- **Evidence:** ranking `internal/daemon/peers.go:939-1029` (rank, local-beats-peer, lexical).
- **Impact:** the stated deterministic winner under 3+ claimants is unproven; a comparator
  regression silently reroutes bare names.
- **Test to write (internal/daemon):** peers `"b" priority 1`, `"a" priority 2`, both advertise
  `db`; **assert** `Entry("db")` resolves to `"b:db"` and `Shadows("db")==["a:db"]`; then equal
  priorities → `"a"` wins and a second sync yields the same winner (metamorphic). Oracle:
  REQ-NAME-4/5.

### F16 — Sharing globs never matched against a qualified name's leaf — **Medium**
- **Clauses:** REQ-SHARE-6, REQ-SHARE-7.
- **Existing:** matrix tests bare displays only.
- **Evidence:** `internal/daemon/peers.go:1064-1108` (display then leaf); spec "globs matching
  route OR leaf".
- **Impact:** `deny=["db"]` must hide `wsl:db` in show mode; a leaf-branch regression lets a
  qualified virtual escape deny.
- **Test to write (internal/daemon):** show mode, `Deny=["db"]`; **assert**
  `!SharedWithPeer("wsl:db") && SharedWithPeer("wsl:github")`; `Allow=["gamma"]` alone →
  only `wsl:c:gamma` shared. Oracle: REQ-SHARE-6/7.

### F17 — `/servers` peer-row contract (origin/shadowed/transitive) untested — **Medium**
- **Clauses:** REQ-DATA-16, -17, -18, -19.
- **Existing:** `TestPeerSharingFiltersServers` decodes names only.
- **Evidence:** `internal/meta/meta.go:257-267`.
- **Impact:** peers consume these fields (origin/transitive drive import consent; shadowed drives
  diagnostics). A regression silently disables show-mode re-export or hides losers.
- **Test to write (internal/meta):** daemon with local `pub` (`peer_export.transitive=true`),
  peer virtual `wsl:db`, contested leaf `gamma`; `GET /servers` with instance header;
  **assert** `pub` row `origin=="local" && transitive==true`; `wsl:db` row `origin=="wsl"`;
  contested bare row `shadowed==["wsl:c:gamma"]`-style losers. Oracle: REQ-DATA-16/17/18.

### F18 — Sync single-flight unverified — **Medium**
- **Clauses:** REQ-LIMIT-4, REQ-LIVE-12.
- **Existing:** `TestSyncReloadRace` only proves no data race/crash.
- **Evidence:** `internal/daemon/peers.go:400-415`.
- **Impact:** reload storms stack 15 s syncs (spec: single-flight 1).
- **Test to write (internal/daemon):** owner catalog blocks on a channel and counts calls;
  goroutine A enters `SyncPeers`; once the first catalog request is in flight, call `SyncPeers`
  from the test goroutine and **assert** it returns while A is still blocked; release; **assert**
  catalog count == 1. Oracle: REQ-LIMIT-4.

### F19 — v1→thin downgrade discipline on non-404 errors untested — **Medium**
- **Clauses:** REQ-PROXY-2, REQ-ERR-9.
- **Existing:** downgrade tested only for 404 (`TestV1DowngradeToThin`).
- **Evidence:** `internal/peerfederation/proxy.go:154-168` and the same `IsNotFound` shape at
  `:199-210, :213-225, :232-249, :254-267, :286-300`.
- **Impact:** a mutation downgrading on *any* error would mask owner failures by silently
  retrying thin (wrong route family, swallowed/duplicated error).
- **Test to write (internal/peerfederation):** fake owner `/federation/v1/call` → 500 and thin
  tool POST → 200; `Proxy.CallTool`; **assert** error non-nil **and** thin counter == 0.
  Oracle: REQ-PROXY-2 ("old peers (404) fall back") + Error table 502.

### F20 — Bulk sync cannot clear an emptied tool list `[INFERENCE]` — **Medium**
- **Clauses:** REQ-DATA-12, REQ-STATE-1.
- **Existing:** `TestBulkCatalogSync` proves seeding, never clearing.
- **Evidence:** `expandBulk` seeds `metas[q]` only when `len(s.Tools) != 0`
  (`internal/daemon/peers.go:649-655`); `seedBulkMeta` overwrites only when present
  (`:773-781`); `mergeVirtuals` clears only when `serverChanged` (`:828-870`); `ListTools`
  returns non-remote `memTools` unconditionally (`internal/daemon/daemon.go:1074-1080`).
- **Impact:** when the owner removes every tool, the importer keeps advertising/calling stale
  tools until config changes — discovery lies (calls then fail owner-side).
- **Test to write (internal/daemon):** v1 catalog row with one tool → sync → `ListTools` len 1;
  second catalog row identical except `"tools":[]` → sync → **assert** `ListTools` len 0.
  Oracle: REQ-DATA-12 (catalog carries the current tool set) + STATE-1. (Would fail today.)

### F21 — Per-server sweep on sync untested — **Medium**
- **Clauses:** REQ-STATE-7, REQ-NAME-1.
- **Existing:** only whole-peer removal (`TestPeerReloadSurvives`).
- **Evidence:** `internal/daemon/peers.go:828-870`.
- **Impact:** an owner dropping one server leaves a ghost virtual + stale cache, contradicting
  the state table's "orphan sweep, aliases rebuilt".
- **Test to write (internal/daemon):** first catalog 2 servers; second catalog only `db`; sync;
  **assert** `Entry("wsl:gone")==nil` and `Names()` lacks it. Oracle: REQ-STATE-7.

### F22 — `peers remove` is not byte-precise (a golden test would fail) — **Medium**
- **Clauses:** REQ-CLI-9.
- **Existing:** `TestRemovePeerBlock` checks only that the peer block/URL are gone and selected
  strings remain.
- **Evidence:** `cmd/oxyp/peers.go:305-333`; after the cut it globally collapses every `\n\n\n`
  to `\n\n` (`:325-330`) and trims leading newlines (`:329`). Only table headers are cut, so a
  dotted key `peers.x.url = "..."` survives while the command reports removal.
- **Impact:** the spec says "cut the table byte-precisely" and "preserving everything else
  byte-for-byte"; unrelated whitespace changes create noisy diffs and can mask/alter config.
- **Test to write (cmd/oxyp):** golden file containing a triple blank line **inside**
  `[servers.a]` plus `[peers.wsl]`; `removePeerBlock(dir,"wsl")`; **assert** the `[servers.a]`
  section bytes are identical to the golden (fails today). Oracle: REQ-CLI-9.

### F23 — Peer HTTP security invariants untested (redirects, bearer-only, no Origin) — **Medium**
- **Clauses:** REQ-SEC-1, REQ-SEC-2, REQ-SEC-3.
- **Existing:** none in `internal/peerfederation`.
- **Evidence:** `CheckRedirect` → `ErrUseLastResponse` (`internal/peerfederation/client.go:147-149`);
  auth header set only from the peer token (`:170-176`); "No Origin header" (`:177-178`).
- **Impact:** a regression following redirects forwards the bearer token to the Location target
  (credential exfiltration beyond the peer); forwarding other auth headers would break
  "auth never travels".
- **Test to write (internal/peerfederation):** ownerA → 302 to ownerB; `c.Health(ctx)`;
  **assert** error non-nil **and** ownerB hit count == 0. Second: owner records all headers;
  with a ctx carrying an unrelated instance only the peer bearer is present and Origin absent.
  Oracle: REQ-SEC-3 / spec Security Considerations.

### F24 — Resource/template/completion proxy contracts untested — **Medium**
- **Clauses:** REQ-PROXY-13; `/federation/v1/complete` is UNASKED (§8).
- **Existing:** prompts covered (`TestV1ProxyFidelity` GetPrompt); no test for
  `Proxy.ReadResource`, `ListResourceTemplates`, `Complete`, nor `client.VResource/VComplete`.
- **Evidence:** `internal/peerfederation/proxy.go:192-228, 286-300`; client
  `internal/peerfederation/client.go:406-490`; handlers `internal/meta/federation.go:330-380, 393-421`.
- **Impact:** content fidelity (`text`/`blob`, mimeType, `_meta`) can regress silently; the
  completion path ships with no contract at all.
- **Test to write (internal/peerfederation):** v1 owner returns
  `{"contents":[{"uri":"test://x","text":"hi","mimeType":"text/plain"}]}`; `Proxy.ReadResource`;
  **assert** one `TextResourceContents` with `Text=="hi"`. Second: catalog row with
  `templates:[{uriTemplate:"test://{id}"}]` → `ListResourceTemplates` returns it.
  Oracle: REQ-PROXY-13.

### F25 — Spawn liveness must preserve owner auth/error untested — **Medium**
- **Clauses:** REQ-LIVE-15.
- **Existing:** `TestOwnerHealthMergeAndClear` covers sync only, not spawn.
- **Evidence:** `internal/daemon/peers.go:99-108` (preserves `Error`/`AuthRequired` while
  stamping `Healthy` from them).
- **Impact:** a successful spawn could erase "owner needs login", sending operators to the wrong
  diagnosis; the spec calls this out explicitly.
- **Test to write (internal/daemon):** import with health `AuthRequired:"run: oxyp login db (on w)"`;
  then `ListTools("w:db")` succeeds; **assert** `HealthOf("w:db").AuthRequired` still set and
  `Healthy==false`. Oracle: REQ-LIVE-15.

### F26 — `enabled=false` / ConfigError peers make zero requests untested — **Low**
- **Clauses:** REQ-CFG-17.
- **Evidence:** skip in `rebuildPeers` (`internal/daemon/peers.go:320-322`), cleanup `:337-348`.
- **Test to write (internal/daemon):** counting owner; peer `Enabled=&false`; `New` + `SyncPeers`;
  **assert** owner request count == 0 and peer absent from `PeerStatuses`. Oracle: REQ-CFG-17.

### F27 — `{file:}` peer secret fail-closed untested — **Low**
- **Clauses:** REQ-CFG-4.
- **Evidence:** `internal/config/expand.go:127-152` (rejects residual file refs);
  test only `${VAR}` (`internal/config/peers_test.go:89-104`).
- **Test to write (internal/config):** `token="{file:/nonexistent}"`; **assert** `ConfigError != ""`
  and `Token` unchanged. Oracle: REQ-CFG-4.

### F28 — `/health` name + anonymous naming + `/peer` pinned list untested — **Low**
- **Clauses:** REQ-CFG-23, REQ-CFG-25, REQ-DATA-4 (part of REQ-DATA-1/3/7).
- **Evidence:** `internal/meta/meta.go:660-697` (writes `name`); tests decode `instance`/
  `servers` only; `TestPeerHandshake` uses local only.
- **Test to write (internal/meta):** daemon `name:"wsl"` → `/health`.name == "wsl"; empty name →
  `""`; show-mode peer with a virtual → `/peer`.servers contains pinned `"wsl:db"`.
  Oracle: REQ-CFG-23/25, DATA-4.

### F29 — Catalog tool optional-field fidelity untested — **Low**
- **Clauses:** REQ-DATA-13 (+ derived extras).
- **Existing:** annotations only (`TestV1ProxyFidelity`).
- **Evidence:** `CatalogTool.ToTool` (`internal/peerfederation/client.go:279-310`) handles
  OutputSchema/Execution/Meta/Icons.
- **Test to write (internal/peerfederation):** catalog tool with all four extras; `ToTool`;
  **assert** each preserved (`RawOutputSchema != nil`, `Execution != nil`, `Meta != nil`,
  `len(Icons)==1`). Oracle: REQ-DATA-13.

### F30 — Visit-chain boundaries beyond the cap untested — **Low**
- **Clauses:** REQ-LIMIT-1 (boundary), REQ-NAME-11 (stability).
- **Evidence:** `internal/peerfederation/visited.go:37-51, 93-107`; tests cover len==8 and
  self/target membership only.
- **Test to write (internal/peerfederation):** `CheckOutbound("self","tgt", 7 ids)` → true
  (boundary accept); `ParseVisited` of a 9-id header → 9 entries and handler-level 508
  (chain >= 8); `WithVisited` truncates to `MaxVisited`. Oracle: REQ-LIMIT-1 boundary.

### F31 — `peers list` / `peers sync` untested — **Low**
- **Clauses:** REQ-CLI-10, REQ-CLI-11.
- **Evidence:** `cmd/oxyp/peers.go:227-260, 373-390`.
- **Test to write (cmd/oxyp):** config with one merge peer + one `enabled=false`; run
  `peersListCmd`; **assert** stdout contains `name: url (disabled, priority N)`; `peersSyncCmd`
  against a fake endpoint returns 200 JSON and non-zero exit on 500. Oracle: REQ-CLI-10/11.

### F32 — Hidden vs nonexistent 404 indistinguishability untested — **Low**
- **Clauses:** REQ-ERR-4, REQ-SHARE-17.
- **Evidence:** both paths emit `server not found` 404 (`internal/meta/meta.go:370-372, 401-403`);
  denied-direct is tested, never compared with a nonexistent name.
- **Test to write (internal/meta):** with instance header, `GET /servers/secret/tools` (denied)
  vs `/servers/nope/tools`; **assert** identical status and body bytes. Oracle: REQ-ERR-4.

### F33 — `oxyp login` refusal of virtual names untested — **Low**
- **Clauses:** REQ-ERR-6.
- **Evidence:** `cmd/oxyp/main.go:830-837` (`peer:server` → "log in there", `local:` strips).
- **Test to write (cmd/oxyp):** config with `[peers.w]`; `loginCmd` with `"w:db"`;
  **assert** error contains `log in there`; `"local:db"` falls through to the oauth-remote check.
  Oracle: Error table row "local login refuses".

### F34 — Peer TLS path (config validation + dial + rebind) untested — **Low**
- **Clauses:** REQ-CFG-16, REQ-LIVE-9, REQ-STATE-5.
- **Evidence:** `validatePeer` TLS rules (`internal/config/config.go:750-765`);
  `NewClient` TLS mapping (`internal/peerfederation/client.go:128-140`).
- **Test to write (internal/peerfederation):** `httptest.NewTLSServer`; client with
  `TLSConfig{Insecure:true}` → `Health` succeeds; without → fails. Oracle: REQ-CFG-16.

## 6. Mutation Chamber — thought experiment (NOT executed)

Read-only contract: no production edit was made. Each row is a prediction from reading the
tests. A survivor is a gap, not a license to weaken tests.

| # | Module | Mutation | Predicted | Verdict |
|---|---|---|---|---|
| 1 | peerfederation/proxy.go | `wrapPeer`: remove 401+`AuthRequired` → `AuthNeededError` branch | PASS | 💀 decorative |
| 2 | peerfederation/proxy.go | `CallTool`: `if !IsNotFound(err)` → `if false` (downgrade on any error) | PASS | 💀 |
| 3 | meta/federation.go | `hashServerRows`: delete tool-name hashing loop | PASS | 💀 |
| 4 | meta/federation.go | `catalogRows`: delete `perms.Resolve` filter | PASS | 💀 |
| 5 | meta/meta.go | `Handler`: delete the 508 loop-guard block (117-125) | PASS | 💀 |
| 6 | meta/meta.go | `handleSearch`: delete peer-view hit filter | PASS | 💀 |
| 7 | daemon/peers.go | `pingPeer`: `return` at function top | PASS | 💀 |
| 8 | daemon/peers.go | `spawnPeer`: delete `CheckLoop` guard | PASS | 💀 |
| 9 | daemon/peers.go | `expandBulk`: delete bulk depth-cap check | PASS | 💀 |
| 10 | peerfederation/identity.go | `Chmod(tmp, 0o600)` → `0o644` | PASS | 💀 |
| 11 | daemon/peers.go | `peerConnEqual`: ignore `Token` | PASS | 💀 |
| 12 | daemon/peers.go | `mergeOwnerHealth`: drop `Healthy = false` on owner error | PASS | 💀 |
| 13 | daemon/peers.go | `rebuildAliases`: `rank <` → `rank >` | FAIL | ✅ `TestPeerMergePriority` |
| 14 | daemon/peers.go | `rebuildAliases`: drop local-beats-peer tie-break | FAIL | ✅ `TestPeerMergeExpansion` |
| 15 | daemon/peers.go | `peerIncluded`: drop leaf match | FAIL | ✅ `TestTransitivePinnedRoute` |
| 16 | daemon/peers.go | `SyncPeers`: drop `dropOrphanVirtuals` | FAIL | ✅ `TestPeerReloadSurvives` |
| 17 | config/peers.go | `ServerTimeout`: drop per-server override | FAIL | ✅ `TestPeerTimeoutMaterialized` |
| 18 | peerfederation/visited.go | `CheckOutbound`: drop target-instance check | FAIL | ✅ `TestVisitedChain` |
| 19 | daemon/peers.go | `markPeerDown`: no-op | FAIL | ✅ `TestPeerDownKeepsStale` |
| 20 | meta/meta.go | `handleServers`: drop `SharedWithPeer` filter | FAIL | ✅ `TestPeerSharingFiltersServers` |

**Predicted kill: 8/20 = 40% — Paper Tiger** on a sample deliberately biased toward suspicion.
Controls 13–20 show the naming/merge/health/collapse core does assert. Survivors 1–12 are the
finding list (F2, F19, F6, F5, F1, F4, F7, F1, F13, F12, F9, F25). If executed, rows 1-12 are
the first mutants to run; rows 11/12 need the F9/F25 tests before they can be killed.

## 7. Resurrection Plan

### Critical — first PR (blocks "verified" claim)
1. F1 loop refusal end-to-end: inbound 508 (self-in-chain + hop cap) and spawn-time refusal with
   zero outbound requests. Files: `internal/meta/federation_peer_test.go`,
   `internal/daemon/peers_loop_test.go`.
2. F2 error taxonomy table (proxy unit + meta handler): 401 hint `(on w)`, 403 blocked,
   403 param-blocked + allowed, 502 attributed + stale health.
3. F3 chain/instance propagation from inbound ctx to owner headers.

### High — next cycle
4. F4 peer-view sharing on search + federation endpoints (+ hidden/nonexistent differential).
5. F5 catalog tool-policy filter.
6. F6 rev: change detection (tool add/remove/resource) + catalog↔rev convergence (plain, denied,
   cold) — likely reveals a live defect (permanent resync).
7. F7 pingPeer: one probe, fan-out, owner health, rev adopt.
8. F8 peers add: self/URL/instance refusal, 508, `--force`, write+preview.
9. F9 token rebind (+ instance re-probe).
10. F10 importer invoke gates (tools 403, params 403, owner sees zero).
11. F11 slow-peer fleet isolation (or correct the spec reason).

### Medium — completeness
12. F12 identity; F13 bulk depth cap; F14 `local:`; F15 multi-peer/tie; F16 leaf globs;
    F17 `/servers` rows; F18 single-flight; F19 downgrade discipline; F20 stale tools on emptied
    catalog; F21 per-server sweep; F22 remove byte-precision; F23 redirect/bearer-only;
    F24 resource/template/complete; F25 spawn preserves owner auth.

### Low — hygiene
F26 enabled=false; F27 `{file:}`; F28 health name/pinned peer list; F29 ToTool extras;
F30 chain boundaries; F31 peers list/sync; F32 404 indistinguishability; F33 login refusal;
F34 TLS path.

### Test files to create (planned; none written in this pass)
1. `internal/peerfederation/identity_test.go` — F12.
2. `internal/peerfederation/proxy_test.go` — F2 (unit table), F19, F24, F29.
3. `internal/peerfederation/client_security_test.go` — F23, F34.
4. `internal/meta/federation_peer_test.go` — F1 (inbound), F2 (handler), F4, F5, F6, F17, F28, F32.
5. `internal/daemon/peers_loop_test.go` — F1 (spawn), F3.
6. `internal/daemon/peers_live_test.go` — F7, F9, F18, F25.
7. `internal/daemon/peers_sync_test.go` — F10, F11, F13, F15, F20, F21.
8. `internal/daemon/peers_alias_test.go` — F14, F16.
9. `internal/config/peers_edges_test.go` — F26 (Load-level), F27, F34 config rules.
10. `cmd/oxyp/peers_cmd_test.go` — F8, F22, F31, F33.

One behavior per test; every test names its clause ID in the name or comment; every oracle from
§5. Fast tier target: +3 s max (`F7`/`F11` use gates, not sleeps; `TestPeerSlowToolRespectsTimeout`
stays the only 1.5 s case).

## 8. N/A, UNASKED, DERIVED

### N/A (verdict with reason)
- **Frontend/mobile/visual** — no rendered UI in this repo (verification strategy: "No
  frontend/mobile").
- **Generated/vendored** — `mcp-go` types (`mcp.Tool`, content blocks) are tested via our
  usage at the boundary, never as our behavior.
- **Trivial accessors** — `Proxy.Peer/Owner/Close/SetLevel`, `peerfederation.ShortID`,
  `PeerError.Error`, `IdentityPath`, `OnNotification` setter: no branch, no observable decision.
- **`daemon.json`/`cache.json`/OAuth-store 0600 (REQ-SEC-9/10/11)** — owned by other specs
  (`V-CLI-005`, `V-DISC-001`, `V-OAUTH-*`); federation only owns `identity.json` (F12).
- **REQ-DATA-19 "additive /servers changes"** — no pre-change snapshot exists to diff; covered
  functionally by F17. Flag as regression-aid only.
- **64 MiB body cap / malformed JSON wrapper robustness** — testing needs a 64 MiB allocation;
  cost outweighs the defect class. Recommend factoring `MaxBody` into a var seam first; until
  then N/A (not silent — this is the justification).
- **REQ-SEC-12/14/15** — trust-model statements and future-work notes; Review, not Test.

### UNASKED (code with no parent clause — derive upward or delete)
- **U1 `/federation/v1/complete`** — route (`internal/meta/meta.go:73`), handler
  (`internal/meta/federation.go:393-421`), client `VComplete`
  (`internal/peerfederation/client.go:479-490`), proxy `Complete` (`proxy.go:286-300`).
  The spec's Boundaries name ListTools/CallTool/resources/prompts only; no Data Contract row.
  Resolution: add a clause + test (F24) or delete the route.
- **U2 Catalog server rows `resources`, `templates`, `prompts`** — emitted
  (`internal/meta/federation.go:64-67`), imported (`peers.go:657-666`), asserted
  (`TestFederationV1RoundTrip`), but the spec's `/federation/v1/catalog` table lists only
  `tools[]`. Add to the spec.
- **U3 Catalog tool extras `outputSchema`, `icons`, `execution`, `_meta`** — `toolFull`
  (`federation.go:46-55`); spec lists `{name, description, title?, inputSchema, annotations?}`.
  Add to the spec (F29).
- **U4 `/federation/v1/rev` endpoint and catalog `rev` field** — implemented
  (`federation.go:255-259`, `:33-38`), load-bearing (import resync), yet absent from the spec's
  Data Contracts. Add a contract row; F6 tests it.
- **U5 `Proxy.SetLevel` no-op / `Subscribe`/`Unsubscribe` refusal** — MCP interface obligations,
  not federation behavior; if no parent in `spec-daemon.md`, mark N/A there.

### DERIVED requirements (code decided; spec silent — flag upward)
- **D1** Rev-polling resync on the 10 s tick (`peers.go:140-157`) — the spec describes catalog
  freshness only implicitly.
- **D2** v1-first routing + self-heal downgrade (`proxy.go:154-168`) — spec says thin fallback
  for old peers; the runtime downgrade flag is derived.
- **D3** `/federation/v1/complete` + resource/prompt/template rows (U1/U2).
- **D4** `/health` is exempt from the 508 loop guard (`meta.go:120`) — spec Error table has no
  exemption. Intent is documented in code ("peer handshake"); add a spec note or drop the
  exemption.
- **D5** `PeerV1` capability stamped into virtual config at import (`peers.go:621`) and route
  family per virtual.
- **D6** `MaxImportColons = MaxVisited - 1` (`peers.go:242`) — consistent with the spec's 7/8.

### Design notes (unverifiable as written → Review/Analysis, never tested as written)
REQ-CFG-16 (TLS wiring — partially testable, F34), REQ-CFG-21 (atomic write — Review; could be
Analysis via crash injection), REQ-NAME-5 (tie determinism — made testable by F15), REQ-LIVE-9
(TLS rebind — F34), REQ-LIVE-10 (atomic swap — `-race` Analysis; add a concurrent-call test if
cheap), REQ-CLI-7 (bounded parallel sweep — Analysis of goroutine bound), REQ-SEC-12/14/15
(trust-model design), REQ-DATA-10 (omission of optional fields — golden test would work; flag
upward), REQ-STATE-8 (alias rebuild — made testable by F15).

### Suggested spec corrections (verification strategy maintenance)
1. Add Data Contract rows for `/federation/v1/rev`, `/federation/v1/call|resource|prompt|complete`
   request/response shapes, and catalog `resources/templates/prompts` + tool extras.
2. State the `/health` loop-check exemption explicitly, or remove it.
3. Correct the "one slow peer can't stall the fleet" reason: current `SyncPeers` is serial
   (F11) — either make sync per-peer concurrent/bounded or soften the claim.
4. Define the rev invariant: `catalog.rev` MUST equal `/rev` for the same state (F6) — and decide
   whether denied tools are excluded from both or neither.

## 9. Known unknowns / NOT RUN

- Mutation rows in §6 are **predictions**, not executions (read-only contract). They are the
  first thing to run when tests land.
- F6/F11/F20 rely on runtime behavior; label `[INFERENCE]` until a failing test proves them.
- No two-daemon end-to-end (real process pair) was run; peerfederation client tests use
  `httptest`. A live A↔B test is a release-tier gap (verification strategy's manual layer).
- Slow-peer budget (15 s) and 10 s tick timings were not exercised to avoid slow/quarantined tests;
  proposed tests use gates and direct `pingPeer` calls instead of wall-clock waits.

## 10. Evidence appendix

Files read (all in scope): `internal/peerfederation/{client,proxy,visited,identity,errors,doc}.go`,
`internal/daemon/peers.go` (1135 lines), `internal/daemon/daemon.go` (Names/PeerNames/Entry/
ListTools/SearchTools/CallTool/pingConnected), `internal/meta/{federation,meta}.go`,
`internal/config/{peers,config,expand}.go`, `cmd/oxyp/{peers,main}.go`, and all federation
`_test.go` files (`peerfederation/client_test.go`, `visited_test.go`, `daemon/peers_test.go`,
`config/peers_test.go`, `cmd/oxyp/peers_test.go`, `meta/meta_test.go`, `meta/meta_security_test.go`).

Existing defenders (46 funcs): daemon 25 (`TestPeerMergeExpansion` … `TestV1DowngradeToThin`),
config 8, peerfederation 4, CLI 3, meta 6 (`TestFederationRev`, `TestFederationV1RoundTrip`,
`TestFederationCatalog`, `TestPeerHandshake`, `TestPeerSharingFiltersServers`,
`TestHealthCarriesIdentity`).

Clause source: blind derivation by `RawPigeon.BlindDeriver` (spec-only constraint enforced),
transcript `history://RawPigeon.BlindDeriver`, 155 clauses / 11 design-notes.

No files outside this report were created or modified. No production code was mutated.
