# ADR: Jcode transport for the Multica `jcode` provider

- **Status:** Accepted
- **Date:** 2026-08-26
- **Deciders:** Jcode provider workstream
- **Spec:** "Jcode Native Runtime Adapter for Multica" (prototype specification)

## Context

The spec asks for a native Multica provider that executes tasks through a
persistent background Jcode server, one Jcode session per Multica task run,
without depending on an undocumented Jcode wire protocol. It requires a spike
answering a fixed question list, then a decision between three approaches:

1. **Direct client** — Multica implements a stable Jcode protocol in Go.
2. **Rust bridge** — a Jcode-native client exposing a stable JSONL protocol.
3. **Upstream API work** — add a public embedding API to Jcode first.

The spike ran against a real installation (`jcode v0.80.2-dev (03d6678b3)`,
Homebrew, macOS) and the upstream source tree (`1jehuang/jcode` @ 57beea2a2).
Raw JSONL transcripts of every probe exchange were captured during the spike.

## Spike findings

Jcode already ships the bridge the spec describes. `jcode acp` ("Run as an
Agent Client Protocol (ACP) adapter backed by the Jcode daemon") is a thin
stdio shim: each ACP session it serves is a connection to the persistent
`jcode serve` daemon, created with `Request::Subscribe { working_dir, … }`.
The session — transcript, provider state, running turn — lives in the daemon,
not the shim.

Answers to the spike question list, each verified live unless noted:

| Question | Answer |
|---|---|
| Public, versioned client library? | Yes, two: ACP v1 via `jcode acp`, and a Unix-socket harness API via `jcode api-bridge` (consumed by `@1jehuang/jcode-sdk`) |
| Unix-socket protocol documented for external clients? | The daemon's own socket protocol (`Request::Subscribe` …) is internal. ACP and the api-bridge are the supported external surfaces |
| Create a session noninteractively? | Yes — `session/new` answered in <1 s (excluding first server spawn) |
| Set working directory? | Yes — `session/new`/`session/load` require an absolute `cwd`; a tool-using prompt created `probe.txt` in exactly that directory |
| Select model/provider? | Yes — `session/set_model {sessionId, modelId}` verified (`claude-fable-5` applied and echoed); `-p/--provider` selects the provider family at spawn |
| Submit a prompt? | Yes — `session/prompt` with `[{type:"text",…}]` |
| Structured event stream? | Yes — `session/update` notifications: `agent_message_chunk`, `tool_call`, `tool_call_update`, `usage_update`, `config_option_update`, `available_commands_update`, plus `agent_thought_chunk` per source |
| Interrupt one session? | Yes — `session/cancel` forwarded as a daemon `Request::Cancel`; a running prompt returned `{stopReason:"cancelled"}` in ~60 ms; other sessions unaffected |
| Query/reattach an existing session? | Yes — `session/load` (replays history as notifications) and `session/resume` (no replay). Verified across shim kill −9 and across daemon restart |
| Stable session ID before execution? | Yes — `session/new` returns `sessionId` before any prompt |
| Final status, output, usage? | Yes — prompt result carries `stopReason` and `usage {inputTokens, outputTokens, cachedReadTokens, cachedWriteTokens, totalTokens}`; text arrives as chunks |
| Concurrent sessions on one server? | Yes — sessions are independent daemon connections; jcode's own swarm mode runs many per server |
| Keepalives vs semantic events? | The ACP surface emits no transport keepalives; every `session/update` is semantic. Multica's inactivity classification treats empty text deltas as non-semantic anyway |

Findings that shaped the design:

- **Server identity is the runtime dir, not just the socket.** The daemon
  holds a single-instance lock per `JCODE_RUNTIME_DIR`; pointing
  `JCODE_SOCKET` somewhere new while sharing the runtime dir fails with
  "Another jcode server process is already running". Isolation therefore
  needs both variables.
- **macOS `SUN_LEN`:** socket paths past ~104 bytes fail with
  "path must be shorter than SUN_LEN". The backend validates this up front.
- **Autostart is built in and race-safe.** `jcode acp` spawns the server
  itself when the socket is dead, under a `<socket>.spawning` lock, and
  reuses a healthy listener otherwise — exactly the spec's "must not start a
  second server" rule, implemented upstream.
- **Resume of a missing session is silent.** `session/resume` with an unknown
  ID returned `{}` and the following prompt ran in a fresh, empty-context
  session under the requested ID. There is no detectable rejection, so the
  provider joins `resumeRejectionUndetectable` (same class as
  copilot/cursor/opencode).
- **Session-scoped MCP is not implemented.** `mcpServers` on
  `session/new`/`session/load` is validated as an array and then ignored
  (upstream test: "non_empty_mcp_servers_are_tolerated_until_session_scoped_
  mcp_is_supported"). MCP for jcode is operator-side (`~/.jcode/mcp.json`),
  so the provider reports MCP as unsupported instead of silently dropping it.
- **Tool calls run unattended.** `jcode acp` never issues
  `session/request_permission`; tool policy belongs to the daemon/config.
  A file-writing prompt completed with no approval round-trip.
- **One prompt per session at a time** — a second `session/prompt` while one
  is running is refused (-32000), matching the spec's prototype scope.
- **`jcode server stop` refuses without `--force`** while sessions may be
  live, and warns that stopping drops headless sessions.

## Decision

**Direct client in Go over ACP, through the upstream `jcode acp` shim, one
shim process per task run, all shims backed by one persistent `jcode serve`
on a Multica-scoped socket.**

- The Go side reuses Multica's shared ACP transport (`hermesClient`), the
  deliverable tracker, MCP capability filtering, and effort/config-option
  helpers — the same infrastructure behind the hermes, kimi, dim, grok,
  qwenpaw, mcode and zeroclaw providers. jcode's ACP dialect was already
  partially proven in-tree: hermes-family custom profiles have run jcode
  binaries, and its `reasoning_effort` config option is verified as genuinely
  applied (GH #6720).
- The per-task `jcode acp` process satisfies the spec's compatibility
  boundary: Multica never speaks the daemon's private socket protocol. The
  "bridge" the spec sketches exists upstream, versioned as ACP protocol 1;
  Multica keeps its own fake-ACP-server test suite as the independent
  compatibility check the spec asks for when the bridge lives upstream.
- The persistent server is scoped to Multica by injecting
  `JCODE_RUNTIME_DIR` (default `~/.multica/jcode`) and `JCODE_SOCKET`
  (default `<runtime-dir>/jcode.sock`) into the shim environment, so task
  execution never collides with — or depends on — the user's interactive
  jcode servers. Operators can point both at an externally managed server.

Rejected alternatives:

- **Bespoke Rust bridge** — would duplicate `jcode acp` feature-for-feature
  while adding a build/distribution problem. Nothing it could expose is
  missing from ACP for the prototype scope.
- **Direct daemon-socket client in Go** — the daemon protocol is internal
  and unversioned; the spec forbids depending on it without a compatibility
  boundary.
- **`jcode api-bridge` harness API** — a credible future transport (it is the
  surface the TypeScript SDK consumes), but it would be a second parallel
  integration pattern in a codebase with ten ACP providers and shared ACP
  infrastructure. Revisit if the provider ever needs api-bridge-only
  capabilities (raw server events, swarm control).

## Consequences

- One `jcode acp` process per active task is the cost of protocol isolation.
  It is a thin shim (the daemon does the work), and killing it never kills
  the shared server or its sessions.
- Cancellation must be explicit: a running turn survives client death by
  design (sessions are detachable), so the backend sends `session/cancel`
  and waits a bounded interrupt window for `stopReason:"cancelled"` before
  tearing the shim down; an unacknowledged cancel logs the session ID as
  potentially orphaned. The shared server is never killed on task
  cancellation.
- Resume continuity cannot be guaranteed: a lost session resumes as a silent
  fresh session (see spike findings), which Multica treats as
  resume-rejection-undetectable.
- Deliberate deviations from the spec text, with reasons:
  - **No `MULTICA_JCODE_BRIDGE_PATH` / bridge deliverable** — the bridge is
    upstream (`jcode acp`).
  - **No `MULTICA_JCODE_SERVER_NAME`** — jcode names and registers servers
    itself (`~/.jcode/servers.json`), keyed by socket; the Multica scope is
    the runtime dir + socket, which the operator controls directly.
  - **The Multica daemon does not stop the jcode server on shutdown.** The
    spec's own recovery model (reattach after daemon restart) requires the
    server and its sessions to outlive the daemon. Operators stop it with
    `JCODE_RUNTIME_DIR=… JCODE_SOCKET=… jcode server stop --force`.
  - **Session persistence uses Multica's existing task-run session-ID
    storage** rather than a parallel record, as the spec itself prefers.
  - **Event model and state machine are Multica's existing
    `agent.Message`/`agent.Result` contract**, which the spec's normalized
    event list maps onto one-to-one.
  - **No new `multica_jcode_*` metric family** — task metrics already carry a
    provider label; a provider-private metric family would be a parallel
    abstraction.
- Feature flag: the provider is detected only when
  `MULTICA_EXPERIMENTAL_JCODE=1` is set, per the spec's rollout gate.

## Upstream follow-ups identified

- Session-scoped MCP configuration (upstream TODO already acknowledges it).
- A detectable "session not found" answer for `session/resume`, so a lost
  session can trigger Multica's fresh-session fallback instead of a silent
  empty-context resume.
- ACP-visible per-turn `modelId` on the prompt result (`_meta.modelId`) for
  exact usage attribution after mid-session model switches.
