---
node_type: spec
title: Daemon Lifecycle, Health, Reload, and Config Dir
status: active
updated: 2026-10-30
tags: [daemon, lifecycle, health, reload, config]
links:
  depends_on: [/overview.md]
  documents: [/internal/daemon/, /internal/config/, /cmd/oxyp/main.go]
  verified_by: [/verification/strategy.md]
---

# Daemon Lifecycle Behavior

## Purpose

This spec covers the daemon's process lifetime: how servers start cold or
eager, when idle children stop, what health means, what reload changes, how
shutdown works, and where config/endpoint/cache files live. It matters
because cold-start-then-reuse is the core.opex promise: listing must never
spawn, calling must revive, and a bad entry must never kill the daemon.

## Boundaries

**Handles:** lifecycle modes, idle sweep, eager warmup, health recording,
reload diffing, graceful shutdown, endpoint file, config dir resolution.
**Explicitly does NOT:** tool ranking (see
[/spec-discovery.md](/spec-discovery.md)), OAuth dances (see
[/spec-oauth.md](/spec-oauth.md)), HTTP route shapes (see
[/spec-http-shim.md](/spec-http-shim.md)).
**If it happens anyway:** cross-cutting concerns defer to their own specs.

## Goals

- **G1:** A fresh daemon answers listings without spawning any downstream
  server, and answers calls by spawning only the target.
- **G2:** Idle local children stop automatically while their metadata stays
  searchable; remotes are never idle-stopped.
- **G3:** `reload` (HTTP or SIGHUP) applies config without dropping
  unrelated connections; listener addr/token changes require a restart with
  a visible warning.
- **G4:** A misconfigured server degrades to unhealthy status; the daemon
  keeps serving everything else.

## Requirements

### Requirement: Lifecycle modes

The system MUST support four lifecycle modes with `""` defaulting to `lazy`.

**As a** operator
**I want** declarative control over cold vs warm servers
**So that** interactive agents stay snappy while batch servers stay hot.

| Mode | Eager start at daemon boot | Idle stopped |
|---|---|---|
| `lazy` (default, incl. `""`) | no | yes, when the effective timeout > 0 |
| `eager` | yes | only when a server- or daemon-level idle timeout is set |
| `keep-alive` | yes | never |
| `lazy-keep-alive` | no | never |

Unknown lifecycle values MUST NOT fail the daemon; the entry gets a
`ConfigError`, stays listed, and reports unhealthy. `enabled = false`
removes the server from the daemon map entirely (404/hidden, disk cache
dropped).

#### Scenario: Cold default

- **GIVEN** a daemon with a local server with no `lifecycle` key
- **WHEN** the daemon finishes booting
- **THEN** no child process exists for that server, and `tools`/`search`
  answer from disk cache or spawn-on-demand only.

*Definition of Done: If this scenario passes, the requirement is met.*

#### Scenario: Unknown lifecycle degrades

- **GIVEN** a server with `lifecycle = "turbo"`
- **WHEN** the daemon loads config
- **THEN** the daemon stays up, the server is listed, and health reports the
  config error.

*Definition of Done: If this scenario fails to stay up, the system is broken.*

Verification: `V-DAEMON-001` (lifecycle unit tests), `V-ZOO-001`
(cold-remote / slow-init edges).

### Requirement: Idle sweep

The system MUST stop idle local children and MUST keep their discovery
caches. The sweep ticker runs every 30 s; the default effective timeout is
10 min (`server idle_timeout` > `daemon idle_timeout` > 10); `0` means
never.

**As a** operator
**I want** idle children reaped without losing searchability
**So that** long-running daemons do not accumulate processes.

Stop requires ALL of: a live client, no spawn in flight, zero in-flight
tool calls, a non-zero `lastUsed` older than the cutoff. The sweep skips
remote servers (closing a remote handle frees nothing). `lastUsed` is set
on successful spawn and on `CallTool` completion only; list/resource/prompt
RPCs neither guard (`inFlight` counts `CallTool` only) nor refresh it.
Stopping closes the client directly and keeps memory + disk tool caches, so
the server stays searchable and restarts on next call.

#### Scenario: Idle local stops, metadata survives

- **GIVEN** a lazy local server called 11 minutes ago with default timeouts
- **WHEN** the 30 s sweep runs
- **THEN** the child is closed and `search` still returns its tools.

*Definition of Done: If this scenario passes, the requirement is met.*

#### Scenario: Busy server is never stopped

- **GIVEN** a lazy local server with a tool call in flight past the cutoff
- **WHEN** the sweep runs
- **THEN** the client is left alone.

*Definition of Done: If a busy server is stopped, the limit is not enforced.*

Verification: `V-DAEMON-002`.

### Requirement: Eager warmup never blocks serving

The system MUST bind the listener and publish the endpoint file BEFORE eager
warmup, and MUST run warmup asynchronously.

**As a** operator
**I want** `/health` reachable during slow eager connects
**So that** one unreachable eager server cannot fail autostart.

Startup order is: `net.Listen` → write `<dir>/daemon.json` → spawn
`EagerStart`/`PingLoop`/`SweepLoop` goroutines → `Serve`. Eager start
connects `eager`/`keep-alive` servers concurrently (skipping config-error
entries); each init is bounded by 30 s and failures only record health.

#### Scenario: Slow eager does not block endpoint

- **GIVEN** an eager server that hangs for 25 s
- **WHEN** the daemon boots
- **THEN** `/health` answers immediately and the server later reports
  healthy or its error.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-DAEMON-003`.

### Requirement: Health model

The system MUST record per-server health as `{connected, healthy, error,
auth_required}` where `auth_required` is a login hint, never an error
string.

**As a** agent harness
**I want** machine-readable per-server state
**So that** I can route around broken servers and prompt login for gated
ones.

Transitions: spawn/initialize or any successful RPC → healthy; ping success
(10 s interval, 3 s timeout, connected clients only) → healthy + latency;
ordinary failure → unhealthy + redacted error; OAuth needed (spawn-time or
mid-session) → unhealthy + `auth_required: "run: oxyp login <name>"` with
no error string; config error → forced unhealthy `config: …`. No record
means never checked. `/health` returns `{"servers": {name: {...}}}` only
(no daemon-level fields); `/servers` adds description/instructions. Cold
servers are never pinged.

#### Scenario: OAuth needed surfaces login hint

- **GIVEN** a remote that 401s with `resource_metadata`
- **WHEN** any RPC runs
- **THEN** health carries `auth_required: "run: oxyp login <name>"` and the
  invoke path answers HTTP 401 with the same hint.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-DAEMON-004`, `V-OAUTH-001`.

### Requirement: Reload semantics

The system MUST apply config changes without restarting: added servers
enter cold; removed/disabled servers are stopped and dropped; modified
servers are stopped, have tools/resources/prompts/instructions cleared, get
a config-generation bump, and stay cold until next use; unchanged servers
keep connections, caches, and health. Disk fingerprints refresh. Client
closes happen outside the daemon lock with a 5 s per-client cap.

**As a** operator
**I want** `oxyp reload` (or `kill -HUP`) to re-read config safely
**So that** I can add servers without dropping agent traffic.

`daemonCfg` (idle/guard/trace values) applies immediately, but the tracer
file itself opens only at startup — enabling trace needs a restart.
Listener addr and bearer token are NEVER applied by reload; the startup
values stay, the endpoint file keeps them, and the daemon logs
`oxyp: daemon addr/token changed — restart required` to stderr. SIGHUP
reload discards errors silently; `POST /reload` returns
`{added, removed, modified}` or HTTP 500. A per-server generation counter
discards clients spawned from pre-reload config.

#### Scenario: Reload swaps one server, keeps others

- **GIVEN** a running daemon with servers A (connected) and B
- **WHEN** the operator edits B's command and runs `oxyp reload`
- **THEN** the response is `{added: [], removed: [], modified: [B]}`,
  A's connection is untouched, and B is cold with cleared caches.

*Definition of Done: If this scenario passes, the requirement is met.*

#### Scenario: Token rotation needs restart

- **GIVEN** a running daemon
- **WHEN** the operator changes `daemon.token` and runs `oxyp reload`
- **THEN** the old token stays valid and a restart warning is logged.

*Definition of Done: If the new token took effect without restart, the
system is broken.*

Verification: `V-DAEMON-005`.

### Requirement: Shutdown and endpoint

The system MUST shut down gracefully on SIGINT/SIGTERM (HTTP shutdown with
10 s timeout, then cancel the spawn context killing stdio children/SSE
streams, then bounded 5 s-per-client closes running concurrently), and MUST
remove the endpoint file.

**As a** operator
**I want** clean restarts with no orphaned children or stale endpoints
**So that** autostart never attaches to a dead daemon.

The endpoint file `<dir>/daemon.json` is `{url, pid, token, startedAt}`;
fresh files are created 0600 (an existing file keeps its mode — tighten it
manually). Stale files are rejected by a dead-PID check. HTTP server
limits are read-header 10 s / read 30 s / write 60 s / idle 120 s.

`GET /health` carries daemon identity as `{pid, startedAt, servers}` so
CLI probes can tell "our daemon" from a stranger on the same port. Daemon
startup pre-flights the endpoint: a live oxyp on our addr is a clean
no-op (`already running (pid N)`, exit 0). A bind failure is classified
the same way — ours is a no-op, anything else is the only real error
(`not our daemon — something else holds the port`).

#### Scenario: Double start attaches, crash recovers, stranger fails loud

- **GIVEN** a running daemon
- **WHEN** a second CLI command runs
- **THEN** it autostarts nothing and talks to the live daemon; after a
  crash, the stale endpoint is rejected and a new daemon starts; when a
  non-oxyp process holds the port, startup fails with the stranger error
  (not a bare bind failure).

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-DAEMON-006`.

### Requirement: Config dir and connectors

The system MUST resolve the config dir as `--dir` > `OXYP_DIR` >
`XDG_CONFIG_HOME/oxyp` > `~/.oxyp`, MUST treat the TOML key as the server
name (no `name =` field), and MUST auto-discover executable files in
`connectors/` (or `daemon.connectors_dir`) by filename stem, with explicit
TOML entries winning case-insensitively.

**As a** operator
**I want** drop-in connectors and portable dir overrides
**So that** a second identity is just a second `--dir`.

Unreadable/missing connectors dir is fine; subdirectories are skipped; a
file needs any executable bit. Discovered entries get
`description = "Auto-discovered connector <filename>"` and
`command = [<absolute path>]`, then normal expansion/validation.

#### Scenario: Drop-in connector appears

- **GIVEN** an executable `connectors/echo.py`
- **WHEN** the daemon loads
- **THEN** a server named `echo` exists with that argv; a TOML
  `[servers.echo]` entry would win over it.

*Definition of Done: If this scenario passes, the requirement is met.*

Verification: `V-CLI-001`, `V-DAEMON-001`.

## Data Contracts

### Endpoint file (`daemon.json`)

| Field | Type | Required | Notes |
|---|---|---|---|
| url | string | yes | `http://<addr>` |
| pid | int | yes | daemon PID, liveness-checked |
| token | string | yes | may be `""` (loopback trust) |
| startedAt | string | yes | RFC3339 |

## State Transitions

| From State | Event | To State | Side Effects |
|---|---|---|---|
| cold | first call/list-miss | connected | child spawned / remote initialized, 30 s budget |
| connected | idle past cutoff (local) | cold (cached) | client closed, metadata kept |
| connected | list_changed / reset / reload-modify | connected or cold | caches cleared, generation bumped |
| any | SIGINT/SIGTERM | stopped | HTTP 10 s drain, clients closed, endpoint removed |

## Error Handling

| Error | HTTP | When | Response |
|---|---|---|---|
| server misconfigured | per-route 404/502 | `ConfigError` entry used | `config: …`, healthy false |
| downstream OAuth needed | 401 | 401 + metadata / mid-session OAuth sentinel | `auth_required: run: oxyp login <name>` |
| other downstream failure | 502 | init/RPC failure | redacted error + probe hint (remote init) |
| reload load failure | 500 | unreadable/unparseable TOML | `{"error": …}` |

## Security Considerations

- The daemon MUST fail `config.Load` when a non-loopback addr pairs with an
  empty token; loopback detection fails closed on empty/unparseable hosts.
- Reload MUST NOT swap the bearer token or listener without a restart.
- `daemon.json` holds the bearer token; fresh files MUST be 0600.

## Limits

| Constraint | Value | Reason |
|---|---|---|
| Initialize budget | 30 s | bound slow servers |
| Per-RPC budget | 10 s | bound slow tools |
| Health ping timeout / interval | 3 s / 10 s | liveness without load |
| Idle sweep ticker / default timeout | 30 s / 10 min | reap without churn |
| Remote metadata TTL | 5 min | bound staleness without push |
| Endpoint write mode | 0600 (new files) | token secrecy |
| HTTP server timeouts | 10/30/60/120 s | slowloris resistance |
| Graceful HTTP shutdown | 10 s | drain in-flight |
| Per-client close cap | 5 s | one wedge child cannot wedge reload |
| Autostart wait / `/health` probe / CLI API | 5 s / 2 s / 60 s | fast attach, patient calls |
