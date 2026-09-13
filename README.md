# Herder

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

## Developing

```bash
go vet ./cmd/... ./internal/...
go test ./cmd/... ./internal/...
```

Scope commands to `./cmd/... ./internal/...`: the repo also contains
synced skill example sources under `agent/` that are not part of this
module's build.
