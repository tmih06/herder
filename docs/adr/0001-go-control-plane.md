# 0001. Go for the control plane

## Context

Herder is a daemon: webhook server, subprocess orchestration (docker,
herdr, gh), SQLite state, and a single-binary install story (SPEC
section 54).

## Decision

Implement the control plane in Go, stdlib-first (`modernc.org/sqlite`
is the one runtime dependency — pure Go, no CGO).

## Consequences

- Single self-contained binary; `CGO_ENABLED=0` cross-compiles to
  linux/darwin/windows in CI.
- Goroutines + `os/exec` cover the dispatch loop and runner seams
  without a framework.
- If embedded virtualization runtimes demand it later, selected sandbox
  components may migrate to Rust; plugins are not required to be Go.
