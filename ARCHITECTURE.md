# Architecture

Herder is the control plane; Herdr is the runtime. One self-contained
binary (`cmd/herder`) runs the daemon, the CLI verbs, and the local
status view over a single SQLite file.

## Ownership boundary

Herder owns orchestration: tasks, issue integration, scheduling,
sandboxes, policy, credentials, validation, delivery, durable state.
Herdr owns the runtime: terminal sessions, agent detection/lifecycle,
panes, prompting, human attachment. Herder reaches Herdr only through
its public CLI/socket API — never a fork — and workers never see the
raw socket.

## High-level diagram

```text
                       ┌─────────────────────┐
                       │ GitHub / Linear /   │
                       │ GitLab / Jira / ... │
                       └──────────┬──────────┘
                                  │
                         webhook / polling
                                  │
                                  ▼
┌──────────────────────────────────────────────────────┐
│                      HERDER                          │
│                                                      │
│   Integration Providers                              │
│          │                                           │
│          ▼                                           │
│      Policy Engine                                   │
│          │                                           │
│          ▼                                           │
│      Task Manager                                    │
│          │                                           │
│          ▼                                           │
│       Scheduler                                      │
│          │                                           │
│   ┌──────┴───────────────────────────────┐           │
│   │                                      │           │
│   ▼                                      ▼           │
│ Sandbox Manager                     Agent Router      │
│   │                                      │           │
│   ▼                                      ▼           │
│ Docker / microVM                   Agent Profile      │
│                                          │           │
│                                          ▼           │
│                                     Herdr Driver      │
│                                          │           │
│                                          ▼           │
│                                   Event Subscriber    │
│                                          │           │
│                                          ▼           │
│                                Validator / Reviewer   │
│                                          │           │
│                                          ▼           │
│                                    Delivery Engine    │
│                                                      │
│                            Event/State Store         │
└──────────────────────────┬───────────────────────────┘
                           │
                           │ local Herdr API
                           ▼
                        HERDR
                           │
              ┌────────────┼────────────┐
              ▼            ▼            ▼
            pane A       pane B       pane C
              │            │            │
              ▼            ▼            ▼
          sandbox A    sandbox B    sandbox C
              │            │            │
            Codex        Claude       OpenCode
```

## Package map

| Package               | Role                                                                                          |
| --------------------- | --------------------------------------------------------------------------------------------- |
| `cmd/herder`          | Single binary: subcommand dispatch, exit-code discipline, `printUsage` surface.               |
| `internal/ingest`     | Source adapters (GitHub, Linear, API) → normalized trigger events → exactly one policy-approved task; dedup by delivery id and source ref. |
| `internal/tasks`      | Task state machine + structured events; pure transition table, no I/O.                        |
| `internal/scheduler`  | Dispatch loop: caps, expiring leases, restart reconciliation, time/resource limits.           |
| `internal/sandbox`    | Worker isolation boundary; `Provider` interface + least-privilege `DockerProvider`.           |
| `internal/agent`      | Herdr launcher + supervision loop; seeds prompts, normalizes agent states.                    |
| `internal/validation` | The gate between "agent says done" and "work may ship": commands + forbidden-path/clean-tree. |
| `internal/deliver`    | Controller-side delivery: staging-repo push, PR, issue comment, stage labels.                 |
| `internal/storage`    | SQLite store: tasks, `task_events`, `webhook_deliveries`, `leases`; one transaction per mutation. |
| `internal/api`        | Local HTTP surface: status view, `/v1/tasks`, `/v1/deliveries`, `/v1/webhooks/{provider}`, `/v1/health`. |
| `internal/dispatch`   | Shared launch pipeline behind `task start|retry|handoff`, `sandbox provision`, scheduler.     |
| `internal/health`     | `doctor` probes: controller, storage, Herdr, Docker reported distinctly.                      |
| `internal/config`     | Strict YAML load, `${ENV}` expansion, field-level validation.                                 |
| `internal/textutil`   | Shared string helpers (truncate, first-line).                                                 |
| `internal/testutil`   | Test scaffolding: scripted `Runner` recorder, temp-dir store.                                 |

## Task state machine

Primary chain (`internal/tasks/state.go`):

```text
DISCOVERED → ELIGIBLE → CLAIMED → QUEUED → PROVISIONING → RUNNING
         → VALIDATING → REVIEWING → DELIVERING → PR_OPEN → DONE
```

Alternative states model failure, intervention, and retry without
skipping the audit trail: `BLOCKED`, `WAITING_FOR_HUMAN`, `PAUSED`,
`RETRYING`, `TIMED_OUT`, `FAILED`, `CANCELLED`. `DONE` and `CANCELLED`
are terminal; `FAILED`/`TIMED_OUT` may only retry or cancel. Illegal
jumps are rejected with an error naming both states and the legal
targets.

Normalized agent states (Herdr-reported, recorded on the task):
`starting`, `working`, `idle`, `blocked`, `done`, `unknown`.

## Event log

Every accepted transition and every meaningful observation appends a
row to `task_events`: `event_type`, `actor_type`, `actor_id`,
`payload_json`, `created_at`. The terminal is never the source of
truth — the event log is the audit trail and the recovery record.
`herder task inspect <id>` renders it.

## Crash recovery, idempotency, leasing

- **Recovery**: on restart the scheduler reconciles three sources —
  SQLite desired state, Herdr runtime state, sandbox provider state.
  `RUNNING` + live pane + live sandbox → resume supervision; pane or
  sandbox gone → `worker.disconnected` and recovery policy; a sandbox
  with uncommitted work is never auto-destroyed.
- **Idempotency**: `webhook_deliveries` dedups by delivery id and a
  `UNIQUE(source_provider, source_ref)` index guarantees one task per
  issue; delivery ops (push, PR, comment) find existing results instead
  of duplicating them.
- **Leasing**: every dispatch takes a row in `leases` (owner, heartbeat,
  expiry) before any subprocess; a dead owner's lease expires and the
  task requeues — never strands, never double-dispatches.

SPEC.md is authoritative for rationale.
