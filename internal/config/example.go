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
