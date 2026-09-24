# Herder

Herder is the control plane for autonomous coding-agent fleets: it turns
labeled issues into delivered pull requests by orchestrating sandboxes
and agents through Herdr. Single context; this glossary is the vocabulary
for issues, specs, code, and reviews.

## Language

**Task**:
One unit of work: the durable row (`tasks` table) tying a source issue to
a repository, an agent profile, a branch, a sandbox, and a Herdr session.
_Avoid_: job, work item, ticket

**Task Event**:
One append-only row in `task_events` recording something that happened to
a task: a transition, a policy decision, an agent-state change, a lease
change. Timestamped and attributable (`actor_type`/`actor_id`).
_Avoid_: log line, audit entry, history record

**Task State**:
A lifecycle state from the transition table in `internal/tasks/state.go`
(`DISCOVERED` … `DONE`, plus `BLOCKED`, `PAUSED`, `RETRYING`, …). Only
legal successors are accepted.
_Avoid_: status (except as the column name), phase, stage

**Agent State**:
The normalized Herdr-reported worker condition recorded on a task:
`starting`, `working`, `idle`, `blocked`, `done`, `unknown`.
_Avoid_: agent status, worker state, pane state

**Sandbox**:
The least-privilege container a task's work runs in, plus its workspace
checkout under `<statedir>/sandboxes/`. One per task; named from the task id.
_Avoid_: container (except as the Docker id), environment, VM

**Agent Profile**:
A named launch configuration in `agents:` (kind, timeout, resources) that
a repository selects via `agent.default`.
_Avoid_: agent config, worker template, persona

**Herdr Session**:
The live Herdr agent session bound to a running task
(`agent_session_id`) on the worker's container-local Herdr server; part
of the durable task ↔ sandbox ↔ machine ↔ session link.
_Avoid_: pane (that's Herdr's object), terminal, connection

**Machine**:
The saved herdr SSH machine profile (`machine_id`) that forwards
`herdr --machine <task-id>` calls to the worker's container-local
server. One per task; the SSH target is the container name resolved
through a per-task `Host` block under `<statedir>/ssh/config.d/`.
_Avoid_: remote, host, node

**Provider**:
A pluggable integration behind an interface: source providers (GitHub
issues) and sandbox providers (`docker` in v0.1).
_Avoid_: backend, driver (except `herdr` driver mode), adapter

**Source Ref**:
The external coordinate of a task's origin, e.g. `owner/repo#123`;
`UNIQUE(source_provider, source_ref)` guarantees one task per issue.
_Avoid_: external id, issue key, reference

**Delivery**:
The controller-side act of shipping validated work: push the branch, open
the PR, comment the issue, advance stage labels (`internal/deliver`).
_Avoid_: publish, deploy, merge

**Delivery (webhook)**:
One inbound webhook receipt, deduplicated by `delivery_id` in
`webhook_deliveries`; each lands one durable decision: accepted,
duplicate, or policy_denied.
_Avoid_: event (that's Task Event), request, message

**Validation Run**:
One pass of the configured gate inside the sandbox — commands,
forbidden-path check, clean-tree check — before delivery may proceed.
_Avoid_: test run, check, CI

**Lease**:
The expiring dispatch ownership row in `leases` (owner, heartbeat,
expires_at) that makes "start this task exactly once" survive crashes.
_Avoid_: lock, claim (except as the CLAIMED state), reservation

**Worker**:
The running combination of sandbox + container-local Herdr server +
Herdr session + coding agent executing one task.
_Avoid_: agent (that's the coding tool), executor, runner

**Agent Kind**:
The coding-agent CLI a profile launches inside the sandbox — `codex`,
`claude`, `opencode`, `gemini` — detected by the worker's own Herdr
server from the real in-container process.
