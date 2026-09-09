# Realtime Payments Ledger

A production-style, real-time payments ledger in Go: double-entry accounting on
PostgreSQL with serializable transactions, Redis-backed idempotency, a gRPC
API, a transactional outbox feeding Kafka audit events, and distributed
tracing.

> **Status: Phase 4 — gRPC API with JWT authentication.**
> The ledger is exposed as a versioned gRPC service, secured with RS256 JWT
> validation and scope-based authorization. Kafka, the transactional outbox and
> tracing are **not implemented** — see [docs/roadmap.md](docs/roadmap.md).

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
- **Idempotent transfers**: a client-supplied key means one logical payment
  produces at most one financial transfer, however many times it is retried
- Redis coordination (`SET NX EX` claim, cached results) with a **UNIQUE
  constraint in PostgreSQL as the final barrier** — deduplication survives
  Redis being unavailable, flushed or expired
- SHA-256 request fingerprints, so reusing a key for a different payment is
  refused rather than silently returning the first result
- A versioned **gRPC API** (`payments.v1.PaymentsService`) over the same
  service layer — the transport adds validation and conversion, nothing else
- **JWT (RS256) access-token validation** with issuer, audience, expiry and
  not-before checks and an explicit algorithm allow-list
- **Scope-based authorization** enforced by an interceptor, so an unauthorized
  request never reaches business code
- Deliberate domain-error to gRPC-status mapping that leaks no internals

Not implemented: Kafka, the transactional outbox, OpenTelemetry, Jaeger, Loki,
Toxiproxy. This service **validates** OAuth2-style JWTs; it is **not an OAuth2
authorization server** — see [docs/AUTHENTICATION.md](docs/AUTHENTICATION.md).

**Measured, not claimed:** across three separate 1,000-attempt runs plus
opposing-direction and four-account variants, these runs recorded zero double
spends, zero negative balances, zero unbalanced ledgers and zero deadlocks,
with money conserved exactly. That is evidence from executed tests, not a
proof of impossibility — see
[docs/results/concurrency-1000.md](docs/results/concurrency-1000.md) for the
numbers and
[docs/CONCURRENCY.md](docs/CONCURRENCY.md#limitations) for the limits.

For idempotency: across five consecutive runs under the race detector, 12
simultaneous requests sharing one idempotency key produced **exactly one
transfer, one debit, one credit and one distinct transfer ID**, with no data
races. Deliberately removing the database constraint made the same tests
produce two transfers, which is what shows where the guarantee actually lives.
See [docs/results/idempotency-concurrency.md](docs/results/idempotency-concurrency.md).

Repeated through the real gRPC transport: 12 simultaneous authenticated
`CreateTransfer` RPCs sharing one idempotency key produced **one transfer, one
debit, one credit and one distinct transfer ID** in five consecutive runs under
the race detector. See
[docs/results/grpc-idempotency-concurrency.md](docs/results/grpc-idempotency-concurrency.md).

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

To call the gRPC API you need a token. `make devtoken` generates a development
keypair and prints the configuration to use:

```sh
make devtoken                  # writes ./.devkeys, prints export lines
# export the printed JWT_ISSUER / JWT_AUDIENCE / JWT_PUBLIC_KEY, then:
make run

export TOKEN=$(go run ./cmd/devtoken -key ./.devkeys/private.pem -quiet)
grpcurl -plaintext -H "authorization: Bearer $TOKEN" \
  -d '{"currency":"USD"}' \
  localhost:9090 payments.v1.PaymentsService/CreateAccount
```

Full worked examples: [docs/API.md](docs/API.md).

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
| `/readyz`  | Readiness | Runs every registered check. PostgreSQL is **required** — `503` if it fails. Redis is **optional**: a failure marks the response `degraded` but keeps `200`, because the service stays correct without it. |
| `/version` | Metadata  | Build version, commit and service identity.                       |

These are the *operational* endpoints, on `:8080`. Payment operations are
served over gRPC on `:9090` — see [docs/API.md](docs/API.md). The standard gRPC
health service is also registered and needs no token.

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
make test-idempotency      # Redis idempotency tests
make test-idempotency-race # the same under the race detector
make test-grpc             # gRPC transport, authentication, authorization
make test-grpc-race        # the same under the race detector
make proto                 # regenerate protobuf code
make proto-check           # fail if the committed generated code is stale
make devtoken              # mint a development JWT and print the public key
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
cmd/devtoken/       Development JWT minting (never deployed)
api/proto/          Protobuf service definitions
internal/gen/       Generated protobuf and gRPC code (committed, CI-verified)
internal/auth/      JWT verification, principal and scopes
internal/grpcapi/   gRPC transport: interceptors, handlers, error mapping
internal/config/    Environment parsing, defaulting and validation
internal/database/  pgx pool: construction, verification, readiness, shutdown
internal/money/     Currency type and minor-unit representation rules
internal/account/   Account records and persistence
internal/transfer/  Atomic double-entry posting, row locking, retry policy
internal/ledger/    Read-only ledger entry access
internal/reconcile/ Balance and ledger reconciliation checks
internal/idempotency/ Key validation, fingerprints, Redis coordination records
internal/redisclient/ Redis connection ownership
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
| [docs/IDEMPOTENCY.md](docs/IDEMPOTENCY.md)     | Idempotency keys, fingerprints, Redis state machine, duplicate recovery and failure behaviour |
| [docs/API.md](docs/API.md)                     | gRPC service, RPCs, money representation, error codes, grpcurl examples |
| [docs/AUTHENTICATION.md](docs/AUTHENTICATION.md) | JWT validation, the OAuth2 relationship, scopes, key configuration, limitations |
| [docs/results/](docs/results/)                 | Measured stress results, with the environment they came from |
| [docs/DATABASE.md](docs/DATABASE.md)           | Tables, constraints, indexes, triggers, migrations, transaction boundaries |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)   | Package layout and design decisions                 |
| [docs/configuration.md](docs/configuration.md) | Every environment variable                          |
| [docs/roadmap.md](docs/roadmap.md)             | What lands in which phase                           |

## Local infrastructure

| Service    | Image                | Host port | Used by the app? |
| ---------- | -------------------- | --------- | ---------------- |
| PostgreSQL | `postgres:16-alpine` | 5432      | **Yes** — financial source of truth |
| Redis      | `redis:7-alpine`     | 6379      | **Yes** — idempotency coordination only |
| Kafka      | `apache/kafka:3.8.0` | 29092     | No — later phase |
| API        | built from source    | 8080 (HTTP), 9090 (gRPC) | — |

Kafka is provisioned so a later phase builds against a stable environment;
nothing connects to it yet. Redis is used for request coordination and is
never a financial correctness boundary — a Redis outage degrades speed, not
safety, and `/readyz` reports it as `degraded` rather than unready.

## Technology

Go · PostgreSQL · Redis · Kafka · gRPC · OAuth2/JWT · OpenTelemetry · Jaeger ·
Loki · Toxiproxy · Docker Compose · GitHub Actions

Implemented so far: Go, PostgreSQL, Redis, gRPC, JWT, Docker Compose, GitHub
Actions. Seven direct Go dependencies — `pgx/v5`, `google/uuid`,
`golang-migrate`, `go-redis/v9`, `grpc`, `protobuf` and `golang-jwt/jwt/v5` —
with the standard library covering logging, HTTP, hashing, randomness and error
handling.

## Repository rules

This repository is developed in reviewed phases. Automated contributors must
not push, merge, force-push or publish releases; the maintainer pushes reviewed
work manually. The full contract is in [AGENTS.md](AGENTS.md).
