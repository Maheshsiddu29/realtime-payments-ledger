# AGENTS.md

Operating contract for any AI agent or automated contributor working in this
repository. These rules are binding and take precedence over convenience.

## 1. Repository safety rules

1. Never push to GitHub.
2. Never merge branches.
3. Never force-push.
4. Never create or publish releases.
5. Local commits are allowed only when the requested phase is complete.
6. Keep commits small and logically scoped.
7. Run tests before each commit.
8. Run gofmt before each commit.
9. Run go vet before each commit.
10. Run go test ./...
11. Run go test -race ./...
12. Do not weaken tests to make CI pass.
13. Do not fabricate performance numbers.
14. Do not claim concurrency/failure guarantees unless verified.
15. Update documentation when architecture changes.
16. Stop after each major development phase.
17. Report:
    - files changed
    - architecture changes
    - tests added
    - commands executed
    - test results
    - known limitations
    - local commits created
18. Wait for human review before starting the next major phase.

The human operator pushes reviewed work manually.

## 2. Project context

A production-style real-time payments ledger built on Go, PostgreSQL, Redis,
Kafka and gRPC. The system is developed in explicit phases; each phase is
reviewed by a human before the next begins.

Target capabilities (not all implemented yet — see `docs/roadmap.md`):
double-entry accounting, serializable PostgreSQL transactions, row-level
locking, Redis-backed idempotency, gRPC APIs, OAuth2/JWT authentication,
transactional outbox, Kafka audit events, distributed tracing, structured
logging, concurrency testing, chaos testing, CI/CD.

## 3. Current phase status

- **Phase 0 — repository foundation: complete.**
  Module layout, configuration loading, health mechanism, graceful shutdown,
  Docker Compose infrastructure, Makefile, CI.
- **Phase 1 and later: not started.** Do not begin the next phase without an
  explicit instruction.

Anything not present in the tree is deliberately out of scope for the current
phase. Do not implement accounts, transfers, ledger entries, idempotency,
authentication, Kafka producers, the outbox, observability exporters or chaos
testing until the phase that owns them is requested.

## 4. Engineering conventions

- **Layout.** `cmd/<binary>` holds entrypoints only — wiring, no business
  logic. `internal/<domain>` holds implementation. Nothing is exported from
  `internal` to the outside world.
- **Dependencies.** The standard library is the default. Every third-party
  module must earn its place; prefer `log/slog`, `net/http` and `database/sql`
  over wrappers.
- **Configuration.** All configuration is read from the environment through
  `internal/config`. No `flag` parsing, no config files, no `os.Getenv` calls
  scattered through packages.
- **Errors.** Wrap with `fmt.Errorf("...: %w", err)`. Never discard an error
  with `_`.
- **Secrets.** Never log credentials. Config values that carry secrets are
  redacted through `Config.Redacted()` before logging.
- **Money.** Amounts are integer minor units (cents). Floating point must
  never represent money.
- **Tests.** Table-driven where practical. Concurrency-sensitive code must
  have a test that fails under `-race` if the synchronisation is removed.

## 5. Validation gate

Every change must pass, before commit:

```sh
make ci      # fmt-check, vet, build, test, race
```

which is equivalent to:

```sh
gofmt -l .            # must print nothing
go vet ./...
go build ./...
go test ./...
go test -race ./...
docker compose config # must parse
```

## 6. Reporting format

At the end of a phase, report: architecture created, directory tree, files
changed, tests created, commands executed, validation results, commits
created, known limitations, and the recommended scope of the next phase.
