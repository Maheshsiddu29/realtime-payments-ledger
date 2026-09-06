# Realtime Payments Ledger

A production-style, real-time payments ledger in Go: double-entry accounting on
PostgreSQL with serializable transactions, Redis-backed idempotency, a gRPC
API, a transactional outbox feeding Kafka audit events, and distributed
tracing.

> **Status: Phase 0 — repository foundation.**
> The API process starts, loads validated configuration, serves health
> endpoints and shuts down gracefully. No accounts, transfers or ledger entries
> exist yet. See [docs/roadmap.md](docs/roadmap.md) for what lands when.

## Quick start

Requirements: Go 1.26+, Docker with Compose v2, GNU Make.

```sh
git clone <this repository>
cd Realtime-PaymentsLedger
cp .env.example .env

make infra-up          # PostgreSQL, Redis and Kafka; waits until healthy
make run               # run the API from source

curl localhost:8080/healthz
curl localhost:8080/readyz
curl localhost:8080/version
```

To run everything, API container included:

```sh
make up                # build and start the whole stack
make logs              # follow logs
make down              # stop, keeping data volumes
make down-volumes      # stop and delete data
```

## Operational endpoints

| Endpoint   | Purpose   | Behaviour                                                        |
| ---------- | --------- | ---------------------------------------------------------------- |
| `/healthz` | Liveness  | `200` whenever the process is serving. Never consults a dependency — a database outage must not trigger a restart loop. |
| `/readyz`  | Readiness | Runs every registered dependency check. `200` when all pass, `503` when any fails, so the process leaves the load-balancer rotation without being killed. |
| `/version` | Metadata  | Build version, commit and service identity.                       |

Phase 0 registers no dependency checks, so `/readyz` reports healthy as soon as
the process is serving. Later phases register PostgreSQL, Redis and Kafka.

These are the *operational* endpoints. Payment operations are served over gRPC
from Phase 4 onward.

## Development

```sh
make help          # list every target
make ci            # the full gate: fmt-check, vet, build, test, test-race
make verify        # make ci plus docker compose config
make test-race     # race detector only
make cover-html    # coverage report at coverage.html
make binary        # build bin/api with version metadata
```

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
internal/config/    Environment parsing, defaulting and validation
internal/health/    Concurrency-safe dependency check registry
internal/httpapi/   Operational HTTP endpoints and server lifecycle
migrations/         SQL schema migrations (empty until Phase 1)
tests/              End-to-end tests that drive the real process
docs/               Architecture, configuration and roadmap
.github/workflows/  CI: lint, test, race, Compose validation, image build
```

Architecture and design rationale: [docs/architecture.md](docs/architecture.md).

## Local infrastructure

| Service    | Image                | Host port | Notes                                          |
| ---------- | -------------------- | --------- | ---------------------------------------------- |
| PostgreSQL | `postgres:16-alpine` | 5432      | Serializable by default, lock waits logged     |
| Redis      | `redis:7-alpine`     | 6379      | AOF persistence, `noeviction`                  |
| Kafka      | `apache/kafka:3.8.0` | 29092     | KRaft single node, no topic auto-creation      |
| API        | built from source    | 8080      | Starts after all three report healthy          |

The API does not connect to any of these yet; they are provisioned so later
phases build against a stable environment.

## Technology

Go · PostgreSQL · Redis · Kafka · gRPC · OAuth2/JWT · OpenTelemetry · Jaeger ·
Loki · Toxiproxy · Docker Compose · GitHub Actions

Phase 0 has **zero third-party Go dependencies** — the standard library covers
structured logging, HTTP and error aggregation. Dependencies are introduced by
the phase that needs them.

## Repository rules

This repository is developed in reviewed phases. Automated contributors must
not push, merge, force-push or publish releases; the maintainer pushes reviewed
work manually. The full contract is in [AGENTS.md](AGENTS.md).
