# Contributing

Herder is a stdlib-first Go control plane. Keep changes boring: reuse the
existing seams (`Runner`, `Store`, `Provider`) instead of adding new ones.

## Build

Requires Go 1.22+.

```bash
go build -o herder ./cmd/herder
```

## Check

Scope every command to `./cmd/... ./internal/...`:

```bash
gofmt -l cmd internal          # must print nothing
go vet ./cmd/... ./internal/...
go test ./cmd/... ./internal/...
golangci-lint run              # config: .golangci.yml (v2 format)
```

The repo also carries synced skill example sources under `agent/` (and
`.agents/`). They are not part of this module's build — bare `go build
./...`, `go vet ./...`, or `go mod tidy` walks them and fails on code that
isn't ours. CI mirrors this scoping (`.github/workflows/test.yml`).

## Commits

Conventional Commits: `type(scope): imperative subject`.

- Types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`.
- Scope: the package or area touched (`scheduler`, `sandbox`, `cli`, …).
- One logical change per commit; the subject says what, the body says why.

## Pull requests

1. Branch from `main`; keep PRs small and single-purpose.
2. Every PR runs test (Go 1.22 + stable, linux/macOS, race + shuffle),
   lint, security, and smoke workflows — all must pass.
3. Describe behavior changes in terms of the task state machine and event
   log, not just the diff.
4. Update docs when you change a contract: `SPEC.md` is authoritative for
   rationale, `ARCHITECTURE.md` for structure, `CONTEXT.md` for vocabulary.

## Issues and triage

Issues live in GitHub. Label vocabulary is defined in
[docs/agents/triage-labels.md](docs/agents/triage-labels.md) — use those
strings, not invented synonyms.

## License

Herder is Apache-2.0. By submitting a contribution you agree it is
licensed under the same terms; see [LICENSE](LICENSE).
