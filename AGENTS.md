## Agent skills

### Issue tracker

Issues live in GitHub Issues (via `gh`). See `docs/agents/issue-tracker.md`.

### Triage labels

Default five canonical labels (needs-triage, needs-info, ready-for-agent, ready-for-human, wontfix). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context (`CONTEXT.md` + `docs/adr/` at root). See `docs/agents/domain.md`.

## Engineering

- KISS / DRY / YAGNI: simplest boring solution; no speculative generality.
- Conventional Commits: `type(scope): imperative subject` (`feat`, `fix`, `docs`, `refactor`, `test`, `chore`).
- Refactor-safe functions: header comment states purpose/why, approach, inputs (params, types, constraints), flow (key steps), returns (value, errors); keep small and pure; update the comment with the code.
