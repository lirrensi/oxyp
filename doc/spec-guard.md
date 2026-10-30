---
node_type: spec
title: Output Guard, Trace, Redaction, Transport, Secrets, and Config
status: active
updated: 2026-10-30
tags: [output-guard, trace, redact, remhttp, config, secrets]
links:
  depends_on: [/overview.md, /PHILOSOPHY.md]
  documents: [/internal/outguard/, /internal/ptrace/, /internal/redact/, /internal/remhttp/, /internal/config/expand.go]
  verified_by: [/verification/strategy.md]
---

# Guard / Trace / Secrets Behavior

## Purpose

This spec covers the protective layer around tool output and secrets: the
output guard that spills giant results, the JSONL tracer, the redactor, the
remote HTTP transport (redirects/TLS), `${VAR}`/`{file:}` expansion, and
the full config schema incl. `cwd`/`inherit_env` and `OXYP_DIR`. It matters
because tool output is untrusted and arbitrarily large, and secrets ride in
headers/env/URLs that must never leak into logs or errors.

## Boundaries

**Handles:** guard caps + spill retention, trace file + caps, redaction
coverage, redirect/TLS policy, expansion per field, config schema,
`cwd`/`inherit_env`/`env`, `OXYP_DIR`.
**Explicitly does NOT:** ranking, OAuth, routes (see sibling specs).
**If it happens anyway:** sibling specs own those behaviors.

## Goals

- **G1:** No single tool result can blow up agent context — over-limit text
  spills to a 0600 file and the model sees a bounded head + path.
- **G2:** Secrets expand at load from env/files, fail closed where
  configured, and never reach logs/errors literally.
- **G3:** Trace captures list/call metadata (never payloads) under explicit
  caps.
- **G4:** Remote fetches follow same-origin POST redirects, strip
  credentials cross-origin, and honor custom CA/mTLS.

## Requirements

### Requirement: Output guard and spill retention

Only `POST /servers/{name}/tools/{tool}` content blocks are guarded, and
only `mcp.TextContent` (value type); image/audio/resource blocks,
`structuredContent`, and `_meta` pass untouched. Over-limit means
`len(text) > max_bytes` OR line-count > `max_lines` (defaults 51200 / 2000;
`0` silently defaults; negatives rejected at load, clamped to 0 at apply).
The head preview cuts lines then bytes then UTF-8-trims; the emitted text
is preview + `\n…[output guard: showing head, N bytes skipped; full output
in <path>]` (or `; spill failed: <err>` — `spilled` stays true). The input
slice is never mutated.

**As a** agent harness
**I want** bounded tool text with full output one file-read away
**So that** context stays small without losing data.

Spill files are `os.TempDir()/oxyp-output-*.txt` at 0600 holding the FULL
original text. Retention pruning runs synchronously on every spill: files
older than 24 h are removed; the rest sort newest-first and anything past
newest-100 or past 100 MiB total is removed. There is NO periodic reaper
and NO shutdown cleanup — a 25-hour-old spill survives until the next
spill. Effective config: server block first (full replacement, NOT a
merge — a server block with nil `Enabled` is armed with server defaults),
then daemon block, else off. Opt-out is the table form
`[servers.x.output_guard] enabled = false` (scalar `output_guard = false`
is a TOML type error). A server block with nil/true `Enabled` re-arms even
when the daemon disabled the guard.

#### Scenario: Chatty tool spills

- **GIVEN** daemon guard armed and a tool returning 1 MiB of text
- **WHEN** invoked
- **THEN** the model sees ≤ cap head + `full output in /tmp/oxyp-output-*`
  and the file at 0600 holds all bytes.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-GUARD-001` (incl. mutation: guard passthrough is caught).

### Requirement: Trace

The system MUST offer one daemon-level JSONL trace file capturing
`tools/list` and `tools/call` metadata (server, method, bytes, latency,
outcome, tool/block notes — NEVER payloads, args, results, auth, or URLs),
opened once at startup (`O_CREATE|O_WRONLY|O_APPEND`, 0600, existing files
tightened, content preserved). Defaults: `<dir>/trace.jsonl`, 262144
bytes, 10000 events; the byte cap can overshoot by one event; the event
cap is per-process-open, not per-file; after either cap `Emit` silently
drops. Enabling trace on the daemon OR any server opens the daemon file;
per-server `trace.file` is rejected (`trace.file is daemon-level only`);
per-server `max_bytes`/`max_events` are IGNORED (only the daemon block
reaches `Open`); only the enable bit is honored per server. Reload never
re-opens the tracer — trace changes need a restart.

**As a** debugger
**I want** a bounded call log without payloads
**So that** I can diagnose "what ran, how long, what happened".

#### Scenario: Trace records calls, not payloads

- **GIVEN** tracing enabled and a tool called with secrets in args
- **WHEN** the call completes
- **THEN** `trace.jsonl` gains one JSON line with method/latency/outcome
  and no arg/result/auth/URL bytes.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-GUARD-002`.

### Requirement: Redaction

Secret values MUST be scrubbed at API/log boundaries via `redact.Body`
(recursive case-insensitive key match over `authorization`,
`proxy-authorization`, `token`, `access-token`, `refresh-token`,
`auth-token`, `api-key`, `client-secret`, `password`, `secret` — any
`_`-`-`-space/case spelling — with plain-text `Bearer/Basic` and
`key=value` scrubbing for non-JSON input, depth cap 32 → `[DEEP]`, 8 KiB
UTF-8 truncation + `…[truncated]`) and `redact.URL` (strip userinfo, query,
fragment, force-query). Production call sites: upstream stderr log lines,
401/502 error bodies, `/health` errors, and probe-hint URLs. NOT covered:
`/servers` health errors, `doctor` details, `Headers`/`HeaderMap`
(no production callers), `cookie`/`id_token`/`private_key` keys, and secret
substrings inside JSON string values.

**As a** operator
**I want** `${VAR}` values to never appear in logs or API errors
**So that** debugging output is safe to paste.

#### Scenario: Token in URL never leaks

- **GIVEN** `url = "https://x/?token=SECRET"` (or a child printing it)
- **WHEN** init fails and `/health` is read
- **THEN** no response or log line contains `SECRET`.

*Definition of Done: If this scenario leaks, the system is broken.*

Verification: `V-GUARD-003`.

### Requirement: Remote transport (redirects, TLS, sessions)

The daemon MUST fetch remotes through `remhttp.NewClient`: proxy from env,
HTTP/2, idle 16/90 s, handshake 10 s; a 10 s client for RPC/OAuth and a
`Timeout: 0` client for long-lived SSE/stream bodies; per-RPC 10 s bounds
via contexts. Redirects: up to **4** followed (5 requests; the 5th errors
`too many redirects (5)`). Same-origin = same scheme AND host (port
counts). 301/302/303 POST is rebuilt as POST same-origin (body replayed
when `GetBody` exists); cross-origin POST becomes GET with no replay.
Cross-origin keeps ONLY `Accept`, `Content-Type`, `Mcp-Session-Id`,
`MCP-Protocol-Version` — `Authorization`/cookies/custom headers are
dropped (note: the session id deliberately crosses origins). TLS:
`ca_file` appends, `cert_file`+`key_file` pair for mTLS (both or neither;
files must stat), `insecure` dev-only, minimum TLS 1.2; unreadable CA/cert
content is silently ignored at dial (sharp edge — prefer `doctor --live`
to catch it). Legacy SSE remotes dial directly with `transport = "sse"`;
Streamable-HTTP failures with `ErrLegacySSEServer` fall back to SSE once
and re-initialize. Session expiry retries once after client reset.

**As a** enterprise user
**I want** custom CA + mTLS + sane redirects
**So that** internal MCP endpoints just work.

#### Scenario: Cross-origin redirect strips credentials

- **GIVEN** a remote that 302s cross-origin
- **WHEN** followed
- **THEN** `Authorization` is dropped, `Mcp-Session-Id` survives, and the
  5th redirect errors.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-GUARD-004`, `V-ZOO-002` (redirect + session batteries).

### Requirement: Secret expansion

`${NAME}` (`[A-Za-z_][A-Za-z0-9_]*` only; no `$VAR`/defaults/nesting) and
`{file:/abs/path}` (`~/` → home, relative → error, content
`TrimRight("\r\n")`, file read BEFORE env) MUST expand at load per field:

**As a** portable-deploy user
**I want** `${GLEAN_TOKEN}` and `{file:/run/secret}` in config
**So that** one TOML works on every box.

| Field | Expanded | Fail-closed |
|---|---|---|
| `daemon.token` | yes | any leftover `${}`/`{file:}` or read error → `Load` hard error |
| server `url`, `headers` values | yes | file error / leftover simple `${}` → per-entry `ConfigError` |
| `oauth.client_id/secret/scope/redirect_uri` | yes | file/env error or leftover `${}`/`{file:}` → `ConfigError` |
| server `env` values | yes | file error → `ConfigError`; leftover `${}` stays LITERAL (local passthrough) |
| description, command, cwd, keys, transport, TLS paths, addr, trace | no | literal |

`{port}` is not a file ref. Malformed `${X:-d}`/`${1}`/`{file:}` pass
literally (fail-open, pinned). Set-but-empty env counts as set.

#### Scenario: Unset remote var fails the entry closed

- **GIVEN** `headers = {Authorization = "Bearer ${MISSING}}"` on a remote
- **WHEN** config loads
- **THEN** the entry gets `ConfigError`, nothing is sent literally, and
  the daemon stays up.

*Definition of Done: If this scenario sends `${MISSING}` literally, the
system is broken.*

Verification: `V-GUARD-005`.

### Requirement: `cwd` / `inherit_env` / `env`

`cwd` is local-only (remote + `cwd`/`inherit_env` → `ConfigError`) and
defaults to the daemon's cwd (`cmd.Dir` only when set). `inherit_env`
defaults true (full daemon env); explicit false gives a minimal
`PATH`/`HOME`/`SYSTEMROOT`-if-set base. `env` overlays append in both
modes (remote `env` validates but is unused) with per-value expansion;
`PYTHONUNBUFFERED=1` is forced unless inherited/overlaid. A custom command
func is used whenever `cwd != ""` or `inherit_env` is explicit false.

**As a** connector author
**I want** hermetic tool environments when I ask for them
**So that** builds reproduce anywhere.

#### Scenario: Hermetic child sees minimal env

- **GIVEN** `inherit_env = false`, `env = {FOO = "bar"}`
- **WHEN** the child spawns
- **THEN** it sees `PATH`/`HOME` (+`SYSTEMROOT`), `FOO=bar`,
  `PYTHONUNBUFFERED=1`, and nothing else inherited.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-GUARD-006`.

### Requirement: Config schema and `OXYP_DIR`

Top-level TOML is EXACTLY `[daemon]` + `[servers.<name>]` (key IS the
name). `[daemon]`: `addr` (default `127.0.0.1:8379` applied after
validation), `token`, `connectors_dir` (default `<dir>/connectors`),
`idle_timeout` (default 10, 0 = never), `output_guard`
(defaults 51200/2000), `trace` (defaults `<dir>/trace.jsonl`, 262144,
10000). Per server: `description`, `command` XOR `url` (URL needs
`http(s)://`), `transport` (`""`/`sse`), `headers` (nonblank, no CR/LF),
`env`, `cwd`, `inherit_env`, `enabled` (nil = true), `tools`
`{allow, deny}` (nonblank globs), `tls` (cert+key both/neither, files
statable), `auth` (`""`/`oauth`, remote-only), `oauth` (`flow`
browser/device/token, `grant_type` `""`/client_credentials + id/secret,
`callback_port ≥ 0`, `timeout ≥ 0`), `lifecycle` enum, `idle_timeout`,
`output_guard`, `trace`. Unknown lifecycle/idle/guard/trace values and
local `transport`/`headers`/`tls`/`auth`/`oauth` (or local-no-command)
become per-entry `ConfigError` — `Load` fails ONLY for
unreadable/unparseable TOML, bad `daemon.token` expansion, or
`ValidateDaemon` failure. Dir precedence:
`--dir` > `OXYP_DIR` > `XDG_CONFIG_HOME/oxyp` > `~/.oxyp`.

#### Scenario: Bad entry never kills daemon

- **GIVEN** one server with `url` + `command` both set
- **WHEN** the daemon loads
- **THEN** that server reports `config: url and command are exclusive`
  and everything else runs.

*Definition of Done: If this scenario kills the daemon, the system is
broken.*

Verification: `V-GUARD-007` (incl. mutation: exclusivity removal caught).

## Data Contracts

### OutputGuardConfig / TraceConfig (effective)

| Field | Type | Default | Notes |
|---|---|---|---|
| output_guard.enabled | *bool | nil = defer | table form only; false = off for scope |
| output_guard.max_bytes / max_lines | int | 51200 / 2000 | 0 → default; negative → load error |
| trace.enabled | bool | false | daemon OR any server opens file |
| trace.file | string | `<dir>/trace.jsonl` | daemon-level only; server file rejected |
| trace.max_bytes / max_events | int | 262144 / 10000 | daemon block only; server values ignored |

## State Transitions

| From State | Event | To State | Side Effects |
|---|---|---|---|
| result small | invoke | inline result | no spill |
| result over-limit | invoke | head + spill path | 0600 file, prune run |
| trace under caps | list/call | event appended | JSONL line |
| trace at caps | list/call | dropped | silent |
| secret unset (remote) | load | entry ConfigError | daemon stays up |

## Error Handling

| Error | When | Response |
|---|---|---|
| spill write/chmod fail | over-limit invoke | head + `spill failed: <err>`, file removed |
| `output_guard = false` scalar | load | TOML type error (use table form) |
| server `trace.file` | load | `trace.file is daemon-level only` |
| non-loopback + empty token | load | hard error |
| TLS files missing | load | `ConfigError` (stat); bad CONTENT fails at dial |

## Security Considerations

- Spill files MUST be 0600; full output (possibly sensitive) persists
  until retention prunes it on the next spill.
- `daemon.token` MUST expand before the loopback check; leftovers MUST
  hard-fail load.
- Remote unset secrets MUST fail the entry closed, never travel literally.
- mTLS `insecure = true` is self-signed-dev-only and MUST be documented
  as such everywhere it appears.

## Limits

| Constraint | Value | Reason |
|---|---|---|
| Guard defaults | 51200 B / 2000 lines | bounded context |
| Spill retention | newest 100 / 100 MiB / 24 h (on-spill prune) | bounded `/tmp` |
| Trace caps | 262144 B / 10000 events | bounded log |
| Redact truncation | 8 KiB + suffix | bounded errors |
| RPC / init | 10 s / 30 s | bounded dial |
| Redirects | 4 followed | loop bound |
