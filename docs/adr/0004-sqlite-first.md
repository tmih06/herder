# 0004. SQLite-first storage

## Context

Herder needs durable state that survives daemon kills, with
transactions strong enough to make "state check + update + event
append" atomic — but no extra infrastructure for a single-host tool
(SPEC section 44).

## Decision

SQLite via `modernc.org/sqlite` (pure Go, keeps `CGO_ENABLED=0`), one
file at `~/.local/state/herder/herder.db`, WAL mode, a single
connection (`SetMaxOpenConns(1)`) so writers serialize in the driver
and UNIQUE constraints stay the cross-process safety net. Persistence
sits behind the `storage.Store` repository surface so PostgreSQL can
replace it later.

## Consequences

- Every mutation is one transaction: transition validation, row
  update, and event append commit or roll back together.
- `UNIQUE(source_provider, source_ref)` + `webhook_deliveries` make
  dedup a database guarantee, not a convention.
- Backup is a file copy; inspection is `sqlite3 herder.db`.
