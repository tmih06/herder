# Herder

**Open-source control plane for autonomous coding-agent fleets, powered by Herdr.**

**Status:** Initial architecture specification
**Target implementation:** Go
**Initial runtime:** Herdr + Docker
**License recommendation:** Apache-2.0
**Primary deployment model:** Single self-contained Herder binary, local-first and self-hostable

---

# 1. Executive Summary

Herder is an open-source autonomous software-engineering orchestration system.

It connects to sources of engineering work such as GitHub Issues and Linear, evaluates tasks against configurable policies, creates isolated development environments, launches coding agents such as Codex, Claude Code, OpenCode, Gemini CLI, or other Herdr-supported agents, supervises those agents through Herdr, validates their work, and delivers the result as a pull request or equivalent artifact.

Herder is not itself a coding agent.

Herder is the **control plane** responsible for deciding:

* what work should be performed;
* when it should run;
* which agent should perform it;
* what environment the agent receives;
* what permissions the agent receives;
* when intervention is necessary;
* whether the result is acceptable;
* how the result should be delivered.

Herdr is the **interactive agent runtime**.

Herdr keeps coding agents inside real terminal panes with their shell, prompts, output, processes, and lifecycle intact. Its automation API provides semantic agent operations such as starting agents, prompting them, reading output, waiting for state changes, and subscribing to events. This allows Herder to automate agents without turning them into opaque subprocesses.

The defining Herder experience is therefore:

> Autonomous enough to operate without a human, transparent enough that a human can inspect the actual agent at any moment.

A user should be able to see:

```text
GitHub Issue #142
       │
       ▼
    Herder
       │
       ├── policy accepted
       ├── sandbox created
       ├── Codex selected
       │
       ▼
     Herdr
       │
       ▼
 Actual interactive Codex session
       │
       ├── inspect
       ├── talk to it
       ├── redirect it
       ├── answer questions
       └── leave it running
```

Herder's long-term goal is to become an open-source **coding factory runtime** capable of managing one agent on a laptop or hundreds of isolated agents across distributed infrastructure.

---

# 2. Product Vision

Traditional coding-agent automation often behaves like a black box:

```text
Task
 ↓
"Agent is running..."
 ↓
Pull Request
```

Herder instead provides:

```text
Task
 ↓
Policy
 ↓
Sandbox
 ↓
Visible agent
 ↓
Observable execution
 ↓
Human intervention when needed
 ↓
Validation
 ↓
Review
 ↓
Pull Request
```

The system should support increasing levels of autonomy without sacrificing observability.

At the simplest level:

```text
1 issue
→
1 sandbox
→
1 coding agent
→
1 pull request
```

At a more advanced level:

```text
1 issue
    │
    ▼
manager agent
    │
    ├── implementation agent
    ├── test agent
    ├── documentation agent
    └── reviewer agent
```

Each worker remains isolated and observable.

---

# 3. Project Positioning

Herder should position itself as:

> **An open-source control plane for autonomous coding agents.**

Alternative short description:

> **Turn issues into isolated, observable coding-agent jobs.**

A longer README-style description:

> Herder connects GitHub, Linear, and other engineering systems to the coding agents you already use. When eligible work appears, Herder creates an isolated development environment, starts Claude Code, Codex, OpenCode, or another Herdr-supported agent, supervises its work, validates the result, and delivers a pull request.
>
> Unlike black-box coding-agent runners, every Herder worker is backed by a real Herdr session. You can attach at any time, watch the agent work, talk to it, redirect it, or take control.

Herder's strongest differentiators should be:

**Agent agnostic.**

Herder does not force users into a proprietary coding-agent implementation.

**Observable.**

The actual terminal-based coding agent remains visible through Herdr.

**Interruptible.**

A human can intervene without destroying the job.

**Local-first.**

A developer should be able to run the entire control plane on their own machine.

**Sandboxed.**

Worker agents should not inherit controller privileges.

**Extensible.**

Issue providers, sandboxes, agent policies, validators, and delivery systems should be pluggable.

**Open source.**

The complete core orchestration platform should remain self-hostable.

---

# 4. Relationship Between Herder and Herdr

Herder and Herdr should remain separate projects.

Herder should not fork Herdr or duplicate its terminal and agent-detection functionality.

The conceptual ownership boundary is:

```text
HERDER
────────────────────────────
Task orchestration
Issue tracking integration
Scheduling
Sandbox management
Policy
Credentials
Security
Validation
Delivery
Audit history
Agent coordination
Durable state


HERDR
────────────────────────────
Terminal sessions
Agent detection
Agent lifecycle
Interactive panes
Live terminal output
Prompting agents
Waiting for states
Human attachment
Terminal manipulation
```

Herdr explicitly exposes its CLI and socket API for scripts, custom clients, automation, and event subscribers. The raw socket API supports long-lived subscriptions and agent operations such as `agent.start`, `agent.prompt`, `agent.read`, `agent.wait`, and pane/session management.

Therefore the desired relationship is:

```text
Herder
   │
   │ Herdr public API
   ▼
Herdr
   │
   ▼
coding agents
```

rather than:

```text
Herder
   └── private fork of Herdr
```

This allows Herder to benefit when Herdr adds new coding agents or improves lifecycle detection.

---

# 5. Product Principles

## 5.1 Glass-box autonomy

Automation should never require sacrificing visibility.

Users must be able to inspect what an agent is doing while the job is running.

---

## 5.2 Human override

Automation must never prevent a human from intervening.

A human should be able to:

```text
inspect
attach
prompt
redirect
pause
stop
retry
handoff
approve
reject
```

a task.

---

## 5.3 Least privilege by default

Workers should receive the minimum authority needed to complete their task.

A coding worker should normally have:

```text
repository checkout
compiler/runtime
package manager
test tools
restricted network
task-scoped agent credentials
```

It should not automatically receive:

```text
Herdr control socket
Docker socket
GitHub App private key
Herder database
host SSH keys
AWS credentials
host home directory
other workers' files
```

---

## 5.4 Durable orchestration

The authoritative task state belongs to Herder, not the terminal.

Restarting Herder should not make it forget which issue produced which sandbox, branch, agent session, or pull request.

---

## 5.5 Provider independence

Core interfaces should avoid assumptions about:

* GitHub;
* Linear;
* Docker;
* Codex;
* Claude;
* a specific cloud provider.

---

## 5.6 Safe failure

When Herder is unsure whether an existing sandbox contains uncommitted work, it should preserve it rather than silently replacing it.

This resembles the durable sandbox philosophy used by systems such as Open SWE, where coding threads retain their sandbox rather than silently discarding inaccessible work.

---

# 6. Initial Scope

Herder v0.1 should implement one workflow extremely reliably:

```text
GitHub issue
      │
agent-ready label
      │
      ▼
    Herder
      │
      ├── claim issue
      ├── create task
      ├── create Docker sandbox
      ├── prepare repository
      ├── launch agent through Herdr
      ├── monitor agent
      ├── validate changes
      ├── push branch
      └── open pull request
```

Initial supported components:

| Component           | v0.1                                                           |
| ------------------- | -------------------------------------------------------------- |
| Language            | Go                                                             |
| Issue source        | GitHub                                                         |
| Trigger             | `agent-ready` label                                            |
| Agent runtime       | Herdr                                                          |
| Agents              | Any usable Herdr-supported CLI agent, initially Codex + Claude |
| Sandbox             | Docker                                                         |
| State               | SQLite                                                         |
| Delivery            | GitHub Pull Request                                            |
| UI                  | CLI + Herdr                                                    |
| Deployment          | Native binary / Docker                                         |
| Validation          | Configurable commands                                          |
| Human approval      | Supported                                                      |
| Multi-agent         | Limited/manual                                                 |
| Distributed workers | No                                                             |

---

# 7. Explicit Non-Goals for v0.1

Herder v0.1 should not attempt to become:

* its own LLM;
* its own coding agent;
* its own terminal emulator;
* its own GitHub replacement;
* a Kubernetes-first platform;
* a distributed scheduler;
* a full CI/CD system;
* an autonomous multi-agent organization;
* a SaaS-only product.

Those capabilities may be integrated later.

---

# 8. High-Level Architecture

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
│                   SQLite Event/State Store            │
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

---

# 9. Core Components

## 9.1 Herder Daemon

`herder daemon` is the primary long-running controller.

Responsibilities:

* load configuration;
* initialize storage;
* initialize provider integrations;
* receive webhooks;
* run scheduler;
* maintain Herdr connection;
* manage task state;
* supervise sandboxes;
* dispatch workers;
* run recovery logic;
* expose local API;
* emit structured events.

The daemon is authoritative for orchestration.

---

# 10. Integration Providers

Issue/project-management systems should implement a common interface.

Conceptually:

```go
type WorkProvider interface {
    Name() string

    Watch(ctx context.Context) (<-chan WorkEvent, error)

    Get(ctx context.Context, ref WorkRef) (*WorkItem, error)

    Claim(ctx context.Context, ref WorkRef) error

    Comment(ctx context.Context, ref WorkRef, body string) error

    UpdateState(ctx context.Context, ref WorkRef, state ExternalState) error

    Deliver(ctx context.Context, delivery DeliveryRequest) (*DeliveryResult, error)
}
```

Potential providers:

```text
GitHub
Linear
GitLab
Gitea
Forgejo
Jira
YouTrack
Plane
Azure DevOps
```

v0.1 implements GitHub.

---

# 11. GitHub Integration

The recommended authentication mechanism is a GitHub App.

The controller owns GitHub credentials.

Worker sandboxes should not receive the GitHub App private key.

Example trigger:

```text
repository: tmih06/meltiply
issue: #182
label: agent-ready
```

Herder receives the webhook and evaluates repository policy.

Herder must deduplicate webhook deliveries.

A unique source identifier should be stored:

```text
github:
  repository_id
  issue_id
  issue_number
  delivery_id
```

---

# 12. Policy Engine

Not every matching event should create an agent.

Policy determines whether work is acceptable.

Possible rules include:

```yaml
repositories:
  tmih06/meltiply:

    triggers:
      labels:
        - agent-ready

    allowed_issue_types:
      - bug
      - enhancement

    denied_paths:
      - ".github/workflows/**"

    agent:
      default: codex
      fallback: claude

    concurrency:
      max: 3

    sandbox:
      provider: docker

    validation:
      - "go test ./..."
      - "golangci-lint run"

    delivery:
      create_pr: true
      auto_merge: false
```

Future policy inputs may include:

```text
labels
repository
author
issue type
estimated complexity
requested agent
priority
security classification
available budget
branch
file scope
time window
```

---

# 13. Task Model

Every accepted unit of work becomes a Herder task.

Example:

```text
Task ID: task_01K...
Source: github:tmih06/meltiply#182
Status: RUNNING

Agent:
    kind: codex
    profile: default

Sandbox:
    provider: docker
    id: sbx_01K...

Git:
    base: main
    branch: herder/182-fix-auth-refresh

Herdr:
    workspace: w3
    pane: w3:p2
```

Tasks are durable entities.

---

# 14. Task State Machine

Primary states:

```text
DISCOVERED
     │
     ▼
ELIGIBLE
     │
     ▼
CLAIMED
     │
     ▼
QUEUED
     │
     ▼
PROVISIONING
     │
     ▼
RUNNING
     │
     ▼
VALIDATING
     │
     ▼
REVIEWING
     │
     ▼
DELIVERING
     │
     ▼
PR_OPEN
     │
     ▼
DONE
```

Alternative states:

```text
BLOCKED
WAITING_FOR_HUMAN
FAILED
CANCELLED
TIMED_OUT
RETRYING
PAUSED
```

State transitions must be persisted.

The terminal is never the sole state database.

---

# 15. Scheduler

The scheduler determines which queued tasks may start.

Scheduler inputs include:

```text
global concurrency
repository concurrency
agent concurrency
sandbox capacity
priority
task age
resource requirements
budget
```

Example:

```yaml
scheduler:
  max_workers: 8

  per_repository:
    max_workers: 3

  per_agent:
    codex: 4
    claude: 4
```

Initially scheduling can be FIFO with priority.

---

# 16. Agent Profiles

An agent profile describes how a worker should be launched.

Example:

```yaml
agents:

  codex-default:
    kind: codex
    command: codex

    limits:
      timeout: 2h

    sandbox:
      cpu: 2
      memory: 4Gi

  claude-reviewer:
    kind: claude
    command: claude

    role: reviewer

    permissions:
      repository: read-only
```

Agent selection should eventually support routing.

Example:

```text
frontend issue
    → Claude

Go backend issue
    → Codex

documentation
    → OpenCode

security-sensitive
    → human review required
```

---

# 17. Herdr Integration

Herdr provides three relevant automation layers:

1. layout/workspace operations;
2. raw pane operations;
3. recognized coding-agent operations.

Its documentation specifically recommends CLI wrappers for simple orchestration and its socket API for custom clients and long-lived event subscriptions.

Herder should therefore evolve in two stages.

## Stage 1

Use Herdr CLI commands during initial development.

This makes debugging easy.

## Stage 2

Use the socket API for long-lived production orchestration.

Herder should fetch:

```bash
herdr api schema --json
```

at development/test time or when performing compatibility checks.

The socket API exposes lifecycle operations and event subscriptions suitable for maintaining an internal runtime view.

---

# 18. Herdr Runtime Model

Each worker maps to a Herdr agent session.

Conceptually:

```go
type AgentSession struct {
    TaskID      string

    AgentKind   string
    AgentName   string

    WorkspaceID string
    TabID       string
    PaneID      string

    State       AgentState

    SandboxID   string

    StartedAt   time.Time
}
```

Herder should maintain:

```text
task ↔ sandbox ↔ Herdr pane ↔ agent
```

as a durable association.

---

# 19. Herdr Event Handling

Herder should subscribe to Herdr events and maintain runtime state.

Relevant changes include:

```text
pane created
pane exited
agent detected
agent state changed
workspace closed
worktree changes
```

Herdr supports event subscriptions specifically for this type of long-lived client.

Agent states should be normalized into Herder concepts such as:

```text
starting
working
idle
blocked
done
unknown
```

Herdr remains responsible for detecting the actual coding-agent state.

Herder interprets those states according to task policy.

---

# 20. Agent Visibility

The user must be able to inspect active agents.

Example:

```bash
herder task list
```

Output:

```text
ID        ISSUE       AGENT       STATE       AGE
task-42   GH-182      codex       working     08m
task-43   GH-191      claude      blocked     04m
task-44   GH-193      opencode    reviewing   02m
```

Detailed inspection:

```bash
herder task inspect task-42
```

Output:

```text
Task:        task-42
Source:      tmih06/meltiply#182

Status:      RUNNING
Agent:       codex
Agent state: working

Sandbox:     sbx-193ab
Provider:    docker

Herdr:
  workspace: w3
  pane:      w3:p7

Git:
  branch: herder/182-auth-refresh
```

---

# 21. Interactive Attachment

A signature feature should be:

```bash
herder task attach task-42
```

This should resolve the appropriate Herdr pane and attach/focus it.

The user should see the real running agent.

Herdr already preserves agents inside real terminal panes with their shell, prompts, logs, and running processes intact.

The workflow becomes:

```text
agent running autonomously
       │
       ▼
human attaches
       │
       ▼
human observes
       │
       ├── does nothing
       │
       ├── talks to agent
       │
       └── redirects agent
       │
       ▼
human detaches
       │
       ▼
agent continues autonomously
```

---

# 22. Intervention Commands

Suggested interface:

```bash
herder task tell task-42 \
  "Do not change the public API. Keep the fix inside auth."
```

Internally:

```text
Herder
   ↓
Herdr agent.prompt
   ↓
agent
```

Other commands:

```bash
herder task pause task-42
herder task resume task-42

herder task stop task-42

herder task retry task-42

herder task handoff task-42 --agent claude

herder task approve task-42

herder task reject task-42

herder task logs task-42
```

---

# 23. Sandbox Architecture

Every coding worker should run in an isolated environment.

Herder should expose:

```go
type SandboxProvider interface {
    Name() string

    Create(ctx context.Context, spec SandboxSpec) (*Sandbox, error)

    Start(ctx context.Context, id string) error

    Exec(ctx context.Context, id string, cmd Command) (*Result, error)

    Attach(ctx context.Context, id string, spec AttachSpec) error

    Stop(ctx context.Context, id string) error

    Destroy(ctx context.Context, id string) error

    Snapshot(ctx context.Context, id string) (*Snapshot, error)

    Inspect(ctx context.Context, id string) (*SandboxStatus, error)
}
```

This should allow alternative runtimes without changing task orchestration.

---

# 24. Sandbox Providers

## v0.1 — Docker

Docker provides the easiest installation experience and broadest compatibility.

Recommended defaults:

```text
no privileged mode
no host PID namespace
no host network namespace
no Docker socket
no Herdr socket
no host home directory
CPU limits
memory limits
process limits
restricted capabilities
explicit writable mounts
```

---

## Lightweight local isolation

Possible future integrations include:

```text
fence
nono
```

Herdr explicitly supports coding agents hidden behind VM/sandbox wrappers using `HERDR_AGENT=<agent>` on the host-visible wrapper process.

Example conceptual launch:

```bash
HERDR_AGENT=codex \
  herder-sandbox attach task-42 -- \
  codex
```

This allows Herdr to continue recognizing the agent even when the real process is inside an isolation wrapper.

---

# 25. MicroVM Providers

Herder should eventually support microVM backends for stronger isolation.

A particularly relevant project is Microsandbox.

Microsandbox provides local hardware-isolated microVMs, OCI image compatibility, detached sandboxes, and an embeddable runtime. It currently exposes Go, Rust, TypeScript, and Python SDKs, which makes it a strong candidate for a Go-based Herder provider.

Conceptually:

```text
Herder
   │
   ├── task A → microVM A
   ├── task B → microVM B
   └── task C → microVM C
```

The runtime currently describes itself as beta, so Docker should remain the default during early Herder development.

Additional future providers may include:

```text
BoxLite
E2B
Daytona
Runloop
Modal
Kubernetes Agent Sandbox
Firecracker-based providers
```

---

# 26. Agent Launching Inside Sandboxes

The host-visible process must remain compatible with Herdr.

Conceptually:

```text
HERDR pane
     │
     ▼
sandbox wrapper
     │
     ▼
isolated environment
     │
     ▼
coding agent
```

Herder may eventually provide shim executables:

```text
codex
claude
opencode
gemini
```

which transparently enter the correct sandbox.

This could allow Herdr's normal agent-start semantics while maintaining isolation.

---

# 27. Git Workspace

Each task receives its own Git workspace.

Possible implementations:

```text
sandbox clone
git worktree
snapshot/template
```

v0.1 should favor an independent repository checkout inside the worker sandbox.

Branch naming convention:

```text
herder/<issue-number>-<slug>
```

Example:

```text
herder/182-fix-refresh-race
```

---

# 28. Credential Boundary

This is a critical system invariant:

> The control plane owns infrastructure credentials.

The worker should not automatically receive:

```text
GitHub App private key
Linear workspace token
Herdr socket
Docker socket
Herder API admin token
cloud root credentials
host SSH keys
```

For Git operations, several models are possible.

v0.1 may allow the agent to modify and commit locally:

```text
agent
 ↓
git commit
```

but Herder performs privileged delivery:

```text
sandbox
 ↓
commit/result
 ↓
Herder validation
 ↓
Herder pushes branch
 ↓
Herder opens PR
```

A future credentials proxy may allow controlled network operations without directly exposing secrets.

Open SWE provides useful prior art here: its architecture keeps GitHub credentials server-side or introduces them through a sandbox proxy rather than treating the agent environment as fully trusted.

---

# 29. Critical Security Invariant: No Raw Herdr Socket

Worker agents must not receive unrestricted:

```text
HERDR_SOCKET_PATH
```

The Herdr API can manipulate workspaces, panes, agents, plugins, and the running server.

Giving an untrusted worker direct access would allow it to escape its logical task boundary and interfere with other agent sessions.

Instead workers receive a narrow Herder Worker API.

Example capabilities:

```text
task.get
task.complete
task.block
task.comment

artifact.publish

agent.ask
agent.notify

subtask.request
```

The worker must not receive unrestricted capabilities such as:

```text
workspace.close
server.stop
pane.close-other-worker
sandbox.create-arbitrary
task.modify-other
```

---

# 30. Worker Capability Tokens

Every worker should receive a temporary capability credential.

Example claims:

```json
{
  "task": "task-42",
  "sandbox": "sbx-17",
  "capabilities": [
    "task.read",
    "task.comment",
    "artifact.write",
    "agent.ask"
  ]
}
```

The token should:

* expire;
* be task-scoped;
* be revocable;
* be auditable.

---

# 31. Network Policy

Network access should be configurable.

Example:

```yaml
sandbox:
  network:
    mode: restricted

    allow:
      - api.openai.com:443
      - api.anthropic.com:443
      - github.com:443
      - proxy.golang.org:443
```

Modes:

```text
none
restricted
unrestricted
```

Default should eventually become restricted.

---

# 32. Prompt Injection Threat Model

Everything entering from an external system must be considered untrusted input.

This includes:

```text
GitHub issue descriptions
GitHub comments
repository files
README instructions
source comments
web pages
generated files
package metadata
PR review comments
```

External content may attempt to instruct the agent to:

```text
read secrets
upload credentials
disable tests
modify workflows
escape the sandbox
control other agents
change task policy
```

Therefore authority must be enforced structurally, not through prompts.

---

# 33. Validation Layer

Agent completion does not mean task completion.

After an agent reports done:

```text
Agent done
    │
    ▼
Herder validation
    │
    ├── formatting
    ├── lint
    ├── unit tests
    ├── integration tests
    ├── forbidden file check
    └── policy check
```

Validation commands are configured by repository.

Example:

```yaml
validation:
  commands:
    - gofmt -w .
    - go test ./...
    - golangci-lint run

  forbidden_changes:
    - ".github/workflows/**"

  require_clean_git: true
```

---

# 34. Reviewer Agents

A task may optionally be reviewed by a second agent.

Flow:

```text
coding agent
     │
     ▼
validation
     │
     ▼
reviewer agent
     │
     ├── approve
     ├── request changes
     └── escalate to human
```

Reviewer sandboxes should preferably be read-only relative to the task output unless specifically authorized.

---

# 35. Delivery Engine

The delivery engine transforms validated work into its external result.

For GitHub:

```text
validated commits
      │
      ▼
push branch
      │
      ▼
create PR
      │
      ▼
comment original issue
      │
      ▼
update labels/state
```

Example label transition:

```text
agent-ready
    ↓
agent-running
    ↓
agent-review
    ↓
completed
```

---

# 36. Example End-to-End Workflow

Issue:

```text
tmih06/meltiply#182

Title:
Fix OAuth refresh race

Labels:
bug
agent-ready
```

Herder receives webhook.

It evaluates:

```text
repository allowed?     yes
label eligible?         yes
concurrency available?  yes
issue already claimed?  no
```

Herder creates:

```text
task_42
```

Then:

```text
task_42
   ↓
CLAIMED
   ↓
Docker sandbox
   ↓
repository checkout
   ↓
herder/182-fix-oauth-refresh
   ↓
Herdr pane
   ↓
Codex
```

Agent receives:

```text
Task goal
Relevant issue metadata
Repository instructions
Allowed operations
Completion requirements
```

During execution:

```text
Herdr state → working
Herdr state → blocked
Herder notification → human
human attaches
human answers
Herdr state → working
Herdr state → done
```

Then:

```text
Herder validation
 ↓
tests pass
 ↓
reviewer approves
 ↓
Herder pushes
 ↓
PR #201 created
 ↓
issue updated
```

---

# 37. Structured Event Log

Herder should maintain its own event history.

Example:

```text
14:01:03 task.discovered
14:01:04 task.claimed

14:01:06 sandbox.creating
14:01:10 sandbox.ready

14:01:11 agent.starting
14:01:16 agent.detected
14:01:17 agent.working

14:03:21 repository.modified
14:04:07 validation.command.started
14:04:14 validation.command.failed

14:04:22 agent.working

14:06:31 validation.command.started
14:06:39 validation.command.succeeded

14:07:02 agent.done

14:07:04 validation.started
14:07:28 validation.passed

14:07:33 delivery.started
14:07:36 pull_request.created
```

---

# 38. Herdr vs Herder Observability

The systems provide complementary visibility.

| Herdr                        | Herder                     |
| ---------------------------- | -------------------------- |
| What is the agent doing now? | What happened to the task? |
| Live terminal                | Durable event history      |
| Prompts                      | Policy decisions           |
| Agent lifecycle              | Sandbox lifecycle          |
| Running processes            | Validation history         |
| Human interaction            | Cost/resource accounting   |
| Pane attachment              | PR/artifact linkage        |

This combination is one of Herder's core advantages.

---

# 39. Agent-to-Agent Communication

Future Herder versions should allow agents to communicate without sharing control-plane privileges.

Message kinds:

```text
notify
ask
reply
```

This mirrors concepts already explored in the Herdr ecosystem by `herdr-mail`, which implements durable asynchronous messaging with correlated ask/reply semantics.

Example:

```text
implementation agent
       │
       │ ask:
       │ "Review this DB migration"
       ▼
     Herder
       │
       ├── authorization
       ├── budget check
       ├── depth limit
       └── routing
       │
       ▼
reviewer agent
```

Agents should not directly manipulate each other's Herdr panes.

---

# 40. Agent Hierarchies

Future task lineage:

```text
task-182
└── manager
    ├── researcher
    ├── backend-worker
    ├── frontend-worker
    ├── test-worker
    └── reviewer
```

Herder owns parent-child relationships.

Example fields:

```text
parent_task_id
root_task_id
spawned_by_agent_id
depth
```

---

# 41. Subtask Safety Limits

Multi-agent expansion must have limits.

Example:

```yaml
agents:
  delegation:
    enabled: true

    max_depth: 2
    max_children: 4
    max_total_agents: 8

    budget:
      max_usd: 20
```

Without these limits an agent could recursively create unlimited work.

---

# 42. CLI

Suggested commands:

```text
herder init

herder daemon

herder doctor

herder status
```

Task operations:

```text
herder task list

herder task inspect <task>

herder task attach <task>

herder task tell <task> <message>

herder task pause <task>

herder task resume <task>

herder task stop <task>

herder task retry <task>

herder task approve <task>

herder task reject <task>
```

Agent operations:

```text
herder agent list

herder agent inspect <agent>
```

Sandbox:

```text
herder sandbox list

herder sandbox inspect <id>

herder sandbox shell <id>
```

Provider:

```text
herder provider list

herder provider test github
```

Server:

```text
herder events

herder logs

herder config validate
```

---

# 43. Configuration

Suggested default:

```text
~/.config/herder/config.yaml
```

Example:

```yaml
server:
  listen: 127.0.0.1:8787

database:
  path: ~/.local/state/herder/herder.db

herdr:
  mode: socket

github:
  app_id: ${HERDER_GITHUB_APP_ID}
  private_key_file: ${HERDER_GITHUB_PRIVATE_KEY}

scheduler:
  max_workers: 4

repositories:

  tmih06/meltiply:

    enabled: true

    trigger:
      labels:
        - agent-ready

    agent:
      default: codex-default

    sandbox:
      provider: docker

    validation:
      commands:
        - go test ./...

    delivery:
      create_pr: true
      auto_merge: false

agents:

  codex-default:
    kind: codex

    timeout: 2h

    resources:
      cpu: 2
      memory: 4Gi
```

---

# 44. Storage

SQLite should be the initial database.

Reasons:

```text
single binary deployment
no additional infrastructure
transaction support
durability
easy backup
easy inspection
sufficient concurrency for single-host execution
```

Herder should hide persistence behind repository interfaces so PostgreSQL can be added later.

---

# 45. Core Data Model

Suggested primary entities:

```text
providers
repositories

work_items

tasks
task_events

agents
agent_sessions

sandboxes

artifacts

validations
validation_runs

deliveries

messages

leases

credentials_metadata
```

---

# 46. Task Table

Conceptually:

```text
tasks

id
source_provider
source_ref

status
priority

repository_id

agent_profile
agent_session_id

sandbox_id

branch_name

created_at
claimed_at
started_at
finished_at

attempt
parent_task_id
root_task_id

error_code
error_message
```

---

# 47. Event Table

```text
task_events

id
task_id

event_type

actor_type
actor_id

payload_json

created_at
```

Everything important should generate an event.

---

# 48. Crash Recovery

On restart Herder should reconcile three sources:

```text
SQLite desired state
Herdr runtime state
sandbox provider state
```

Example:

```text
SQLite says RUNNING
Herdr pane exists
sandbox exists
```

→ resume supervision.

If:

```text
SQLite says RUNNING
Herdr pane missing
sandbox exists
```

→ mark worker disconnected and apply recovery policy.

If:

```text
SQLite says RUNNING
sandbox exists with uncommitted work
```

Herder must not automatically destroy it.

---

# 49. Idempotency

All external events must be safe to process more than once.

Examples:

```text
GitHub webhook delivery ID
Linear event ID
task dispatch lease
delivery operation
PR creation
```

A duplicate GitHub webhook must not create two workers for one issue.

---

# 50. Leasing

Before execution:

```text
task
 ↓
worker lease
```

The lease should have:

```text
owner
acquired_at
heartbeat
expires_at
```

This prepares Herder for future distributed workers while remaining simple locally.

---

# 51. Herdr Plugin

Herder should optionally ship a Herdr plugin.

The plugin is not the primary daemon.

Herdr plugins are manifest-driven executable tools capable of startup hooks, actions, event hooks, pane entrypoints, and other integrations. Startup hooks run once rather than acting as a general daemon supervisor.

Therefore the plugin should mainly provide UI/integration operations.

Potential actions:

```text
Open Herder Dashboard
List Active Tasks
Dispatch Current Issue
Pause Worker
Restart Herder
Open Task
```

Potential pane:

```text
Herder dashboard
```

The plugin may start/detach `herder daemon`, but Herder remains responsible for its lifecycle.

This pattern is already used by projects such as `herdr-dispatch`, whose Herdr plugin launches a detached dispatcher daemon.

---

# 52. Existing Herdr Ecosystem

Herder should learn from but not depend on existing experimental Herdr tools.

`herdr-dispatch` already validates several important ideas:

```text
task
→
worker
→
Herdr pane
→
coding agent
→
review boundary
```

It describes itself as a dispatcher that brings up worker agents in Herdr panes for ready tasks.

`herdr-sched` demonstrates:

```text
cron
webhooks
file triggers
daemon architecture
policy gates
```

for Herdr-oriented automation.

`herdr-mail` demonstrates:

```text
send
ask
reply
```

communication semantics for Herdr agents.

Herder should treat these projects as useful architectural prior art while presenting one cohesive system.

---

# 53. Open SWE as Product Prior Art

Open SWE demonstrates that an open-source software factory can accept engineering work from systems such as GitHub and Linear, execute work inside isolated sandboxes, validate it, and deliver pull requests. It also exposes sandbox providers as a replaceable abstraction.

Herder differs in a fundamental way:

```text
Open SWE
    ↓
ships its own agent runtime architecture


Herder
    ↓
orchestrates existing coding agents through Herdr
```

Herder's differentiator is therefore:

> Use the coding agent you already trust while retaining one consistent orchestration layer.

---

# 54. Language Choice

Go is recommended for the first implementation.

Reasons:

```text
single-binary distribution
good process control
strong networking support
excellent concurrency primitives
easy HTTP/webhook servers
mature SQLite support
mature Docker APIs
simple cross-compilation
fast startup
easy daemon implementation
```

A possible future reason to migrate selected sandbox components to Rust would be tighter integration with embedded virtualization runtimes.

There is no requirement that plugins use Go.

---

# 55. Repository Layout

Suggested layout:

```text
herder/
│
├── cmd/
│   └── herder/
│
├── internal/
│   ├── api/
│   ├── config/
│   ├── events/
│   ├── herdr/
│   ├── policy/
│   ├── scheduler/
│   ├── storage/
│   ├── tasks/
│   └── worker/
│
├── provider/
│   ├── github/
│   └── linear/
│
├── sandbox/
│   ├── docker/
│   ├── microsandbox/
│   └── local/
│
├── agent/
│   ├── profiles/
│   └── router/
│
├── validator/
│
├── delivery/
│   └── github/
│
├── plugins/
│   └── herdr/
│
├── web/
│
├── docs/
│   ├── architecture/
│   ├── security/
│   ├── providers/
│   └── sandbox/
│
├── examples/
│
├── SPEC.md
├── SECURITY.md
├── CONTRIBUTING.md
├── LICENSE
└── README.md
```

---

# 56. Public Extension Model

Herder should eventually expose stable interfaces for:

```text
WorkProvider
SandboxProvider
Validator
AgentRouter
DeliveryProvider
PolicyExtension
EventSink
```

Extensions should eventually be able to run out-of-process.

A versioned protocol avoids requiring every integration to compile against exactly the same Go version.

Possible approaches:

```text
gRPC
ConnectRPC
JSON-RPC
MCP
subprocess protocol
```

The first version may keep providers in-tree until the interfaces stabilize.

---

# 57. Local API

The daemon should expose a local API used by:

```text
CLI
future web dashboard
Herdr plugin
worker capability API
external automation
```

Potential endpoints:

```text
GET  /v1/tasks
GET  /v1/tasks/:id

POST /v1/tasks/:id/pause
POST /v1/tasks/:id/resume
POST /v1/tasks/:id/cancel

POST /v1/tasks/:id/message

GET  /v1/agents

GET  /v1/events

GET  /v1/sandboxes
```

WebSocket or SSE may expose live events.

---

# 58. Event Architecture

Internally, Herder should use structured events.

Examples:

```text
work.discovered

task.created
task.claimed
task.started
task.blocked
task.completed
task.failed

sandbox.created
sandbox.started
sandbox.stopped

agent.started
agent.state_changed
agent.prompted
agent.stopped

validation.started
validation.failed
validation.passed

delivery.started
delivery.completed

review.requested
review.completed
```

Events enable future integrations such as:

```text
Discord
Slack
Prometheus
OpenTelemetry
web UI
audit exports
```

without coupling them directly to task logic.

---

# 59. Cost and Usage Accounting

Future versions should record:

```text
agent runtime
token usage where available
model cost
CPU time
memory
sandbox lifetime
network usage
retry count
```

Policy may then support:

```yaml
budget:
  per_task_usd: 5

  per_day_usd: 50
```

A budget breach should transition a task into:

```text
WAITING_FOR_HUMAN
```

rather than silently continuing.

---

# 60. Human Approval Gates

Policies should support approval before sensitive operations.

Examples:

```text
modifying CI workflows
database migrations
infrastructure code
dependency major upgrade
security-sensitive files
production deployment
large diff
budget exceeded
```

Flow:

```text
agent result
   ↓
policy gate
   ↓
WAITING_FOR_HUMAN
   ↓
approve / reject
```

---

# 61. Notifications

Notification providers may eventually include:

```text
Herdr
CLI
desktop notification
Discord
Slack
email
webhooks
```

Important events:

```text
agent blocked
agent requests approval
validation failure
task completed
task failed
PR created
budget exceeded
```

---

# 62. Deployment Models

## Native

```bash
herder daemon
```

Requirements:

```text
Herdr
selected coding agents
Docker if using Docker provider
```

This should be the best developer experience.

---

## Docker

Possible controller deployment:

```text
docker compose

herder
database volume
Herdr environment
sandbox access layer
```

Care must be taken around Docker socket access.

Giving the Herder controller Docker access is fundamentally different from giving workers Docker access.

Workers should never inherit it.

---

## Future distributed architecture

```text
                 Herder Controller
                        │
               durable task queue
                        │
           ┌────────────┼────────────┐
           ▼            ▼            ▼
        Worker A     Worker B     Worker C
           │            │            │
       microVMs       Docker      Kubernetes
```

The controller remains authoritative.

---

# 63. Open-Source Licensing

Recommended license:

**Apache License 2.0**

Reasons:

```text
permissive
commercial adoption friendly
explicit patent grant
widely accepted by infrastructure projects
compatible with commercial integrations
similar licensing philosophy to Herdr
```

AGPL-3.0 would be appropriate only if the project explicitly wants hosted providers to publish server-side modifications.

The initial recommendation is Apache-2.0.

---

# 64. Open-Source Governance

The project should publish:

```text
README.md
SPEC.md
ARCHITECTURE.md
SECURITY.md
CONTRIBUTING.md
CODE_OF_CONDUCT.md
ROADMAP.md
```

Important architectural decisions should eventually be documented as ADRs:

```text
docs/adr/

0001-go-control-plane.md
0002-herdr-runtime.md
0003-sandbox-provider-interface.md
0004-sqlite-first.md
0005-worker-capability-boundary.md
```

---

# 65. Herder Security Model

Security should be considered a first-class product feature.

The trust hierarchy is:

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

The coding agent must be treated as potentially compromised.

This is true even if the model provider itself is trusted, because prompt injection can originate from repository content.

---

# 66. Required Security Invariants

Herder should eventually enforce the following invariants:

1. A worker cannot control another worker.

2. A worker cannot obtain the Herdr controller socket.

3. A worker cannot access the Docker daemon.

4. A worker cannot access Herder's database.

5. A worker cannot access controller secrets.

6. A worker cannot silently modify protected files.

7. A task cannot exceed resource limits indefinitely.

8. External events cannot create duplicate workers.

9. Sensitive delivery actions require policy authorization.

10. Every privileged action must be attributable to an actor.

---

# 67. v0.1 Success Criteria

Herder v0.1 is successful when all of the following works reliably:

```text
GitHub issue receives agent-ready label
           │
           ▼
Herder receives webhook
           │
           ▼
one and only one task created
           │
           ▼
Docker sandbox created
           │
           ▼
repository prepared
           │
           ▼
Herdr session created
           │
           ▼
Codex or Claude launched
           │
           ▼
user can watch agent through Herdr
           │
           ▼
user can send agent an instruction
           │
           ▼
agent finishes
           │
           ▼
tests run
           │
           ▼
Herder collects validated changes
           │
           ▼
Herder pushes branch
           │
           ▼
Pull Request created
           │
           ▼
original issue updated
```

Additionally:

```text
restart Herder during running task
```

must not corrupt task state.

And:

```text
duplicate webhook
```

must not create duplicate workers.

---

# 68. v0.2

Focus:

**Reliability + providers**

Add:

```text
Linear provider
GitLab provider

retry policy
better recovery
resource accounting
network policies
notification hooks
approval gates
```

---

# 69. v0.3

Focus:

**Sandbox ecosystem**

Add:

```text
Microsandbox provider
additional microVM backend
remote sandbox providers
sandbox snapshots
prebuilt development images
```

---

# 70. v0.4

Focus:

**Collaboration**

Add:

```text
agent messaging
ask/reply
subtasks
reviewer agents
task lineage
manager agents
delegation policy
```

---

# 71. v0.5

Focus:

**User experience**

Add:

```text
web dashboard
task timelines
live terminal links
resource usage
agent fleet overview
visual task graph
```

Possible dashboard:

```text
┌──────────────────────────────────────────────────────┐
│ HERDER                                  4/8 workers  │
├──────────────────────────────────────────────────────┤
│ #182 OAuth race     Codex      working       08:21  │
│ #191 API export     Claude     testing       06:32  │
│ #193 Navbar         OpenCode   blocked       03:11  │
│ #194 Docs           Codex      reviewing     02:02  │
└──────────────────────────────────────────────────────┘
```

Selecting a task should expose:

```text
timeline
issue
agent
sandbox
files changed
validation
messages
PR
```

with a prominent:

```text
OPEN IN HERDR
```

action.

---

# 72. v1.0 Vision

Herder v1.0 should support:

```text
multiple issue providers

multiple sandbox providers

multiple coding agents

durable task execution

human intervention

multi-agent workflows

review pipelines

policy enforcement

cost controls

observable agent fleets

distributed workers
```

while retaining the ability to operate simply as:

```bash
herder daemon
```

on one developer machine.

---

# 73. Long-Term Workflow Example

A mature Herder installation could operate like:

```text
GitHub
Linear
Support tickets
Scheduled maintenance
Security scanners
         │
         ▼
     HERDER
         │
     Policy Engine
         │
         ▼
 Manager Agent
         │
 ┌───────┼──────────┐
 ▼       ▼          ▼
research backend  frontend
agent    agent     agent
 │        │          │
 └────────┼──────────┘
          ▼
      test agent
          │
          ▼
      reviewer
          │
          ▼
   deterministic validation
          │
          ▼
     human gate
          │
          ▼
      Pull Request
```

Every box representing a coding agent can still correspond to an inspectable Herdr session.

---

# 74. Central Product Idea

The most important distinction Herder should preserve is:

```text
Other systems:

Issue
  ↓
mysterious cloud agent
  ↓
PR


Herder:

Issue
  ↓
policy
  ↓
sandbox
  ↓
REAL AGENT SESSION
  ↓
       ┌──────────── human can look inside
       │
       ▼
 implementation
  ↓
validation
  ↓
review
  ↓
PR
```

The goal is not maximum autonomy at any cost.

The goal is:

> **Trustworthy autonomy through isolation, observability, and human control.**

---

# 75. Recommended Project Tagline

Primary:

> **Herder — the open-source control plane for coding-agent fleets.**

Secondary:

> **Turn issues into isolated, observable coding-agent jobs.**

Developer-oriented:

> **Your agents. Your infrastructure. Your eyes on everything.**

Herdr relationship:

> **Herdr lets you manage coding agents. Herder lets you manage fleets of them.**

---

# 76. Final Architectural Principle

The project should maintain one clean hierarchy:

```text
External work systems
        │
        ▼
      HERDER
 orchestration / trust
        │
        ▼
     SANDBOX
 isolation boundary
        │
        ▼
      HERDR
 agent interaction
        │
        ▼
   CODING AGENT
 implementation
```

More precisely, Herder owns the sandbox lifecycle while Herdr owns the interactive agent representation around the sandboxed process.

The agent is powerful inside its assigned environment.

Herder remains authoritative outside it.

That separation is what allows Herder to become increasingly autonomous without turning autonomous coding into an opaque or uncontrollable system.
