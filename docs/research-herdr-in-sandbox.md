# Research: a herdr server inside each worker sandbox

Backing notes for issue #19, verified against herdr 0.9.1 (installed at
`~/.local/bin/herdr`, statically linked) and herdr.dev docs. Records what
the implementation relies on and what was confirmed live versus inferred.

## The model

Each worker container runs `herdr server` as PID 1. The controller
registers the container as a saved SSH machine (`herdr machine add
--label <task-id> <container>`) and forwards every workspace/pane/agent
command with `herdr --machine <task-id> …`. Human attach is
`herdr --remote <container>` — the full remote UI of that machine's
session (`agent attach` is not forwarded over `--machine`).

## Verified facts (herdr 0.9.1)

- `herdr machine add --label <label> <ssh-target>` prepares the remote
  server and saves a profile; `machine list --json` prints a **bare
  array** of `{id, label, target, session, enabled, selected}` — the
  profile `id` is what `machine remove` accepts (labels are not).
- `machine add` is **not idempotent**: it appends a duplicate profile
  when the label already exists. Ensure-by-list is required before add.
- `machine add` runs `ssh -F <managed-config>` which itself `Include`s
  `~/.ssh/config` — so a per-task `Host` block must live where OpenSSH
  reads it. We append one managed `Include <statedir>/ssh/config.d/*`
  line to `~/.ssh/config`; per-task files drop in and out freely.
- Forwarded `--machine` calls run plain `ssh` with
  `StrictHostKeyChecking=yes` — the host key must already be in the
  task's `UserKnownHostsFile`. `accept-new` on the Host block seeds it
  during `machine add`; a per-task known_hosts means teardown deletes
  the file outright and a recreated container never collides with a
  stale global entry.
- `herdr --remote <ssh-target>` opens the remote UI directly — the
  glass-box attach path.
- `pane process-info --pane <id>` reports
  `{"result":{"process_info":{"foreground_process_group_id":N,"shell_pid":M}}}`.
  `pgid == shell_pid` means the pane's foreground is the shell itself —
  the reliable agent-dead signal. Matching process *names* fails for
  script agents that exec another binary.
- `agent get` keeps answering for a named session after the agent
  process exits (the pane falls back to its shell), so a bare
  successful `get` is not proof of life — the process-info check is.
- `agent prompt` on a dead agent types into the pane's shell — the
  prompt would execute as shell commands. `SendPrompt` must check
  `Running` first.
- `agent rename <name> --clear` releases a session name held by a stale
  pane; `agent_name_taken` is the stderr token on collision.
- `workspace list` JSON: `{"result":{"workspaces":[{"workspace_id","label",…}]}}`;
  a remote server restart restores panes as shells with no agent
  record, so relaunch closes same-label workspaces first.
- `workspace create --label X --cwd /workspace --env HERDR_AGENT=<kind>
  --no-focus` returns `{"result":{"root_pane":{"pane_id"},"workspace":{"workspace_id"}}}`.
- `pane run` joins argv with raw spaces and the pane shell re-parses —
  the command token must be space/metachar-free (validated before run).
- Saved-machine check: the remote server must be a
  `detached_server_daemon` — a session leader. `herdr server` as
  container PID 1 satisfies this with no `--init` (tini as PID 1 would
  break it: the server must be the session leader itself).

## Container-side SSH (verified design)

- `sshd -i` (inetd mode) inside `docker exec -i` needs no published
  port and no daemon: `ProxyCommand docker exec -i <ctr> /usr/sbin/sshd
  -i -e -h <hostkey> -o UsePAM=no -o PidFile=none -o
  PasswordAuthentication=no -o PermitRootLogin=no`.
- `sshd -i` authenticates a *named* login, so the container uid needs a
  passwd entry. `docker exec -u 0` appends `worker:*:uid:gid` — uid 0
  can write root-owned files even with `--cap-drop ALL` (cap-drop
  removes capabilities; root still owns /etc/passwd, and writing a
  file you own needs no DAC override).
- Same-uid login skips privsep problems: the worker uid == the
  container's `--user` uid, so sshd never setuids across accounts.
- `docker cp` preserves source owner+mode: the controller-written
  authorized_keys (0644) and host key (0600) land owned by the worker
  uid — no chown needed (CAP_CHOWN is dropped).
- Worker home is `/tmp/herder-home`: the uid is dynamic so `/home`
  can't be pre-owned; `/tmp` is world-writable. `HOME` is passed via
  `--env` on create and `-e HOME=…` on exec probes.
- The entrypoint `sh -c 'mkdir -p "$HOME/.ssh" && exec herdr server'`
  needs only `sh` + `herdr` in the image; `exec` makes herdr PID 1 so a
  server crash exits the container (reconcile sees a dead worker).

## Open verification items (from the issue)

- `machine add` non-interactivity when the remote is already
  compatible: the add path is scripted end-to-end in tests; a live
  `docker exec`-transport add was not exercised here — the demo covers
  it (`demo/demo.sh` asserts the profile + forwarded status).
- `sshd -i` under `--cap-drop ALL` single-uid: design above; the demo
  image is the proving ground.
- Per-container herdr memory overhead for scheduler caps: unmeasured;
  `herdr server` is a single static binary (~24 MB on disk). If it
  matters, raise `agents.*.resources.memory` — the cap is config, not
  code.
- `agent wait`/event subscription latency over `--machine`: Stage 1
  polls (`agent get` on the supervisor tick); the socket subscription
  is the Stage 2 upgrade in ADR-0002.

## Deferred (explicit non-goals)

- Kubernetes provider: the transport maps 1:1 (`kubectl exec -i <pod>
  -- sshd -i`), but no provider exists yet.
- Herdr socket API (Stage 2 of ADR-0002).
- Publishing forwarded ports for user-facing dev servers (`ssh -L` or
  `docker -p` later).
