---
node_type: spec
title: CLI — Commands, Doctor, Add, and Import
status: active
updated: 2026-10-30
tags: [cli, doctor, add, import, connectors]
links:
  depends_on: [/overview.md, /spec-daemon.md, /spec-http-shim.md]
  documents: [/cmd/oxyp/, /internal/importcfg/]
  verified_by: [/verification/strategy.md]
---

# CLI Behavior (Commands / Doctor / Add / Import)

## Purpose

This spec covers the `oxyp` command surface: 13 subcommands, the REST
wrappers that autostart the daemon, offline/live `doctor`, URL probing
`add`, and third-party `import`. It matters because the CLI is the
operator's whole control plane — every flag, prompt, exit code, and file
mode is contract.

## Boundaries

**Handles:** command table, autostart, doctor modes, add probe/append,
import sources/shapes/translation/rendering, confirmation prompts, file
modes.
**Explicitly does NOT:** daemon internals, ranking, OAuth flows (see
sibling specs).
**If it happens anyway:** sibling specs own the semantics.

## Goals

- **G1:** Every read/write command works with zero daemon management
  (autostart, attach, or clear "did not come up").
- **G2:** `doctor` distinguishes config errors from live failures from
  auth-gated servers, offline and live.
- **G3:** `add`/`import` never write unprobed garbage and never leave
  credentials world-readable.
- **G4:** Imported foreign configs become valid `oxyp.toml` that loads
  first try.

## Requirements

### Requirement: Command table and autostart

The binary MUST offer 13 subcommands under global `[--dir <dir>]` (which
beats all dir resolution): `daemon`, `call`, `servers`, `tools`, `search`,
`describe`, `status`, `reload`, `shim`, `login`, `doctor`, `add`, `import`.

**As a** operator
**I want** one binary for everything
**So that** no separate client tools are needed.

| Command | Args / flags | Action |
|---|---|---|
| `daemon` | `--dir` only | foreground daemon (trace, 10/30/60/120 s HTTP, 0600 endpoint, SIGHUP, 10 s shutdown) |
| `call` | `<server> <tool> [json]` | `POST /servers/{s}/tools/{t}` `{parameters}`; bad JSON → `bad json args`; ≥400 → exit 1 |
| `servers` | `--filter` | `GET /servers[?filter=]` |
| `tools` | `<server>` | `GET /servers/{s}/tools` |
| `search` | `<query>`; `--server/--regex/--limit=12/--offset=0` | `GET /tools?…`; cached metadata, never spawns |
| `describe` | `<server> <tool>` | `GET /servers/{s}/tools/{t}` |
| `status` | — | `GET /health` (ignores HTTP status) |
| `reload` | — | `POST /reload`; ≥400 → exit 1 |
| `shim` | — | stdio JSON-RPC ↔ daemon HTTP |
| `login` | `<server>`; `--complete <url-or-code>` | flow dispatch, 5 min ctx; port rules per OAuth spec |
| `doctor` | `--live/--json` | offline validation or 15 s-per-server live probe |
| `add` | `<url>`; `--name/--write` | probe + append `[servers.<name>]` |
| `import` | `--from all\|cursor\|claude\|vscode\|opencode\|omp`; `--write` | preview + append foreign servers |

Shared: 60 s HTTP client, bearer from `daemon.json`, pretty JSON print,
`exit 1` on Cobra errors and checked ≥400s. Autostart: read endpoint
(reject empty URL / `PID ≤ 0` / dead PID), probe `/health` (2 s timeout),
else spawn `oxyp daemon --dir <dir>` and poll 50×100 ms (≈5 s) or fail
`daemon did not come up`.

#### Scenario: First command boots the daemon

- **GIVEN** no daemon running
- **WHEN** `oxyp servers` runs
- **THEN** a daemon starts, the endpoint writes, and the listing prints.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-CLI-001`.

### Requirement: Doctor (offline vs live)

Offline `doctor` MUST validate with `config.Load` only (no network):
disabled → `disabled` (`skipped (enabled = false)`); `ConfigError` →
`failed` (`config: …`); else `ok` (`remote <url>` / `local %q`). Live
`doctor --live` MUST build an in-process daemon with NO disk cache and
probe each enabled/valid server with a 15 s context (daemon RPCs further
capped by 10 s req / 30 s init budgets): `AuthNeededError` → `needs-auth`
+ `run: oxyp login <name>`; other error → `failed` (loop continues);
success → `ok` + tool count. Output is indented JSON with `--json`, else
human lines; exit is 1 iff any result is `failed`/`needs-auth` (disabled
ignored). Missing `oxyp.toml` fails before either mode. Names sort.

**As a** operator
**I want** "is it config or is it live" in one command
**So that** I triage without curl.

#### Scenario: Live doctor separates auth from broken

- **GIVEN** one OAuth-gated and one refused-port server
- **WHEN** `oxyp doctor --live` runs
- **THEN** results are `needs-auth` and `failed` respectively, exit 1.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-CLI-002`.

### Requirement: Add (probe, preview, append)

`add` MUST derive/sanitize a unique name (`--name` trimmed + sanitized, or
`NameFromURL`: valid `http(s)://` + host, strip `www.`, dots→dashes, drop
port; `UniqueName` → `base`, `base-2`, …), build a URL-only entry
(`description = "Added via oxyp add"`, never `auth = "oauth"`), probe it
through an in-process daemon (15 s budget): other-errors abort with no
write; `needs-auth` still writes with notes. It MUST always preview
(`<name>: ok (N tools)` or `needs-auth…` + exact TOML block), then write
only on `--write` or an interactive `y`/`yes` (EOF/error = no; decline
prints `not written`, exit 0). Append uses `AppendBlocks` (create 0600,
tighten group/world-readable to 0600 with a note), prints
`added [servers.<name>] to <path>` plus `next: oxyp login <name>` when
gated.

**As a** operator
**I want** paste-URL → working server
**So that** onboarding is one command.

#### Scenario: Add a live server

- **GIVEN** `https://mcp.example.com/mcp` serving 2 tools
- **WHEN** `oxyp add <url> --write` runs
- **THEN** the probe reports `ok (2 tools)` and `oxyp.toml` gains a
  `[servers.mcp-example-com]` block at 0600.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-CLI-003`.

### Requirement: Import (sources, shapes, translation, rendering)

`import` MUST read cursor/claude/vscode/opencode/omp configs (first
existing file per app, fixed order cursor→claude→vscode→opencode→omp,
`--from all` default), skip missing/unreadable/unparseable sources with
stderr notes, drop only the failing entry on conversion errors, sort +
sanitize + uniquify names against config + earlier imports, preview the
full text (`nothing to import` → exit 0), and append on confirmation
(same prompt/`--write`/`AppendBlocks` as `add`).

**As a** migrant from another harness
**I want** my servers carried over verbatim
**So that** switching costs one command.

Accepted shapes: cursor/claude/omp `{"mcpServers": …}`; vscode merges
`{"mcp": {"servers": …}}` < top-level `{"servers": …}` <
`{"mcpServers": …}` (later wins); opencode `{"mcp": …}` with JSONC
comments stripped. Translation: `Imported from <app>`; `enabled`
carried; URL wins over command (noted); truthy oauth → `auth = "oauth"`;
remote `env`/`cwd` and local `headers`/`transport`/`oauth` silently
dropped (only timeout + oauth-remote produce notes); neither command nor
URL → entry error. Rendering: header exactly `[servers.<name>]` (NEVER a
bare `[servers]` super-header), field order
description/command/url/transport/headers/env/cwd/enabled/auth, sorted-key
inline maps, foreign notes as `# note: …` with CR/LF flattened,
`RenderAll` newline-joined with trailing newline; `AppendBlocks` keeps
valid TOML across repeated appends (blank-line separated).

#### Scenario: Import vscode top-level servers

- **GIVEN** a vscode `mcp.json` with top-level `{"servers": {…}}`
- **WHEN** `oxyp import --from vscode` runs
- **THEN** every server previews as `[servers.<name>]` blocks and `--write`
  appends them loadably.

*Definition of Done: If this scenario misses servers, the system is
broken.*

Verification: `V-CLI-004` (incl. mutation: neither-command-nor-url
translation is caught).

### Requirement: File modes and confirmation

`AppendBlocks` MUST create `oxyp.toml` at 0600 (mkdir 0755) and MUST
tighten an existing group/world-readable file to 0600 with the note
`tightened <path> to 0600 (held credentials)`. Prompts go to stderr
(`Write to <path>? [y/N]`), read one stdin line, accept only `y`/`yes`
case-insensitively.

**As a** security-conscious operator
**I want** imported bearer tokens never world-readable
**So that** migration cannot leak credentials.

#### Scenario: Import tightens 0644

- **GIVEN** an existing `oxyp.toml` at 0644
- **WHEN** `oxyp import --write` appends
- **THEN** the file is 0600 and the note prints.

*Definition of Done: If this scenario leaves 0644, the system is broken.*

Verification: `V-CLI-005`.

## Data Contracts

### Doctor result

| Field | Type | Required | Notes |
|---|---|---|---|
| name | string | yes | server name, sorted |
| status | string | yes | `ok` / `failed` / `needs-auth` / `disabled` |
| tools | int | no | live `ok` count |
| detail | string | no | error / login hint / skip reason |

## State Transitions

| From State | Event | To State | Side Effects |
|---|---|---|---|
| no daemon | CLI command | daemon ensured | spawn + 5 s wait or reuse |
| no config | add | empty base | uniqueness from CLI only |
| preview shown | confirm/`--write` | appended | 0600 create/tighten + notes |
| preview shown | decline/EOF | not written | exit 0 |

## Error Handling

| Error | When | Response |
|---|---|---|
| bad JSON call args | `call` | `bad json args`, exit 1 |
| probe failure (non-auth) | `add` | error, nothing written |
| unknown `--from` | `import` | usage error |
| per-entry convert fail | `import` | entry dropped, rest continue |
| source unreadable | `import` | stderr note, source skipped |
| daemon did not come up | any auto cmd | clear failure after 5 s |

## Security Considerations

- `oxyp.toml` holds credentials in headers/env; it MUST be 0600
  (create + tighten).
- `login` MUST NOT print tokens; `--complete` input MUST NOT echo secrets.
- Imported literal `Authorization` values SHOULD be rotated to `${VAR}`
  by the operator (follow-up note, not auto-rewritten).

## Limits

| Constraint | Value | Reason |
|---|---|---|
| Autostart wait | 5 s (50×100 ms) | fast attach |
| Live probe (doctor/add) | 15 s per server | bounded triage |
| Daemon RPC inside probe | 10 s req / 30 s init | consistent budgets |
| CLI API timeout | 60 s | patient calls |
