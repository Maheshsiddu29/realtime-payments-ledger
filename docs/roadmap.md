# Roadmap

The system is built in reviewed phases. Each phase ends with a human review;
work does not continue into the next phase automatically (see `AGENTS.md`).

## Phase 0 — Repository foundation ✅ complete

Module layout, environment-driven configuration with validation, health
mechanism (liveness/readiness), graceful shutdown, Docker Compose stack
(PostgreSQL, Redis, Kafka), Dockerfile, Makefile, CI.

No business domain, no database driver, no external clients.

## Phase 1 — Persistence and the double-entry core ✅ complete

Database connectivity and the accounting schema.

- pgx connection pool wired to the existing configuration, with a bounded
  connect timeout.
- `cmd/migrate` plus four versioned up/down migrations: `accounts`,
  `transfers`, `ledger_entries`, and the balance-invariant triggers.
- Amounts as `BIGINT` minor units with an explicit currency; deferred
  constraint triggers enforcing that every transfer's entries sum to zero.
- A PostgreSQL readiness check registered with `internal/health`, turning
  `/readyz` into a real signal.
- Repository layer and atomic transfer posting, with integration tests against
  a real PostgreSQL instance and a CI job to run them.

Explicitly **not** done in this phase: concurrency hardening. See
[LEDGER_DESIGN.md](LEDGER_DESIGN.md#transaction-isolation-honestly).

## Phase 2 — Transfers under concurrency ✅ complete

- Deterministic account lock ordering: both rows locked with
  `SELECT ... FOR UPDATE` in canonical UUID order, so opposing transfers
  cannot deadlock. Lock order is kept separate from source/destination roles.
- Bounded retry on SQLSTATE `40001` and `40P01`, classified through
  `pgconn.PgError`, with full-jitter backoff and context cancellation.
- `internal/reconcile`: stored balances checked against the ledger, plus
  per-transfer entry verification and negative-balance detection.
- Concurrency, deadlock-regression and double-spend integration tests, all
  barrier-synchronised and timeout-bounded.
- `cmd/stress`: a deterministic load generator emitting JSON results.
- Measured results in [results/concurrency-1000.md](results/concurrency-1000.md).

Design and limitations: [CONCURRENCY.md](CONCURRENCY.md).

Not done in this phase: per-account admission control, which the measurements
show is the real fix for a hot account.

## Phase 3 — Idempotency

- Redis-backed idempotency keys with a first-writer-wins claim.
- Replay of a completed request returns the original response; a concurrent
  duplicate is rejected rather than double-applied.
- Redis readiness check registered.

## Phase 4 — gRPC API and authentication

- gRPC service definitions and server, alongside the existing health listener.
- OAuth2/JWT validation as a gRPC interceptor, with scope-based authorization.
- gRPC health service mirroring `/readyz`.

## Phase 5 — Transactional outbox and Kafka audit events

- `outbox` table written in the same transaction as the ledger entries.
- Relay publishing to Kafka with at-least-once delivery and consumer-side
  deduplication.
- Kafka readiness check registered.

## Phase 6 — Observability

- OpenTelemetry tracing exported to Jaeger, spanning gRPC, PostgreSQL and Kafka.
- Structured logs shipped to Loki, correlated with traces by trace ID.
- Service metrics: transfer rate, serialization-retry rate, outbox lag.

## Phase 7 — Chaos and resilience testing

- Toxiproxy in front of PostgreSQL, Redis and Kafka.
- Tests asserting behaviour under latency, packet loss and hard disconnects:
  no double-spend, no lost audit event, correct readiness transitions.

## Phase 8 — CI/CD hardening

- Integration and chaos suites in CI against ephemeral services.
- Image publishing, vulnerability scanning, and migration checks on pull
  requests.
