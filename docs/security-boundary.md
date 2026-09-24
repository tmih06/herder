# Security Boundary

Herder treats the coding agent as potentially compromised — even when the
model provider is trusted, because prompt injection can arrive through
repository content. Authority is therefore enforced structurally, never
through prompts. SPEC.md sections 28-32 and 65-66 are authoritative for
rationale; this file records what v0.1 actually enforces.

## Trust hierarchy

```text
Most trusted
────────────────────
Herder controller
Herdr controller session
Sandbox provider
Worker sandbox
Coding agent
Repository content
Issue/comment content
External network content
────────────────────
Least trusted
```

## Credential boundary

The control plane owns infrastructure credentials. A worker never
receives:

- GitHub App private key / `GH_TOKEN`
- Herdr socket (`HERDR_SOCKET_PATH`)
- Docker socket
- Herder API admin token / the Herder database
- Host SSH keys, host home directory, cloud credentials

The agent may commit locally inside its sandbox, but every remote write
runs on the controller host under the controller's GitHub identity
(`internal/deliver`): the branch is fetched into a controller-owned bare
staging repo, the fetched SHA is verified against the validated head, and
only that SHA is pushed — authenticating through `gh auth
git-credential`, so no secret enters argv, the environment, or the
sandbox. Pushing from the agent-writable `.git` would execute
agent-controlled hooks and honor agent config; the staging repo exists
precisely to prevent that.

## No raw Herdr socket

The Herdr API can manipulate workspaces, panes, agents, and the running
server. A worker with `HERDR_SOCKET_PATH` could escape its task boundary
and interfere with other sessions — so workers never get it. Herder
reaches Herdr only through its public CLI/socket API, and the worker
container is created without the socket mount.

## Worker-local Herdr server (issue #19, enforced)

Each worker container runs its own `herdr server` as PID 1, reached over
SSH — never over a published port or a mounted socket. The transport is
`ProxyCommand docker exec -i <container> /usr/sbin/sshd -i`: sshd runs in
inetd mode inside the container, so the only path in is the Docker exec
channel the controller already owns.

- **Authentication**: one controller-owned ed25519 keypair under
  `<statedir>/ssh/`; only the public key enters the sandbox as
  `authorized_keys` for the `worker` login. Password auth, PAM, and root
  login are disabled on the injected sshd command line.
- **Host-key verification**: each task gets a dedicated `known_hosts`
  plus a per-task sshd host key generated on the controller and injected
  at provision — `StrictHostKeyChecking accept-new` against a file that
  dies with the task, never the user's global known_hosts.
- **Scope**: the container-local server governs only that worker's
  workspaces/panes/agents. It holds no credentials, no host socket, and
  no path to other machines — the machine profile is a one-way control
  channel, not a trust grant.
- **Config blocks**: per-task `Host <container>` stanzas live in
  `<statedir>/ssh/config.d/`, included from `~/.ssh/config` by one
  managed Include line; teardown removes the block, known_hosts, and
  host key with the machine profile.

## Worker sandbox enforcement (v0.1, enforced)

`internal/sandbox/docker.go` `create()` builds every worker container
with:

- `--cap-drop ALL` — all Linux capabilities dropped
- `--security-opt no-new-privileges:true`
- `--user <uid>:<gid>` matching the workspace owner (cap-drop removes even
  root's DAC override, so the worker must own its files)
- `--cpus`, `--memory`, `--pids-limit` resource caps
- `--network bridge` — no host network namespace; no published ports
  (SSH reaches the container through `docker exec`, not TCP)
- Exactly one mount: the task workspace at `/workspace` — no host PID
  namespace, no Docker socket, no Herdr socket, no host home
- `HERDER_TASK=<task-id>` env + `herder-managed` labels for attribution

Host-side git calls on the agent-writable workspace neutralize
repo-controlled config (`GitArgs`: hooks, fsmonitor disabled).

## Aspirational (not yet enforced in v0.1)

- **Worker capability tokens** (SPEC 30): task-scoped, expiring,
  revocable credentials for a narrow Herder Worker API (`task.get`,
  `task.complete`, `artifact.publish`, …). v0.1 workers get no Herder API
  at all — the boundary is the absence of access, not a token.
- **Network policy** (SPEC 31): `none`/`restricted`/`unrestricted` modes
  with an allow-list. v0.1 uses plain bridge networking.
- **Credentials proxy** for controlled network operations without
  exposing secrets.
- **Protected-file enforcement beyond validation**: `forbidden_changes`
  is checked at the gate, not continuously.

## Prompt-injection threat model

Everything entering from an external system is untrusted input: issue
descriptions and comments, repository files, README instructions, source
comments, web pages, generated files, package metadata, PR review
comments. Injected content may try to make the agent read secrets, upload
credentials, disable tests, modify workflows, escape the sandbox, control
other agents, or change task policy.

The structural answers in v0.1: no credentials in the worker, no control
sockets in the worker, a validation gate that checks forbidden paths and
a clean tree before anything ships, and a delivery path that pushes only
the SHA the gate verified.
