---
node_type: verification
title: oxyp Verification Strategy
status: active
updated: 2026-10-30
tags: [verification, testing, zoo, evidence]
links:
  depends_on: [/overview.md, /spec-daemon.md, /spec-discovery.md, /spec-oauth.md, /spec-http-shim.md, /spec-guard.md, /spec-cli.md]
  verifies: [/spec-daemon.md, /spec-discovery.md, /spec-oauth.md, /spec-http-shim.md, /spec-guard.md, /spec-cli.md]
---

# oxyp Verification Strategy

## Scope and intent

Correct for oxyp means: the six behavior specs hold against the Go
implementation — cold discovery, guarded invoke with media intact, four
OAuth flows with refresh, CSRF-gated HTTP, honest doctor/add/import, and
fail-closed secrets. A **test result** (exit 0) is not a **verification
result** (the check exercises the intended spec scenario with inspectable
evidence) and neither is a **validation result** (a human agrees the
behavior solves the operator's problem — e.g. real-client shim attach,
real-IdP login). This strategy names the real toolchain, the exact
commands, the evidence each layer produces, the traceability from spec to
check, and the gaps we admit.

## System surfaces

- Backend/API: daemon HTTP (10 routes + gate), lifecycle/reload/sweep,
  metacache, search, perms, guard/spill, trace, redact, remhttp, OAuth.
- Workers/CLI: 13 Cobra commands, stdio shim JSON-RPC, connectors.
- Data: `<dir>/oxyp.toml`, `<dir>/daemon.json`, `<dir>/cache.json`,
  `<dir>/oauth/`, `<dir>/trace.jsonl`, `/tmp/oxyp-output-*.txt`.
- Integrations: downstream stdio (any language), Streamable HTTP, legacy
  SSE, OAuth IdPs, foreign `mcp.json` shapes.
- No frontend/mobile. UI-layer checks do not apply; state this instead of
  leaving readers guessing.

## Toolchain (exact, as used)

| Tool | Version / location | Proves |
|---|---|---|
| Go test runner | Go 1.26.8 (`go test`) | unit/component/integration |
| Race detector | `go test -race` | concurrency on store/callback/metacache/ptrace/meta/daemon paths |
| `go vet` / `gofmt` | Go 1.26.8 | static sanity on touched packages |
| Mutation spot-checks | transient prod edits, restored | suite actually asserts (6/6 killed 2026-10-30) |
| Zoo batteries | `oxyp/zoo/` — `stdio/zoo_stdio.py`, `zoo_needy.py`, `zoo_node.js`, `http/zoo_http.py`, `zoo_oauth.py` (fake IdP), `zoo_tls.py` + openssl certs | downstream compat: stdio 22/22, HTTP 16/16, redirect 3/3, OAuth 3/3, TLS 5/5 |
| Manual client matrix | hermes / omp / opencode entry shapes | shim attach (currently UNTESTED live — gap) |
| Anubis review | `.agents/reports/Anubis_Findings_2026_10_30.md` | whole-tree security audit (substantially STALE — see §9) |
| Osiris test pass | `.agents/reports/Osiris_Judgment_2026_10_30.md` | 36 new test files, ~290 funcs, condemned gaps list |

## Verification taxonomy

| Layer | What it proves | Evidence |
|---|---|---|
| Static / quality | syntax, fmt, vet-clean | tool exit + report |
| Unit | decision logic (perms, rank/page, guard, redact, expand, render, TLS validate) | assertions |
| Component / service | one package vs fixtures (metacache file, oauth store/callback/device/CC, meta handlers, daemon close/reload) | package test report |
| Integration | daemon + in-process transports, CLI + in-process daemon (doctor/add/import probes) | package report + temp-dir artifacts |
| Contract / API | route shapes, status codes, gate order, error envelopes, redirect policy | handler tests + captured exchanges |
| Zoo / compat | stdio/HTTP/SSE/OAuth/TLS batteries vs fake servers | `doc/ZOO.md` matrix + `zoo/*_test.go` |
| Manual / exploratory | live-client shim attach, real-IdP login, browser CSRF/rebind | checklist + notes (MISSING — gaps) |
| Operational / release | build, autostart, endpoint liveness, reload, shutdown | command output, endpoint file, logs |

## Suite tiers

| Tier | Command | Prerequisites | Cadence |
|---|---|---|---|
| Fast / smoke | `go build ./...` | Go 1.26.8 | every change |
| Fast / smoke | `go vet ./internal/... ./cmd/...` | Go toolchain | every change |
| Standard | `go test -count=1 ./internal/... ./cmd/...` (~90 s: daemon ~41 s, oauth ~25 s, cmd ~28 s, rest <2 s) | Go toolchain, loopback ports | every PR / merge |
| Long / exhaustive | `go test -race ./internal/... ./cmd/...` | same + race runtime | pre-release, weekly |
| Long / exhaustive | `go test ./zoo/...` or per-battery (`-run 'TestZoo|OAuth|TLS|Search|Guard|Media|Timeout'`) incl. timeout edges | Python 3 + node for stdio zoo, openssl certs, loopback | pre-release |
| Release | live-client shim attach (hermes/omp/opencode) + real-IdP login + browser CSRF check | external boxes/accounts | per release (MANUAL — gap) |

## Traceability matrix

| Claim | Check ID | Layer | Tool / command | Environment | Evidence | Status |
|---|---|---|---|---|---|---|
| Lifecycle modes, enabled-false, config-error soft-fail | V-DAEMON-001 | component | `go test ./internal/daemon/ ./internal/config/` | local | lifecycle/entry tests | verified |
| Idle sweep local-only, busy-guard, cache kept | V-DAEMON-002 | component | `go test ./internal/daemon/ -run Lifecycle` | local | `lifecycle_test.go` | verified |
| Eager async, endpoint before warmup | V-DAEMON-003 | component | `go test ./internal/daemon/ ./cmd/oxyp/` | local | startup-order tests | verified |
| Health model + auth_required | V-DAEMON-004 | component/contract | `go test ./internal/daemon/ ./internal/meta/` | local | health/meta tests | verified |
| Reload diff + addr/token-needs-restart | V-DAEMON-005 | component | `go test ./internal/daemon/ -run Reload` | local | `reload_edge_test.go` | verified |
| Shutdown, endpoint 0600/PID/liveness | V-DAEMON-006 | component | `go test ./cmd/oxyp/ -run 'TestMain|Endpoint'` | local | `main_test.go` | verified |
| Cache format, fingerprint, corrupt→empty, atomic 0600 | V-DISC-001 | unit/component | `go test ./internal/metacache/` | local | metacache tests | verified |
| Invalidation (config/push/TTL/reset), search-never-spawns | V-DISC-002 | integration | `go test ./internal/daemon/ ./internal/metacache/` + zoo notify | local + zoo | daemon/cache tests, ZOO notify row | partial (notify unobservable — gap) |
| Word ranking tiers/weights/order | V-DISC-003 | unit | `go test ./internal/search/` | local | `search_edge_test.go` | verified (mutation: name-weight removal caught) |
| Regex mode + 400 on bad pattern | V-DISC-004 | unit/contract | `go test ./internal/search/ ./internal/meta/` | local | search + meta tests | verified |
| Pagination contract incl. past-end quirk | V-DISC-005 | unit | `go test ./internal/search/ -run Page` | local | page tests | verified |
| Deny-wins, invisible+blocked, allow-all default | V-DISC-006 | unit/contract | `go test ./internal/perms/ ./internal/meta/` | local | perms + C1 tests | verified (mutation: deny removal caught) |
| OAuth 4-flow battery vs fake IdP | V-OAUTH-001 | zoo | `go test ./zoo/ -run OAuth` + `internal/oauth` | local | ZOO OAuth 3/3 | partial (zoo IdP ignores client_id/PKCE — gap) |
| Browser + `--complete` cross-process | V-OAUTH-002 | integration | `go test ./internal/oauth/ ./internal/daemon/ -run 'Browser|Complete|Pending'` | local | callback/pending/reg tests | verified |
| Device RFC 8628 poll/approve | V-OAUTH-003 | zoo/integration | zoo device test + `device_test.go` | local | ZOO device row | verified |
| Token verify ok/expired | V-OAUTH-004 | integration | `go test ./internal/daemon/ -run Token` | local | flows tests | verified |
| Client-credentials POST + resource | V-OAUTH-005 | unit | `go test ./internal/oauth/ -run CC` | local | `cc_test.go` | verified |
| DCR sidecar + restart refresh | V-OAUTH-006 | integration | `go test ./internal/oauth/ -run 'Reg|Pending'` | local | reg/pending tests | partial (real-IdP validation missing — gap) |
| Auto-detect vs explicit auth, mid-session map | V-OAUTH-007 | component | `go test ./internal/daemon/ ./internal/meta/` | local | mapAuthErr + 401 tests | verified |
| Route table + envelopes + never-spawn lists | V-HTTP-001 | contract | `go test ./internal/meta/` | local | `meta_test.go` (23 funcs) | verified |
| Bearer→Host→Origin→CT gate | V-HTTP-002 | contract | `go test ./internal/meta/ -run Security` | local | `meta_security_test.go` | verified |
| 401 vs 502 + probe hints (TLS-aware) | V-HTTP-003 | contract/component | `go test ./internal/meta/ ./internal/daemon/` | local | 401-shape + probe tests | verified |
| Shim 5 tools, JSONL, dispatch, auth surfacing | V-HTTP-004 | component | `go test ./cmd/oxyp/ -run Shim` | local | `shim_test.go` (5-tool invariant) | verified |
| Media passthrough text+image+resource | V-HTTP-005 | zoo/component | zoo media row + `shim_test.go` verbatim | local | ZOO media ✅ | verified |
| Guard text-only + spill 0600 + prune | V-GUARD-001 | unit/component | `go test ./internal/outguard/ ./internal/meta/` | local | spill/edge tests | verified (mutation: passthrough caught) |
| Trace open/caps/no-payloads | V-GUARD-002 | unit | `go test ./internal/ptrace/` | local | ptrace edge tests | verified |
| Redact keys/URL/truncation | V-GUARD-003 | unit | `go test ./internal/redact/` | local | redact edge tests | partial (known key/value gaps — §9) |
| Redirects/TLS/session/retry | V-GUARD-004 | unit/zoo | `go test ./internal/remhttp/` + zoo redirect/session | local | remhttp edge + ZOO | verified (cap documented as 4-follow) |
| Expansion per-field + fail-closed | V-GUARD-005 | unit | `go test ./internal/config/ -run Expand` | local | expand edge tests | verified (mutation: exclusivity caught) |
| cwd/inherit_env/env | V-GUARD-006 | component | `go test ./internal/daemon/ ./internal/config/` | local | spawn-env tests | verified |
| Config schema: exclusive url/command, loopback-token rule | V-GUARD-007 | unit | `go test ./internal/config/ -run Validate` | local | validate tests | verified (mutation caught) |
| 13 commands + autostart | V-CLI-001 | component | `go test ./cmd/oxyp/ -run 'TestMain|Shim'` | local | `main_test.go` (16 funcs) | verified |
| Doctor offline/live, 15 s, exit codes | V-CLI-002 | component | `go test ./cmd/oxyp/ -run Doctor` | local | `doctor_test.go` (12) | verified |
| Add probe/preview/append | V-CLI-003 | component | `go test ./cmd/oxyp/ -run Add` | local | `add_test.go` (13) | verified |
| Import shapes/translation/render | V-CLI-004 | unit/component | `go test ./internal/importcfg/ ./cmd/oxyp/ -run Import` | local | parse/translate/render/roundtrip + import tests | verified (mutation: null-command caught) |
| 0600 create/tighten + prompt | V-CLI-005 | unit | `go test ./internal/importcfg/ -run Append` | local | `append_test.go` | verified |
| Zoo stdio 22/22, HTTP 16/16, TLS 5/5 | V-ZOO-001…004 | zoo | `go test ./zoo/...` + `doc/ZOO.md` | local + py/node/openssl | ZOO matrix | verified (weak-assert rows — gap) |
| Shim live-client attach | V-ZOO-005 | manual | hermes/omp/opencode boxes | external | checklist | **missing** |

## Environments and data

- Local: Linux (WSL2 verified), Go 1.26.8, loopback ports, temp dirs;
  `OXYP_DIR`/`--dir` isolate state per test daemon. Secrets policy: tests
  use fake IdP/tokens and `${VAR}` fixtures — never real credentials.
- Zoo: `zoo/stdio/*` (flag matrix, needy, node), `zoo/http/*` (streamable,
  session, SSE, OAuth IdP, TLS) with committed openssl certs; reset per
  test (in-process daemons, no shared state).
- CI: no CI config ships in this tree — standard/long tiers are local +
  pre-release manual today (gap: wire `go build/vet/test` into CI).

## Failure interpretation

1. Re-run the failing package with `-count=1` (flake check), then `-race`
   (concurrency?), then read the assertion — is the EXPECTATION stale
   (docs win → file a code-flag) or is the CODE wrong?
2. A green test exercising the wrong behavior is not proof: check the
   traceability row — does the test name the spec scenario (e.g. deny
   removal FAILS without the branch)? The 2026-10-30 mutation run (6/6
   killed) is the evidence our key suites do.
3. Zoo failure? Read `doc/ZOO.md` row notes first (timeouts, junk-stdout,
   list_changed are EXPECTED-failure shapes, not regressions).
4. `doctor --live` vs unit disagreement? Trust live (15 s budget, real
   sockets) over mocks, then pin the difference as a test.

## Coverage gaps and risks (honest)

From Osiris (`Osiris_Judgment_2026_10_30.md` §Condemned) + Anubis staleness
review + scout code facts:

- **UNDEFENDED (no test, would fail/hang — fix first):** metacache slice
  aliasing is FIXED (clone in/out) but keep the regression watch;
  remaining: oauth duplicate-callback blocking send; daemon waiter-channel
  on generation mismatch; in-flight list repopulating cache after reload;
  trace + failed `tools/list` nil-deref; `RenderAll`/`AppendBlocks`
  duplicate-`[servers]` (scout: currently NOT emitted — re-verify before
  fixing).
- **PARTIAL (pinned as current behavior):** `Entry`/`DaemonConfig` shallow
  copies share maps; `Names()` unsorted; `redact.Body` skips plain-text
  inside JSON values and `cookie`/`id_token`/`private_key` keys; outguard
  negative-limit clamp + trailing-newline note; ptrace one-event byte
  overshoot + per-open (not per-file) event cap; search `Page` past-end
  `next = len` quirk + repeated-word rescoring; `atoiOr("12abc")` → 12;
  device DCR `client_secret_post` advertise vs Basic send; `Store` fixed
  `.tmp` collision.
- **ZOO weak asserts:** unicode/dup names logged not asserted; notify-changed
  unobservable end-to-end; wrong-content-type no-assert; NO shim end-to-end
  (needs `serveShim` seam); live-client matrix (hermes/omp/opencode)
  entirely manual/untested.
- **NOT covered live:** real IdPs (zoo IdP ignores `client_id`/PKCE, so DCR
  validation + refresh-against-strict-IdP are unproven); browser
  PNA/DNS-rebinding enforcement; performance under large catalogs; `go
  vet`/race/build were NOT run during the read-only Anubis review.
- **Anubis staleness warning:** at least 10 Anubis findings no longer match
  code (daemon.token expansion, OAuth expansion, spill cleanup, guard
  opt-out, probe TLS/headers, EagerStart ordering, add/import 0600,
  mid-session 401 mapping, shim auth_required, trace enable). Treat that
  report as HISTORICAL input, not current verdict. Genuine residual edges
  (device DCR Basic-vs-POST, callback-port fallback silence, `.tmp`
  collisions, `/servers` unredacted errors, `daemon.json` existing-mode
  preservation) are documented in the specs above as current behavior.

## Maintenance trigger

When behavior, tools, commands, CI, environments, or architecture change,
this document MUST be reviewed: add/rename a `V-*` ID, update the command,
re-run the tier, and record evidence. New spec requirements MUST arrive
with a traceability row or an explicit `missing` status — never imply
coverage.
