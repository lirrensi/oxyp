# lazy-mcp — your ideas, extracted + exact change list

Source: session `20261028_075216_646eaa13` (182 msgs, 93 exported) + today in this folder.
Repo: `./lazy-mcp` (v2.10.4, clean). No code touched — this file is the deliverable.

## 1. Your vision, verbatim (08:06)

> Client integration with three stupid-simple tools: list servers (+search by name),
> list tools for a named server, call (server, function, args). Gateway runs in the
> background holding all connections, dynamically refreshed. Client holds one
> connection instead of many.

lazy-mcp is that shape + 1: `list_servers` → `list_commands` → `describe_commands`
→ `invoke_command`. The 4th (describe) is schemas-on-demand so the list stays thin.

## 2. What you wanted (requirements)

- R1. 100 servers addable, kept out of context (07:55). Context cost flat.
- R2. MCP router / gateway exists — don't reinvent blindly (07:59).
- R3. CLI client — you liked it (08:17).
- R4. HTTP from outside, not just loopback (08:17). One daemon serves local + remote.
- R5. Daemon holding 100 stdio children; remote access feels like home (08:18).
- R6. Others can also use it (08:18) — later cooled by the no-tenancy warning, no explicit verdict.
- R7. Cold connections: connect only when named, never all-at-once (08:21).
- R8. Programmatic use from Python AND Node, from anywhere: `oxyp.gitlab.list_issues(...)` (08:30, 08:33).
- R9. Wrap own REST APIs as MCP (08:41).
- R10. One registry, 50 tools defined once, addressable from any client; non-MCP clients get generated bindings or a mini stateless MCP shim; CLI tools ingestible as MCP (08:42, your "dream").
- R11. Single process: N clients multiplex onto one daemon↔child stdio pipe, deduped by server name (08:52–08:54). Clients CONNECT, never launch.
- R12. Don't load the gateway as MCP — load as native plugin/integration where possible (08:55).
- R13. Daemon + two doors: direct JSON-RPC to socket, or mini-mcp shim needing no client integration (09:04, your diagram).
- R14. One-command install from git, no publish: `uv tool install git+...` and its Rust equal (09:09–09:10).
- R15. Rename the cutie (09:10). DECIDED 2026-10-28: `oxyp` (supersedes `OXYC` typo; `meowd`/`warren`/`flotilla` dropped).
- R16 (today). True hiding: denied tool/server disappears from ALL output — not listed, not describable, not callable. Server stays in config, hidden until re-enabled. Documented.

## 3. Improvement ideas → exact change list

### In this repo (`./lazy-mcp`, TypeScript)

| ID | Change | lazy-mcp today | Notes |
|----|--------|----------------|-------|
| C1 | Discovery filtering: denied ⇒ invisible in `list_commands`, `describe_commands` throws not-found, `invoke_command` still blocked | `deny` blocks invoke only; tool still listed + described | Same `resolveAction`, applied at discovery. Per-tool + per-server. Your #1 ask. |
| C2 | `enabled: false` = official hide-a-server | Already works (constructor + reload skip; e2e-tested) | Verify + document only. No code. |
| C3 | `tools.include` / `tools.exclude` per server | No field; `default:deny`+globs approximates but doesn't silence | Your 08:19 ask. Composes with C1. |
| C4 | Cold by default: monitor probes only connected servers (or per-server lazy flag) | First tool call wakes monitor → connects ALL servers | `healthMonitor:{enabled:false}` is today's workaround. Decide: default flip vs flag vs docs. |

### New work (NOT this repo)

| ID | Change |
|----|--------|
| C5 | CONSUME: client-independent SDKs (Python + Node) over HTTP — dynamic `oxyp.<server>.<tool>(...)`, parallel via gather/Promise.all, chaining in plain code. No codegen, no host APIs. |
| C6 | DEFINE: author-side — your tools written once in one manifest → emitted as MCP / function-calling JSON / REST / CLI. Opposite of C5 (define vs consume). |
| C7 | Deploy topology (no code): one HTTP daemon on loopback, every client via `url:`. `command:` per client = N daemon copies. |
| C8 | Rust packaging: static musl + `cargo-dist` release pipeline + `mytool update` self-update from release API. |
| C9 | Unpublished install: `cargo install --git …` documented as the Rust equal of `uv tool install git+…`. |
| C10 | Rename: DECIDED → `oxyp`. |

### Raised by assistant, no verdict from you

- D1 `ask` via MCP elicitation (works on Hermes, fails closed elsewhere).
- D2 `ask` = confirmation UX, not access control.
- D3 Batch invoke. D4 Result-side trimming. D5 Semantic ranking. D6 Per-tool timeout/retry. D7 `tools/list_changed`. D8 Child sandboxing. D9 `alwaysExpose` resident tools.

## 4. You liked

- 3-tool enumerate shape; server-name search over tool-description search.
- `describe_commands` (thin list + on-demand schemas).
- Shell CLI, HTTP transport, single-process multiplexing.
- `uv tool install` (PATH-correct, no toolchain on target).
- Mini stateless MCP shim as legit server.
- mcpd's infra half (daemon, declarative config, secrets) — noted, no verdict.
- mcporter/mcpd links — asked for, received, no verdict.

## 5. You disliked / rejected

- X1. PyPI shim (TS bundle + 40 MB `nodejs-wheel` to run JS). "its ts not py?"
- X2. Denied-but-visible tools (→ C1).
- X3. Connect-all on first probe, no tiers/quarantine/backoff (→ C4).
- X4. stdio-per-client daemon copies — angrily rejected; service model only.
- X5. "Hermes already has this, no proxy needed" — angrily rejected. Proxy stays.
- X6. `mcp-` prefix / hyphenated names — swamp, ruled out.
- X7. Shared one-token multi-user gateway — warned (guest = you, incl. your OAuth tokens); you didn't push back, treated as rejected.
- X8. ChatGPT legacy `connector_id` path — deprecated, ignore.

## 6. Decisions needed (your 10 min)

1. C1+C3: first (only?) code change to this repo — confirm scope?
2. C4: flip cold default, add flag, or docs-only?
3. C10: DECIDED → `oxyp`. Done.
4. D1 (`ask`): build or stay static allow/deny?
5. Confirm C5/C6/C8 are separate repos, not this one?
+
## 7. C5/C6 clarified (2026-10-28, this chat)
+
**C5 = consume anything from code.** SDK (`oxyp` package; `gw` was an unapproved placeholder from the 08:31 sketch, now dropped) is a dumb HTTP client + dynamic-attribute trick. Nothing is really imported: `oxyp.gitlab.list_issues(...)` resolves `gitlab` + `list_issues` to strings at call time and POSTs one `invoke_command`. New tools callable instantly, no install per server. Address from `OXYP_URL`/`OXYP_TOKEN` or `cli-endpoint.json`. Parallel = `asyncio.gather` / `Promise.all` (N concurrent POSTs; gateway multiplexes onto one shared `ServerManager`). Chaining = plain variables. Opencode does this inside its client (owns both ends); C5 does it outside any client (owns neither end — just HTTP). Server-side batch (`invoke_many`, D3) is optional later optimization, not required.
+
**C6 = define your own tools once.** One manifest (name, description, schema, handler binding, auth per tool) + emitters: native MCP server (stdio+HTTP), OpenAI/Anthropic function JSON, OpenAPI/REST, CLI, LangChain adapters. Mini stateless shim (3-method HTTP handler: `initialize` + `tools/list` + `tools/call`) for clients without MCP. Relationship: C6 produces servers, C5 consumes them through the gateway.
+
**C6 concrete example.** Say you own an internal CRM REST API. Today you'd hand-write N adapters. With C6 you write ONE `crm.tools.yaml`:
+
```yaml
tools:
+  - name: get_customer
+    description: Fetch a customer by ID
+    inputSchema: { type: object, properties: { id: { type: string } }, required: [id] }
+    handler: { kind: http, method: GET, url: "https://crm.internal/api/customers/{id}" }
+    auth: { type: bearer, env: CRM_TOKEN }
+  - name: create_ticket
+    description: Open a support ticket for a customer
+    inputSchema: { type: object, properties: { id: { type: string }, subject: { type: string } }, required: [id, subject] }
+    handler: { kind: http, method: POST, url: "https://crm.internal/api/tickets" }
+    auth: { type: bearer, env: CRM_TOKEN }
```
+
Then the emitters generate, no hand-writing: (a) an MCP server you add to the gateway as one entry (agents call it via C5 as `oxyp.crm.get_customer(id=...)`); (b) OpenAI/Anthropic function JSON for direct LLM binding; (c) a REST façade or CLI (`crm get-customer --id ...`); (d) a mini stateless shim so a non-MCP client can still `tools/list` + `tools/call` it. One definition, every door. When the API changes you edit the YAML, not five adapters.
