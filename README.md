# oxyp

<p align="center">
  <img src="assets/oxyp-logo.jpg" alt="oxyp — one daemon, every connector" width="720" />
</p>

<p align="center">
  <img src="https://img.shields.io/badge/version-0.3.0-blueviolet?style=flat-square" alt="Version 0.3.0" />
  <img src="https://img.shields.io/badge/license-MIT-green?style=flat-square" alt="MIT License" />
  <img src="https://img.shields.io/github/go-mod/go-version/lirrensi/oxyp?style=flat-square" alt="Go version" />
  <img src="https://img.shields.io/badge/mcp-stdio%2Bhttp-success?style=flat-square" alt="MCP stdio+http" />
  <img src="https://img.shields.io/badge/binary-single_%E2%9C%93-orange?style=flat-square" alt="Single binary" />
</p>

<p align="center">
  <strong>One daemon, every connector</strong> — a lazy MCP gateway in Go<br />
  20–30 servers behind <em>5 tools</em> — your agent's context never notices.
</p>

<p align="center">
  <em>Not schema spam on every turn. Cold connections, on-demand schemas,<br />
  one TOML entry per server, tokens the client never sees.</em>
</p>

<p align="center">
  <b>Two servers, twenty, or a hundred (I dare you)</b>
</p>

<p align="center">
  Alternative to wiring N servers into every client by hand.
</p>

---

## What is oxyp?

oxyp or oxy-p is a **client-agnostic gateway** between your agents and your MCP servers. Instead of wiring N servers into every client — and paying full schema spam on every turn — each client gets **one stdio shim** with 5 static meta-tools. A localhost **daemon** owns every downstream connection and wakes it only when called.

```text
┌──────────────────────────────────────────────────┐
│  your client (Claude / Cursor / opencode / omp)  │
│    one shim: 5 tools, forever                    │
└───────────────────────┬──────────────────────────┘
                        │ stdio MCP
┌───────────────────────▼──────────────────────────┐
│  oxyp shim — instant answers, connects to nothing│
└───────────────────────┬──────────────────────────┘
                        │ HTTP + bearer (autostarts daemon)
┌───────────────────────▼──────────────────────────┐
│  oxyp daemon — owns every connection             │
│   ├── local children (cold → spawn → idle-stop)  │
│   ├── remotes (Streamable HTTP / legacy SSE)     │
│   ├── cache.json — schemas on disk, searchable   │
│   └── oauth/ — 0600 tokens, clients never see    │
└──────────────────────────────────────────────────┘
```

Your agent sees exactly this, forever:

| Tool | Does |
| ---- | ---- |
| `list_servers` | what's connected, what's healthy |
| `list_tools` | thin tool list for one server |
| `search_tools` | ranked search across **all** servers by words |
| `describe_tool` | full schemas for named tools, on demand |
| `call` | run it |

Context stays flat whether you have 2 servers or 30: schemas load only for the tool about to be called. Add one server, remove one, hide a tool — the agent's config never changes; it still sees the same 5 tools.

---

## Features — more than a multiplexer

Regular MCP multiplexers fan out stdio and call it a day. oxyp is a **lazy gateway + lifecycle manager + search engine + OAuth vault + federated mesh + per-client views** — all in one static binary.

| Regular multiplexer | oxyp |
| --- | --- |
| Spawns every server on every turn | **Cold by default** — spawn on first use, idle-stop after 10 min, `cache.json` keeps discovery instant with zero children running. `lifecycle = lazy / eager / keep-alive / lazy-keep-alive` per server. |
| Schema spam: N servers × M tools in every prompt | **5 meta-tools forever** — `list_servers / list_tools / search_tools / describe_tool / call`. Schemas load on demand. 2 servers or 100, context stays flat. |
| No search, just `tools/list` | **Search everything, spawn nothing** — ranked `search_tools` across all servers (words, regex, pagination) from disk. Cold stays cold. |
| Client holds tokens / OAuth dance per client | **OAuth the client never sees** — browser / device / token / `client_credentials`, DCR + auto-detect, tokens in `0600` files. Headless via `${VAR}` / `{file:/path}`. |
| All tools visible or nothing | **Deny means invisible** — per-server `allow`/`deny` globs (deny wins), `[params]` allowlists (extra args 403), `enabled = false` parks, `pinned` + `new_tools = hide` freeze upstream drift until `oxyp refresh`. |
| One view for every client | **Per-client views** — `[groups.*]` subsets + ad-hoc `--servers` whitelist / `--exclude` blacklist (exact IDs or `*`/`?` globs, deny wins) via flags, `OXYP_GROUP` / `OXYP_SERVERS` / `OXYP_EXCLUDE`, `?group=&servers=&exclude=`, and `POST /mcp` tool args. One folder sees 5 servers, another sees all 10. |
| One machine, one config | **Federation** — mount other daemons as your own (`oxyp peers add`). Merge vs namespace, priority winners, `wsl:db` qualified + `local:db` escape hatch, transitives (`b:c:gamma`), owner re-runs gates, tokens never travel. |
| Localhost only | **Remote + custom connect** — `OXYP_URL` / `OXYP_TOKEN` for LAN, `POST /mcp` for URL-only clients, Win↔WSL / container / `ssh -L` adoption (no second daemon), per-server `TLS`, `token_header` remap, `passthrough_headers`, Streamable HTTP → legacy SSE fallback. |
| Text gets truncated, images break | **Output guard + media passthrough** — text capped with spill-to-`0600`-file + `file://` pointer; image/audio/resource blocks ride verbatim for vision clients. |
| Hope it works | **Operability** — `doctor [--live]` (config + group audit + probe + pin/hide drift), `reload` / `kill -HUP` without dropping healthy children, `refresh`, `add` (probe + append), `import` (Cursor/Claude/VS Code/opencode/omp), `connectors/` auto-discovery, metadata-only `trace.jsonl`, secret redaction in logs. |

Everything below runs behind those 5 tools — agents drive it through the shim, humans use the CLI against the same daemon.

### 💤 Laziness as a feature

Connections start cold, spawn on first use, stop themselves when idle. Tool metadata lives on disk so discovery works with zero children running. Your laptop fans never notice 30 servers. Tune per server: `lifecycle`, `idle_timeout`, `tool_timeout`.

### 🔍 Search everything, spawn nothing

`search_tools` ranks across **all** servers by words (name/server/description, regex + pagination). It reads `cache.json` on disk — cold servers stay cold, asleep children stay asleep, answers stay instant.

### 🔐 OAuth the client never sees

Browser / device / token / service flows, DCR + auto-detect, tokens in `0600` files held by the daemon. The client gets results, never credentials. Headless? Token flow with `${VAR}` / `{file:/path}` secrets.

### 🙈 Deny means invisible

Per-server `allow`/`deny` globs — denied tools vanish from list/describe/search AND block on call, always wins over allow. `[params]` restricts args per tool. Whole servers park with `enabled = false`. `pinned` + `new_tools = hide` freeze upstream until you `refresh`. No approval popups: the daemon has no UI, permissions are config.

### 👀 Per-client views — groups + whitelist/blacklist

10 servers connected, but this folder's client should only see 5? Or all except 2?

```bash
# allowlist: only these (exact IDs or globs, comma-separated)
oxyp servers --servers github,linear,wsl:*
OXYP_SERVERS="github,linear" oxyp search "pull requests"

# blocklist: all except these (deny wins over allow)
oxyp servers --exclude gamma,*-prod
OXYP_EXCLUDE="gamma" oxyp call gamma run_query '{}'  # 403 server not in scope

# curated group + ad-hoc filter combine (AND)
oxyp servers --group coding --exclude linear
```

Same knobs everywhere: `--group/--servers/--exclude` flags + `OXYP_GROUP` / `OXYP_SERVERS` / `OXYP_EXCLUDE` env for CLI + `oxyp shim` (one view per client, agent can't escape it), `?group=&servers=&exclude=` for REST, and `group/servers/exclude` tool args + `POST /mcp?servers=&exclude=` for URL-only clients. Unknown group → 404, outside scope → 403 (`server not in scope`, legacy `server not in group` when only groups exclude).

### 🌐 Federation: mount other daemons as servers

Auth lives where it was earned — your Win box has the GitHub token, WSL has Linear, and neither can move. Peering mounts the other daemon's servers as your own; calls proxy to the owner (which re-runs its gates), tokens never travel.

```bash
oxyp peers add http://192.168.1.5:8379 --write   # handshake, preview, write [peers.<name>]
oxyp servers                                      # their servers appear merged with yours
oxyp call gamma run_query '{}'                    # runs on the owner, output streams back
```

Naming: merge mode (default) floats shared names to the priority winner (local rank 0 beats peers unless outbid); every claimant stays reachable qualified (`wsl:db`), peer-beaten locals via `local:db`. Namespace mode (`mode = "namespace"`) claims no bare names — five GitLabs, zero collision. Transitives compose by prefixing (`b:c:gamma`) when both sides opt in (owner `[servers.x.peer_export] transitive = true` + middle `[daemon.peer_sharing] mode = "show"`).

Trust: peers hold your operator bearer token — sharing controls what auto-mounts, not a vault door. If you wouldn't hand a daemon your token, don't peer with it. Full contract: [doc/spec-federation.md](doc/spec-federation.md).

### 🔌 Custom connect — remote, URL-only, and weird networks

MCP shim (any client, one `{command: oxyp, args: [shim]}` line), CLI (`servers / tools / search / describe / call / status …`), the daemon's own localhost REST, and `POST /mcp` Streamable HTTP for URL-only clients. Same servers, same gates, same cache — pick your surface.

Remotes get hardened transport: per-server `TLS` (`ca_file/cert_file/key_file`), `token_header` remap (e.g. `X-API-Key`), `passthrough_headers` allowlist per call, Streamable HTTP with legacy SSE fallback, 15 s probe hints on dead URLs.

### ✂️ Output guard with spill files

Downstream text gets size-capped with spill-to-disk + `file://` pointer; image/audio/resource blocks pass through untouched so vision clients get real media. Secrets ride `${VAR}` / `{file:}` — expanded at load, redacted in logs, never stored.

### 🩺 Doctor, reload, import

`doctor [--live]` validates config or probes every server for real. `reload` (or `kill -HUP`) re-reads config without dropping healthy children. `import` converts Cursor/Claude/VS Code/opencode/omp configs to oxyp TOML. `connectors/` auto-discovers one-file servers by filename.

---
## Install — one copy-paste

One line, zero thinking — downloads the release binary, puts it on your PATH,
and verifies it runs:

```bash
curl -fsSL https://raw.githubusercontent.com/lirrensi/oxyp/main/install.sh | sh
```

Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/lirrensi/oxyp/main/install.ps1 | iex
```

Open a new shell and `oxyp` is ready to go. sh options (passed through the pipe):
`sh -s -- --go` builds from source via `go install` instead of downloading;
`--version v0.3.0` pins a release; `--dir DIR` overrides the install dir.
No prebuilt binary for your platform? Use `--go` (needs Go 1.21+).
ps1 options (`-Go`, `-Version`, `-Dir`) need the script on disk — `iex` can't
take args, so download it first (see the header comment).

Prefer to do it by hand? `go install` drops the binary in `~/go/bin` — point
`GOBIN` at a dir already on your PATH:

```bash
GOBIN="$HOME/.local/bin" go install github.com/lirrensi/oxyp/cmd/oxyp@latest
```

One static binary on your PATH either way. No runtime, no keychain, no machine-bound state. `oxyp` + `oxyp.toml` runs your agent anywhere.

```bash
oxyp servers                                        # autostarts the daemon; empty for now
oxyp add https://api.githubcopilot.com/mcp/ --name github --write
oxyp login github                                   # OAuth in the browser (or token flow)
oxyp search "list pull requests"                    # ranked, across all servers — nothing spawns
oxyp describe github list_pull_requests             # full schema, on demand
oxyp call github get_me '{}'                        # run a tool straight from the CLI
```

Already have MCPs in Cursor, Claude, VS Code, opencode, or omp? `oxyp import --write` converts them to oxyp TOML.

## Point your client at it

The shim speaks MCP over stdio — one entry per client (use the absolute path from `which oxyp` if your client spawns without a login shell):

Claude Desktop / Cursor / VS Code / omp — `mcpServers.<name>`:

```json
{ "command": "oxyp", "args": ["shim"] }
```

opencode — `mcp.<name>` in `opencode.jsonc`:

```json
{ "type": "local", "command": ["oxyp", "shim"] }
```

hermes — `mcp_servers.<name>` in `config.yaml`:

```yaml
command: oxyp
args: [shim]
```

## Remote: your servers, from anywhere on the LAN

One daemon at home, every laptop/agent pointing at it. On the box that
hosts the daemon:

```toml
[daemon]
addr = "0.0.0.0:8379"
token = "{file:~/.oxyp/bearer}"   # required for non-loopback — plain HTTP, home nets only
```

On any other machine (needs only the `oxyp` binary, no config):

```bash
export OXYP_URL=http://192.168.1.10:8379 OXYP_TOKEN=$(cat ~/.oxyp/bearer)
oxyp servers        # talks to home, never spawns locally
oxyp search "pull requests"
oxyp call github get_me '{}'
```

Same for the shim — put `OXYP_URL`/`OXYP_TOKEN` in the client's `env`
and the 5 meta-tools proxy to home:

```json
{ "command": "oxyp", "args": ["shim"],
  "env": { "OXYP_URL": "http://192.168.1.10:8379", "OXYP_TOKEN": "…" } }
```

Rules: only `http(s)://host[:port]` parses (bad value = hard error, no
silent local fallback); unreachable remotes fail fast, never autostart;
`daemon`/`stop`/`restart`/`login`/`doctor`/`add`/`import` are local-only
and refuse with `OXYP_URL` set — run them where the daemon lives. Token
goes over plain HTTP, so this is for trusted LANs or `ssh -L` tunnels,
never the open internet.

No `OXYP_URL` needed when the port is already shared: daemon on Win +
client in WSL (mirrored networking), a container, or an `ssh -L`
forward — if the PID on file is dead here but a live oxyp answers the
same URL/addr under your token, the CLI/shim just attaches instead of
spawning a second copy. `stop` refuses adopted daemons (`already
running elsewhere`) — stop it where it lives.

Per-folder clients: give each client its own view without touching `oxyp.toml`:

```json
{ "command": "oxyp", "args": ["shim", "--servers", "github,linear"],
  "env": { "OXYP_URL": "http://192.168.1.10:8379", "OXYP_TOKEN": "…" } }
```

```json
{ "command": "oxyp", "args": ["shim", "--exclude", "gamma,*-prod"] }
```

URL-only clients use `POST /mcp?servers=github&exclude=gamma` or `servers` / `exclude` tool args. See Features above for the full scoping contract.

## CLI cheat sheet

| Command | What it does |
| ------- | ------------ |
| `oxyp servers [--group --servers --exclude]` | list servers (autostarts the daemon) |
| `oxyp tools <server> [--group --servers --exclude]` | thin tool list (denied tools invisible) |
| `oxyp search "query" [--server --group --servers --exclude]` | ranked search, cached metadata, never spawns |
| `oxyp describe <server> <tool> [--group --servers --exclude]` | full schema for one tool |
| `oxyp call <server> <tool> '<json>' [--group --servers --exclude]` | invoke a tool |
| `oxyp status` | daemon + per-server health (global view, ignores scope) |
| `oxyp doctor [--live]` | validate config (+ group audit), or probe every server (+ pin/hide drift) |
| `oxyp add <url> [--name --description] [--write]` | probe a URL + append a server entry (description shows in servers/search) |
| `oxyp import [--from …] [--write]` | convert cursor/claude/vscode/opencode/omp configs |
| `oxyp login <server>` | OAuth login (browser/device/token per config) |
| `oxyp peers add <url> [--as/--token/--mode --description] [--write]` | handshake + append a peer entry (description shows in peers list) |
| `oxyp peers list` / `remove <name>` / `sync` | view, cut, or resync peer mounts |
| `oxyp reload` | re-read config (or `kill -HUP`) |
| `oxyp refresh <server>` | force live re-fetch, replace the pin/hide snapshot (shows added/removed) |
| `oxyp stop` | stop the daemon (idempotent) |
| `oxyp restart` | stop + start (picks up addr/token changes) |

`--group` (or `OXYP_GROUP`) scopes to a `[groups.*]` subset; `--servers` (or `OXYP_SERVERS`) whitelists only these server IDs/globs; `--exclude` (or `OXYP_EXCLUDE`) hides these (deny wins). All three AND together; `oxyp shim --group/--servers/--exclude` fixes one view per client. URL-only clients use `POST /mcp` (same 5 tools, same `group/servers/exclude` args + `?servers=&exclude=`).

## Configure

`~/.oxyp/oxyp.toml` is self-documenting — first run writes a ~30-line guide header, so reading the file teaches the syntax and diffs show what you changed. (override order: `--dir` > `OXYP_DIR` > `XDG_CONFIG_HOME/oxyp` > `~/.oxyp`):

```toml
# oxyp.toml — one daemon, every connector. Edit, save, then: oxyp reload
# QUICK ADD (so nobody forgets a description):
#   oxyp add https://.../mcp --name github-corp1 --description "corp1 github, SSO" --write

[daemon]
addr = "127.0.0.1:8379"   # empty token = loopback trust ONLY

[servers.github-personal]
description = "personal github - oss + side quests"  # shown in servers/search, filter matches it
url = "https://api.githubcopilot.com/mcp/"
auth = "oauth"
new_tools = "hide"       # upstream additions stay hidden until reviewed
pinned = true            # freeze discovery to the cache snapshot
# `oxyp refresh github-personal` re-trusts upstream when you're ready (freezing never self-heals)
tool_timeout = 60        # per-RPC seconds (default 10)
token_header = "X-API-Key"  # static Authorization goes out under this name

[servers.github-personal.tools]
allow = ["*"]
deny = ["delete_*"]       # deny = invisible + blocked, always wins

[servers.github-personal.params]
create_issue = ["title", "body", "labels"]  # extra args 403

[servers.github-corp1]
description = "corp1 github - day job, SSO"  # two GitHubs? name + description tells them apart
url = "https://ghe.corp1.example/mcp/"

[servers.notes]
description = "local notes - fast, private"
command = ["python3", "/srv/mcp/notes.py"]   # local stdio child INSTEAD of url

[groups.coding]
description = "code review tools"  # groups have descriptions too - what is this view for?
servers = ["github-personal"]      # missing members skipped; doctor reports them

[peers.wsl]
description = "wsl box - holds linear token"  # peers have descriptions too - whose tokens?
url = "http://192.168.1.5:8379"
# mode = "merge"  # or "namespace"
```

Secrets ride `${VAR}` / `{file:/path}` — expanded at load, redacted in logs, never stored by oxyp. Drop executable one-file servers into `~/.oxyp/connectors/` and they're auto-discovered by filename. Bad entries degrade to "unhealthy" in `status`; they never kill the daemon. Duplicates? Use distinct names (`github-personal`, `github-corp1`, `memory-local` vs `memory-remote`) + descriptions — `servers --filter corp` and `--server github-corp1` do the rest.

## 🤖 For AI agents

<details>
<summary><b>Agent quickstart — expand, copy, paste to your agent</b></summary>

```text
Install oxyp (lazy MCP gateway) and connect it:

1. Run: curl -fsSL https://raw.githubusercontent.com/lirrensi/oxyp/main/install.sh | sh
   (zero-config installer: binary + PATH + verify; add -s -- --go to build from source)
2. Run: oxyp servers            (autostarts the daemon)
3. Migrate existing MCPs: oxyp import --write
   (covers cursor/claude/vscode/opencode/omp)
   …or add one: oxyp add <url> --name <name> --write
4. If a server needs auth: oxyp login <name>
   (browser flow; use the token flow with env-var secrets when headless)
5. Point this client at the shim: {"command": "oxyp", "args": ["shim"]}
   (absolute path from `which oxyp` if the client has no login shell)
6. Verify: oxyp search "<a word>" and oxyp status

Rules for using oxyp afterwards: you only ever see 5 tools.
To use any downstream tool: search_tools → describe_tool → call.
Never guess schemas — describe first.
```

</details>

## Honest limits

- **No per-call approval popups.** The daemon has no UI to ask through — permissions are config (`deny`/hide, `params`, groups). Ask-gates belong on the client side of the pipe.
- **Two doors, same 5 tools.** MCP clients attach via the shim (`command: [oxyp, shim]`, local or `OXYP_URL` remote) or raw Streamable HTTP at `POST /mcp` (URL-only clients, same meta-tools, same gates; bearer + `Origin` checks apply).
- **One user per daemon dir** — a different identity is a second `--dir`.
- **Proven, not promised:** 46 real GitHub tools through the daemon — search, schema, invoke — and this very repo was created with `oxyp call github create_repository`. Dogfooded on day one.

## Docs

- [overview](doc/overview.md) — architecture + system map
- [daemon](doc/spec-daemon.md) — lifecycle, health, reload, idle sweep
- [discovery](doc/spec-discovery.md) — cache, search, permissions
- [OAuth](doc/spec-oauth.md) — 4 flows, DCR, auto-detect
- [HTTP + shim](doc/spec-http-shim.md) — API, shim, media passthrough
- [guard/secrets/config](doc/spec-guard.md) — output guard, trace, secrets
- [CLI](doc/spec-cli.md) — every command, every flag
- [federation](doc/spec-federation.md) — peer daemons, virtual servers, sharing
- [clients](doc/CLIENTS.md) — opencode / omp / hermes attach notes
- [stances](doc/PHILOSOPHY.md) — what oxyp refuses to do, and why

## Acknowledgments

Clean rewrite, no fork — but the good ideas are borrowed with love:

- [mozilla-ai/mcpd](https://github.com/mozilla-ai/mcpd) — daemon lifecycle, stderr piping, health pings, TOML config, SIGHUP reload
- [gitlab-org/ai/lazy-mcp](https://gitlab.com/gitlab-org/ai/lazy-mcp) — the lazy meta-tools shape (list → describe → invoke), lazy connect, glob permissions

## Contributing

Issues and PRs welcome. Keep it boring, keep it portable: single binary, no keychain, no machine-bound state.

## Why its Oxy-p, why oxy if its not in rust?!

It started in rust! 10 min in I evaluated my entire life and found out I am not ready to become a femboy yet so I reconsidered.

## License

MIT — see [LICENSE](LICENSE).

---

*Twenty servers. Five tools. Zero context crap. 🐾*
