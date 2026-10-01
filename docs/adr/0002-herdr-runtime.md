# 0002. Herdr as the agent runtime

## Context

Herder needs terminal sessions, agent detection, and human attachment.
Herdr already owns exactly that layer and exposes a CLI plus a socket
API (SPEC sections 4, 17-18).

## Decision

Herder orchestrates Herdr, never forks it. The runtime is the CLI
surface — `herdr workspace create`, `agent start`, `agent get`,
`agent prompt`, `agent read`, `agent rename --clear`, `pane
process-info`, `pane close`, `status server`, `machine add|list|remove` —
because it is debuggable and needs no protocol client. There is no
runtime-mode selector or second socket driver; supported operations use
the native public CLI.

Each worker is one Herdr agent session on the worker's own
container-local server (issue #19): the container runs `herdr server`
as PID 1, registered as a saved SSH machine labelled by task id, and
every command is forwarded with `herdr --machine <task-id>` — real
in-container process detection, no comm spoofing. The durable link is
task ↔ sandbox ↔ machine ↔ session. Human attach is
`herdr --remote <container>` — the full remote UI of the worker's
server (`agent attach` is the one agent command `--machine` does not
forward).

### Native `agent start` (Herdr ≥ 0.9.1, verified live on 0.9.1)

Launch is a single forwarded call, not `pane run` plus client-side
polling: `agent start <name> --kind <kind> --pane <id>` runs the kind's
canonical executable, detects the real process in the same pane, binds
the name, and returns only once the agent is ready for interactive
input (default 30 s, `--timeout` up to 300000 ms). Verified against
installed herdr 0.9.1:

- Success returns only at readiness; a startup `blocked` screen returns
  `agent_not_ready` immediately while the agent stays live and named —
  Herder keeps that pane so a human can unblock it.
- `agent_pane_busy` means the pane is not an available shell; Herder
  retries it briefly because a freshly created pane can still be
  initializing.
- `agent_name_taken` reports the holder; Herder clears a confirmed-dead
  record (`agent rename <name> --clear`) once and retries.
- Detection clears the agent record and name on exit — `agent get`
  returns `agent_not_found` — but lags a `kill -9` by roughly a second.
  In that window `agent prompt` could type into a shell, so Herder still
  gates prompts and liveness on `pane process-info` (foreground process
  group ≠ shell pid).

Sources:

- [Herdr agent automation (v0.9.3)](https://raw.githubusercontent.com/herdrdev/herdr/v0.9.3/docs/next/website/src/content/docs/agent-automation.mdx).
- [Herdr CLI reference (v0.9.3)](https://raw.githubusercontent.com/herdrdev/herdr/v0.9.3/docs/next/website/src/content/docs/cli-reference.mdx).
- Installed `herdr agent start --help` and isolated-session lifecycle probes
  on 0.9.1; a real Docker/SSH pipeline with client 0.9.1 and worker 0.9.3
  exercised startup, native state reporting, prompts, retry, handoff, and
  daemon-crash recovery without duplicate agents or workspaces.

## Consequences

- Herdr owns each supported kind's executable and detection; Herder's
  config list is launch policy (`codex`, `claude`, `opencode`, `gemini`),
  not a second executable registry.
- The host Herdr socket is controller-only; workers use their own
  container-local servers, never the host socket (see ADR-0005).
- The session name is bound by `agent start`, so launch failures split
  into "pane leaked nothing" (pre-start and start failures) and "named
  agent live but not prompted" (blocked start, prompt failure).
