# 0005. Worker capability boundary

## Context

The coding agent is treated as potentially compromised: prompt
injection can arrive through repository content, so authority must be
structural, not prompt-level (SPEC sections 28-30).

## Decision

Workers get no control surfaces at all in v0.1. The controller holds
every credential (GitHub App key, `gh` auth, Herdr socket, Docker
socket, the database); the worker container is created with
`--cap-drop ALL`, `no-new-privileges`, resource caps, and exactly one
mount — the task workspace. No `HERDR_SOCKET_PATH`, no Docker socket,
no tokens. Privileged delivery (push, PR, issue writes) runs
controller-side through a staging repo that never executes
agent-controlled git config.

## Consequences

- A compromised worker can only affect its own workspace checkout.
- The planned narrow Worker API (task-scoped capability tokens:
  `task.get`, `task.complete`, `artifact.publish`, …) is an addition,
  not a relaxation — the raw Herdr socket stays controller-only
  forever.
- Enforced-vs-aspirational detail: docs/security-boundary.md.
