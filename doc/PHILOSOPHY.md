# oxyp stances

Opinionated lines we hold so the design stays small. Short versions live in
`README.md#stances`; this is the canonical text.

## 1. Secret procurement is the harness's job

oxyp expands `${VAR}` and `{file:/abs/path}` at load and redacts secret
values at selected boundaries — upstream stderr log lines, 401/502 error
bodies, `/health` errors, probe-hint URLs (recursive key match,
case-insensitive, 8 KiB truncation). That is the whole secret story.
(Known limits, pinned as current behavior: `/servers` health errors and
`doctor` details pass through raw; `cookie`/`id_token`/`private_key` keys
and secret substrings inside JSON string values are not scrubbed. See
[spec-guard](spec-guard.md).)

oxyp will NEVER run secret commands (`!cmd`), vault lookups, or CLIs to
fetch secrets. Procurement is execution — spawning helpers, touching the
network, caching credentials — and execution belongs on the harness side
of the pipe. A pipe does not reach out; it carries what it is given.

If your harness wants Vault/1Password/AWS: resolve to env vars or files
before launch, then hand oxyp a config that only references names. Unset
vars fail remote URL/header entries closed (local `env` keeps them
literally; malformed `${X:-d}` passes literally). Daemon-held OAuth
tokens (`~/.oxyp/oauth/`, 0600) are the one exception — they are runtime
state the daemon earned itself, not third-party secrets it fetched.

## 2. Portability is the feature: one binary + config = agent anywhere

`oxyp` is a single static binary. Its config is one TOML file with
`${VARS}` placeholders. Drop both on any box — laptop, container, fresh
VM, a server over ssh — set env, run. No keychain, no machine-bound
state, no install dance, no account migration. Your agent is free to go
wherever the harness runs.

This constrains everything else: nothing may assume macOS Keychain,
Windows Credential Manager, a login session, or a GUI. Secrets arrive as
env/file (stance 1). OAuth dance degrades to device flow or SSH-forwarded
callback on headless boxes. Cache (`cache.json`) is disposable — delete
it and the daemon rebuilds. If a feature needs machine-bound state, the
feature is wrong.

## 3. No approval prompts in the daemon

The daemon has no UI to ask through and wants none. There is no
allowlist-with-confirm, no "approve this call?" round-trip, no pending
queue. Config `deny` (tool hidden: invisible in list/search, blocked on
call) plus `enabled = false` (server hidden) is the entire permission
story.

If a harness wants ask-gates, it builds them on its side of the pipe —
it owns the user, the chat surface, the policy engine. oxyp stays a pipe:
deny-means-invisible, fail closed, no callbacks upstream. Visible-but-
gated (list the tool, then beg approval at call time) is explicitly out
of scope: it leaks capability metadata to the model while pretending to
be safe.
