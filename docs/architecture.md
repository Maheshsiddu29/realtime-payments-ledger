# Architecture

## Current state (Phase 0)

Phase 0 is the repository foundation: a process that starts, loads validated
configuration, reports health, and shuts down cleanly. No business domain has
been implemented yet.

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
                      └───────┬────────────────┬───────┘
                              │                │
                              ▼                ▼
              ┌───────────────────────┐  ┌──────────────────────┐
              │   internal/httpapi    │─▶│  internal/health     │
              │ /healthz · /readyz    │  │  check registry      │
              │ /version · shutdown   │  │  (empty in Phase 0)  │
              └───────────────────────┘  └──────────────────────┘
```

Docker Compose provides PostgreSQL, Redis and Kafka alongside the API. **The
API process does not connect to any of them yet** — they exist so that later
phases have a stable local environment to build against.

## Package layout

| Package            | Responsibility                                                        |
| ------------------ | --------------------------------------------------------------------- |
| `cmd/api`          | Entrypoint. Wiring and signal handling only; no business logic.        |
| `internal/config`  | Environment parsing, defaulting and validation. The only reader of env.|
| `internal/health`  | Concurrency-safe registry of named dependency checks.                  |
| `internal/httpapi` | Operational HTTP surface and server lifecycle.                         |
| `tests`            | End-to-end tests that drive the process as an operator would.          |

Everything sits under `internal/`, so no package can be imported by an outside
module. That constraint is deliberate: the public contract of this system is
its gRPC API, not its Go types.

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

The check registry bounds the whole evaluation with a timeout and contains
panics, so a single wedged or buggy dependency probe cannot stall or crash the
readiness path.

### Shutdown is graceful and bounded

`signal.NotifyContext` converts SIGTERM and SIGINT into context cancellation.
The server stops accepting connections and drains in-flight requests within
`HTTP_SHUTDOWN_TIMEOUT`; anything still running when the budget expires is
forcibly closed rather than leaked. `stop_grace_period` in Compose is set above
that budget so the container is not killed mid-drain.

This matters more here than in most services: a payment request cut off
mid-flight leaves a client unable to tell whether it succeeded.

### The standard library is the default

Phase 0 has zero third-party dependencies. `log/slog` covers structured
logging, `net/http` covers the operational endpoints, and `errors.Join` covers
aggregate validation errors. Dependencies are added when a phase genuinely
needs them (a PostgreSQL driver, a Kafka client, gRPC), not preemptively.

## Local infrastructure

| Service    | Image                | Host port | Notes                                             |
| ---------- | -------------------- | --------- | ------------------------------------------------- |
| PostgreSQL | `postgres:16-alpine` | 5432      | `default_transaction_isolation=serializable`, lock logging on |
| Redis      | `redis:7-alpine`     | 6379      | AOF persistence, `noeviction`                     |
| Kafka      | `apache/kafka:3.8.0` | 29092     | KRaft single node, topic auto-creation disabled   |
| API        | built from source    | 8080      | Waits for all three to be healthy                 |

PostgreSQL defaults to `SERIALIZABLE` at the server level so that a `psql`
session behaves like the application does. Redis uses `noeviction` because
silently evicting an idempotency key would allow a duplicate payment. Kafka
disables topic auto-creation so that a typo in a topic name fails loudly
instead of creating a phantom stream.

## What is deliberately absent

Accounts, transfers, ledger entries, the database driver, Redis idempotency,
OAuth2/JWT authentication, the gRPC server, the transactional outbox, the Kafka
producer, OpenTelemetry tracing, and chaos testing. Each belongs to a later
phase — see [roadmap.md](roadmap.md).
