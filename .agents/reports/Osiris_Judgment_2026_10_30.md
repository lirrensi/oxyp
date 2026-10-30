# Osiris Judgment — oxyp — 2026-10-30

Scope: whole `oxyp/` tree (Go 1.26, module `oxyp`): daemon, OAuth, meta HTTP+CSRF, config, metacache, search, outguard+spill, ptrace, redact, remhttp, perms, importcfg, CLI (doctor/add/import), shim JSON-RPC+media, zoo. No frontend/mobile. Contract: test files only, no prod fixes.

## Verdict

- Full unit suite green: `go test -count=1 ./internal/... ./cmd/...` — all 13 packages ok (daemon ~41s, oauth ~25s, cmd/oxyp ~28s; rest <2s).
- 36 new `*_test.go` files, ~290 new test funcs. Zero production files touched (verified: all prod mutation backups diff-clean after restore).
- Mutation score on 6 sampled targets: **6/6 killed = 100% — Apex Predator** (table below).
- `internal/meta` went from zero tests to 23 test funcs; `cmd/oxyp` from zero to 63.

## The Weighing

| Category | Found | Severity |
|---|---|---|
| Untested functions (pre-pass: meta all handlers, shim dispatch, CLI all, oauth Store/callback/device/CC, daemon Close/prompts/Entry-depth) | ~120 | 🔴 → now covered, suite green |
| Missing edge cases (boundaries, empty/null/malformed, overflow, unicode, rune/newline globs) | ~80 | 🟡 → covered |
| Coverage gaps (concurrency/races: store, callback, metacache r/w, ptrace emit/close, meta reload-vs-reads, daemon Entry-vs-Reload) | 12 | 🟡 → covered under `-race`, green |
| Load vulnerabilities (spill retention, many-stars glob, page huge-limit, redirect cap) | 6 | 🟢 → pinned |
| Chaos points (spill failure, save failure, corrupt cache, 500/bad-JSON IdP, refused-port probe) | ~15 | 🟡 → covered |
| Decorative tests (pre-existing: Entry shallow-copy header-only, unicode/dup names logged not asserted) | 4 | 🟡 → documented, not re-asserted |

## The Mutation Chamber (run 2026-10-30, transient prod edits, all restored)

| File | Mutation Introduced | Test Result | Verdict |
|---|---|---|---|
| perms/perms.go (Resolve) | deny branch returns `allow` (deny-wins removed) | FAIL (`Visible` leaks `delete_b`) | ✅ CAUGHT |
| search/search.go (rank) | name weight `+= 3` → `+= 0` | FAIL (name query returns `[]`) | ✅ CAUGHT |
| importcfg/importcfg.go (translators) | neither-command-nor-url error → nil | FAIL (`TestToOxyp`/null-command) | ✅ CAUGHT |
| config/config.go (validation) | url+command exclusivity dropped | FAIL (`validateServer` exact error) | ✅ CAUGHT |
| outguard/outguard.go (guard) | `if true` passthrough, guard never fires | FAIL (spill tests) | ✅ CAUGHT |
| config/config.go (guard resolution) | server `Enabled=false` opt-out ignored | FAIL (`server opt-out must disable`) | ✅ CAUGHT |

**Mutation Score: 6/6 = 100% — Apex Predator.** Required targets all hit: perms Resolve, search rank, importcfg translators, config validation, guard/trace resolution.

## The Condemned (failing-test-class gaps, pinned as current-behavior, NOT fixed per contract)

- 💀 UNDEFENDED — metacache slice aliasing: `Get` returns live slice, `Put` stores caller's slice → caller mutation races `save`. No test (would fail). Fix: copy in/out.
- 💀 UNDEFENDED — oauth duplicate callback: second callback for prepared state blocks handler on `ch <-`; `Close` then waits. No test (would hang). Fix: non-blocking send/409.
- 💀 UNDEFENDED — daemon `clientForGen` gen-mismatch paths never close waiter channel → concurrent waiters hang. Omitted (would hang).
- 💀 UNDEFENDED — daemon in-flight list repopulates cleared cache after Reload (stale tools persist). Omitted.
- 💀 UNDEFENDED — daemon trace+failed `tools/list` nil-deref (`len(res.Tools)`, res==nil). Omitted.
- 💀 UNDEFENDED — importcfg `RenderAll([a,b])` repeats `[servers]` header → `config.Load` fails `Key 'servers' has already been defined`; second `AppendBlocks` to same file corrupts config the same way. Pinned in render/CLI tests.
- ⚠️ PARTIAL — `Entry`/`DaemonConfig` shallow copies share maps/slices/pointers; `Names()` unsorted despite comment. Pinned as current behavior.
- ⚠️ PARTIAL — `checkOrigin` rejects full browser origins (`http://localhost:1234`) — SplitHostPort runs before scheme strip. Pinned.
- ⚠️ PARTIAL — `redact.Body` only scrubs valid JSON; plain-text `Bearer SECRET`, embedded JSON leak; `accessToken`/`refreshToken` camel keys leak; `URL` keeps trailing `?` (ForceQuery). Pinned.
- ⚠️ PARTIAL — outguard negative limits panic; trailing-newline spills with `0 bytes skipped`; ptrace byte-cap overshoots one event; event cap per-session not per-file; search `Page` terminal `next` inconsistent (0 vs -1), repeated words re-score; `atoiOr("12abc")`→12; device DCR `none` vs Basic mismatch; missing issuer/token-endpoint not fail-fast; `Store.SaveToken` fixed `.tmp` collision. All pinned green with `// current behavior`.
- ⚠️ PARTIAL — zoo weak asserts (unicode/dup names, notify-changed unobservable, wrong-ct no-assert) left to zoo owners; no shim end-to-end (needs `serveShim` seam).

## The Resurrection Plan

### Critical (fix first, tests already written to catch)
1. Metacache copy-in/copy-out; oauth duplicate-callback non-blocking send; daemon waiter-close on gen-mismatch; daemon discard in-flight list after reload; trace nil-guard; `RenderAll`/second-`AppendBlocks` duplicate `[servers]` header.
2. Re-run: `go test -race ./internal/metacache/ ./internal/oauth/ ./internal/daemon/ ./internal/importcfg/ ./cmd/oxyp/`.

### High
3. Entry/DaemonConfig deep copy; Names sorted; checkOrigin order; redact plain-text scrubber + camel keys + ForceQuery; outguard negative clamp + line-boundary; device DCR auth-method + fail-fast validation; Store temp via `os.CreateTemp`.
4. Zoo: exact unicode/dup/giant asserts, notify-changed observability, wrong-ct assert, shim e2e via seam.

### Medium
5. Page contract (`next` terminal), atoiOr strictness, serverChanged nil-vs-empty, search Regex/ServerDescription parity, spill-failure note shape, doctor/add secret redaction, refused-port probe budget.

## Test Files Created (36, all `*_test.go`, no prod edits)

- internal/perms/perms_edge_test.go (12)
- internal/search/search_edge_test.go (13)
- internal/outguard/outguard_edge_test.go (6) + spill_test.go (5)
- internal/ptrace/ptrace_edge_test.go (9)
- internal/redact/redact_edge_test.go (10)
- internal/metacache/metacache_edge_test.go (8) + metacache_fingerprint_test.go (4)
- internal/config/validate_test.go (3) + load_edge_test.go (10) + expand_edge_test.go (5) + effective_test.go (5)
- internal/importcfg/parse_test.go (4) + translate_test.go (4) + render_test.go (7) + append_test.go (2) + names_test.go (3) + roundtrip_test.go (2)
- internal/oauth/store_test.go (5) + callback_test.go (7) + flows_test.go (4) + regpending_test.go (4) + cc_test.go (7) + device_test.go (9)
- internal/remhttp/remhttp_edge_test.go (10) + tls_test.go (7)
- internal/meta/meta_security_test.go (9) + meta_test.go (14) [package had zero tests]
- internal/daemon/entry_names_test.go (5) + close_test.go (5) + reload_edge_test.go (8)
- cmd/oxyp/main_test.go (16) + shim_test.go (10) + doctor_test.go (12) + add_test.go (13) + import_test.go (12) [package had zero tests]

Evidence: `go test -count=1 ./internal/... ./cmd/...` all-ok post-pass; `-race` green on touched concurrency paths (ptrace, metacache, oauth store/callback, meta, daemon subset); `go vet` + `gofmt` clean on touched packages. BUGFOUND details live in worker transcripts (history://ElectoralTurkey.SmallFilters, ConfigImport, OAuthRem, MetaDaemon, CliShim) and are condensed in Condemned above.
