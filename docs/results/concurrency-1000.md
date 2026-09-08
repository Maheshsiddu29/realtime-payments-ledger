# Concurrency results — up to 1,000 concurrent transfer attempts

Every number here was produced by an executed run on the date below. Nothing
is estimated, extrapolated or rounded up. Machine-readable output for the
headline run is in
[`concurrency-1000-oneway.json`](concurrency-1000-oneway.json).

## Environment

| | |
| --- | --- |
| Date | 2026-09-08 (UTC) |
| Hardware | Apple M3 Pro, 12 CPUs, macOS (darwin/arm64) |
| Go | go1.26.5 |
| PostgreSQL | 16.14, `postgres:16-alpine` in Docker Compose |
| PostgreSQL flags | `default_transaction_isolation=serializable`, `max_connections=200`, `deadlock_timeout=1s`, `log_lock_waits=on` |
| Connection pool | `POSTGRES_MAX_OPEN_CONNS=25` (the default) unless stated |
| Isolation | `SERIALIZABLE`, set per transaction |
| Lock order | Canonical UUID order, lowest first |
| Retry budget | 20 attempts, 1 ms base delay, 100 ms cap, full jitter |

These numbers characterise **this machine and this configuration**. They are
not a claim about production hardware, and PostgreSQL running in Docker on
macOS is not a performance-representative deployment.

## How to reproduce

```sh
make infra-up
POSTGRES_DB=ledger_stress go run ./cmd/migrate up

# progressive scenarios
make stress ATTEMPTS=100
make stress ATTEMPTS=500
make stress-1000
```

Each run below started from an empty ledger
(`TRUNCATE ledger_entries, transfers, accounts CASCADE`) and created and funded
its own accounts.

## Scenario: one-way, single hot account

1,000 goroutines released simultaneously, all debiting the same source account.
This is the worst realistic case for contention: every transaction wants the
same row.

| Attempts | Successful | Exhausted retries | Unexpected | Serialization retries | Deadlocks | Max attempts for one transfer | Duration |
| -------: | ---------: | ----------------: | ---------: | --------------------: | --------: | ----------------------------: | -------: |
| 100      | 100        | 0                 | 0          | 701                   | 0         | 15                            | 435 ms   |
| 500      | 425        | 75                | 0          | 5,633                 | 0         | 20                            | 1,444 ms |
| 1000 (run 1) | 657    | 343               | 0          | 12,356                | 0         | 20                            | 2,853 ms |
| 1000 (run 2) | 700    | 300               | 0          | 11,770                | 0         | 20                            | 2,545 ms |
| 1000 (run 3) | 665    | 335               | 0          | 12,356                | 0         | 20                            | 2,463 ms |

Invariants, every run above:

| Check | Result |
| --- | --- |
| Negative balances | **0** |
| Ledger invariant violations | **0** |
| Conservation of money held | **yes** (200000 before, 200000 after) |
| Reconciliation clean | **yes** |
| Pending transfers left behind | **0** |
| Unexpected failures | **0** |
| Ledger entries | exactly 2 per completed transfer |

## Scenario: opposing directions

1,000 attempts alternating `A → B` and `B → A` over one pair of accounts. This
is the scenario deterministic lock ordering exists to survive.

| Attempts | Successful | Exhausted | Serialization retries | **Deadlocks** | Duration |
| -------: | ---------: | --------: | --------------------: | ------------: | -------: |
| 1000     | 682        | 318       | 12,086                | **0**         | 2,506 ms |

Conservation held, reconciliation clean, zero negative balances.

## Scenario: four-account ring

1,000 attempts moving money around `A → B → C → D → A`, so every neighbouring
pair is contended from both sides.

| Attempts | Successful | Exhausted | Serialization retries | **Deadlocks** | Duration |
| -------: | ---------: | --------: | --------------------: | ------------: | -------: |
| 1000     | 859        | 141       | 9,510                 | **0**         | 2,349 ms |

Conservation held, reconciliation clean, zero negative balances. Spreading load
across four accounts raised completions from ~68 % to ~86 % at the same
concurrency, which is what less per-row contention looks like.

## Deterministic suite (runs in CI)

`go test -race -tags=integration ./...`, released simultaneously via a barrier:

| Test | Attempts | Result | Serialization retries | Deadlocks |
| --- | ---: | --- | ---: | ---: |
| Overspend protection | 20 (only 10 affordable) | exactly 10 succeeded, 10 refused for funds | 80 | 0 |
| All affordable succeed | 50 | 50 succeeded | 254 | 0 |
| Opposing directions | 120 | 120 succeeded | 673 | 0 |
| Four-account ring | 150 | 150 succeeded | 669 | 0 |
| Progressive | 10 / 25 / 50 / 100 | exactly half succeeded each time | 34 / 98 / 227 / 493 | 0 |

Zero exhausted retries and zero unexpected failures throughout.

## Finding: a larger connection pool makes throughput *worse*

1,000 attempts against a single hot account, sweeping `POSTGRES_MAX_OPEN_CONNS`
with everything else held constant:

| Pool size | Successful | Exhausted | Serialization retries | Duration |
| --------: | ---------: | --------: | --------------------: | -------: |
| 8         | **989**    | 11        | 6,144                 | 2,318 ms |
| 12        | 908        | 92        | 8,349                 | 2,353 ms |
| 16        | 782        | 218       | 10,330                | 2,692 ms |
| 25        | 649        | 351       | 12,368                | 2,806 ms |
| 50        | 504        | 496       | 14,452                | 2,941 ms |
| 100       | **316**    | 684       | 16,698                | 3,618 ms |

Success rate falls monotonically as the pool grows, and wall-clock time gets
worse at the same time. Under `SERIALIZABLE`, transactions contending for one
row are not queued — they are aborted — so additional concurrency against that
row buys only aborts.

**Correctness was unaffected at every pool size**: zero negative balances, zero
invariant violations, conservation held, reconciliation clean.

The implication is that the right fix for a hot account is admission control —
bounding how many transfers contend per account — not a larger pool. That is
recommended for a later phase; it is not implemented here.

## Retry-budget measurements

How many attempts are actually needed, single hot account, all released at once
(pool at pgxpool's default):

| Concurrency | 5 attempts | 10 attempts | 20 attempts |
| ----------: | ---------: | ----------: | ----------: |
| 20          | 9 / 20     | 20 / 20     | 20 / 20     |
| 50          | 19 / 50    | 49 / 50     | 50 / 50     |
| 100         | 36 / 100   | 84 / 100    | 100 / 100   |

This is why `DefaultRetryPolicy.MaxAttempts` is 20 rather than the more usual
3–5: at 5 attempts, more than half of a merely 20-way contended workload is
refused.

## Deadlock regression evidence

`lockOrder` was temporarily reverted to transfer-direction order and the suite
re-run, to confirm the tests detect the regression:

| Lock order | Deadlocks (40P01) | Opposing test duration | Test outcome |
| --- | ---: | ---: | --- |
| Canonical UUID order | **0** | 0.45 s | pass |
| Transfer direction | **20** | 13.8 s | **fail** |

The four-account ring test independently reported 11 deadlocks under the same
mutation. The 13.8 s is `deadlock_timeout` (1 s) being hit repeatedly.

Notably, even with broken ordering 119 of 120 transfers still completed — the
`40P01` retry handling absorbed the deadlocks — and **no invariant was
violated**. Deadlocks cost throughput and latency, not correctness.

## What these results support, and what they do not

Supported by the runs above:

- **Observed zero double spends across 1,000 concurrent transfer attempts**,
  repeated three times, plus opposing-direction and four-account variants.
- Observed zero negative balances, zero unbalanced transfers and zero
  reconciliation discrepancies in every executed run.
- Observed zero deadlocks with canonical lock ordering, and a reproducible
  deadlock count without it.
- Money was conserved exactly in every run.

**Not** supported:

- That double spends are mathematically impossible for every workload. These
  runs are evidence, not a proof.
- Any throughput or latency claim for production hardware. The durations here
  describe one laptop running PostgreSQL in Docker.
- That 1,000 simultaneous transfers against one account is a *usable* operating
  point. It is not — roughly a third are refused with exhausted retries. It is
  a stress boundary, and the results above show where it lies.
