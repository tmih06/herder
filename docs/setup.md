# Setup

## Prerequisites

- **Go 1.22+** — builds the single `herder` binary.
- **Herdr** — the `herdr` CLI in PATH and a running Herdr server; Herder
  launches and supervises agents through it.
- **Docker** — the `docker` CLI plus a reachable daemon; the default (and
  only v0.1) sandbox provider.
- **`gh auth login`** — delivery runs as the controller's GitHub identity:
  `gh auth git-credential` authenticates pushes and `gh` opens PRs and
  comments. No token is ever handed to a worker.

## Quickstart

```bash
go build -o herder ./cmd/herder

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

`scripts/demo.sh` runs the full end-to-end demo: labeled issue → task →
sandbox → agent → validation → PR.

## Configuration

`herder init` writes `examples/herder.yaml` (pinned identical to
`internal/config.ExampleYAML`). Every field:

```yaml
server:
  listen: 127.0.0.1:8787        # local status view + webhook endpoint

database:
  path: ~/.local/state/herder/herder.db   # SQLite state file

herdr:
  mode: socket                  # socket | cli | disabled — how Herder
                                # talks to the Herdr runtime

github:
  app_id: ${HERDER_GITHUB_APP_ID}           # env-expanded at load
  private_key_file: ${HERDER_GITHUB_PRIVATE_KEY}

scheduler:
  max_workers: 4                # global concurrency cap
  per_repository:               # optional per-repo caps
    tmih06/meltiply: 3
  per_agent:                    # optional per-agent-kind caps
    codex: 4
  lease_ttl: 2m                 # dispatch lease lifetime (default 2m)
  dispatch_interval: 2s         # scheduler poll interval (default 2s)

repositories:
  tmih06/meltiply:
    enabled: true               # disabled repos are policy-denied
    trigger:
      labels:
        - agent-ready           # labels that mark work as agent-ready
    agent:
      default: codex-default    # must name an entry in agents: below
    sandbox:
      provider: docker          # v0.1: docker only
    validation:
      commands:                 # run inside the sandbox before delivery
        - go test ./...
      forbidden_changes:        # repo-relative globs the agent must not touch
        - ".github/workflows/**"
      require_clean_git: true   # refuse to ship uncommitted/untracked work
    delivery:
      create_pr: true
      auto_merge: false
      labels:                   # issue stage labels
        running: agent-running
        review: agent-review
        completed: completed

agents:
  codex-default:                # profile name referenced by agent.default
    kind: codex                 # codex | claude | opencode | gemini
    timeout: 2h                 # per-task agent time limit; empty = none
    resources:
      cpu: 2                    # container --cpus
      memory: 4Gi               # container --memory
```

Notes:

- `${ENV}` references expand at load; a missing variable fails validation.
- Decoding is strict: unknown fields are rejected with a field-level
  message, and `config validate` reports every problem at once.
- `--config PATH` or `$HERDER_CONFIG` overrides the default location.

## Where state lives

- Config: `~/.config/herder/config.yaml`
- Database: `~/.local/state/herder/herder.db` (tasks, events, deliveries,
  leases — survives daemon restarts)
- Workspaces: `<statedir>/sandboxes/` (one checkout per task)
