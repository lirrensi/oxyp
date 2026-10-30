# oxyp

<p align="center">
  <img src="https://img.shields.io/badge/version-0.2.0-blueviolet?style=flat-square" alt="Version 0.2.0" />
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

oxyp is a **client-agnostic gateway** between your agents and your MCP servers. Instead of wiring N servers into every client — and paying full schema spam on every turn — each client gets **one stdio shim** with 5 static meta-tools. A localhost **daemon** owns every downstream connection and wakes it only when called.

```
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

## What it can do

Everything below runs behind those 5 tools — agents drive it through the shim, humans use the CLI against the same daemon.

### 🔍 Search everything, spawn nothing

`search_tools` ranks across **all** servers by words (name/server/description, regex + pagination). It reads `cache.json` on disk — cold servers stay cold, asleep children stay asleep, answers stay instant.

### 💤 Laziness as a feature

Connections start cold, spawn on first use, stop themselves when idle. Tool metadata lives on disk so discovery works with zero children running. Your laptop fans never notice 30 servers.

### 🔐 OAuth the client never sees

Browser / device / token / service flows, DCR + auto-detect, tokens in `0600` files held by the daemon. The client gets results, never credentials. Headless? Token flow with `${VAR}` / `{file:/path}` secrets.

### 🙈 Deny means invisible

Per-server `allow`/`deny` globs — denied tools vanish from list/describe/search AND block on call, always wins over allow. Whole servers park with `enabled = false`. No approval popups: the daemon has no UI, permissions are config.

### ✂️ Output guard with spill files

Downstream text gets size-capped with spill-to-disk + `file://` pointer; image/audio/resource blocks pass through untouched so vision clients get real media. Secrets ride `${VAR}` / `{file:}` — expanded at load, redacted in logs, never stored.

### 🩺 Doctor, reload, import

`doctor [--live]` validates config or probes every server for real. `reload` (or `kill -HUP`) re-reads config without dropping healthy children. `import` converts Cursor/Claude/VS Code/opencode/omp configs to oxyp TOML. `connectors/` auto-discovers one-file servers by filename.

### 🛠️ Three ways to drive it

MCP shim (any client, one `{command: oxyp, args: [shim]}` line), CLI (`servers / tools / search / describe / call / status …`), and the daemon's own localhost REST. Same servers, same gates, same cache — pick your surface.

---
## Install — one copy-paste

Prerequisite: Go 1.21+ (the toolchain auto-upgrades to whatever oxyp needs).

```bash
go install github.com/lirrensi/oxyp/cmd/oxyp@latest
```

One static binary on your PATH. No runtime, no keychain, no machine-bound state. `oxyp` + `oxyp.toml` runs your agent anywhere.

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

## CLI cheat sheet

| Command | What it does |
| ------- | ------------ |
| `oxyp servers` | list servers (autostarts the daemon) |
| `oxyp tools <server>` | thin tool list (denied tools invisible) |
| `oxyp search "query"` | ranked search, cached metadata, never spawns |
| `oxyp describe <server> <tool>` | full schema for one tool |
| `oxyp call <server> <tool> '<json>'` | invoke a tool |
| `oxyp status` | daemon + per-server health |
| `oxyp doctor [--live]` | validate config, or probe every server |
| `oxyp add <url> [--name] [--write]` | probe a URL + append a server entry |
| `oxyp import [--from …] [--write]` | convert cursor/claude/vscode/opencode/omp configs |
| `oxyp login <server>` | OAuth login (browser/device/token per config) |
| `oxyp reload` | re-read config (or `kill -HUP`) |
| `oxyp stop` | stop the daemon (idempotent) |
| `oxyp restart` | stop + start (picks up addr/token changes) |

## Configure

`~/.oxyp/oxyp.toml` (override order: `--dir` > `OXYP_DIR` > `XDG_CONFIG_HOME/oxyp` > `~/.oxyp`):

```toml
[daemon]
addr = "127.0.0.1:8379"   # empty token = loopback trust ONLY

[servers.github]
url = "https://api.githubcopilot.com/mcp/"
auth = "oauth"

[servers.github.tools]
allow = ["*"]
deny = ["delete_*"]       # deny = invisible + blocked, always wins

[servers.notes]
command = ["python3", "/srv/mcp/notes.py"]   # local stdio child
```

Secrets ride `${VAR}` / `{file:/path}` — expanded at load, redacted in logs, never stored by oxyp. Drop executable one-file servers into `~/.oxyp/connectors/` and they're auto-discovered by filename. Bad entries degrade to "unhealthy" in `status`; they never kill the daemon.

## 🤖 For AI agents

<details>
<summary><b>Agent quickstart — expand, copy, paste to your agent</b></summary>

```text
Install oxyp (lazy MCP gateway) and connect it:

1. Run: go install github.com/lirrensi/oxyp/cmd/oxyp@latest
   (needs Go 1.21+; make sure ~/go/bin is on PATH)
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

- **No per-call approval popups.** The daemon has no UI to ask through — permissions are config (`deny`/hide). Ask-gates belong on the client side of the pipe.
- **The daemon's HTTP API is oxyp's own REST, not MCP.** MCP clients attach via the shim (local or `OXYP_URL` remote); pointing a raw MCP client at the daemon URL won't speak MCP.
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
- [clients](doc/CLIENTS.md) — opencode / omp / hermes attach notes
- [stances](doc/PHILOSOPHY.md) — what oxyp refuses to do, and why

## Acknowledgments

Clean rewrite, no fork — but the good ideas are borrowed with love:

- [mozilla-ai/mcpd](https://github.com/mozilla-ai/mcpd) — daemon lifecycle, stderr piping, health pings, TOML config, SIGHUP reload
- [gitlab-org/ai/lazy-mcp](https://gitlab.com/gitlab-org/ai/lazy-mcp) — the lazy meta-tools shape (list → describe → invoke), lazy connect, glob permissions

## Contributing

Issues and PRs welcome. Keep it boring, keep it portable: single binary, no keychain, no machine-bound state.

## License

MIT — see [LICENSE](LICENSE).

---

*Twenty servers. Five tools. Zero context crap. 🐾*
