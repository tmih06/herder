# Herder

[![Tests](https://github.com/tmih06/herder/actions/workflows/test.yml/badge.svg)](https://github.com/tmih06/herder/actions/workflows/test.yml)
[![Lint](https://github.com/tmih06/herder/actions/workflows/lint.yml/badge.svg)](https://github.com/tmih06/herder/actions/workflows/lint.yml)
[![Security](https://github.com/tmih06/herder/actions/workflows/security.yml/badge.svg)](https://github.com/tmih06/herder/actions/workflows/security.yml)
[![Smoke](https://github.com/tmih06/herder/actions/workflows/smoke.yml/badge.svg)](https://github.com/tmih06/herder/actions/workflows/smoke.yml)

Open-source control plane for autonomous coding-agent fleets, powered by Herdr.
See [SPEC.md](SPEC.md) for the full architecture.

## Install

Requires Go 1.22+.

```bash
go build -o herder ./cmd/herder
```

## Quickstart (v0.1)

```bash
# 1. Write the example config for one repository and one agent.
./herder init
# writes ~/.config/herder/config.yaml (use --config PATH to choose elsewhere)

# 2. Validate it. Bad triggers, unknown agent profiles, and unknown
#    sandbox providers fail here with a field-level message.
./herder config validate

# 3. Start the controller. It initializes local state and serves a
#    status view (default http://127.0.0.1:8787).
./herder daemon

# 4. In another shell: zero-task status view, then full diagnostics.
./herder status
./herder doctor   # controller, storage, Herdr, Docker reported distinctly
```

## Task workflow

```bash
./herder task create --repo OWNER/REPO --source-ref OWNER/REPO#123
./herder task transition <id> ELIGIBLE
./herder task event --payload '{"note":"hi"}' <id> agent.working
./herder task inspect <id>
./herder task list
```

Tasks and their event history live in SQLite (default
`~/.local/state/herder/herder.db`) and survive a daemon kill and restart.
Illegal state jumps are rejected; every accepted jump appends a structured,
timestamped, attributable event.

## Sandbox workflow

```bash
./herder sandbox provision <task-id>   # isolated container + branch checkout
./herder sandbox exec <id> -- go test ./...  # output + exit recorded on the task
./herder sandbox list
./herder sandbox inspect <id>
./herder sandbox shell <id>            # interactive shell (needs a TTY)
./herder sandbox stop|destroy <id>     # unknown ids are a logged no-op
```

Each task gets a least-privilege container (no privileged mode, dropped
capabilities, CPU/memory/process caps, bridge networking, one workspace
mount) on a deterministic `herder/<issue>-<slug>` branch under
`<statedir>/sandboxes/`. Re-provisioning a dirty workspace refuses rather
than discards; exec output lands in `sandbox.exec` task events.

## Developing

```bash
go vet ./cmd/... ./internal/...
go test ./cmd/... ./internal/...
```

Scope commands to `./cmd/... ./internal/...`: the repo also contains
synced skill example sources under `agent/` that are not part of this
module's build.
