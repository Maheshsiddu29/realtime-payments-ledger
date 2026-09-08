# Idempotency

How the ledger makes a repeated payment request produce at most one financial
transfer.

The guarantee, stated precisely:

> **The same idempotency key can never be associated with more than one
> transfer row.** A caller that retries the same request gets the original
> transfer back; a caller that reuses the key for a *different* payment is
> refused.

Measured results are in
[results/idempotency-concurrency.md](results/idempotency-concurrency.md).

## Why payments get retried

A client sends a payment and then hears nothing. The request may have
succeeded, may have failed, or may still be running — from the outside these
look identical. The only safe thing a client can do is ask again.

The failure that matters is a **lost response**:

```
Client                    Server                  PostgreSQL
  │                          │                          │
  │ ── pay $100 ──────────▶  │                          │
  │                          │ ── COMMIT ────────────▶  │  money moved
  │                          │ ◀───────── ok ────────── │
  │      ✗ response lost ◀── │                          │
  │                          │                          │
  │ ── pay $100 (retry) ──▶  │                          │
```

Without idempotency the retry moves the money a second time. The client cannot
tell the difference, so the server has to.

## What an idempotency key is

A string the client generates once per logical payment and reuses on every
retry of that payment. It is **opaque** to this system: no format is imposed,
so a UUID, a ULID or `pay_01HQ8XZ4...` are all fine. Only the bounds are
enforced — 1 to 255 bytes, valid UTF-8, no whitespace or control characters —
in Go by `idempotency.ParseKey` and again by a `CHECK` constraint in
PostgreSQL.

Whitespace and control characters are rejected rather than trimmed. A key with
a stray newline is almost always a client bug, and silently accepting it would
make two requests that look identical in a log resolve to different keys.

## Why Redis alone is not enough

Redis is fast and it is the right place to detect a duplicate in one round
trip. It is the wrong place to *guarantee* anything, because:

- it can be flushed;
- a key can expire;
- it can be unavailable at exactly the moment a retry arrives;
- a `COMMIT` can succeed and the process die before Redis is told.

Every one of those turns a Redis-only design into a duplicate payment. So the
final barrier is in PostgreSQL:

```sql
ALTER TABLE transfers
    ADD CONSTRAINT transfers_idempotency_key_unique UNIQUE (idempotency_key);
```

The key is written by the same `INSERT` that creates the transfer, so the
constraint decides the winner atomically. There is no window in which a key is
reserved but its transfer does not exist, and no second table to keep
consistent.

**This was verified by deliberately breaking it.** With the key no longer
persisted — leaving Redis as the only protection — flushing Redis and retrying
produced **two completed transfers**. With the constraint in place the same
test produces one. Redis is an optimisation; the constraint is the guarantee.

The column is nullable, and PostgreSQL treats NULLs as distinct in a UNIQUE
constraint, so transfers posted without a key — internal transfers, the load
generator, everything from Phases 1 and 2 — are unaffected.

## The request fingerprint

A key must not silently accept a *different* payment:

```
key = payment-123    A → B  $100      ← first request
key = payment-123    A → C  $900      ← must NOT return the first result
```

Every request is fingerprinted over its financial identity: source account,
destination account, amount and currency. The canonical form is explicit,
fully ordered and version-prefixed:

```
v1
source=11111111-1111-1111-1111-111111111111
destination=22222222-2222-2222-2222-222222222222
amount_minor=10000
currency=USD
```

SHA-256 of those bytes is the fingerprint. Nothing unstable is included — no
timestamps, no request ids, no map iteration — so the same logical payment
fingerprints identically in any process, on any host, at any time. The version
prefix means that if the covered fields ever change, the change is visible as
a version bump rather than as a mysterious conflict.

The fingerprint is stored **in both places**: hex in the Redis record, and raw
bytes in `transfers.request_fingerprint`. Storing it in PostgreSQL is what
lets a conflict still be detected after Redis has been flushed.

Same key + different fingerprint ⇒ `ErrIdempotencyConflict`, and the second
request is **not executed**.

## The Redis record

Namespaced, with the naming centralised in one place so it cannot drift:

```
idempotency:transfer:<key>
```

```json
{
  "state": "COMPLETED",
  "fingerprint": "9f86d081884c7d65...",
  "transfer_id": "3f2504e0-4f89-11d3-9a0c-0305e82c3301",
  "claimed_at": "2026-09-08T06:00:00Z",
  "completed_at": "2026-09-08T06:00:00.142Z"
}
```

### State machine

```
        (absent)
           │
           │ SET NX EX  ── someone else won ──▶ read their record
           ▼
      PROCESSING ──────── transfer committed ──────▶ COMPLETED
           │                                             │
           │ business rejection: claim released          │ TTL
           ▼                                             ▼
        (absent)                                      (absent)
                                        PostgreSQL still holds the key
```

Only two states, deliberately. A `FAILED` state was considered and rejected:
caching a failure would mean a payment refused for insufficient funds stays
refused after the account is topped up. So:

| Outcome | Cached? | Reasoning |
| --- | --- | --- |
| Transfer completed | **Yes** | A fact that cannot change. |
| Insufficient funds, unknown account, currency mismatch | **No** | The claim is released; the same key may legitimately succeed later. |
| Redis unavailable | **No** | Nothing to cache into. |

## The atomic claim

Ownership is taken with a single command:

```
SET idempotency:transfer:<key> <record> NX EX <processing-ttl>
```

A `GET` followed by a `SET` would be a race — two callers could both observe a
missing key, both decide they own it, and both post. `SET NX` decides a winner
inside Redis, in one round trip, with no lock to release and no Lua needed.

There is one narrow race left: `SET NX` can fail because a claim exists, and
the following `GET` can find nothing because that claim's lease expired in
between. The key is genuinely free at that point, so the claim is retried — a
bounded three attempts, not a spin. Reporting "in progress" there would stall a
payment on a claim that no longer exists. *(This was a real bug, found by a
test that expired a lease under a concurrent claim.)*

## TTLs

| Record | Default | Configured by | Purpose |
| --- | --- | --- | --- |
| `COMPLETED` | 24 h | `IDEMPOTENCY_TTL` | How long a replay can be answered without touching the database. |
| `PROCESSING` | 30 s | `IDEMPOTENCY_PROCESSING_TTL` | The lease on an in-flight request. |

**Redis expiry never weakens deduplication.** The completed record expiring
only means the next replay costs one indexed `SELECT` instead of a Redis
`GET` — the UNIQUE constraint has no TTL and still refuses a second transfer.
This is tested three ways: key deleted, key expired, whole database flushed.

The processing lease is what stops a crashed holder from blocking a key
forever. If the process that claimed a key dies, the claim expires after 30
seconds and another request may take over — and PostgreSQL uniqueness still
prevents that takeover from producing a second transfer.

Configuration is validated so the lease can never outlive the cached result,
which would leave a key blocking payments after the result it guards had gone.

## The request flow

```
                  Client
                    │  key=abc
                    ▼
        ┌───────────────────────┐
        │  SET NX EX  (claim)   │──── already held ──┐
        └───────────┬───────────┘                    │
                    │ acquired                       ▼
                    │                    ┌───────────────────────┐
                    │                    │ fingerprint differs?  │
                    │                    │   → CONFLICT, stop    │
                    │                    │ else look in Postgres │
                    │                    │   found → return it   │
                    │                    │   PROCESSING → 409    │
                    │                    └───────────────────────┘
                    ▼
        ┌───────────────────────────────────────┐
        │        PostgreSQL transaction         │
        │  SERIALIZABLE, rows locked in         │
        │  canonical UUID order                 │
        │    ├── transfer  (+ idempotency key)  │
        │    ├── balances                       │
        │    └── ledger entries                 │
        │              COMMIT                   │
        └───────────────────┬───────────────────┘
                            ▼
                 SET record = COMPLETED
                            ▼
                         Client
```

And the recovery path, which is the one that matters:

```
        PostgreSQL COMMIT succeeds
                    │
        Redis update fails, or the process dies
                    ✗
        Client retries with the same key
                    │
                    ▼
        INSERT hits transfers_idempotency_key_unique
                    │
                    ▼
        Load the original transfer by key
                    │
                    ▼
        Verify the stored fingerprint matches
                    │
                    ▼
        Return the same result, repair Redis
```

## Failure behaviour

### Redis unavailable *before* the transfer

**Policy: fall back to PostgreSQL and continue.** The payment is not refused.

This is a deliberate choice over failing closed. The UNIQUE constraint provides
the complete deduplication guarantee on its own, so refusing would trade
availability for nothing. What is lost is speed: duplicate detection costs a
database lookup instead of a Redis `GET`, and there is no cached response.

The result reports `CoordinationDegraded` so a caller — and the logs — can see
the service is running without its cache.

This is tested, not assumed: with Redis pointed at a dead port, a repeated
request still resolves to one transfer, and a *conflicting* request is still
rejected, because the fingerprint is in PostgreSQL too.

### Redis unavailable *after* the commit

The money has already moved and PostgreSQL holds the key, so a failure to
cache the result cannot fail the payment. It is logged, and the next retry
recovers from the database through the duplicate path above.

### The process crashes between COMMIT and the Redis update

Identical from the outside to the case above: the transfer and its key are
durable, Redis knows nothing. The retry hits the UNIQUE constraint, loads the
original transfer, verifies the fingerprint, and returns the same result — then
repairs the Redis record so the next replay is cheap.

Tested by posting through a service with no Redis at all, which reproduces
that state exactly, then retrying through a normal one.

### Another request is already PROCESSING

**Policy: check PostgreSQL, then report in progress immediately.** No polling.

Polling would hold a goroutine and a connection for the duration of somebody
else's transaction and would still have to give up eventually. The database is
checked first, because the holder may have committed without yet caching the
result — that check is what lets most concurrent duplicates resolve to the real
transfer rather than an error.

If PostgreSQL has nothing yet, the caller gets `ErrIdempotencyInProgress` and
decides for itself whether to retry. A retry moments later resolves, which the
concurrency test asserts explicitly.

## Redis is not a lock manager

Redis coordinates **requests**. It has nothing to do with account concurrency.

Balance safety is entirely Phase 2's: `SERIALIZABLE` transactions,
`SELECT ... FOR UPDATE` on both accounts in canonical UUID order, and bounded
retry on serialization failures. There is no `redis lock account:A` anywhere,
and there must never be — a distributed lock over a database row would be both
slower and weaker than the row lock the database already provides.

The two mechanisms are independent and both still apply: a test posts twenty
concurrent transfers with *distinct* keys against an account holding only ten
transfers' worth, and exactly ten succeed.

## Readiness

Redis is registered as an **optional** health check. `/readyz` reports it and
marks the response `degraded` when it is down, but does not return 503.

Withdrawing traffic from every replica because a dependency the process can
serve without is unavailable turns a partial outage into a total one. Since a
Redis outage costs speed and not correctness, the honest signal is "serving,
degraded" rather than "not ready". PostgreSQL remains a **required** check:
without it the process genuinely cannot serve.

## Limitations

1. **A conflicting request is refused, not queued.** A key reused for a
   different payment returns an error; the client must use a new key.
2. **Concurrent duplicates may be told "in progress".** They resolve on retry,
   but the first response is an error rather than the transfer. A short bounded
   wait would smooth this at the cost of holding a goroutine.
3. **Rejections are not cached**, so a client retrying an underfunded payment
   re-runs it against the database each time. That is intentional, but it means
   a hot rejected key is not free.
4. **The fingerprint covers only the financial identity** — source,
   destination, amount, currency. Two requests differing only in some future
   non-financial field would be treated as the same payment.
5. **No cross-region coordination.** Redis is a single logical store here.
6. **Keys are never garbage collected from PostgreSQL.** They live as long as
   the transfer rows do, which is correct for deduplication and means the
   column grows with the ledger.
