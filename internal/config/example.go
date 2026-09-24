package config

// ExampleYAML is the canonical starter config written by `herder init`
// and mirrored at examples/herder.yaml. A test pins the two together so
// they cannot drift apart.
const ExampleYAML = `server:
  listen: 127.0.0.1:8787

database:
  path: ~/.local/state/herder/herder.db

herdr:
  mode: socket

github:
  app_id: ${HERDER_GITHUB_APP_ID}
  private_key_file: ${HERDER_GITHUB_PRIVATE_KEY}
  # HMAC secret for X-Hub-Signature-256 on POST /v1/webhooks/github.
  # Empty means deliveries are not verified (local dev only).
  webhook_secret: ${HERDER_GITHUB_WEBHOOK_SECRET}

linear:
  # HMAC secret for Linear-Signature on POST /v1/webhooks/linear.
  webhook_secret: ${HERDER_LINEAR_WEBHOOK_SECRET}
  # Linear team key -> repository routing table; unmapped teams record
  # a policy_denied delivery instead of queueing work.
  teams:
    ENG: owner/repo

api:
  # Bearer secret for POST /v1/tasks direct submissions. Empty disables
  # the endpoint entirely (the daemon never runs an unauthenticated queue).
  secret: ${HERDER_API_SECRET}

scheduler:
  max_workers: 4
  per_repository:
    owner/repo: 3
  per_agent:
    codex: 4
  lease_ttl: 2m
  dispatch_interval: 2s

repositories:
  owner/repo:
    enabled: true
    # local: /srv/git/owner-repo.git   # clone/push a filesystem path instead
    #                                  # of github.com — no forge, no PR, no
    #                                  # issue labels; delivery pushes the
    #                                  # task branch straight to this path.
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
      forbidden_changes:
        - ".github/workflows/**"
      require_clean_git: true
    delivery:
      create_pr: true
      auto_merge: false
      labels:
        running: agent-running
        review: agent-review
        completed: completed

agents:
  codex-default:
    kind: codex
    timeout: 2h
    resources:
      cpu: 2
      memory: 4Gi
`
