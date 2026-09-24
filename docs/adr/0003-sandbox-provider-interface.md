# 0003. Sandbox provider interface

## Context

Workers must run isolated, but the isolation runtime will evolve:
Docker today, microVMs and remote providers later (SPEC sections
23-24). Task orchestration must not change when the runtime does.

## Decision

All sandboxing goes through the `sandbox.Provider` interface
(`internal/sandbox/sandbox.go`): `Create/Start/Exec/Attach/Stop/
Destroy/Snapshot/Inspect` over a `Spec`. v0.1 ships one provider,
`DockerProvider`, which shells to the `docker`/`git` CLIs behind the
`Runner` seam — no SDK dependency, tests script subprocesses.

## Consequences

- Config names the provider (`sandbox.provider: docker`); unknown
  providers fail validation at load.
- Least-privilege defaults live in the provider, not the orchestrator:
  cap-drop ALL, no-new-privileges, resource caps, one workspace mount.
- New runtimes (microsandbox, microVM, remote) implement the same
  interface without touching tasks, scheduler, or delivery.
