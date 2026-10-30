# Zoo compat matrix

Run: `go test ./...` (full suite ~90s incl. timeout edges).
Zoo servers: `zoo/stdio/zoo_stdio.py` (flag matrix), `zoo_needy.py`
(sampling/elicitation/roots requests), `zoo_node.js` (non-Python runtime),
`zoo/http/zoo_http.py` (Streamable HTTP + session + legacy SSE matrix),
`zoo/http/zoo_oauth.py` (fake IdP: metadata/DCR/authorize/token),
`zoo/http/zoo_tls.py` (self-signed CA + mTLS, openssl certs).

## stdio battery — 22/22 ✅ 2026-10-30

| Case | Result | Notes |
|---|---|---|
| clean | ✅ | list 2 + call echo |
| junk-stdout (non-JSON line) | ✅ | mcp-go skips unparseable lines |
| blank-stdout | ✅ | skipped |
| stderr-flood (5000×200B) | ✅ | drained, no deadlock |
| slow-init 2s | ✅ | within 30s init timeout |
| slow-tool 2s | ✅ | within 10s request timeout |
| slow-tool 15s | ✅ | **fails clean at 10.0s** with deadline exceeded |
| hang-forever | ✅ | **killed clean at 10.0s** |
| crash-on-3rd-call | ✅ | `transport closed`, server unhealthy, daemon alive |
| bad-json-on-call | ✅ | hangs till 10s timeout (read loop skips junk, waits) |
| wrong-proto-version | ✅ | `unsupported protocol version`, clean refuse |
| paginated 150 tools | ✅ | all 150 listed (mcp-go auto-paginates + cursor-loop guard) |
| unicode names | ✅ | pass through untouched |
| dup names | ✅ | listed as-is |
| zero tools | ✅ | empty list, no crash |
| bad-schema tool | ✅ | clear `unparseable tool list` error (strict — see gap 3) |
| giant result 1MB | ✅ | single-line JSON passes (no read cap in mcp-go) |
| notify list_changed | ✅ | **cache invalidated** (wired 2026-10-28) |
| node runtime | ✅ | cross-runtime clean |
| media (text+image+resource_link) | ✅ | typed blocks survive daemon→HTTP; structuredContent preserved |
| needy (sampling+elicitation+roots requests) | ✅ | declined `No request handler`, no hang |

## HTTP battery — 16/16 ✅ 2026-10-28

| Case | Result | Notes |
|---|---|---|
| clean | ✅ | list 2 + call echo |
| bearer-ok | ✅ | static Authorization forwarded |
| bearer-missing → 401 | ✅ | clean `authorization required`, unhealthy recorded |
| custom-header | ✅ | X-Tenant forwarded |
| sse-stream response | ✅ | mcp-go parses event-stream |
| same-origin redirect | ✅ | POST+body preserved (301/302/303 rebuilt) |
| broken-json body | ✅ | clean decode error |
| wrong content-type | ✅ | `unexpected content type: text/plain`, clean refuse |
| paginated 150 | ✅ | all 150 |
| slow 15s | ✅ | times out on init path (30s budget) |
| connection refused | ✅ | clean error + unhealthy recorded |
| cold remote | ✅ | no dial until first use |
| session-id required | ✅ | Mcp-Session-Id captured + echoed |
| session-expire → re-init | ✅ | 404 → clear → re-initialize once → success |
| legacy SSE auto-fallback | ✅ | ErrLegacySSEServer → SSE dial → list+call |
| legacy SSE explicit | ✅ | `transport: sse` → list |

## Redirect unit battery — 3/3 ✅

| Case | Result | Notes |
|---|---|---|
| same-origin 302 keeps POST+body | ✅ | rebuilt in CheckRedirect |
| cross-origin strips all but protocol hdrs | ✅ | Authorization/X-Tenant/X-Custom all dropped; Accept/Content-Type/Mcp-Session-Id/MCP-Protocol-Version survive |
| redirect loop cap | ✅ | 4 redirects followed (5 requests; 5th errors) |

## OAuth battery — 3/3 ✅ 2026-10-28

Browser: full headless flow vs fake IdP (DCR + PKCE + auto-approve authorize +
code exchange + refresh-capable tokens):
401 → AuthNeededError + `auth_required` health (no error) → `Login` →
DCR → authorize URL → fake-browser callback → exchange → 0600 token file →
authed list + call. Includes lowercase-`bearer` normalization on refresh.

Device (RFC 8628, ported from hermes `mcp_oauth_device.py`): zoo IdP serves
`device_authorization_endpoint`; test drives discover → DCR → device code →
poll (pending → slow_down → approve) → tokens → authed call. No browser, no
callback socket. `flow = "device"`, timeout knob.

Token: pre-seeded `Authorization` verified with one probe (ping, else
tools/list); good → ok, bad → clear failure. No dance, no handler.
`flow = "token"`.

## TLS battery — 5/5 ✅ 2026-10-28

| Case | Result | Notes |
| self-signed fails by default | ✅ | `certificate signed by unknown authority` |
| custom CA succeeds | ✅ | `tls.ca_file` |
| insecure succeeds | ✅ | `tls.insecure` |
| mTLS without cert fails | ✅ | `tls: certificate required` |
| mTLS with cert succeeds | ✅ | `tls.cert_file/key_file` |

## Header secrets + redaction — unit ✅

`${VAR}`/`{file:...}` expansion at load; remote fail-closed on unset;
0600 token writes atomic (temp+rename); log/error redaction (recursive,
case-insensitive, 8 KiB bound).

## Known gaps (remaining)

1. bad-json mid-stream hangs to timeout instead of failing fast. Acceptable.
2. bad-schema kills the WHOLE list — mcp-go has no raw path to salvage good
   tools. Clear error surfaces. Wontfix unless a real server hits it.
3. dup tool names pass through silently — should log a warning. Cheap TODO.
4. No 429/5xx retry/backoff (neither donor has it either). TODO if needed.
5. No WebSocket transport (lazy-mcp explicitly errors too). Nobody needs it.
6. Cross-process OAuth refresh not serialized (lazy-mcp has same gap).
   Single daemon per dir makes this rare. TODO if multi-daemon sharing.

## Client-side (shim) compat — manual

| Client | Entry | Status |
|---|---|---|
| hermes | `command: oxyp shim` | untested — needs `mcp_servers` entry on a hermes box |
| omp | `mcpServers: {type: stdio}` | untested |
| opencode | `mcp.<name> type: local` | untested |

Shim speaks `initialize/2024-11-05 + tools/list + tools/call + ping`,
static 5 tools (`list_servers`, `list_commands`, `describe_commands`,
`invoke_command`, `search_tools`), stdout pure JSONL — matches every
client's strictest requirements. Live-client test = next quest
(see `V-ZOO-005` in [verification/strategy](verification/strategy.md) —
currently missing).
