# Idempotency results — concurrent duplicate requests

Every number here came from an executed run on the date below. Nothing is
estimated.

## Environment

| | |
| --- | --- |
| Date | 2026-09-08 (UTC) |
| Hardware | Apple M3 Pro, 12 CPUs, macOS (darwin/arm64) |
| Go | go1.26.5 |
| PostgreSQL | 16.14, `postgres:16-alpine` in Docker Compose |
| Redis | 7.4.11, `redis:7-alpine` in Docker Compose, `appendonly yes`, `maxmemory-policy noeviction` |
| Isolation | `SERIALIZABLE`, canonical UUID row locking (Phase 2, unchanged) |
| Idempotency TTL | 24 h completed, 30 s processing lease |

## How to reproduce

```sh
make infra-up
make test-idempotency-race
```

## Headline: 12 simultaneous requests, one key

Twelve callers, released together through a closed-channel barrier, all
sending the **same** source, destination, amount, currency and idempotency key.

Run **under the race detector**, five times:

| Run | Callers | Created | Replayed | In progress | Unexpected | Distinct transfer IDs | Transfer rows | Data races |
| --: | ------: | ------: | -------: | ----------: | ---------: | --------------------: | ------------: | ---------: |
| 1 | 12 | 1 | 3 | 8 | 0 | **1** | **1** | 0 |
| 2 | 12 | 1 | 1 | 10 | 0 | **1** | **1** | 0 |
| 3 | 12 | 1 | 3 | 8 | 0 | **1** | **1** | 0 |
| 4 | 12 | 1 | 5 | 6 | 0 | **1** | **1** | 0 |
| 5 | 12 | 1 | 3 | 8 | 0 | **1** | **1** | 0 |

Invariants, every run:

| Check | Result |
| --- | --- |
| Transfer rows created for the key | **1** |
| Distinct transfer IDs returned | **1** |
| Financial debits | **1** |
| Financial credits | **1** |
| Ledger entries total | **2** |
| Ledger invariant violations | **0** |
| Negative balances | **0** |
| Pending transfers left behind | **0** |
| Money conserved | **yes** |
| Reconciliation clean | **yes** |
| Unexpected errors | **0** |
| Data races | **0** |

The created/replayed/in-progress split varies with timing; the financial
outcome does not. Exactly one caller creates the transfer, the rest either
replay it or are told the request is in progress.

Callers told "in progress" then retry, and the test asserts that every one of
them resolves to the **same** transfer ID with no second posting.

Without the race detector the same test typically resolves more callers
directly — one observed run: 10 succeeded, 9 of them recovered from
PostgreSQL, 2 told in progress, still one transfer.

## PostgreSQL uniqueness with Redis bypassed entirely

Eight callers, same key, released together, through a service with **no
coordination store at all** — every request goes straight to PostgreSQL.

| Callers | Created | Recovered from PostgreSQL | In progress | Distinct IDs | Transfer rows | Ledger entries |
| ------: | ------: | ------------------------: | ----------: | -----------: | ------------: | -------------: |
| 8 | **1** | 7 | 0 | **1** | **1** | **2** |

This is the direct evidence that Redis is not what makes deduplication work.

## Failure and recovery scenarios

All executed, all passing:

| Scenario | Result |
| --- | --- |
| Redis unavailable before the transfer | Payment proceeds, degraded; retry returns the original transfer; 1 row |
| Redis unavailable, conflicting request | Still rejected — the fingerprint is in PostgreSQL |
| Redis key deleted, then retry | Original transfer recovered from PostgreSQL; 1 row |
| Whole Redis database flushed, then retry | Original transfer recovered; 1 row |
| Redis key expired, then retry | Original transfer recovered; 1 row |
| Crash after COMMIT, before the Redis update | Original transfer recovered; 1 row; Redis record repaired |
| Stale `PROCESSING` claim, transfer already committed | Resolves against PostgreSQL; 1 row |
| Abandoned `PROCESSING` claim, nothing committed | Reported in progress while leased, then claimable after expiry; 1 row |
| Same key, different amount / destination / currency | `ErrIdempotencyConflict`; second payment **not executed**; 1 row |
| Same key, different amount, after Redis flush | Still `ErrIdempotencyConflict` from the stored fingerprint |
| Two different keys, identical request | Both execute — 2 transfers, as intended |
| Rejected request (insufficient funds), then funded, then retried | Not cached; the retry succeeds; 1 row |
| Invalid keys (empty, space, newline) | Rejected; 0 transfer rows |

## Phase 2 protections still hold

Twenty concurrent transfers with **distinct** keys against an account holding
only ten transfers' worth:

| Callers | Succeeded | Final source balance | Total balances | Negative balances |
| ------: | --------: | -------------------: | -------------: | ----------------: |
| 20 | **10** | 0 | unchanged | **0** |

Idempotency did not weaken account concurrency safety.

## Mutation testing

The tests were verified to actually detect the failures they describe, by
breaking the implementation deliberately:

| Mutation | Observed result |
| --- | --- |
| Stop persisting `idempotency_key` (Redis-only protection) | **2 completed transfers** after a Redis flush, delete or expiry — a real duplicate payment. Four tests failed. |
| Skip fingerprint verification during recovery | A key reused for a different amount was silently accepted after a Redis flush. Two tests failed. |

Both were reverted. The first is the clearest statement of the design: without
the database constraint, Redis losing a record causes a double payment.

## What these results support

Supported:

- **Observed exactly one financial transfer for 12 simultaneous same-key
  requests, in five consecutive runs under the race detector**, with one
  distinct transfer ID returned, one debit, one credit and no invariant
  violations.
- Observed correct deduplication with Redis unavailable, flushed, expired, and
  lost between `COMMIT` and the cache write.
- Observed zero data races across all idempotency tests under `-race`.

Not supported:

- That duplicate transfers are mathematically impossible for every workload.
  These are executed tests, not a proof.
- Any throughput or latency claim. Durations were not the object of these
  tests and are not reported.
