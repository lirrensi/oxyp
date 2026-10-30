# oxyp

One binary + one config = your agent, anywhere. Drop `oxyp` and `oxyp.toml`
on any box, set env, run — no keychain, no machine-bound state, no install
dance. See [stances](doc/PHILOSOPHY.md).

One daemon, every connector. A lazy MCP gateway in Go: N downstream MCP
servers behind 5 meta-tools, cold connections, deny-means-invisible
permissions, one-file connectors auto-discovered from `~/.oxyp/connectors/`.

Steals best practices from `mozilla-ai/mcpd` (spawn, stderr piping, health
pings, TOML, SIGHUP reload) and `gitlab-org/ai/lazy-mcp` (4 meta-tools,
lazy connect, endpoint file, glob permissions) — clean rewrite, no fork.

## Stances

Canon: [doc/PHILOSOPHY.md](doc/PHILOSOPHY.md).

- **Secrets: the harness procures, oxyp carries.** `${VAR}`/`{file:}`
  expand at load, redacted in logs. oxyp never runs secret commands or
  vault CLIs — procurement is execution, and execution is harness
  territory.
- **Portability is the feature.** Single binary, `${VARS}` config, nothing
  machine-bound. If a feature needs a keychain or a GUI, the feature is
  wrong.
- **No approval prompts in the daemon.** No UI to ask through. Config
  `deny`/hide is the whole permission story; ask-gates live on the
  harness side of the pipe. Visible-but-gated is out of scope.

```
<dir>/  (--dir > OXYP_DIR > XDG_CONFIG_HOME/oxyp > ~/.oxyp)
  oxyp.toml          # daemon addr/token + server entries
  connectors/        # executable one-file MCP servers, auto-discovered by name
  daemon.json        # written at runtime: url, pid, token, startedAt (0600 on create)
```

Canon docs: [overview](doc/overview.md), [daemon](doc/spec-daemon.md),
[discovery](doc/spec-discovery.md), [OAuth](doc/spec-oauth.md),
[HTTP + shim](doc/spec-http-shim.md), [guard/secrets/config](doc/spec-guard.md),
[CLI](doc/spec-cli.md), [verification strategy](doc/verification/strategy.md).

## Config

Top-level scalars + `[daemon]` are daemon-wide; everything else lives under
its own `[servers.<name>]`. The key IS the name — no `name =` field, no
detached tables, no global tool policy.

```toml
[daemon]
addr = "127.0.0.1:8379"
token = ""  # empty = loopback trust ONLY; non-loopback addr requires a token

[servers.echo]
description = "my connector"
command = ["python3", "/path/to/echo.py"]
# cwd = "/work/project"   # local-only: child workdir (default = daemon's cwd)
# inherit_env = false     # local-only: clean minimal env (PATH/HOME only)
                          # + explicit env overlays; default true
# env = { FOO = "bar" }   # child env additions (both modes)

[servers.echo.tools]
allow = ["*"]
deny = ["delete_*"]  # deny always wins; deny = invisible + blocked
```

Bad entries never kill the daemon: they show unhealthy in `status` with the
config error, everything else keeps running. `enabled = false` hides a
server entirely (C2). A server with no `[tools]` block allows all.
`daemon.token` supports `${VAR}`/`{file:}` expansion (fail-closed on
unset vars). `oxyp reload` keeps the startup addr/token: changing them
requires a restart (the daemon logs a warning).

Files dropped in `connectors/` (executable) become servers named by file
stem. Explicit TOML entries with the same name win. New `oxyp.toml` files
are created 0600; existing group/world-readable files are tightened to
0600 on `add`/`import` (credentials ride in headers/env).

## Lifecycle

```toml
[daemon]
idle_timeout = 10  # minutes before an idle stdio child is stopped (0 = never)

[servers.db]
command = ["db-mcp"]
lifecycle = "lazy"  # default: cold, stop when idle
# lifecycle = "eager"          # connect at startup, no idle stop unless configured
# lifecycle = "keep-alive"     # connect at startup, never idle-stop
# lifecycle = "lazy-keep-alive" # cold start, never idle-stop after first use
# idle_timeout = 0             # per-server override
```

Idle-stop is local-only (closing a remote handle frees nothing). Stopping
never drops tool metadata — the server stays searchable and restarts on
next call. Only in-flight tool calls guard the sweep; list/resource/prompt
RPCs do not.

Tool metadata persists in `<dir>/cache.json` (default `~/.oxyp/cache.json`;
dir follows --dir > OXYP_DIR > XDG_CONFIG_HOME/oxyp > ~/.oxyp):
list/describe work cold and survive restarts. Entries invalidate on
fingerprint config change, removal/disable, upstream `list_changed`
(stdio + SSE remotes), client reset, or a 5-min TTL revalidation for
Streamable-HTTP remotes (search itself never revalidates/spawns).
Over-limit tool text spills to `oxyp-output-*` temp files in os.TempDir()
(0600); each spill prunes to the newest 100 files / 100 MiB / 24h
(no background reaper — old spills linger until the next spill).

Per-server opt-out for the daemon-wide output guard (table form only —
scalar `output_guard = false` is a TOML type error):

```toml
[servers.noisy.output_guard]
enabled = false
```

Per-server `trace.file` is rejected (daemon-level only); enabling trace on
the daemon or any server opens the daemon trace file (per-server
max_bytes/max_events are ignored; trace changes need a restart). The HTTP
API checks bearer → Host → Origin → Content-Type (in that order, before
routing): empty token = loopback trust ONLY (non-loopback + empty token
fails load); loopback binds reject foreign `Host`/`Origin`; state-changing
requests with a body require `Content-Type: application/json`.
## Remote servers

```toml
[servers.glean]
url = "https://company.glean.com/mcp"
headers = { Authorization = "Bearer ${GLEAN_TOKEN}" }  # ${VAR}/{file:} expanded
# transport = "sse"   # only for SSE-only endpoints; auto-fallback otherwise

[servers.glean.tls]
# ca_file = "/path/ca.crt"   # custom CA
# cert_file/key_file = ...    # mTLS
# insecure = true             # self-signed dev only
```

`${VAR}` and `{file:/abs/path}` expand at load; unset vars fail remote
URL/header entries closed (local `env` keeps them literally; malformed
`${X:-d}`/`${1}` pass literally). Redirects: POST preserved same-origin,
cross-origin keeps only Accept/Content-Type/Mcp-Session-Id/
MCP-Protocol-Version (credentials stripped), 4 redirects followed
(5 requests total). Remote tool changes refresh on a 5-min TTL (stale
served on revalidation failure); Streamable-HTTP remotes get no live push
in this stack. Local stdio + SSE remotes still get live list_changed push.

## Auth (daemon-held)

Tokens live in `~/.oxyp/oauth/` (0600), owned by the daemon. Callers never
see them. Different identity = second daemon (`--dir`).

```toml
[servers.github]
url = "https://api.githubcopilot.com/mcp/"
auth = "oauth"

[servers.github.tools]
allow = ["*"]
deny = ["delete_*"]

[servers.github.oauth]
flow = "browser"  # default
# flow = "device" # headless: type code on any device (RFC 8628)
# flow = "token"  # pre-seeded: verify static Authorization, no dance
# grant_type = "client_credentials" # service account: id+secret, no browser
# client_id/client_secret/scope/callback_port/timeout — all optional (DCR)
# redirect_uri = "http://localhost:{port}/callback" # {port} = bound port
```

```sh
oxyp login github   # browser: open URL, approve, callback on ephemeral port
                    # device: open verification_uri, type user_code
                    # token: verifies the header works, reports ok/expired
oxyp login --complete '<pasted-callback-url>' github  # no loopback? finish here
```

Browser login binds an OS-assigned port by default (`callback_port` pins
:8378-style behavior; `redirect_uri` with `{port}` advertises it). A
`redirect_uri` WITHOUT `{port}` names its own fixed port — the CLI binds it
(a `callback_port` mismatch is an immediate error, not a hang).
Any remote that 401s with `resource_metadata` is treated as OAuth even
without `auth = "oauth"` (explicit `auth` forces it).

DCR registrations (`client_id`/`secret`/auth-method/issuer) persist per
server as `<key>.client.json` next to the token, and pending browser flows
carry the same credentials — so `login --complete` works cross-process and
refresh survives daemon restarts, even against IdPs that validate
`client_id`. Device and `client_credentials` token requests send
`resource=<mcp URL>` (RFC 8707) for resource-enforcing servers.

## Usage

```sh
oxyp servers                # list (autostarts daemon if needed)
oxyp tools <server>         # thin list (denied tools invisible — C1)
oxyp search "query"         # ranked search across servers (never spawns)
oxyp describe <server> <tool>
oxyp call <server> <tool> '{"arg":1}'
oxyp status                 # health per server
oxyp reload                 # re-read config + connectors (or kill -HUP)
oxyp login <server>        # oauth: browser/device/token per servers.oauth.flow
oxyp doctor [--live] [--json]  # offline validation, or probe every server (15s each)
oxyp add <url> [--name] [--write]  # probe + append a [servers.<name>] entry
oxyp import [--from cursor|claude|vscode|opencode|omp|all] [--write]  # preview TOML, write on confirm
```

Raw daemon HTTP (no dedicated CLI verbs for resources/prompts — `curl` the daemon):

```sh
curl $OXYP/servers/<name>/resources          # list + init instructions
curl "$OXYP/servers/<name>/resources/<uri>"  # read one (?uri= also works)
curl $OXYP/servers/<name>/prompts            # prompt templates (may be [])
```

Resources/prompts are live passthrough (they spawn cold servers; no disk
cache — bodies are live data). Servers without support return `[]`.

MCP clients attach via the shim with one line:

```json
{ "command": ["oxyp", "shim"] }
```

The shim speaks `initialize` / `tools/list` / `tools/call` with the 5
meta-tools (`search_tools` ranks by words over cached metadata, never
spawns) and forwards to the daemon over HTTP. `invoke_command` passes
downstream content blocks through natively — image/audio/resource blocks
reach the client untouched (never textified), so vision-capable clients
get real media.

## Build / test

```sh
go build ./... && go test ./...
```
