# Roadmap

The system is built in reviewed phases. Each phase ends with a human review;
work does not continue into the next phase automatically (see `AGENTS.md`).

## Phase 0 — Repository foundation ✅ complete

Module layout, environment-driven configuration with validation, health
mechanism (liveness/readiness), graceful shutdown, Docker Compose stack
(PostgreSQL, Redis, Kafka), Dockerfile, Makefile, CI.

No business domain, no database driver, no external clients.

## Phase 1 — Persistence and the double-entry core

Database connectivity and the accounting schema.

- PostgreSQL driver and connection pool wired to the existing config.
- Migration runner and the first migrations: `accounts`, `transfers`,
  `ledger_entries`.
- Amounts as `BIGINT` minor units with an explicit currency; a database
  constraint enforcing that every transfer's entries sum to zero.
- A PostgreSQL readiness check registered with `internal/health`, turning
  `/readyz` into a real signal.
- Repository layer with integration tests against a real PostgreSQL instance.

## Phase 2 — Transfers under concurrency

- Transfer service performing balanced double-entry writes.
- `SERIALIZABLE` transactions with retry on serialization failure (`40001`).
- Row-level locking with a deterministic lock ordering to avoid deadlocks.
- Concurrency tests: parallel transfers over the same accounts must never
  produce a negative balance or a lost update.

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
