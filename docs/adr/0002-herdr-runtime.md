# 0002. Herdr as the agent runtime

## Context

Herder needs terminal sessions, agent detection, and human attachment.
Herdr already owns exactly that layer and exposes a CLI plus a socket
API (SPEC sections 4, 17-18).

## Decision

Herder orchestrates Herdr, never forks it. Stage 1 (v0.1): CLI
orchestration — `herdr agent start|send|get|read|attach|pane close` —
because it is debuggable and needs no protocol client. Stage 2 (later):
the socket API for long-lived event subscriptions. Each worker is one
Herdr agent session, bound durably as task ↔ sandbox ↔ session, with
`HERDR_AGENT=<kind>` on the host-visible wrapper so Herdr attributes
the agent inside the sandbox.

## Consequences

- Herder inherits every agent Herdr learns to detect, for free.
- The raw socket is a controller-only surface; workers never see it
  (see ADR-0005).
- `herdr.mode: socket|cli|disabled` in config selects the driver.
