# Concurrency

How transfer posting behaves when many transfers run at once: what protects
correctness, what the costs are, and what has actually been measured.

Measured results live in [results/](results/). This document explains the
mechanisms; the results files record what was observed.

## The problem

A transfer is a read-modify-write on two account balances. Run two of them at
once against the same account and the classic failure is a lost update:

```
T1  read A = 1000        T2  read A = 1000
T1  write A = 1000 - 600     T2  write A = 1000 - 600
                             both succeed; A = 400, but 1200 left the account
```

That is a double spend. A second failure mode appears once transfers run in
both directions: `A → B` and `B → A` each lock one row and wait for the other,
and neither can proceed.

Three mechanisms address these, in layers.

## 1. SERIALIZABLE isolation

Every posting transaction runs at `SERIALIZABLE`, set explicitly per
transaction rather than relying on a server default:

```go
tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
```

PostgreSQL guarantees that concurrent `SERIALIZABLE` transactions produce a
result equivalent to running them one after another in *some* order. When it
cannot prove that, it aborts one with SQLSTATE `40001` rather than committing a
result no serial order could produce.

This is the outer guarantee: whatever else is true, the committed history is
equivalent to a serial one.

## 2. `SELECT ... FOR UPDATE` on both accounts

Isolation alone would catch conflicts, but only at `COMMIT`, and only by
throwing away all the work. Explicit row locks make the common case cheap:

```sql
SELECT id, currency, balance_minor FROM accounts WHERE id = $1 FOR UPDATE
```

A concurrent transfer touching the same account blocks on the lock instead of
reading a balance that is about to change. Blocking briefly is far cheaper
than doing the entire transfer twice.

Both rows are locked *before* any balance is modified, so the balance a
transfer reads is the balance it debits.

## 3. Deterministic lock ordering

This is the part that prevents deadlocks, and it is the whole reason a
canonical order exists.

If each transfer locked its own source first, then `A → B` and `B → A` would
request the same two rows in opposite orders:

```
T1 (A→B)   lock A ✓   lock B ✗ waiting on T2
T2 (B→A)   lock B ✓   lock A ✗ waiting on T1
                      → cycle; PostgreSQL aborts one with 40P01
```

Instead, both rows are locked in **canonical UUID order — lowest first** —
regardless of which is the source:

```go
func lockOrder(a, b uuid.UUID) (first, second uuid.UUID) {
	if bytes.Compare(a[:], b[:]) <= 0 {
		return a, b
	}
	return b, a
}
```

The acquisition order becomes a property of the *pair of accounts*, not of the
direction of the transfer. Every transaction touching the same two rows
requests them in the same sequence, so no cycle can form. Any total order
would work; UUID byte order is used because it is already available, stable,
and free of ties.

Two separate statements are issued rather than one
`WHERE id = ANY(...) ORDER BY id FOR UPDATE`, because with a single statement
the order in which rows are actually locked depends on the query plan — and the
ordering guarantee is precisely what this code exists to provide.

### Lock order is not transfer direction

Locking lowest-UUID-first means that for roughly half of all transfers the
**destination is locked before the source**. The debit must still land on the
source.

Lock acquisition and business roles are therefore kept strictly apart:
`lockAccounts` locks in canonical order and then maps the rows back to source
and destination **by matching ids**, never by which row was locked first.
Debiting "whichever row was locked first" would silently reverse half of all
transfers while leaving every balance invariant intact — no conservation or
zero-sum check would catch it.

`TestLockOrderDoesNotSwapSourceAndDestination` covers this directly: it picks
accounts with known relative UUIDs and asserts the debit lands on the source in
both orderings.

### Evidence that the ordering matters

Reverting `lockOrder` to transfer-direction order and re-running the suite
produced, on this machine:

| Lock order            | Deadlocks (40P01) | Opposing-transfer test duration |
| --------------------- | ----------------- | ------------------------------- |
| Canonical UUID order  | **0**             | 0.45 s                          |
| Transfer direction    | **20**            | 13.8 s                          |

The 13.8 s is `deadlock_timeout` (1 s) being hit repeatedly. The tests fail
loudly in that configuration, so the regression cannot return unnoticed.

## Serialization failures and retries

`40001` is not a defect. Under `SERIALIZABLE` it is the *designed* outcome of
contention, and a correct application retries it.

### Which errors are retried

Classified by SQLSTATE through `pgconn.PgError`, never by matching text in the
error message — messages are localised and reworded between releases, whereas
SQLSTATE codes are part of the SQL standard:

| SQLSTATE | Name                    | Retried | Why                                                                 |
| -------- | ----------------------- | ------- | ------------------------------------------------------------------- |
| `40001`  | `serialization_failure` | **Yes** | Expected under contention. The transaction was fully rolled back.   |
| `40P01`  | `deadlock_detected`     | **Yes** | See below. Also a complete rollback, so retrying is equally safe.   |
| anything else | —                  | No      | Not known to be safe or transient.                                  |

Both codes belong to SQL class 40, *Transaction Rollback*. PostgreSQL
guarantees the transaction was rolled back completely before reporting either,
so a retry starts from a clean slate and cannot double-apply anything.

**Why `40P01` is retried even though lock ordering should prevent it.** The
ordering makes a cycle impossible *among transfer postings*, and the measured
deadlock count for every scenario in [results/](results/) is zero. It is
retried anyway because the cost is nil and the alternative is a spurious
failure: a concurrent administrative statement, a future code path, or
maintenance touching the same rows in a different order can still create a
cycle that a transfer merely loses. The mutation test above also shows the
retry working — with deliberately broken ordering, 119 of 120 transfers still
completed despite 20 deadlocks.

### Which errors are never retried

Business rejections are not `PgError`s at all, so they cannot be mistaken for
retryable conditions:

- `ErrInsufficientFunds`
- `ErrSourceAccountNotFound`, `ErrDestinationAccountNotFound`
- `ErrCurrencyMismatch`
- `ErrInvalidAmount`, `ErrSameAccount`
- validation failures, which are rejected before any database work
- permanent database errors — constraint violations, undefined tables

A refusal that got retried into a success would be a serious defect. Two tests
guard it: `TestBusinessRejectionsAreNotRetried` asserts an insufficient-funds
rejection costs exactly one attempt, and `TestValidationFailuresCostNoAttempt`
asserts a malformed command reaches the database zero times.

### Retry policy

```go
var DefaultRetryPolicy = RetryPolicy{
	MaxAttempts: 20,
	BaseDelay:   time.Millisecond,
	MaxDelay:    100 * time.Millisecond,
}
```

Bounded by construction: at most 20 attempts and roughly 1.3 s of cumulative
backoff before the transfer is refused with `ErrRetriesExhausted`. There is no
unbounded loop, and the loop terminates on the attempt counter regardless of
what the database does.

Backoff is **full jitter** — a uniformly random duration in `[0, ceiling]`,
where the ceiling doubles per attempt up to `MaxDelay`. Randomising the whole
interval rather than adding jitter to a fixed delay is what actually breaks up
a thundering herd; without it, every transaction that conflicted at the same
moment wakes at the same moment and conflicts again.

**Context cancellation is honoured during backoff.** A cancelled request stops
immediately rather than sleeping out the rest of its delay.

**Why 20 and not 3–5.** This is larger than a typical retry budget, for a
reason specific to `SERIALIZABLE`: PostgreSQL does not queue contending
transactions. A transaction that waits on a row lock and then finds the row was
modified by a committed transaction is *aborted* with `40001` rather than
allowed to re-read. Contention therefore produces an abort rate that rises with
concurrency instead of a queue, and the retry budget has to absorb it. Measured
on this workload, 5 attempts completed only 9 of 20 simultaneous transfers;
20 attempts completed all of them, and all of 120. The value was chosen from
measurement, and the measurements are in [results/](results/).

Retry policy lives outside the transaction. `Post` runs `postOnce` — one
transaction, BEGIN to COMMIT, with no retry logic of any kind — inside the
bounded loop. Transactional correctness and retry policy can therefore be
reviewed and tested separately, and the transfer logic exists in exactly one
place.

## Exhausted retries are refusals, not corruption

When a transfer uses its whole budget it returns `ErrRetriesExhausted` and
**nothing was written**. The ledger is exactly as correct as if the request had
never arrived. This matters when reading the results files: at 1000
simultaneous transfers against a single account a substantial fraction exhaust
their retries, and in every measured run the accounting invariants still held
perfectly.

Exhaustion is an availability and throughput property. Double spends,
unbalanced ledgers and negative balances are correctness properties. They are
reported separately and should never be conflated.

## What the invariants are, and what checks them

| Invariant                                        | Enforced by                                    | Verified by                              |
| ------------------------------------------------ | ---------------------------------------------- | ---------------------------------------- |
| No account is overdrawn                          | `FOR UPDATE` + guarded `UPDATE` + `CHECK`      | Direct SQL query after every stress run  |
| Every completed transfer's entries sum to zero   | Deferred constraint trigger at `COMMIT`         | `reconcile.Check`                        |
| Exactly one debit and one credit per transfer    | Unique `(transfer_id, account_id)` + Go check   | `reconcile.Check`                        |
| Entry currencies match the transfer              | Deferred constraint trigger                     | `reconcile.Check`                        |
| Entries reference the transfer's own accounts    | Go, plus reconciliation                         | `reconcile.Check`                        |
| Money is conserved across a transfer workload    | The above, together                             | Total balances compared before and after |
| Stored balances match the ledger                 | Written in one transaction                      | `reconcile.Check`                        |

## Reconciliation

`internal/reconcile` compares stored balances against the ledger. Nothing in
PostgreSQL ties the two together — they are written in the same transaction,
but no constraint says they must correspond — so this is the check that they
do.

For each account:

```
stored balance  ==  baseline funding  +  SUM(that account's ledger entries)
```

### The baseline exception

Phase 1 deliberately has no production code path that puts money into the
system: accounts are created at zero, and the only way to reach a funded state
is a direct balance write from test or development tooling. That write creates
money from nothing and produces no ledger entries.

Reconciliation must therefore be told about that money, or every funded account
would look like a discrepancy. **This is an honest exception, not a property of
the ledger.** For a baseline account the check proves only that *movements
since funding* are fully explained by the ledger — not that the entire balance
is. An account with no baseline is reconciled strictly, against an expected
contribution of zero.

When a later phase models funding as a real deposit with its own ledger
entries, `reconcile.Baseline` and this exception can be deleted.

## Test determinism

Concurrency tests use no `sleep` for synchronisation. Goroutines block on a
closed-channel barrier so they are released together — making the contention
real rather than an artefact of staggered starts — and completion is a
`WaitGroup`. Every concurrency test carries a context timeout, so an
unresolved deadlock fails the test instead of hanging CI.

All scenarios are fixed rather than randomised, so a CI failure is
reproducible. The stress runner is likewise deterministic: the same flags
always produce the same workload.

Everything runs under `go test -race -tags=integration ./...`. Database
serialization conflicts are not Go data races and are handled separately, by
the retry loop.

## Limitations

1. **Throughput on a single hot account is bounded, and does not improve with
   concurrency.** Measurements show the opposite: raising the connection pool
   from 8 to 100 at 1000 simultaneous transfers lowered completions from 989 to
   316 while wall-clock time got *worse*. Under `SERIALIZABLE`, added
   concurrency against one row buys only aborts. The fix is admission control —
   queueing per account so a bounded number of transfers contend at once — not
   a bigger pool.
2. **No claim of impossibility.** The measured runs recorded zero double spends
   and zero invariant violations. That is evidence, not proof: it says these
   workloads on this machine behaved correctly, not that every possible
   workload must.
3. **Deadlock freedom is argued, not proven.** Canonical ordering makes a cycle
   among transfer postings impossible by construction, and no deadlock has been
   observed. Other statements against these tables — an administrative update,
   a future feature — are not bound by that ordering.
4. **Retries are per-process.** Nothing coordinates retry storms across
   replicas; each process backs off independently.
5. **No retry metrics are exported.** Counts are returned in-process via
   `transfer.Attempts` and consumed by tests and the load generator.
   Prometheus, OpenTelemetry and the rest belong to the observability phase and
   were deliberately not added here.
6. **Measurements are environment-specific.** All numbers come from one
   machine, described in each results file. They characterise this setup, not
   any production deployment.
