# Migrations

SQL schema migrations for the ledger database, applied by `cmd/migrate`.

| Version | Migration                    | Contents                                                  |
| ------- | ---------------------------- | --------------------------------------------------------- |
| 1       | `create_accounts`            | `accounts`, the `updated_at` trigger.                     |
| 2       | `create_transfers`           | `transfers`, FK indexes, completed-transfer immutability. |
| 3       | `create_ledger_entries`      | `ledger_entries`, one-leg-per-account, append-only.       |
| 4       | `enforce_balanced_transfers` | Deferred constraint triggers enforcing `SUM(...) = 0`.    |

Full schema reference: [../docs/DATABASE.md](../docs/DATABASE.md).

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
   transfer's entries sum to zero — is enforced by deferred constraint
   triggers, not only by application code. A plain `CHECK` cannot express a
   cross-row aggregate; see
   [LEDGER_DESIGN.md](../docs/LEDGER_DESIGN.md#the-invariants-and-what-enforces-each-one).
5. **Migrations run inside a transaction.** Each file wraps its statements in
   `BEGIN`/`COMMIT`, so a migration that fails partway leaves nothing behind.
   PostgreSQL supports transactional DDL; do not defeat it without a comment
   explaining why.
6. **Index creation on a populated table uses `CONCURRENTLY`**, which cannot
   run inside a transaction and therefore belongs in its own migration.

## Applying migrations

Use the Make targets, which run `cmd/migrate` with the standard `POSTGRES_*`
environment variables:

```sh
make migrate-up          # apply all pending migrations
make migrate-down        # roll back exactly one migration
make migrate-down-all    # roll back everything (destroys data)
make migrate-version     # print the current version
make migrate-redo        # down-all then up, proving both directions
```

**The application never migrates at start-up.** Replicas booting together would
race to change the schema, and a destructive operation should not hide inside a
routine restart.

Both directions are exercised by `TestMigrationsRoundTrip` and by CI on every
push, so a broken down-migration is caught before an incident needs it.
