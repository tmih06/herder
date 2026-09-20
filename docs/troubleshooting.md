# Troubleshooting

## Start with `herder doctor`

```bash
herder doctor   # alias: herder health
```

Four sections, each reported distinctly:

| Section      | Meaning                                                        | Failure is |
| ------------ | -------------------------------------------------------------- | ---------- |
| `controller` | Config loaded and validated                                    | fatal      |
| `storage`    | SQLite file opens and migrates                                 | fatal      |
| `herdr`      | `herdr` binary in PATH and its server answering                | warning    |
| `docker`     | `docker` binary in PATH and the daemon answering `docker info` | warning    |

Herdr/Docker warnings are setup hints, not fatal: task state still works
without them, but provisioning and agent launch will fail until fixed.

## Where state lives

- Database: `~/.local/state/herder/herder.db` (or `database.path` in your
  config). Tasks, events, webhook deliveries, and leases all live here
  and survive a daemon kill.
- Workspaces: `<statedir>/sandboxes/<task>/` — one checkout per task.

## Reading a task's history

```bash
herder task inspect <id>   # task row + full event history
herder task list           # every task with its normalized agent state
herder ingest log          # recorded webhook deliveries and decisions
```

Every accepted transition, policy decision, agent-state change, lease
event, and failure lands as a structured event — the event log answers
"what happened" before you touch logs.

## Common failures

| Symptom | Cause | Fix |
| ------- | ----- | --- |
| `agent: workspace create …` fails, task FAILED with `agent.start_failed` | Herdr server not running (or `herdr` missing) | Start Herdr (`herdr`), re-check `herder doctor`, then `herder task retry <id>` |
| `agent prompt` retries then fails (`agent_not_ready`) | Herdr pane exists but the agent inside hasn't come up | Check the pane via `herder task attach <id>` / `task logs`; retry once the agent binary is installed in the sandbox image |
| `docker found but daemon not answering` / `permission denied … docker.sock` | dockerd down, or your user lacks socket access | Start dockerd; add user to the `docker` group or fix socket permissions |
| Provision fails with a dirty-workspace error (`DirtyError`) | Re-provisioning would discard uncommitted work | Inspect `<statedir>/sandboxes/<task>/`, commit or clean by hand, then re-provision — Herder refuses rather than destroys |
| `ingest` prints `duplicate` | Same `delivery_id` or same issue already claimed | Expected no-op; the surviving task id is printed. Not a bug |
| `policy_denied` on ingest | Repo unknown/disabled, or trigger label missing | Check `repositories.<name>.enabled` and `trigger.labels` in config |
| `unknown agent kind` / `unknown agent profile` | `agent.default` or `--agent` names nothing in `agents:`, or `kind` isn't codex/claude/opencode/gemini | Fix the profile name or kind in config; `config validate` catches this at load |
| `dispatch already in progress (lease held by …)` | Another dispatcher holds the task's lease | Wait for `scheduler.lease_ttl` expiry (dead owners requeue automatically) or finish the in-flight dispatch |
| `sandbox … not ready` on `task start` | Container missing or not running | `herder sandbox provision <task-id>` first; `sandbox inspect <id>` shows status |
| `task logs`/`attach` fails | Session gone (`ErrSessionGone`) | The pane exited; `task inspect` shows `agent.exited`/`worker.disconnected` — `task retry <id>` starts a fresh attempt |

## Still stuck

- `herder task event --payload '{…}' <id> <type>` appends a custom event
  when you need to annotate a task during debugging.
- The daemon's status view (`GET /`) and `GET /v1/health` report the same
  health the CLI does.
- Escalate with the task id, the `task inspect` output, and the doctor
  report — all three are durable and copy-pasteable.
