# 0002. PostgreSQL with pgx and goose migrations

Date: 2026-11-03 · Status: Accepted

## Context
Metadata (repositories, components, assets, users, roles, tokens, tasks,
findings, audit) is relational, needs transactions spanning several tables on
every publish, and must support full-text search, partitioning for the audit
log and advisory locks for leader election.

## Decision
- PostgreSQL 16 or newer is the only metadata store.
- Driver: [pgx v5](https://github.com/jackc/pgx) with `pgxpool`. Migrations use
  [goose](https://github.com/pressly/goose) through the `database/sql` adapter
  `stdlib.OpenDBFromPool`, so the pool is shared.
- Migrations are plain SQL files in `internal/db/migrations`, embedded in the
  binary and numbered sequentially (`00007_name.sql`). Every file has a working
  `Down` section; a test applies all migrations up, down to zero and up again
  against a real PostgreSQL (`HANGAR_TEST_DATABASE_URL`) in a throwaway schema.
- `hangar serve` refuses to start on an out-of-date schema unless started with
  `--migrate` (or `database.migrate_on_start`); `hangar migrate up|down|status`
  manages it explicitly.
- Identifiers are UUIDs (`gen_random_uuid()`), so rows can be replicated
  between sites without key collisions.
- The database enforces invariants it can: repository name pattern, enum-like
  CHECK constraints, privilege syntax, an append-only audit table (UPDATE and
  DELETE rejected by trigger; retention drops monthly partitions).
- Queries may be written by hand with pgx or generated with sqlc; generated code
  is committed.

## Consequences
- One operational dependency for state; backups are `pg_dump` / PITR.
- Full-text search starts in PostgreSQL (`tsvector` on components); a search
  engine adapter can be added later without changing the source of truth.
