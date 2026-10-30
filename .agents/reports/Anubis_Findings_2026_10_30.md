# Anubis Findings — oxyp — 2026-10-30

Repo: `/home/user/code/oxyp/oxyp` (Go, no `.git` in tree; whole-tree review, quest-new code plus the listed integration points: spawn, remoteStack, reload, shim).
Method: read-only source review. Dependency behavior was verified in `github.com/mark3labs/mcp-go@v1.4.0` under the local module cache where the finding depends on it.

## Threat Model Snapshot (pre-analysis)

- **Profile**: local-first CLI + daemon that spawns arbitrary configured MCP servers, holds OAuth tokens, imports foreign app configs, writes cache/trace/spill files, and optionally binds a loopback HTTP API.
- **Assets**: OAuth access/refresh tokens, `${VAR}`/`{file:}`-supplied upstream credentials, the user's filesystem/processes reachable through downstream tools, tool output (spill files), daemon config integrity.
- **Entry points**: downstream MCP server metadata and responses (stdio and remote), imported `mcp.json`-class files, HTTP API requests (unauthenticated when `daemon.token = ""`), browser callbacks, `oxyp.toml`, CLI/shim arguments.
- **Trust boundaries**: browser page ↔ loopback daemon; local user ↔ daemon-held tokens; daemon ↔ spawned child; daemon ↔ remote MCP/IdP; untrusted imported config ↔ user config; model/client ↔ shim (tool names and filters are attacker-influenced).

Findings are ordered CRITICAL → HIGH → MEDIUM → LOW.

---

## CRITICAL

### CRITICAL — Downstream client lifecycle is bound to the triggering HTTP request context
**Location**: `internal/daemon/daemon.go:340-382` (`clientFor`), `internal/daemon/daemon.go:384-455` (`spawn`), `internal/daemon/daemon.go:600-604` (`startAndInit`); call sites `internal/meta/meta.go:138,165,191,232,248,336`.
**Problem**: Every HTTP handler passes `r.Context()` into `ListTools`/`CallTool`/`Instructions`/`ListResources`/`ReadResource`/`ListPrompts`, and those pass it to `clientFor` → `spawn` → `c.Start(ctx)`. mcp-go's `Stdio.Start` hands that context to `exec.CommandContext` (`transport/stdio.go:294-306` in mcp-go v1.4.0), and `SSE.Start` derives the long-lived stream context from it (`transport/sse.go:225-236`). Go's `net/http` cancels the per-request context immediately after `ServeHTTP` returns (`server.go:2069`). The child process is therefore killed and the SSE stream canceled as soon as the first HTTP response is written. When the transport dies, `Stdio.SendRequest` returns `ErrTransportClosed`, but oxyp only resets a remote client on `transport.ErrSessionTerminated` (`daemon.go:676`, `daemon.go:1089`), so `m.client` stays cached in the dead state.
**Impact**: A lazy local server answers exactly one request; every subsequent `oxyp call` on the same daemon fails with `transport closed` until the idle sweeper clears the cache (default 10 minutes) or the daemon restarts. Legacy SSE remotes lose their receive stream immediately after the first request. Only eager/keep-alive children survive, because `EagerStart` uses the daemon signal context (`cmd/oxyp/main.go:133`). This defeats the core "lazy cold start then reuse" model.
**Fix**: Create/Start downstream clients with a daemon-lifetime context (a spawn context derived from the daemon's shutdown context, or `context.WithoutCancel` on the request context) and use the request context only for per-RPC calls. Cancel the spawn context on shutdown/reload. Add a regression test that sends two successive HTTP `POST /servers/{name}/tools/{tool}` requests to one local server and asserts the second succeeds.

---

## HIGH

### HIGH — DCR client credentials are never persisted; refresh and manual `--complete` lose the client_id
**Location**: `internal/daemon/login.go:136-163` (rebuilt handler for `--complete`), `internal/oauth/pending.go:13-19` (pending stores only state/verifier/redirect), `internal/daemon/daemon.go:531-549` (`remoteStack` builds the handler from config only), `internal/daemon/login.go:280-307` (device DCR).
**Problem**: mcp-go performs Dynamic Client Registration inside `GetAuthorizationURL` and keeps the resulting `client_id`/`client_secret`/auth method/issuer in handler memory only. oxyp never records them. A new process for `oxyp login --complete` (`CompleteBrowserLogin` with `h == nil`) builds a fresh handler from config only; `ProcessAuthorizationResponse` reconstructs the transaction with empty credentials and `refreshToken` calls `boundCredentials`, which returns the configured (empty) client id. The same happens to any handler recreated after a daemon restart. The zoo IdP cannot catch this because `/token` ignores `client_id` (`zoo/http/zoo_oauth.py:141-158`).
**Impact**: For the default DCR flow, `--complete` fails with `invalid_client`/`invalid_grant` on any IdP that validates the client, and refresh after a daemon restart fails when the access token expires, forcing interactive re-login. The README's headless `--complete` recovery path is broken in practice.
**Fix**: Persist the registration (`client_id`, `client_secret`, token-endpoint auth method, credentials issuer) alongside the pending flow/token for each storage key, and populate `transport.OAuthConfig{ClientID, ClientSecret, CredentialsIssuer, TokenEndpointAuthMethod}` in both `remoteStack` and `CompleteBrowserLogin`. Add a test whose token endpoint rejects a mismatched `client_id`.

### HIGH — Device-flow tokens are stored without an issuer, so they can never be refreshed
**Location**: `internal/daemon/login.go:299-307`.
**Problem**: `loginDevice` constructs `transport.Token` with access/refresh/expiry only; `Issuer` is empty. In mcp-go, `refreshToken` rejects an empty issuer immediately (`transport/oauth.go:415-418`), so the saved refresh token is unusable. The device flow is the headless path; unlike the browser path it does not go through mcp-go's bound-token save.
**Impact**: When the device access token expires, every call fails and the user must re-run `oxyp login` with a new device code; a headless deployment loses unattended renewal even though a refresh token was granted.
**Fix**: Set `Issuer: md.Issuer` on the saved token (and use the same bound-save path as the browser flow); add a test that expires a device token and verifies a refresh occurs without user interaction.

### HIGH — `Reload` mutates `managed.cfg` and `daemonCfg` while call paths read them lock-free
**Location**: `internal/daemon/daemon.go:1283` (`d.daemonCfg = cfg.Daemon`), `internal/daemon/daemon.go:1319` (`m.cfg = ns`); lock-free readers: `daemon.go:144-150` (`traceFor`), `daemon.go:1215-1230` (`sweepIdle`), `daemon.go:384-455` (`spawn`), `daemon.go:652-676`/`1078-1090`; `internal/meta/meta.go:92-99` (Entry pointer used after the lock is released).
**Problem**: `Reload` holds `d.mu.Lock`, but most readers take neither `d.mu` nor `m.mu`. `Entry` returns `&m.cfg`, and handlers read the fields after releasing the lock. `traceFor` reads `d.daemonCfg` after `d.mu.RUnlock`, and `sweepIdle` reads it with no lock. A reload concurrent with a call, sweep, or status request is a Go data race; multi-word fields (strings, slices) can tear.
**Impact**: Race detector failures and torn config reads. A torn read can produce an inconsistent entry (e.g., `IsRemote()` true while `Command` is nil, panicking at `argv[0]` in `spawn`), or use a mixed old/new idle timeout or guard. Reload is user-triggered and can coincide with agent traffic.
**Fix**: Make `ServerEntry` immutable: store `*config.ServerEntry` in `managed` and swap the pointer under `m.mu` (readers copy it under the lock); guard `d.daemonCfg` with `d.mu` or an atomic pointer. Never assign fields of the live entry.

### HIGH — `Reload` can adopt a connection created from the pre-reload config
**Location**: `internal/daemon/daemon.go:340-382` (`clientFor`), `internal/daemon/daemon.go:1280-1322` (`Reload`).
**Problem**: `Reload` never inspects `m.connecting`. If a cold start is in flight when the entry is modified, the spawn completes after `Reload` has cleared `m.client` and set the new `m.cfg`, and `clientFor` then publishes the client built from the old config. `Reload` reports the server as modified and returns success.
**Impact**: After a reload that changes a URL or command, calls silently keep using the previous endpoint/child until the client is reset by session expiry or the idle sweeper; the new config is not in effect despite the CLI's success report. Combined with the request-context bug, a dead old client can also be cached.
**Fix**: Add a config generation counter to `managed`; `Reload` increments it and waits for (or cancels) `m.connecting`; `spawn` captures the generation and closes/discards the new client if the generation changed before publishing.

### HIGH — Empty token disables authentication regardless of bind address
**Location**: `internal/config/config.go:424-437` (`ValidateDaemon`), `cmd/oxyp/main.go:118-125` (`meta.New(..., cfg.Daemon.Token)`), `internal/meta/meta.go:60-67` (`Handler`).
**Problem**: `token == ""` skips the only auth check, and `ValidateDaemon` never requires a token when `daemon.addr` is a non-loopback address (`0.0.0.0`, `::`, a LAN IP). The README documents empty token as "loopback trust", but the code trusts any peer that can connect.
**Impact**: A user who sets `addr = "0.0.0.0:8379"` and leaves the default token exposes unauthenticated tool invocation, health/error readout, and `/reload` to the network. Downstream tools can mutate files and systems with the daemon user's privileges.
**Fix**: In `ValidateDaemon`, parse `Addr` (net.SplitHostPort + net.ParseIP); if the host is not loopback, require a non-empty token, and reject a literal loopback-only configuration that implies otherwise. Optionally refuse to start the listener when a non-loopback address is paired with an empty token.

### HIGH — Loopback API accepts cross-origin browser requests and arbitrary Host headers
**Location**: `internal/meta/meta.go:60-67` (only bearer check), `internal/meta/meta.go:310-336` (invoke decodes any content type), `internal/meta/meta.go:399-406` (`/reload`), `cmd/oxyp/main.go:118-125`.
**Problem**: With the default empty token, the handler performs no `Origin`, `Host`, or `Content-Type` validation. A web page can send a simple CORS request (`Content-Type: text/plain`) whose body is JSON to `POST /servers/{name}/tools/{tool}` and to `/reload`; the body is decoded regardless of content type. Because `Host` is also unchecked, DNS rebinding (page origin resolves to 127.0.0.1) allows a page to read the JSON responses, including tool results.
**Impact**: [ASSUMPTION] the browser and daemon share a host, and the default empty token is used. Any visited page can invoke any allowed downstream tool (filesystem, shell connectors, delete tools) and trigger reloads; rebinding lets it read the results. Deny lists only mitigate.
**Fix**: Validate `Host` against the configured bind address; reject state-changing requests with a non-empty, non-loopback `Origin`; require `Content-Type: application/json` (forces a preflight that the daemon does not grant); and/or require a bearer token even on loopback. Add an explicit CORS-deny policy.

### HIGH — `GET /servers` spawns servers and re-probes every failed one
**Location**: `internal/meta/meta.go:92-117` (`Connected` set from any health record; `Instructions` call), `internal/daemon/daemon.go:809-833` (`Instructions` → `clientFor`), `internal/daemon/daemon.go:1134-1143` (`recordFailure` creates the record).
**Problem**: `Connected` is set whenever `HealthOf(name) != nil`, which includes failed spawns and idle-stopped servers, not actual connections. `Instructions` then calls `clientFor`, spawning any server whose client is nil. The API list has no timeout and iterates servers sequentially. A failed remote is retried with a full initialize (30s budget) on every list request; an idle-stopped local is respawned (and, under the context bug, immediately killed). The code comment claims "Never blocks list".
**Impact**: `list_servers` through the shim and `oxyp servers` can block for tens of seconds or minutes, retry every broken server on every call, and resurrect idle children, defeating the lifecycle. With several unreachable remotes, the CLI's 60s client timeout fires while the daemon keeps spawning.
**Fix**: Report connection state from `m.client != nil` (under `m.mu`), not from the health record; in `handleServers` only return already-cached instructions (never initiate a connect), or omit them. If live retrieval is needed, bound it per server and run it asynchronously.

### HIGH — Long-lived SSE/notification streams are killed by the 10-second HTTP client timeout
**Location**: `internal/daemon/daemon.go:516` (`httpClient := remhttp.NewClient(d.reqTO, tlsCfg)`), `internal/daemon/daemon.go:469-479` and `493-505` (`WithHTTPClient` for SSE), `internal/daemon/daemon.go:480-488` (`WithHTTPTimeout` + basic client for Streamable HTTP).
**Problem**: `reqTO` is 10s. Go's `http.Client.Timeout` covers reading the response body, and an SSE stream's body is long-lived. mcp-go's `SSE.readSSE` does not reconnect; when the timeout cancels the stream the transport stops receiving responses. Streamable HTTP uses the same client for the standalone GET stream and any event-stream POST response.
**Impact**: After 10 seconds of SSE idle (or immediately, due to the request-context finding), legacy SSE servers stop answering calls and time out. If continuous listening were enabled for Streamable HTTP, the notification stream would be torn down every 10s and re-opened after a 1s retry gap.
**Fix**: Give streaming transports a client with `Timeout: 0` (timeouts via per-request contexts and `WithResponseTimeout`); keep a short-timeout client only for ordinary RPCs. Add an SSE test that idles longer than `reqTO` before the next call.

### HIGH — `daemon.token` is not expanded; `${VAR}` becomes the literal bearer token
**Location**: `internal/config/config.go:42-46` (`Token`), `internal/config/config.go:253-280` (`Load` expands only server entries), `internal/config/expand.go:60-88` (`expandServerSecrets` covers URL/headers/env), `internal/meta/meta.go:61-63` (raw comparison).
**Problem**: `${VAR}` and `{file:...}` expansion is applied only to server URL/headers/env. `daemon.token = "${OXYP_TOKEN}"` is used literally, is written to `daemon.json`, and becomes the expected bearer string; validation does not reject a leftover `${...}` in the daemon block.
**Impact**: The operator believes the token is supplied from the environment, but a predictable literal is the credential (e.g., anyone who guesses `${OXYP_TOKEN}` can authenticate if the address is exposed). The secret cannot be carried via env/file as the secret stance requires, so users are pushed to hardcode it in `oxyp.toml`.
**Fix**: Expand `daemon.token` with the same `expandValue` path and fail closed if any `${...}`/`{file:...}` remains. Apply the same rule to OAuth client fields (see next finding). [ASSUMPTION] daemon-wide `${VAR}` use is expected per `doc/PHILOSOPHY.md` §§1–2.

---

## MEDIUM

### MEDIUM — `oauth.client_id`/`client_secret`/`scope` are not expanded
**Location**: `internal/config/expand.go:60-88`, `internal/config/config.go:210-224` (`OAuthConfig`), `internal/daemon/daemon.go:541-546`.
**Problem**: Headers and env values are expanded; the OAuth block is not. Validation only requires the fields to be non-empty, so `${VAR}` passes and is sent literally to the IdP; `{file:...}` sends the path string.
**Impact**: Static-client login fails with `invalid_client` for the documented env/file secret style, or the literal placeholder/path is transmitted. Operators are forced to store `client_secret` in plaintext `oxyp.toml`.
**Fix**: Expand `ClientID`, `ClientSecret`, `Scope`, and `RedirectURI` in `expandServerSecrets`, failing closed on leftover `${...}` exactly as headers do.

### MEDIUM — `client_credentials` auth-method selection is unreachable
**Location**: `internal/oauth/clientcredentials.go:22-24` (`TokenEndpointAuthMethod string json:"-"`), `internal/oauth/clientcredentials.go:61-75`.
**Problem**: The field is explicitly excluded from JSON and never assigned, so `method` is always `client_secret_post`; the `client_secret_basic` branch is dead. RFC 8414 advertises `token_endpoint_auth_methods_supported` (an array), which is not decoded.
**Impact**: Authorization servers that accept only `client_secret_basic` reject this flow even though the code comment promises support. The dead branch also hides the incorrect `url.QueryEscape` encoding for Basic credentials (spaces become `+`, not `%20`).
**Fix**: Decode `token_endpoint_auth_methods_supported []string`, choose `client_secret_basic` when it is the only offered method (or when configured), and encode Basic credentials with RFC 6749 percent-encoding.

### MEDIUM — Streamable-HTTP servers never deliver `list_changed` to the daemon
**Location**: `internal/daemon/daemon.go:480-488` (`dialRemote` options), `internal/daemon/daemon.go:440-451` (notification handler), `internal/daemon/daemon.go:652-678` (cache invalidation).
**Problem**: Neither `WithContinuousListening` nor `Client.Listen`/`subscriptions/listen` is used. mcp-go's Streamable HTTP transport defaults `getListeningEnabled=false`; the modern protocol (LATEST is 2026-07-28 in mcp-go v1.4.0) removed the standalone GET in favor of `subscriptions/listen`, which oxyp never opens. Server-initiated notifications outside a request's POST response are therefore dropped.
**Impact**: Remote tool/resource/prompt changes do not invalidate the memory or disk cache; `search`, `tools`, and `describe` serve stale metadata until a config change or restart, contrary to the README's "upstream list_changed" claim.
**Fix**: After `Initialize`, open `Client.ListenAsync` with the relevant filters for modern servers, or enable continuous listening for legacy ones; close the subscription with the client.

### MEDIUM — Mid-session OAuth 401s are reported as generic downstream errors
**Location**: `internal/daemon/daemon.go:1078-1090` (`CallTool` only handles `ErrSessionTerminated`), `internal/daemon/daemon.go:1134-1143` (`recordFailure` only maps `*AuthNeededError`), `internal/daemon/daemon.go:420-436` (mapping exists only in `spawn`), `internal/meta/meta.go:360-372`.
**Problem**: mcp-go returns `*transport.OAuthAuthorizationRequiredError` on any 401 when an OAuth handler is attached (Streamable HTTP `sendHTTP`:705-729; SSE:651-670, 831+). Only the spawn/initialize path converts that to `AuthNeededError`. `CallTool`/`ListTools`/`Resources`/`Prompts` store the raw error and the HTTP layer returns 502.
**Impact**: When a token expires or is revoked mid-session, clients get opaque 502s, `/health` shows a generic error instead of `auth_required`, and the documented `oxyp login` recovery is not surfaced; the client is not reset.
**Fix**: Centralize post-connect error mapping: detect `transport.ErrOAuthAuthorizationRequired`/`ErrAuthorizationRequired` in every operation, return `AuthNeededError` (populated with the login command), and set auth-required health.

### MEDIUM — VS Code `mcp.json` (top-level `servers`) is not imported
**Location**: `internal/importcfg/importcfg.go:187-211` (`ParseVSCode`), `internal/importcfg/importcfg.go:179-186` (`ParseMCPServerMap`).
**Problem**: The parser only decodes `"mcp": {"servers": ...}` and `"mcpServers"`. VS Code's dedicated `mcp.json` (the file the source list explicitly includes at `~/.config/Code/User/mcp.json`) uses a top-level `"servers"` object. That shape produces no entries and falls through to `ParseMCPServerMap`, which errors with "no mcpServers key".
**Impact**: `oxyp import --from vscode` silently misses every server for the common `mcp.json` shape; the user only sees a parse warning and an empty import.
**Fix**: Decode a top-level `Servers map[string]genericEntry json:"servers"` and merge it with the other shapes before falling back.

### MEDIUM — `import`/`add` create `oxyp.toml` as 0644 with copied credentials
**Location**: `internal/importcfg/importcfg.go:396-415` (`os.WriteFile(path, ..., 0o644)`), `internal/importcfg/importcfg.go:270-300` (`ToOxyp` copies headers/env), `cmd/oxyp/import.go:90-100`, `cmd/oxyp/add.go:130-140`.
**Problem**: Foreign config `headers` (e.g., `Authorization: Bearer lin_x`) and env values are written verbatim into `oxyp.toml`; a new file is created 0644 in a 0755 directory, and an existing file keeps its mode. Every other oxyp credential file (`daemon.json`, OAuth tokens, spill, trace) is 0600. There is no conversion to `${VAR}`.
**Impact**: Upstream API tokens copied from other apps become readable by other local users, contradicting the stated 0600 secret policy and the portability/secret stance.
**Fix**: Create with 0600 (and chmod existing files to 0600, or refuse group/world-readable targets). Optionally rewrite literal `Authorization`/env secrets as `${VAR}` with a note.

### MEDIUM — `GET /resources` drops instructions when the server lacks resources support
**Location**: `internal/meta/meta.go:191-196` vs `internal/meta/meta.go:207`.
**Problem**: The `isUnsupported(err)` branch returns `{"resources": [], "instructions": ""}` without calling `Instructions`. Instructions are an initialize-time core field, independent of the optional resources capability.
**Impact**: Clients using the documented "list + init instructions" endpoint receive no instructions for tools-only servers that do send them, even though the value is cached from a live init. The only way to get it is a different endpoint that has its own spawn bug.
**Fix**: Fetch instructions before the unsupported branch and include the value in both responses (the server is already connected at that point).

### MEDIUM — `Reload` closes clients synchronously under the daemon-wide lock
**Location**: `internal/daemon/daemon.go:1280-1322` (`d.mu.Lock`, `m.client.Close()` under `m.mu`), contrasted with `internal/daemon/daemon.go:1340-1370` (`Close` uses a per-close 5s bound).
**Problem**: `Reload` holds `d.mu` for the whole diff and calls `m.client.Close()` directly. mcp-go's stdio `Close` waits up to `gracefulShutdownTimeout` (2s) plus `forceKillTimeout` (3s), and again 3s, per child; remote teardown can also block. No goroutine/timeout is used, unlike `Daemon.Close`.
**Impact**: A reload or SIGHUP can freeze every API handler for seconds per modified/removed server, and can exceed the CLI's 60s timeout while the daemon is still closing children. A child that ignores the graceful path blocks the entire API until the close returns.
**Fix**: Snapshot the clients under a short critical section and close them outside `d.mu` with the same bounded goroutine pattern as `Close()`.

### MEDIUM — Daemon writes its endpoint and binds HTTP only after eager warmup
**Location**: `cmd/oxyp/main.go:133-160` (`d.EagerStart(ctx)` before `writeEndpoint` and `ListenAndServe`), `cmd/oxyp/main.go:169-194` (`ensureDaemon` waits 5s).
**Problem**: `EagerStart` is synchronous (each unreachable eager/keep-alive server can consume the 30s init budget) before the endpoint file is written and before the listener binds. `ensureDaemon` polls for endpoint + `/health` for only 5 seconds.
**Impact**: A cold autostart with a slow/unreachable eager server fails with "daemon did not come up" even though the daemon is still starting; the first command after boot fails, and the foreground daemon is unreachable during warmup.
**Fix**: Bind the listener and write the endpoint before `EagerStart`, expose a "starting" health state, and run warmup asynchronously; increase/back off the autostart wait using `/health` rather than the file alone.

### MEDIUM — `probeRemote` ignores per-server TLS and headers
**Location**: `internal/daemon/daemon.go:1030-1086` (bare `&http.Client{Timeout: timeout}` + `hc.Get`), invoked at `internal/daemon/daemon.go:429-434`.
**Problem**: The probe builds a default-TLS, anonymous client. It ignores `cfg.TLS` (CA/mTLS/insecure) and `cfg.Headers`, so servers configured for a custom CA, mTLS, or header auth are probed as plain unauthenticated requests. The resulting hint is appended to the real error.
**Impact**: For `tls.ca_file`/`insecure`/mTLS entries the hint can say "TLS verification failed — check tls.ca_file" or "server needs auth" even though those are configured; users chase the wrong fix. The probe also adds latency on every failed initialize and embeds the full URL (including query secrets) in health/502 output.
**Fix**: Use `remhttp.NewClient` with `cfg.TLS` and send `cfg.Headers`; skip the probe when TLS/headers make it non-representative; redact the URL in hints.

### MEDIUM — Spill files are never cleaned up
**Location**: `internal/outguard/outguard.go:78-99`, usage at `internal/meta/meta.go:345-351`.
**Problem**: Every over-limit text result creates a new file in `os.TempDir()`; nothing tracks or removes files, and there is no cleanup on daemon shutdown or retention bound.
**Impact**: With `output_guard` armed and a chatty/large-result tool, `/tmp` grows without bound; a long-running daemon can fill the disk, and full tool output (potentially sensitive) persists indefinitely.
**Fix**: Add retention (max files/bytes/age) with a reaper, or clean on daemon shutdown; at minimum document the directory and provide a cleanup command.

### MEDIUM — OAuth callback can arrive before `Wait` registers the state
**Location**: `internal/daemon/login.go:241-245`.
**Problem**: `showURL(url)` is called before `cb.Wait(ctx, state)`. The callback handler only knows registered states and returns 400 "unknown oauth state" for an early callback; `Wait` then blocks until the context ends (5 minutes in the CLI).
**Impact**: Any caller whose `showURL` auto-opens a browser/fake browser (the zoo test pattern at `zoo/oauth_test.go:100-140`) can lose the code and hang the login; retry requires a fresh login. [ASSUMPTION] the approval can complete faster than the next statement, as the tests model.
**Fix**: Register the state/channel before invoking `showURL` (split `Wait` into `Register` + `Await`), then call `showURL`.

### MEDIUM — Ignored `http.NewRequest` errors cause nil-pointer panics in the shim and CLI
**Location**: `cmd/oxyp/shim.go:89-130` (`get`/`post`), `cmd/oxyp/main.go:197`, `cmd/oxyp/main.go:220`.
**Problem**: Paths are built by concatenating raw server/tool names and raw filters (`/servers/<srv>/tools/<cmd>`, `?filter=` + raw string). `http.NewRequest`'s error is discarded. Invalid URL escapes (`%zz`) or control characters (newlines in upstream tool names or model-supplied filters) make `req` nil; `req.Header.Set` (when a token is set) or `http.Client.Do(nil)` then panics.
**Impact**: A malicious/buggy downstream server that advertises a tool name with `%zz`/a newline, or a model that passes such a filter to `list_servers`, crashes the shim process (the MCP client) and the CLI. Server names with `/` also misroute.
**Fix**: Check the error and return it; URL-escape path segments with `url.PathEscape` and build queries with `url.Values`; never call Do with a nil request.

### MEDIUM — List handlers dereference `Entry` without a nil check after a concurrent reload
**Location**: `internal/meta/meta.go:92-99` (`handleServers`), `internal/meta/meta.go:376-384` (`handleHealth`).
**Problem**: `Names()` and `Entry()` take separate lock acquisitions. A concurrent `POST /reload` that removes a server makes `Entry(name)` return nil while the name from `Names()` is still in the loop; both handlers dereference `e`.
**Impact**: A concurrent reload plus `GET /servers` or `GET /health` panics in the handler (`net/http` recovers per request, so the request fails and a stack trace is logged). Reload is user-triggerable while an agent polls health.
**Fix**: Nil-check `Entry` and skip/404; preferably snapshot names+entries under one lock.

### MEDIUM — Documented redaction is never applied
**Location**: `internal/redact/redact.go:1-92` (no production callers), `README.md:19-22`, `doc/PHILOSOPHY.md:8-10`; leak paths `internal/daemon/daemon.go:1030-1086` (URL in probe hint), `internal/daemon/daemon.go:1134-1143` (raw error into health), `internal/daemon/daemon.go:620-640` (child stderr logged verbatim), `internal/meta/meta.go:360-372` (raw error bodies).
**Problem**: The `redact` package is used only by its tests. Health and 502 bodies carry raw `err.Error()`, probe hints embed the full expanded URL, and child stderr is logged at Info without scrubbing. Expanded `${VAR}` values in URLs/headers can therefore reach API responses and daemon logs.
**Impact**: Secret values (query/URL credentials, anything a child prints to stderr) leak to `/health`, `/servers`, 502 bodies, and logs; the documented "redacted in logs" guarantee is false.
**Fix**: Route health/error/log strings and probe hints through `redact` (including URL query redaction and header redaction), and add a test that a token in a configured URL never appears in `/health`.

### MEDIUM — `redirect_uri` without `{port}` is advertised while the CLI binds an ephemeral port
**Location**: `cmd/oxyp/main.go:474-483` (port 0 unless `callback_port`), `internal/daemon/login.go:175-187` (template without `{port}` wins over `cb.Port()`), `internal/daemon/daemon.go:569-578`.
**Problem**: If `oauth.redirect_uri` names a fixed port (e.g. `http://localhost:8378/callback`) and `callback_port` is unset, the CLI binds `:0` but `browserHandler` returns the template verbatim; the IdP redirects to the fixed port where nothing is listening.
**Impact**: Login hangs until the 5-minute context and DCR registers the unusable URI; no error explains the mismatch.
**Fix**: When the template has no `{port}`, bind the port named in it (and validate it matches `callback_port`), or require `{port}`/`callback_port` when a fixed template is present; reject mismatches at config validation.

### MEDIUM — `Reload` does not apply `daemon.token`/`daemon.addr` changes
**Location**: `cmd/oxyp/main.go:118-133` (`meta.New` with the startup token; the reload closure only calls `d.Reload`), `internal/daemon/daemon.go:1280-1283`.
**Problem**: The listener address and bearer token are fixed when the HTTP server/meta handler is created. `Reload` updates only `d.daemonCfg` (idle/guard/trace) and server entries; `daemon.json` keeps the old token.
**Impact**: A rotated/revoked token remains valid after `oxyp reload`, and the CLI keeps using the old token from `daemon.json`; the operator must restart, contrary to "re-read config".
**Fix**: Either reject listener/auth changes at reload with a clear log, or drain and rebind the listener / swap the token atomically and update the endpoint file.

### MEDIUM — Device and client_credentials token requests omit the RFC 8707 `resource` parameter
**Location**: `internal/oauth/device.go:172-205` (`PollDeviceToken` form), `internal/oauth/device.go:143-150` (`RequestDeviceCode`), `internal/oauth/clientcredentials.go:55-70`.
**Problem**: These flows send only grant/client/scope/secret fields. mcp-go's browser flow includes `resource` (verified in `transport/oauth.go` `ProcessAuthorizationResponse`/`refreshToken`), as MCP authorization requires for resource binding.
**Impact**: Authorization servers that enforce resource binding reject device and client_credentials login (`invalid_target`); the zoo IdP ignores the parameter, so the tests do not catch it.
**Fix**: Pass the MCP endpoint URL into these helpers and add `resource=<mcp URL>` to the device authorization, token, and refresh requests. [ASSUMPTION] the target AS enforces the MCP resource parameter.

### MEDIUM — `OutputGuard` per-server disable is impossible; the documented `false` is a load error
**Location**: `internal/config/config.go:107-109` (comment "false disables"), `internal/config/config.go:170-189` (`EffectiveGuard`), `internal/config/config.go:424-437`.
**Problem**: The field is `*OutputGuardConfig`. Writing `output_guard = false` (as the comment advertises) is a TOML type mismatch and `config.Load` fails, so the daemon will not start. There is also no representation for "disable the daemon-wide guard for this server": nil means "inherit", not "off".
**Impact**: A user following the comment bricks startup; no per-server opt-out exists for a noisy/trusted server when the daemon guard is armed.
**Fix**: Add an explicit `enabled` flag (or a sentinel such as `output_guard = { enabled = false }`), make `EffectiveGuard` honor it, and correct the comment. Add a config load test for the advertised syntax.

---

## LOW

### LOW — `metacache.save` uses one shared temp path and can lose newer updates
**Location**: `internal/metacache/metacache.go:135-168`.
**Problem**: Every `Put`/`Drop` unlocks the mutex and then calls `save`, which snapshots under a read lock and writes `path + ".tmp"`. Two concurrent saves write the same temp file (interleaved truncate/write), and a snapshot taken before a newer `Put` can be renamed after it, dropping the newer entry. `New` treats corruption as an empty cache.
**Impact**: Rare cache corruption or lost updates; search falls back to no metadata and spawns on next use. Not a correctness failure (the cache is disposable), but it contradicts the atomic-write comment.
**Fix**: Serialize saves (a dedicated write mutex or a single saver goroutine) and use a unique temp name per save (`os.CreateTemp` in the same dir) before rename.

### LOW — Per-server trace configuration is partially dead
**Location**: `cmd/oxyp/main.go:103-115` (tracer opened only from `cfg.Daemon.Trace`), `internal/daemon/daemon.go:144-150` (`traceFor` only checks `Enabled`), `internal/config/config.go:184-204` (`EffectiveTrace` returns per-server `File`).
**Problem**: A server-level `[servers.x.trace] enabled = true` without `[daemon.trace]` never creates a tracer, so nothing is recorded; a per-server `trace.file` is ignored because the only file is the daemon-level one.
**Impact**: Operators believe per-server trace overrides work (the config type says override) but get no output or the wrong file. Silent.
**Fix**: Open the tracer when any enabled server or the daemon enables trace, or reject per-server trace without a daemon trace; drop or honor `Trace.File` per server with a documented precedence.

### LOW — `AuthNeededError` never carries the authorize URL, and the shim discards `auth_required`
**Location**: `internal/daemon/daemon.go:47-56` (type), `internal/daemon/daemon.go:426-427` (constructed with `Server` only), `internal/meta/meta.go:367-372` (401 body has the hint), `cmd/oxyp/shim.go:118-126` (only `m["error"]` is surfaced).
**Problem**: `AuthNeededError.Error()` prints "open " with an empty URL. The 401 JSON body's `auth_required: "run: oxyp login <name>"` is dropped by the shim's error path.
**Impact**: The primary MCP client (shim) shows an unactionable "needs OAuth: open " message; users cannot tell how to recover.
**Fix**: Populate the URL via `AuthURLFor` or include the login hint in the error text, and have the shim prefer `auth_required` from the response body.

### LOW — HTTP server has no read/write/idle timeouts or body limit
**Location**: `cmd/oxyp/main.go:109-112` (`&http.Server{Addr, Handler}` only), `internal/meta/meta.go:330-335` (unbounded JSON decoder).
**Problem**: The server sets no `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, or `MaxHeaderBytes`, and `handleInvoke` decodes an unbounded body.
**Impact**: If the daemon is bound to a non-loopback address (see the token finding), unauthenticated peers can slowloris or send oversized bodies to exhaust resources. On loopback the impact is limited.
**Fix**: Set sane server timeouts and a body limit (`http.MaxBytesReader`), especially for non-loopback binds.

---

## Threat Model Snapshot

- **Profile**: local-first CLI/daemon ("AI-tooling" + local service); single binary, no tenant isolation; holds OAuth tokens and spawns configured executables.
- **Assets**: OAuth access/refresh tokens and their DCR client credentials; `${VAR}`/`{file:}` upstream credentials; the user's machine and files reachable through downstream tools; tool output spilled to temp files; config integrity.
- **Entry points**: downstream MCP metadata/notifications/responses; the HTTP API (unauthenticated by default); browser OAuth callbacks; imported third-party configs; `oxyp.toml`; shim/CLI arguments; daemon environment inherited by children.
- **Trust boundaries evaluated**: browser ↔ loopback API (CSRF/DNS rebinding); network peer ↔ API (empty token + non-loopback); daemon ↔ child process (exec context, env, cwd); daemon ↔ remote MCP/IdP (TLS, redirects, DCR, token refresh); untrusted imported config ↔ local secret store; model/client ↔ shim (tool names, filters, media passthrough); reload ↔ in-flight calls.

## Coverage

- **Analyzed**: all Go packages under `internal/` and `cmd/oxyp` (`config`, `daemon`, `login`, `meta`, `metacache`, `oauth` incl. device/client_credentials/pending, `outguard`, `perms`, `ptrace`, `redact`, `remhttp`, `search`, `importcfg`, `add`, `doctor`, `import`, `shim`, `main`), plus `README.md`, `doc/PHILOSOPHY.md`, `doc/ZOO.md`, `doc/CLIENTS.md`, and the relevant mcp-go v1.4.0 transport sources for context/stream/registration behavior. Tests were read to understand intended behavior (not executed). The repo has no `.git`, so no diff was available; the review is whole-tree with focus on the quest areas and the listed integration points.
- **Not analyzed / not validated**: live behavior against real IdPs (DCR validation, device polling, resource binding) and real browsers (PNA/DNS-rebinding enforcement) — the repo's zoo IdP ignores client_id and PKCE, so those paths cannot be proven green by the existing tests; the Python zoo servers were read only enough to confirm their behavior; performance under large tool catalogs was not benchmarked; no `go vet`/race detector/build was run (read-only review constraint); `smoke-server.py` and test-only helpers received only a read.
- **Confidence**: High. The CRITICAL finding is proven from Go's `net/http` and mcp-go's `exec.CommandContext` semantics; the OAuth and reload findings are proven from mcp-go's handler/refresh code and oxyp's lock usage. Lower confidence is flagged inline where the impact depends on a specific IdP/browser behavior.

## Verdict

- Correctness: **incorrect** — the daemon cannot serve a second call to a lazily spawned local server, and the default OAuth DCR flow cannot complete or refresh across processes.
- Confidence: 0.95. The CRITICAL finding is proven from Go `net/http` and mcp-go `exec.CommandContext` semantics; findings whose impact depends on IdP/browser behavior are labeled `[ASSUMPTION]`.
- Blocking for merge: the CRITICAL and all HIGH findings (10 items) should be fixed before this quest is considered complete.
