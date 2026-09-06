# Migrations

SQL schema migrations for the ledger database. **This directory is empty by
design in Phase 0** — the schema arrives with the double-entry accounting
phase.

## Naming convention

Files are ordered, paired, and applied by [golang-migrate][gm]:

```
000001_create_accounts.up.sql
000001_create_accounts.down.sql
000002_create_ledger_entries.up.sql
000002_create_ledger_entries.down.sql
```

- A six-digit, zero-padded, strictly increasing version prefix.
- A short snake_case description of what the migration does.
- Every `.up.sql` has a matching `.down.sql`. A migration that cannot be
  reversed must say so explicitly in the down file rather than being omitted.

[gm]: https://github.com/golang-migrate/migrate

## Rules

1. **Migrations are append-only.** Once a version has been applied anywhere
   beyond a local machine, it is immutable. Fix a mistake with a new version.
2. **One logical change per migration.** A failure should be attributable to a
   single intent.
3. **Money is never floating point.** Amounts are `BIGINT` minor units with a
   separate currency column, never `FLOAT` or `REAL`.
4. **Constraints belong in the database.** The double-entry invariant — every
   transfer's entries sum to zero — is enforced by the schema, not only by
   application code.
5. **Migrations run inside a transaction.** PostgreSQL supports transactional
   DDL; do not defeat it without a comment explaining why.
6. **Index creation on a populated table uses `CONCURRENTLY`**, which cannot
   run inside a transaction and therefore belongs in its own migration.

## Applying migrations

The migration runner is wired up in the persistence phase. Until then, these
are the intended commands:

```sh
migrate -path ./migrations -database "$POSTGRES_DSN" up
migrate -path ./migrations -database "$POSTGRES_DSN" down 1
migrate -path ./migrations -database "$POSTGRES_DSN" version
```
