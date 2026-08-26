# Jcode provider (experimental)

Multica can execute tasks through [Jcode](https://github.com/1jehuang/jcode),
a Rust coding agent with a persistent background server. Unlike
process-per-task providers, every Jcode task runs as one **session** on one
shared, Multica-scoped `jcode serve` daemon: the per-task process Multica
spawns (`jcode acp`) is a thin adapter, sessions survive Multica daemon
restarts, and cancelling one task interrupts only its own session.

The transport decision and the probe evidence behind this integration are in
[docs/adr/ADR-JCODE-TRANSPORT.md](adr/ADR-JCODE-TRANSPORT.md).

## Prerequisites

- macOS or Linux (Windows is out of scope for the prototype).
- Jcode **v0.80.0 or newer** on the machine that runs the Multica daemon.
- A Jcode provider/model configured and authenticated for the daemon's OS
  user (`jcode login`, or an existing `~/.jcode` setup). Multica reuses the
  user's Jcode auth and configuration; it never writes to them.

## Install and authenticate Jcode

```bash
brew install 1jehuang/jcode/jcode   # or the installer from the Jcode docs
jcode login
jcode --version
```

Verify a provider works end to end once (`jcode run "say hi"`) before wiring
it into Multica — task failures caused by missing provider credentials are
much easier to diagnose in Jcode's own CLI.

## Enable the feature flag

The provider is gated off by default. Start the Multica daemon with:

```bash
MULTICA_EXPERIMENTAL_JCODE=1 multica daemon start
```

While the flag is unset, Jcode is not detected, is never registered as a
runtime, and Multica never starts `jcode serve` — an installed jcode binary
has zero effect on existing providers.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `MULTICA_EXPERIMENTAL_JCODE` | unset (off) | Feature flag; the provider exists only when set |
| `MULTICA_JCODE_PATH` | `jcode` from `PATH` | Jcode executable |
| `MULTICA_JCODE_MODEL` | empty | Daemon-wide default model id (from `jcode`'s model catalog, e.g. `claude-fable-5`) |
| `MULTICA_JCODE_RUNTIME_DIR` | `~/.multica/jcode` | Runtime dir of the Multica-scoped Jcode server (its single-instance lock lives here) |
| `MULTICA_JCODE_SOCKET` | `<runtime-dir>/jcode.sock` | Unix socket of that server |
| `MULTICA_JCODE_AUTOSTART` | `true` | Start the server on demand when the socket is dead |
| `MULTICA_JCODE_MAX_CONCURRENT` | `4` | Maximum simultaneously executing Jcode tasks per server socket |
| `MULTICA_JCODE_INTERRUPT_TIMEOUT` | `10s` | How long a cancelled task waits for the session to acknowledge the interrupt |

Socket paths must be absolute and short — Unix sockets are limited to ~104
bytes on macOS. Multica validates this before launching anything and the
error names the variable to fix.

Per-agent **custom args** are appended to `jcode acp …` and may use Jcode's
own selection flags (for example `-p openai` or `--provider-profile work`).
Arguments that would break the transport or the server pinning (`serve`,
`login`, `--socket`, `--resume`, `--remote-working-dir`, …) are filtered out
and logged.

## Managed versus external server

**Managed (default).** With autostart enabled, the first task (or model
discovery) starts `jcode serve` on the Multica-scoped socket under Jcode's
own spawn lock; concurrent starts cannot double-spawn, and a healthy existing
server is always reused. The server keeps running between tasks and across
Multica daemon restarts — that is what makes session reattachment possible —
so Multica deliberately does not stop it on daemon shutdown. Stop it
yourself with:

```bash
JCODE_RUNTIME_DIR=~/.multica/jcode JCODE_SOCKET=~/.multica/jcode/jcode.sock jcode server stop --force
```

**External.** Set `MULTICA_JCODE_AUTOSTART=0` and point
`MULTICA_JCODE_SOCKET` (and `MULTICA_JCODE_RUNTIME_DIR`) at a server you
manage. When the socket is unreachable, tasks fail fast with a clear error
instead of starting a server; Multica never terminates an external server.

The Multica-scoped server is intentionally separate from any interactive
`jcode` sessions the same user runs: Jcode holds a single-instance lock per
runtime dir, and Multica pins its own (`JCODE_RUNTIME_DIR`/`JCODE_SOCKET` are
injected into every task process, overriding inherited environment).

## Detection and registration

With the flag set and the binary on `PATH` (or `MULTICA_JCODE_PATH`), the
daemon probes `jcode`, enforces the minimum version (0.80.0), and registers
the `jcode` runtime with the workspace like any other provider. Check:

```bash
multica daemon probe-runtimes
```

The runtime appears as **Jcode** in the workspace's runtime list.

## Creating a Jcode-backed agent

Create an agent as usual and pick **Jcode** as its runtime. Model and
reasoning-effort pickers are populated from Jcode's live catalog (the same
list `/model` shows in the Jcode TUI); leaving the model empty uses the
Jcode configuration's default. The MCP tab is hidden for Jcode agents —
session-scoped MCP is not yet supported by `jcode acp`, so configure MCP
servers in Jcode's own configuration (`~/.jcode/mcp.json`) instead.

Task context (instructions, issue, skills) is delivered through `AGENTS.md`
in the task worktree, which Jcode loads as session bootstrap input.

## Concurrency

Each executing task holds one session slot for the configured socket
(default 4; `MULTICA_JCODE_MAX_CONCURRENT`). Additional claimed tasks wait
for a slot and respect cancellation/timeout while waiting. Each task gets
its own worktree and its own Jcode session; sessions never interleave.

## Logs and health

- Task-level progress (text, thinking, tool calls, usage) streams into the
  Multica task as with any provider; provider-level errors from Jcode's
  stderr are surfaced in the task result.
- The daemon log carries `[jcode:stderr]` lines plus structured entries with
  the session id and socket for every launch, interrupt, and finish.
- Server-side state: `jcode server` / `~/.jcode/servers.json` list every
  registered server (name, socket, pid, sessions), including the
  Multica-scoped one.

## Cancellation behavior

Cancelling a Multica task sends `session/cancel` for that session only and
waits up to `MULTICA_JCODE_INTERRUPT_TIMEOUT` for the turn to acknowledge
(`stopReason: "cancelled"`). The shared server and all other sessions are
unaffected. If the acknowledgment never arrives, the task still ends as
cancelled, the session id is logged as potentially orphaned, and the session
is left to Jcode's own lifecycle — inspect it with `jcode --resume` or the
Jcode TUI.

## Recovery and known limitations

- **Multica daemon restart:** sessions live in the Jcode server, so the
  standard Multica retry/resume path reattaches with `session/resume` and
  continues the conversation.
- **Jcode server death mid-task:** the task fails with a transport error;
  the platform retry resumes the session after the server is back
  (autostart brings it back on the next attempt).
- **Silent fresh resume:** Jcode currently accepts `session/resume` for an
  unknown session id by silently creating a fresh, empty-context session
  under that id. A resume after Jcode-side session loss therefore continues
  without history and without a warning — Multica cannot detect this case
  (tracked as an upstream follow-up in the ADR).
- Prompts are one-at-a-time per session; token usage is reported per turn
  and attributed to the session's model.
- Model discovery creates a short-lived session on the persistent server;
  Jcode's session retention prunes it.

## Troubleshooting

- **Provider missing:** flag unset, binary not found, or version below
  0.80.0 — `multica daemon probe-runtimes` shows what was detected;
  `jcode --version` shows the version.
- **`protocolVersion` mismatch / initialize failure:** the installed jcode
  is too old or too new for the pinned ACP contract (protocol version 1).
  Upgrade jcode (`jcode update`), or pin `MULTICA_JCODE_PATH` to a
  compatible build. The task error carries Jcode's own message.
- **"socket path … SUN_LEN" or socket-length error:** point
  `MULTICA_JCODE_SOCKET`/`MULTICA_JCODE_RUNTIME_DIR` at a shorter path.
- **"Another jcode server process is already running for runtime dir":**
  two servers were asked to share one runtime dir. Give Multica its own
  `MULTICA_JCODE_RUNTIME_DIR` (the default already is separate).
- **Model errors on task start:** the chosen model is not usable with the
  Jcode provider configuration on this machine — verify with `jcode run`
  or the TUI's `/model`.

## Disabling and uninstalling

Unset `MULTICA_EXPERIMENTAL_JCODE` and restart the daemon: the provider
disappears and nothing Jcode-related runs. To remove the Multica-scoped
server and its state:

```bash
JCODE_RUNTIME_DIR=~/.multica/jcode JCODE_SOCKET=~/.multica/jcode/jcode.sock jcode server stop --force
rm -rf ~/.multica/jcode
```

Existing Jcode-backed agents keep their configuration but cannot run until
the flag is re-enabled.
