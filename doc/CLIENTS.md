# Client MCP patterns — opencode / omp / hermes

Surveyed 2026-10-28. Full reports: `agent://ComfortableHamster` (opencode+omp),
`agent://AlternativeOctopus` (hermes). This is the distilled compat surface
oxyp must survive as a *server* (via shim) and as a *client* (to downstream).

## Config shapes (for oxyp docs: how to point each client at us)

| Client | File | Local entry | Remote entry |
|---|---|---|---|
| opencode 1.18.1 | `~/.config/opencode/opencode.jsonc`, key `mcp.<name>` | `type:"local"`, `command: string[]`, `environment: {}`, `cwd`, `enabled`, `timeout` (ms, >0) | `type:"remote"`, `url`, `headers`, `oauth` obj\|false |
| omp 18.13.1 | `~/.omp/agent/mcp.json`, key `mcpServers.<name>` | `type:"stdio"` (default), `command: string`, `args: []`, `env: {}`, `cwd` | `type:"http"`\|`"sse"`, `url`, `headers`, `oauth`, `auth` |
| hermes 0.22.5 | `~/.hermes/config.yaml`, key `mcp_servers.<name>` | `command`, `args: []`, `env: {}`, `cwd` | `url`, `headers`, `auth: oauth`, `transport: sse` (else Streamable HTTP) |

oxyp gives every client the same two doors:
- stdio: `command: ["oxyp", "shim"]` (hermes/omp/opencode-local shape) — no PATH
  dependency beyond `oxyp` on PATH; absolute path safest (all three spawn
  without a login shell).
- remote: `url: http://127.0.0.1:8379` — **NOT MCP**. oxyp's HTTP is its own
  REST (meta-tools), not Streamable HTTP. Remote MCP clients canNOT point at
  it directly. Document: remote MCP stays stdio-shim-over-ssh or future
  Streamable HTTP mode.

## Transports in the wild (what downstream servers speak)

| Transport | Who speaks it | oxyp today |
|---|---|---|
| stdio JSON-RPC/JSONL | all local servers | ✅ supported (cold spawn, 30 s init / 10 s RPC) |
| Streamable HTTP (`POST`, `Mcp-Session-Id`, session re-init) | hermes/omp/opencode remote, ChatGPT | ✅ supported (auto; 5-min metadata TTL, no live push) |
| Legacy SSE (`GET /sse` + `POST /messages/`) | old servers, omp `type:sse`, hermes `transport:sse` | ✅ supported (`transport = "sse"` or auto-fallback on legacy error) |
| OAuth 2.0/2.1 + PKCE, DCR | linear, GitHub, glean… | ✅ supported daemon-held (browser/device/token/client_credentials) — see [spec-oauth](spec-oauth.md) |
| mTLS, custom CA, identity headers | enterprise (hermes supports) | ✅ supported (`[servers.x.tls]`, headers forwarded; session id crosses redirects) |

## Gotchas to survive (server side — our shim + daemon)

1. **Stdout must be pure JSONL.** All three parse every line as JSON; a banner,
   blank line, or log on stdout kills the connection (opencode #24414, OMP
   `MCPTransportError`, hermes strict). oxyp shim: stdout = JSON-RPC only,
   all logs to stderr. Daemon children: same rule enforced by US on THEM —
   a connector printing junk breaks OUR parse; surface as unhealthy with the
   offending line.
2. **Stderr must be drained, never blocks.** opencode pipes-but-never-reads
   (can block child); OMP drains+discards; hermes tees to `mcp-stderr.log`.
   oxyp: drain to daemon log (done in `pipeStderr`). Zoo must include a
   chatty-stderr server.
3. **Env: full vs filtered.** opencode/OMP pass FULL parent env + config merge;
   hermes passes a FILTERED baseline (PATH/HOME/USER/LANG/LC_ALL/TERM/SHELL/
   TMPDIR + XDG_* + explicit `env`). oxyp daemon: full `os.Environ()` + entry
   `env` (opencode-style). Consequence: secrets in daemon env leak to ALL
   children — document, and consider hermes-style filtering later.
4. **No shell.** All spawn argv directly. `command: ["sh","-c",...]` if you need
   pipes. oxyp connectors: executable files, argv from TOML — same rule.
5. **Timeouts disagree everywhere.** opencode: 30 s impl vs 5 s docs, `0`
   illegal. OMP: 30 s, global env override, `0` disables. hermes: 300 s tool /
   60 s connect. lazy-mcp: 10 s downstream. oxyp: 30 s init / 10 s request /
   3 s ping. Zoo must include slow-init (>30 s), slow-tool (>10 s), and
   never-responding servers to prove our timeouts fire and report cleanly.
6. **Tool-name mangling collides.** opencode `<server>_<tool>` (lossy
   sanitize, last-wins); OMP `mcp__<server>_<tool>` (lowercase, 64-char cap,
   deterministic winner); hermes `mcp__<server>__<tool>` (double underscore,
   collisions skipped). oxyp meta-tools are fixed names — no collision. But
   downstream tool names with `-`/`.`/unicode must pass through untouched;
   zoo includes weird names.
7. **Eager connects everywhere (except hermes lazy/cache).** opencode/OMP
   connect all at startup (OMP 250 ms gate + SQLite cache + deferred tools).
   oxyp is cold — BETTER. But our shim must answer `tools/list` instantly
   (static 5 tools ✅) and `initialize` without connecting downstream
   (autostarts the daemon — fine — but must not connect downstream ✅).
8. **OAuth callback is loopback-bound.** opencode `:19876`, OMP `:3000`,
   hermes dynamic, lazy-mcp `:8947`. Daemon-held tokens (our model) mean
   re-auth happens at the daemon's box — document the SSH-forward trick.
9. **Header auth shapes:** `Authorization: Bearer` static (all), OAuth auto on
   401 (opencode/OMP/hermes), mTLS + custom CA + identity header (hermes
   only). Zoo remote servers must cover: no-auth, bearer-ok, bearer-wrong
   (401), header-echo (prove we forward), redirect (credential strip?),
   self-signed TLS (fail clean), mTLS-required (fail clean for now).
10. **Protocol versions float.** OMP sends `2025-11-25`; hermes negotiates
    legacy→`2026-07-28 server/discover`; mcp-go uses `LATEST`. Zoo: old-version
    server, version-mismatch server, `server/discover`-only server.
11. **Pagination + `tools/list_changed`.** opencode paginates (1000 pages max);
    hermes/OMP honor `list_changed`. oxyp daemon: must paginate downstream
    `tools/list` (mcp-go does?) and invalidate cache on `list_changed`.
    Verify in zoo.
12. **Sampling/elicitation/roots.** hermes: sampling ON by default, elicitation
    form-mode; servers may REQUEST sampling/elicitation/roots from us.
    mcp-go client: check what happens on `sampling/createMessage` —
    must decline cleanly, not hang. Zoo: needy server requesting sampling +
    elicitation + roots.

## Zoo matrix (transports × headers × breakage)

Axes:
- **T**: stdio-py, stdio-node, stdio-binary(rust/go), http-streamable,
  http-json-only (no SSE), sse-legacy, http-oauth-sim (401→token→200)
- **H**: no-auth, bearer-ok, bearer-bad, custom-header-required,
  redirect-302-same-origin, redirect-302-cross-origin, self-signed-tls,
  mtls-required, cors-preflight, chunked-sse, gzip-body
- **B**: clean, slow-init(35s), slow-tool(15s), hang-forever, crash-on-start,
  crash-mid-call, stdout-junk-line, stdout-blank-line, stderr-flood,
  bad-json-rpc (missing id), wrong-proto-version, discover-only,
  paginated-tools(150), list_changed-after-60s, sampling-request,
  elicitation-request, roots-request, giant-tool-result(5MB),
  unicode-tool-names, duplicate-tool-names, zero-tools, invalid-schema

Priority: all of T-stdio × B (we must nail local), then http-streamable × H
(requires building remote support — currently MISSING), SSE/OAuth after.

## Verdict for oxyp scope

- v1.1: stdio zoo green (all breakage handled: timeouts, junk, crash, weird
  names, pagination, list_changed, sampling-decline).
- v1.2: Streamable HTTP downstream support (client) + zoo H-axis.
- v1.3: OAuth (daemon-held, browser-callback-at-home) + SSE-legacy if any
  target server still needs it.
- Docs: per-client shim config blocks (opencode/omp/hermes shapes above).
