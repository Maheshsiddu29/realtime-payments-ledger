# Architecture

## Current state (Phase 4)

The process serves a JWT-authenticated gRPC payments API and an HTTP
operational listener, backed by PostgreSQL and Redis, and shuts both down
cleanly.

```
                         Client
                           │  gRPC + Authorization: Bearer <JWT>
                           ▼
              ┌────────────────────────────┐
              │  logging interceptor       │
              └─────────────┬──────────────┘
                            ▼
              ┌────────────────────────────┐
              │  authentication interceptor│  RS256, iss/aud/exp/nbf
              │    fails ⇒ UNAUTHENTICATED │
              └─────────────┬──────────────┘
                            ▼
              ┌────────────────────────────┐
              │  authorization interceptor │  method → required scope
              │    fails ⇒ PERMISSION_DENIED
              └─────────────┬──────────────┘
                            ▼
              ┌────────────────────────────┐
              │  PaymentsService (thin)     │  validate · call · convert
              └─────────────┬──────────────┘
                            ▼
              ┌────────────────────────────┐
              │  application service layer │  account · transfer · ledger
              └─────────────┬──────────────┘
                            ▼
              ┌────────────────────────────┐
              │  idempotency coordination  │  SET NX EX · cached results
              └──────┬──────────────┬──────┘
                     ▼              ▼
                 PostgreSQL       Redis
            (source of truth)  (coordination)
```

Nothing above the service layer contains business logic: the transport
validates, calls one service method, and converts the result. There is exactly
one implementation of a transfer.

## Process lifecycle

```
                        ┌──────────────────────────────┐
   environment ────────▶│      internal/config         │
   variables            │  parse · default · validate  │
                        └──────────────┬───────────────┘
                                       │ Config (validated)
                                       ▼
   SIGINT/SIGTERM ──▶ ┌────────────────────────────────┐
                      │           cmd/api              │
                      │  wiring · logger · signals     │
                      └───┬─────────────┬──────────┬───┘
                          │             │          │
                          ▼             ▼          │
          ┌───────────────────────┐  ┌──────────────────────┐
          │   internal/httpapi    │─▶│  internal/health     │
          │ /healthz · /readyz    │  │  check registry      │
          │ /version · shutdown   │  │  └── "postgres" ─────┼──┐
          └───────────────────────┘  └──────────────────────┘  │
                                                               │
                          ┌────────────────────────────────────┘
                          ▼
          ┌───────────────────────────┐
          │    internal/database      │   pgxpool
          │  New · Verify · Ping      │
          └─────────────┬─────────────┘
                        │ *pgxpool.Pool
        ┌───────────────┼────────────────┐
        ▼               ▼                ▼
 ┌──────────────┐ ┌──────────────┐ ┌──────────────┐
 │ int/account  │ │ int/transfer │ │  int/ledger  │
 │ Create · Get │ │ Post · Get   │ │ EntriesFor…  │
 └──────┬───────┘ └──────┬───────┘ └──────┬───────┘
        │                │                │
        └────────────────┼────────────────┘
                         ▼
              ┌─────────────────────┐
              │     PostgreSQL      │
              │ accounts            │
              │ transfers           │      ◀── schema applied by cmd/migrate,
              │ ledger_entries      │          never by the API process
              │ + deferred triggers │
              └─────────────────────┘
```

Redis is now connected: it coordinates idempotent requests. It is deliberately
**not** part of the financial source of truth — the UNIQUE constraint on
`transfers.idempotency_key` is what prevents duplicate transfers, and it keeps
working while Redis is down. See [IDEMPOTENCY.md](IDEMPOTENCY.md).

Kafka is still provided by Docker Compose but **the application does not
connect to it.** It exists so later phases build against a stable local
environment.

## Package layout

| Package             | Responsibility                                                        |
| ------------------- | --------------------------------------------------------------------- |
| `cmd/api`           | Entrypoint. Wiring and signal handling only; no business logic.        |
| `cmd/migrate`       | Applies and reverses schema migrations. Run by an operator, not at boot.|
| `cmd/stress`        | Development tooling: concurrent load generator. Never deployed.         |
| `cmd/devtoken`      | Development tooling: mints test JWTs. Never deployed, not an authorization server. |
| `internal/config`   | Environment parsing, defaulting and validation. The only reader of env.|
| `internal/database` | Owns the pgx pool: construction, verification, readiness, shutdown.    |
| `internal/money`    | Currency type and validation. Minor-unit representation rules.         |
| `internal/account`  | Account records and their persistence.                                 |
| `internal/transfer` | Atomic double-entry posting, row locking and the retry policy.         |
| `internal/ledger`   | Read-only access to ledger entries.                                    |
| `internal/reconcile`| Verifies stored balances and ledger entries against each other.        |
| `internal/idempotency`| Key validation, request fingerprints and the Redis coordination record. No financial logic. |
| `internal/redisclient`| Owns the Redis connection.                                           |
| `internal/auth`     | JWT verification, the authenticated principal and scopes.              |
| `internal/grpcapi`  | gRPC transport: service registration, interceptors, error mapping, lifecycle. |
| `internal/gen`      | Generated protobuf and gRPC code. Never edited by hand.                |
| `internal/health`   | Concurrency-safe registry of named dependency checks.                  |
| `internal/httpapi`  | Operational HTTP surface and server lifecycle.                         |
| `tests`             | End-to-end, integration and concurrency tests.                         |

Everything sits under `internal/`, so no package can be imported by an outside
module. The public contract of this system is its API, not its Go types.

## Design decisions

### Configuration is validated once, at start-up

`config.Load` reports **every** problem it finds rather than the first, so a
misconfigured deployment is fixed in one pass instead of one restart per typo.
Secrets are masked by `Config.Redacted()` and `Postgres.RedactedDSN()` before
anything is logged; the raw password never reaches a log line.

Production is treated differently from development: `POSTGRES_SSLMODE=disable`
is rejected outright when `APP_ENV=production`.

### Liveness and readiness are different questions

- `/healthz` (liveness) asks *is this process working?* It never consults a
  dependency. A database outage must not cause an orchestrator to restart every
  API pod — that turns a dependency outage into an availability incident.
- `/readyz` (readiness) asks *should traffic be routed here?* It runs every
  registered check and returns `503` if any fails, so the process leaves the
  load-balancer rotation without being killed.

Phase 1 registers PostgreSQL as the first real check, so `/readyz` is now a
genuine signal rather than a formality.

The check registry bounds the whole evaluation with a timeout and contains
panics, so a single wedged or buggy dependency probe cannot stall or crash the
readiness path.

### An unreachable database is not fatal at start-up

Two failure modes are treated differently:

- **Invalid configuration** — a malformed DSN or an unusable pool setting — is
  fatal. No amount of waiting fixes it, so the process exits with a clear error.
- **An unreachable database** is not fatal. The process starts, logs the
  failure at `ERROR`, and reports itself unready. An orchestrator therefore
  withholds traffic instead of restarting the pod in a crash loop, and the
  service recovers by itself once PostgreSQL comes back.

This is visible behaviour, not silent degradation: a deployment whose database
is misconfigured never goes green.

### The schema is applied by an operator, never at boot

`cmd/migrate` is a separate binary. Migrating on start-up would make replicas
race to change the schema, tie the schema version to whichever build booted
first, and hide a destructive operation inside a routine restart.

### Money movement is one transaction

`transfer.Service.Post` holds a single `SERIALIZABLE` transaction across the
balance reads, both balance updates, both ledger inserts, the invariant check
and the completion update. Either all of it is durable or none of it is.

PostgreSQL enforces the double-entry invariant itself, using deferred
constraint triggers that run at `COMMIT`. That enforcement is not an
application convention — it applies to `psql` sessions and future services
equally. See [LEDGER_DESIGN.md](LEDGER_DESIGN.md).

Both account rows are locked with `SELECT ... FOR UPDATE` before any balance
changes, in **canonical UUID order** rather than transfer direction, so that
`A → B` and `B → A` request the same rows in the same sequence and cannot
deadlock. Lock acquisition order and business roles are kept strictly separate:
the debit always lands on the source, whichever row was locked first.
Serialization failures are retried under a bounded policy outside the
transaction. See [CONCURRENCY.md](CONCURRENCY.md).

### The transport is thin, and the security checks precede it

Authentication and authorization are unary interceptors, so both run before any
handler. That makes "an unauthorized request cannot create financial state" a
structural property rather than something each handler must remember: a
rejected call never reaches the idempotency layer, never claims a key, never
opens a transaction.

The scope policy is a single method-to-scope table. An RPC missing from it is
denied, so adding an RPC without deciding its permission fails closed, and a
test walks the generated service descriptor to catch that at build time.

Domain errors are mapped to gRPC codes in one deliberate place. Nothing
internal — SQLSTATEs, constraint names, driver text, key material — reaches a
client; unmapped errors become `INTERNAL` with a fixed message and the detail
goes to the log.

### Two listeners, one lifecycle

HTTP serves operational probes for infrastructure; gRPC serves payments for
clients. Different audiences, different exposure, different ports. Both are
started together, both receive the shutdown signal, and the process waits for
both to drain before the connection pools close — so an in-flight transfer
finishes committing rather than losing its connection mid-transaction. The gRPC
stop races `GracefulStop` against the configured budget and falls back to
`Stop`, so shutdown always terminates.

### Redis coordinates, PostgreSQL decides

Idempotency is layered so that Redis can fail in any way without permitting a
duplicate payment. Redis makes duplicate detection fast — a single
`SET NX EX` claim and a cached result — while the UNIQUE constraint on
`transfers.idempotency_key` is what actually guarantees at most one transfer
per key.

Every Redis failure mode therefore degrades performance rather than
correctness, and each is tested: unavailable before the transfer (the request
falls back to PostgreSQL), unavailable after the commit, flushed, expired, and
lost because the process died between `COMMIT` and the cache write. All of them
recover the original transfer through the database.

Redis is never used to lock an account. Balance safety remains entirely the
row locking and `SERIALIZABLE` transactions of Phase 2, which are unchanged.

### The ledger package cannot write

`internal/ledger` exposes no way to create, update or delete an entry. Entries
are written only by the transfer posting, inside the transaction that moves the
balances, so a movement and its record cannot come apart. The write lives in
`internal/transfer` rather than behind an exported `ledger.CreateEntry`
precisely so that no other code can reach it.

### Shutdown is graceful and bounded

`signal.NotifyContext` converts SIGTERM and SIGINT into context cancellation.
The server stops accepting connections and drains in-flight requests within
`HTTP_SHUTDOWN_TIMEOUT`; anything still running when the budget expires is
forcibly closed rather than leaked. The connection pool is closed after the
server has drained, so in-flight requests keep their connections until they
finish.

This matters more here than in most services: a payment request cut off
mid-flight leaves a client unable to tell whether it succeeded.

### Dependencies are added by the phase that needs them

Phase 0 had none. Phase 1 adds exactly three direct dependencies:

| Module                            | Why                                      |
| --------------------------------- | ---------------------------------------- |
| `github.com/jackc/pgx/v5`         | PostgreSQL driver and connection pool.   |
| `github.com/google/uuid`          | UUID generation for identifiers.         |
| `github.com/golang-migrate/migrate/v4` | Schema migrations, used only by `cmd/migrate` and the test harness. |
| `github.com/redis/go-redis/v9`    | Redis client for idempotency coordination. |
| `google.golang.org/grpc`          | The API transport.                        |
| `google.golang.org/protobuf`      | Wire format and generated code runtime.   |
| `github.com/golang-jwt/jwt/v5`    | JWT verification. Token cryptography is never hand-rolled. |

Structured logging is still `log/slog`, and the HTTP surface is still
`net/http`.

## Local infrastructure

| Service    | Image                | Host port | Used by the app? |
| ---------- | -------------------- | --------- | ---------------- |
| PostgreSQL | `postgres:16-alpine` | 5432      | **Yes**          |
| Redis      | `redis:7-alpine`     | 6379      | **Yes** — idempotency coordination |
| Kafka      | `apache/kafka:3.8.0` | 29092     | No — later phase |
| API        | built from source    | 8080      | —                |

PostgreSQL defaults to `SERIALIZABLE` at the server level so that a `psql`
session behaves like the application does, and logs lock waits and slow
statements to make concurrency debugging possible in Phase 2. Redis uses
`noeviction` because silently evicting an idempotency key would allow a
duplicate payment. Kafka disables topic auto-creation so that a typo in a topic
name fails loudly instead of creating a phantom stream.

## What is deliberately absent

OAuth2/JWT authentication, the gRPC server, the transactional outbox, the
Kafka producer, OpenTelemetry tracing, and chaos testing. Each belongs to a later phase; see [roadmap.md](roadmap.md).

Retry counts are returned in-process through `transfer.Attempts` and consumed
by tests and the load generator. No metrics are exported: Prometheus and
OpenTelemetry belong to the observability phase and were deliberately not added
here just to count retries.
