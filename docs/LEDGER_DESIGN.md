# Ledger design

This document explains how money is represented and moved, in plain language.
It is the reference for what the system guarantees — and, just as importantly,
what it does not.

## Double-entry accounting in one page

Every movement of money is recorded twice: once as money leaving somewhere, and
once as money arriving somewhere. The two records are written together or not
at all.

Alice sends Bob $100.00:

| Account | Entry     | Meaning                     |
| ------- | --------- | --------------------------- |
| Alice   | `-10000`  | debit — money left Alice    |
| Bob     | `+10000`  | credit — money reached Bob  |
|         | **`0`**   | the two always cancel out   |

The point of writing it twice is that the books can be checked. If the entries
for a transfer do not sum to zero, money was invented or destroyed, and that is
a bug you want to find at the moment it happens rather than during an audit
months later.

### Debits and credits are signs, not columns

Many ledgers use two columns, `debit` and `credit`, each holding a positive
number. This system uses one signed column instead:

- **negative** `amount_minor` = a debit, money leaving the account
- **positive** `amount_minor` = a credit, money arriving

One signed column makes the invariant a plain sum — `SUM(amount_minor) = 0` —
instead of a comparison between two aggregates, and it makes the "does not
balance" check something the database can evaluate directly.

## Money is integers, never floating point

All amounts are **integers in minor units**: the smallest unit the currency
has. For USD that is cents.

| Amount    | Stored as |
| --------- | --------- |
| $10.25    | `1025`    |
| $0.01     | `1`       |
| $1,000.00 | `100000`  |

The column type is `BIGINT`, which holds roughly ±9.2 × 10^18 — about 92
quadrillion dollars in cents. That is not a limit anyone will reach.

**Floating point is never used for money, anywhere.** Not `float32`, not
`float64`, not `REAL`, not `DOUBLE PRECISION`. The reason is that binary
floating point cannot represent most decimal fractions exactly. In Go:

```go
0.1 + 0.2          // 0.30000000000000004, not 0.3
```

One such error is invisible. A million of them, compounded across a ledger,
is money that does not exist. Integers have no such problem: `1025` is exactly
`1025`, and adding integers is exact.

The only place a decimal point should ever appear is in the user interface,
when a minor-unit integer is formatted for a human to read.

### Currency

An amount without a currency is meaningless, so every account, transfer and
ledger entry carries an ISO 4217 code such as `USD` or `EUR`. Codes are
validated as exactly three upper-case letters, in Go by `money.Currency` and in
PostgreSQL by a `CHECK` constraint.

**Currencies are never converted implicitly.** A transfer between a USD account
and a EUR account is rejected, not guessed at. A real conversion needs an
exchange rate, a rate source, a timestamp and two additional ledger entries to
record the spread — none of which this phase models.

## The account balance model

An account holds a balance in exactly one currency.

```
accounts
  id             UUID
  currency       VARCHAR(3)   'USD'
  balance_minor  BIGINT       75000   -- $750.00
```

The balance is stored, not recomputed from the ledger on every read. That is a
deliberate trade-off:

- **Stored balance**: reading a balance is a single-row lookup. The risk is
  that the balance and the ledger could disagree.
- **Derived balance** (`SUM` over all entries for the account): they can never
  disagree, but every read scans the account's whole history.

This system stores the balance and keeps it honest by only ever changing it in
the same transaction that writes the matching ledger entries. A later phase can
add a reconciliation job that verifies stored balances against the ledger; the
data needed for it is already there.

### Accounts start at zero

`CreateAccount` always produces `balance_minor = 0`, and there is no
application code path that sets a balance to anything else.

This follows directly from double-entry: if a new account could be created
already holding $500, that $500 would be a credit with no matching debit. The
books would not balance from the very first row, and no later check could tell
you where the money came from.

Real funding is a transfer from somewhere else — a bank's own funding or
settlement account. A later phase models that as an explicit deposit with its
own pair of ledger entries. Until then, **the production system has no way to
put money into itself**, which is the safe state to be in.

Integration tests need funded accounts, so there is a test-only `fund` helper
that writes a balance directly. It lives in `tests/harness_test.go`, has no
equivalent in any non-test package, and is documented there as creating money
from nothing. It exists to set up a starting state, and would be an obvious
bug anywhere else.

## The transfer lifecycle

A transfer is the intent to move money, plus the record of whether it happened.

```
    pending ──────▶ completed        (money moved, entries written)
       │
       └──────────▶ (nothing)        (rejected: the whole transaction rolls back)
```

`pending` exists only inside the posting transaction — no caller ever observes
it, because the transaction either completes or disappears. `failed` is defined
in the schema for later phases (where a transfer may be recorded as attempted
and rejected); Phase 1 never writes it.

Posting runs these steps inside **one** database transaction:

1. validate the command — amount positive, currency well-formed, accounts differ
2. load the source account
3. load the destination account
4. reject if either does not exist
5. reject if the two account currencies differ
6. reject if the transfer currency does not match them
7. reject if the source balance is below the amount
8. insert the transfer row as `pending`
9. debit the source with a guarded `UPDATE`
10. credit the destination
11. insert the debit and credit ledger entries
12. verify in Go that the entries sum to zero and number exactly two
13. mark the transfer `completed`
14. commit — PostgreSQL re-checks the balance invariant here

If anything fails at any step, the transaction rolls back and **nothing**
persists: no transfer row, no ledger entries, no balance change.

### Worked example

Alice has $1,000.00, Bob has $500.00. Alice sends Bob $250.00.

Before:

```
Alice  100000
Bob     50000
total  150000
```

Ledger entries written:

```
transfer 7f3a…  Alice  -25000 USD
transfer 7f3a…  Bob    +25000 USD
                sum         0    <- balanced
```

After:

```
Alice   75000
Bob     75000
total  150000    <- unchanged
```

The total is the same before and after. That is **conservation of money**, and
it is asserted directly by `TestMoneyIsConservedAcrossATransfer`.

## The ledger is append-only

A ledger entry is never updated or deleted. It is a historical record of
something that happened, and history does not change.

To correct a mistake you post a **reversing entry** — a new transfer in the
opposite direction — so the original error and its correction are both visible.
This is what makes an audit trail worth having.

This is enforced by a PostgreSQL trigger that rejects `UPDATE` and `DELETE` on
`ledger_entries` outright. Completed transfers are protected the same way.

## The invariants, and what enforces each one

This is the section to read carefully, because it is easy to overstate what a
database guarantees.

### Invariant 1 — every transfer's ledger entries sum to zero

**Enforced by PostgreSQL.**

A plain `CHECK` constraint cannot express this. `CHECK` is evaluated per row
against that row's own values; it cannot aggregate across rows, and PostgreSQL
rejects subqueries inside `CHECK` precisely because the result would not be
re-validated when *other* rows change. A constraint like
`CHECK (SUM(amount_minor) = 0)` is not valid SQL here and would be a lie if it
were.

The correct tool is a **deferred constraint trigger**:

```sql
CREATE CONSTRAINT TRIGGER ledger_entries_transfer_must_balance
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION ledger_entries_assert_balanced();
```

`DEFERRABLE INITIALLY DEFERRED` means the check runs at `COMMIT`, not after
each statement. That is what makes it usable: the debit and the credit are
inserted one after the other, and the intermediate state — one entry, not yet
balanced — is allowed to exist inside the transaction. By the time the
transaction commits, the entries must balance, or **the `COMMIT` itself fails**
and the whole transaction is rolled back.

This holds for anything that talks to the database: this Go code, a `psql`
session, a future service, a buggy script. It is not an application convention.

A second constraint trigger on `transfers` closes the complementary hole: a
transfer marked `completed` with no entries at all, which the first trigger
would never see because nothing was inserted into `ledger_entries`.

Both are proven by tests that bypass the application and write raw SQL:
`TestDatabaseRejectsUnbalancedLedgerEntriesAtCommit` and
`TestDatabaseRejectsCompletedTransferWithNoEntries`.

### Invariant 2 — balances never go negative

**Enforced by PostgreSQL**, via `CHECK (balance_minor >= 0)` on `accounts`.

The application also checks the balance before debiting, and the debit itself
is a guarded update:

```sql
UPDATE accounts SET balance_minor = balance_minor - $2
WHERE id = $1 AND balance_minor >= $2
```

The application check exists to produce a precise error message. The guarded
`UPDATE` and the `CHECK` constraint are the actual authority. This was verified
by deleting the application check and confirming that the database still
refuses the overdraft.

### Invariant 3 — a movement and its record are inseparable

**Enforced by PostgreSQL transactions.** Balance changes and ledger entries are
written in the same transaction, so there is no committed state in which one
exists without the other.

### Invariant 4 — history is immutable

**Enforced by PostgreSQL triggers** on `ledger_entries` (all `UPDATE` and
`DELETE`) and on `transfers` (any mutation of a `completed` row).

**Limitation:** these are row-level triggers, so they do not fire for
`TRUNCATE`, and nothing stops a superuser from dropping the table or disabling
the trigger. Append-only here means "no application or ordinary SQL session can
rewrite history", not "the bytes are physically immutable". Genuine
tamper-evidence needs restricted database roles and off-box archival, which are
operational concerns beyond this phase.

### Invariant 5 — exactly two entries per transfer

**Enforced partly in PostgreSQL, partly in Go.**

- PostgreSQL: `UNIQUE (transfer_id, account_id)` means a transfer cannot post
  two entries against the same account. Combined with
  `CHECK (amount_minor <> 0)` and Invariant 1, a balanced transfer must have at
  least two entries, since one non-zero entry cannot sum to zero.
- Go: the posting asserts `COUNT(*) = 2` before completing.

The database permits more than two balanced entries, deliberately: multi-leg
postings (a transfer plus a fee, say) are a natural extension, and encoding
"exactly two" into the schema would have to be undone later.

### What is enforced in Go only

- Currency codes are validated before reaching the database — though the
  `CHECK` constraints would catch a bad code anyway.
- The "exactly two entries" count, as above.
- Precise error classification (`ErrInsufficientFunds` vs
  `ErrCurrencyMismatch`), so callers get a meaningful failure rather than a
  raw constraint violation.

## Transaction isolation, honestly

Transfers are posted at **`SERIALIZABLE`**, PostgreSQL's strongest isolation
level, set explicitly per transaction:

```go
tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
```

At this level PostgreSQL guarantees that concurrent transactions produce a
result equivalent to running them one after another. When it cannot guarantee
that, it aborts one of them with a serialization failure (SQLSTATE `40001`).

**What this phase does not do, and does not claim:**

- There is **no retry loop**. A serialization failure is returned to the caller
  as an error. Under contention a caller will see failures that a retry would
  have resolved.
- There is **no explicit row locking**. The posting reads accounts without
  `SELECT ... FOR UPDATE`, relying on `SERIALIZABLE` plus the guarded `UPDATE`.
- There is **no deterministic lock ordering**, so nothing here prevents
  deadlocks between transfers touching the same pair of accounts in opposite
  directions.
- **No concurrency testing has been performed.** The correctness demonstrated
  by this phase's tests is single-threaded correctness. No claim is made about
  behaviour under concurrent load, and specifically **no claim is made about
  1,000 concurrent transfer attempts.**

Deterministic lock ordering, a bounded retry loop on `40001`, double-spend
tests and concurrency stress testing are Phase 2. Until those exist and pass,
treat this system as correct for sequential use and unproven under concurrency.
