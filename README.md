# Realtime Payments Ledger

A production-style, real-time payments ledger in Go: double-entry accounting on
PostgreSQL with serializable transactions, Redis-backed idempotency, a gRPC
API, a transactional outbox feeding Kafka audit events, and distributed
tracing.

> **Status: Phase 2 — concurrency safety.**
> Accounts, transfers and ledger entries exist and are enforced by the
> database, and transfer posting is hardened against concurrent execution.
> Redis, Kafka, gRPC, authentication and tracing are **not implemented** —
> see [docs/roadmap.md](docs/roadmap.md).

## What works today

- Accounts with balances in integer minor units, one currency each
- Atomic double-entry transfer posting in a single `SERIALIZABLE` transaction
- The invariant `SUM(ledger_entries.amount_minor) = 0` enforced **by
  PostgreSQL**, via deferred constraint triggers that run at `COMMIT`
- Append-only ledger and immutable completed transfers, enforced by triggers
- Versioned up/down migrations, applied by an operator, never at boot
- PostgreSQL-backed readiness, separate from liveness
- Graceful shutdown that drains in-flight requests before closing the pool
- Both account rows locked with `SELECT ... FOR UPDATE` in **canonical UUID
  order**, so opposing `A → B` and `B → A` transfers cannot deadlock
- Bounded retry of serialization failures (`40001`) and deadlocks (`40P01`),
  classified by SQLSTATE; business rejections are never retried
- Reconciliation of stored balances against the ledger
- A concurrent load generator with machine-readable results

Not implemented: Redis idempotency, Kafka, the outbox, gRPC, OAuth2/JWT,
OpenTelemetry, Jaeger, Loki, Toxiproxy.

**Measured, not claimed:** across three separate 1,000-attempt runs plus
opposing-direction and four-account variants, these runs recorded zero double
spends, zero negative balances, zero unbalanced ledgers and zero deadlocks,
with money conserved exactly. That is evidence from executed tests, not a
proof of impossibility — see
[docs/results/concurrency-1000.md](docs/results/concurrency-1000.md) for the
numbers and
[docs/CONCURRENCY.md](docs/CONCURRENCY.md#limitations) for the limits.

## Quick start

Requirements: Go 1.26+, Docker with Compose v2, GNU Make.

```sh
git clone <this repository>
cd Realtime-PaymentsLedger
cp .env.example .env

make infra-up          # PostgreSQL, Redis and Kafka; waits until healthy
make migrate-up        # create the ledger schema
make run               # run the API from source

curl localhost:8080/healthz
curl localhost:8080/readyz     # 200 once PostgreSQL is reachable
curl localhost:8080/version
```

To run everything, API container included:

```sh
make up                # build and start the whole stack
make migrate-up        # the API never migrates on its own
make logs
make down              # stop, keeping data volumes
make down-volumes      # stop and delete data
```

## PostgreSQL

PostgreSQL 16 is required — the application connects to it at start-up and
will report itself unready without it.

```sh
make migrate-up          # apply all pending migrations
make migrate-version     # current schema version
make migrate-down        # roll back exactly one migration
make migrate-down-all    # roll back everything (destroys data)
make migrate-redo        # down-all then up, proving both directions
make psql                # psql shell against the Compose database
```

Migrations live in [`migrations/`](migrations/) and are applied by
`cmd/migrate`. **The API process never migrates on start-up**: replicas would
race to change the schema, and a destructive operation should not hide inside a
routine restart.

Schema reference: [docs/DATABASE.md](docs/DATABASE.md).

## Operational endpoints

| Endpoint   | Purpose   | Behaviour                                                        |
| ---------- | --------- | ---------------------------------------------------------------- |
| `/healthz` | Liveness  | `200` whenever the process is serving. Never consults PostgreSQL — a database outage must not trigger a restart loop. |
| `/readyz`  | Readiness | Runs every registered check, including PostgreSQL. `200` when all pass, `503` when any fails, so the process leaves the load-balancer rotation without being killed. |
| `/version` | Metadata  | Build version, commit and service identity.                       |

These are the *operational* endpoints. Payment operations are served over gRPC
from Phase 4 onward; today the ledger is reachable only from Go code and tests.

## Money representation

Amounts are **integers in minor units** — cents for USD, so $10.25 is `1025`,
stored as `BIGINT`. Floating point is never used for money anywhere: `float32`,
`float64`, `REAL` and `DOUBLE PRECISION` cannot represent decimal fractions
exactly, and the error compounds into money that does not exist.

Every amount carries an ISO 4217 currency code, validated as three upper-case
letters in Go and by a `CHECK` constraint in PostgreSQL. Currencies are never
converted implicitly; a cross-currency transfer is rejected.

New accounts always start at `balance_minor = 0`. An account that came into
existence already holding money would be a credit with no matching debit. There
is deliberately **no production code path that puts money into the system** —
funding becomes an explicit deposit with its own ledger entries in a later
phase.

Full explanation: [docs/LEDGER_DESIGN.md](docs/LEDGER_DESIGN.md).

## Development

```sh
make help                  # list every target
make ci                    # fast gate: fmt-check, vet, build, test, test-race
make test                  # fast suites only, no infrastructure needed
make test-integration      # PostgreSQL integration tests (needs make infra-up)
make test-concurrency      # deterministic concurrency tests, verbose
make test-concurrency-race # the same under the race detector
make verify                # everything: ci + compose config + integration + race
make cover-html            # coverage report at coverage.html
make binary                # build bin/api with version metadata
```

### Stress testing

`cmd/stress` is development tooling. It drives `internal/transfer` in-process
(there is no transfer API yet) and creates and funds its own accounts, so point
it at a scratch database:

```sh
POSTGRES_DB=ledger_stress go run ./cmd/migrate up
make stress ATTEMPTS=100 POSTGRES_DB=ledger_stress
make stress-1000 POSTGRES_DB=ledger_stress
make stress-json ATTEMPTS=500 SCENARIO=opposing POSTGRES_DB=ledger_stress
```

Scenarios are `oneway`, `opposing` and `ring`, all deterministic. The runner
exits non-zero if any invariant was violated.

### Test layout

Integration tests are guarded by the `integration` build tag, so the default
path stays fast and needs no infrastructure:

```sh
go test ./...                        # fast: unit + in-process end-to-end
go test -tags=integration ./...      # adds the PostgreSQL integration tests
```

Integration tests run against a **real PostgreSQL**, never mocks — the
behaviour under test (transaction atomicity, deferred constraint triggers,
`CHECK` constraints) belongs to the database. They use their own database from
`POSTGRES_TEST_DB` (default `ledger_test`), created and migrated automatically,
so they can never touch development data. If PostgreSQL is missing they fail
loudly rather than skipping.

`make ci` is the gate every commit must pass. See [AGENTS.md](AGENTS.md) for the
full contributor contract.

## Configuration

Everything comes from the environment; there are no config files or flags.
Invalid configuration stops the process at start-up and reports **every**
problem at once rather than the first. Secrets are masked before any value is
logged.

Full reference: [docs/configuration.md](docs/configuration.md). Local defaults:
[.env.example](.env.example).

## Project layout

```
cmd/api/            Process entrypoint: wiring and signal handling only
cmd/migrate/        Schema migration runner (operator-run, never at boot)
cmd/stress/         Concurrent load generator (development tooling)
internal/config/    Environment parsing, defaulting and validation
internal/database/  pgx pool: construction, verification, readiness, shutdown
internal/money/     Currency type and minor-unit representation rules
internal/account/   Account records and persistence
internal/transfer/  Atomic double-entry posting, row locking, retry policy
internal/ledger/    Read-only ledger entry access
internal/reconcile/ Balance and ledger reconciliation checks
internal/health/    Concurrency-safe dependency check registry
internal/httpapi/   Operational HTTP endpoints and server lifecycle
migrations/         Versioned up/down SQL migrations
tests/              End-to-end, integration and concurrency tests
docs/               Architecture, ledger design, database and configuration
.github/workflows/  CI: lint, test, race, integration, concurrency, Compose,
                    image; plus a manual-only stress workflow
```

Documentation:

| Document                                       | Contents                                            |
| ---------------------------------------------- | --------------------------------------------------- |
| [docs/LEDGER_DESIGN.md](docs/LEDGER_DESIGN.md) | Double-entry accounting, money representation, invariants and exactly what enforces each one |
| [docs/CONCURRENCY.md](docs/CONCURRENCY.md)     | Locking, retries, deadlock prevention, reconciliation and limitations |
| [docs/results/](docs/results/)                 | Measured stress results, with the environment they came from |
| [docs/DATABASE.md](docs/DATABASE.md)           | Tables, constraints, indexes, triggers, migrations, transaction boundaries |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)   | Package layout and design decisions                 |
| [docs/configuration.md](docs/configuration.md) | Every environment variable                          |
| [docs/roadmap.md](docs/roadmap.md)             | What lands in which phase                           |

## Local infrastructure

| Service    | Image                | Host port | Used by the app? |
| ---------- | -------------------- | --------- | ---------------- |
| PostgreSQL | `postgres:16-alpine` | 5432      | **Yes**          |
| Redis      | `redis:7-alpine`     | 6379      | No — later phase |
| Kafka      | `apache/kafka:3.8.0` | 29092     | No — later phase |
| API        | built from source    | 8080      | —                |

Redis and Kafka are provisioned so later phases build against a stable
environment. Nothing in the application connects to them yet.

## Technology

Go · PostgreSQL · Redis · Kafka · gRPC · OAuth2/JWT · OpenTelemetry · Jaeger ·
Loki · Toxiproxy · Docker Compose · GitHub Actions

Implemented so far: Go, PostgreSQL, Docker Compose, GitHub Actions. Three
direct Go dependencies — `pgx/v5`, `google/uuid` and `golang-migrate` — with
the standard library covering logging, HTTP, randomness and error handling.

## Repository rules

This repository is developed in reviewed phases. Automated contributors must
not push, merge, force-push or publish releases; the maintainer pushes reviewed
work manually. The full contract is in [AGENTS.md](AGENTS.md).
