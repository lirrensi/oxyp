# oxyp

[![Go](https://img.shields.io/github/go-mod/go-version/lirrensi/oxyp)](https://go.dev/)
[![License](https://img.shields.io/github/license/lirrensi/oxyp)](LICENSE)

**One daemon, every connector.** A lazy MCP gateway in Go: 20–30 MCP servers behind 5 tools — your agent's context never notices.

## The problem

Every MCP server wired into an agent dumps its full tool list — names, descriptions, JSON schemas — into context on *every turn*. Two servers feel fine. Ten servers (≈300 tools) bury the model in schema spam: slower answers, dumber answers, bigger bills. And every add/remove/rekey means touching each client's config by hand.

MCP servers *should* be easier to move around than a thousand tools. With oxyp, they are.

## The fix: 5 tools, N servers

Point your client at oxyp instead of at N servers. Your agent sees exactly this, forever:

| Tool | Does |
| ---- | ---- |
| `list_servers` | what's connected, what's healthy |
| `list_commands` | thin tool list for one server |
| `search_tools` | ranked search across **all** servers by words |
| `describe_commands` | full schema for one tool, on demand |
| `invoke_command` | run it |

Context stays flat whether you have 2 servers or 30: schemas load only for the tool about to be called. Connections stay cold until first use, idle children stop themselves, and tool metadata lives on disk — search works even with everything asleep.

Servers become one TOML entry each. Add one, remove one, hide a tool — the agent's config never changes; it still sees the same 5 tools.

The grown-up bits are included: daemon-held OAuth (browser / device / token / service flows, tokens in `0600` files the client never sees), deny-means-invisible permissions, an output guard with spill files, live doctoring, and one-file `connectors/` auto-discovery.

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

## How it works

```text
your client (Claude / Cursor / opencode / omp / hermes)
  │  stdio MCP: initialize / tools/list / tools/call
  ▼
oxyp shim — 5 static meta-tools, instant answers, connects to nothing
  │  HTTP + bearer to the localhost daemon (autostarts it)
  ▼
oxyp daemon — owns every downstream connection
  ├── local children over stdio (spawned cold, stopped when idle)
  ├── remotes over Streamable HTTP / legacy SSE
  ├── cache.json — tool metadata on disk, searchable while asleep
  └── oauth/ — daemon-held tokens (0600), clients never see them
```

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
To use any downstream tool: search_tools → describe_commands → invoke_command.
Never guess schemas — describe first.
```

</details>

## Honest limits

- **No per-call approval popups.** The daemon has no UI to ask through — permissions are config (`deny`/hide). Ask-gates belong on the client side of the pipe.
- **The daemon's HTTP API is oxyp's own REST, not MCP.** Remote access is shim-over-ssh until a Streamable-HTTP server mode exists.
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
