# Roadmap

One line per item; no dates. SPEC.md sections 67-72 are authoritative.

## v0.1 — current

The demo: a labeled GitHub issue becomes a delivered PR with no duplicate
workers and no corrupted state.

- GitHub `agent-ready` label → webhook → exactly one task created
- Docker sandbox provisioned with the repository checked out
- Herdr session created; Codex or Claude launched inside the sandbox
- User can watch the agent through Herdr and send it an instruction
- Validation gate runs; Herder pushes the branch, opens the PR, updates the issue
- Restarting Herder mid-task does not corrupt task state
- A duplicate webhook does not create a duplicate worker

## v0.2 — reliability + providers

- Linear provider
- GitLab provider
- Retry policy
- Better recovery
- Resource accounting
- Network policies
- Notification hooks
- Approval gates

## v0.3 — sandbox ecosystem

- Microsandbox provider
- Additional microVM backend
- Remote sandbox providers
- Sandbox snapshots
- Prebuilt development images

## v0.4 — collaboration

- Agent messaging
- Ask/reply
- Subtasks
- Reviewer agents
- Task lineage
- Manager agents
- Delegation policy

## v0.5 — user experience

- Web dashboard
- Task timelines
- Live terminal links
- Resource usage
- Agent fleet overview
- Visual task graph

## v1.0 vision

Multiple issue providers, sandbox providers, and coding agents; durable
task execution; human intervention; multi-agent workflows; review
pipelines; policy enforcement; cost controls; observable fleets;
distributed workers — while still running as `herder daemon` on one
developer machine.
