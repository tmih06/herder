# 0002. Herdr as the agent runtime

## Context

Herder needs terminal sessions, agent detection, and human attachment.
Herdr already owns exactly that layer and exposes a CLI plus a socket
API (SPEC sections 4, 17-18).

## Decision

Herder orchestrates Herdr, never forks it. Stage 1 (v0.1): CLI
orchestration — `herdr workspace create|pane run|agent get|rename|
prompt|read|pane close|pane process-info|machine add|list|remove` —
because it is debuggable and needs no protocol client. Stage 2 (later):
the socket API for long-lived event subscriptions. Each worker is one
Herdr agent session on the worker's own container-local server (issue
#19): the container runs `herdr server` as PID 1, registered as a saved
SSH machine labelled by task id, and every command is forwarded with
`herdr --machine <task-id>` — real in-container process detection, no
comm spoofing. The durable link is task ↔ sandbox ↔ machine ↔ session.
Human attach is `herdr --remote <container>` — the full remote UI of
the worker's server.

## Consequences

- Herder inherits every agent Herdr learns to detect, for free.
- The raw socket is a controller-only surface; workers never see it
  (see ADR-0005).
- `herdr.mode: socket|cli|disabled` in config selects the driver.
