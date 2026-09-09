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
- **Phase 1 — PostgreSQL persistence and the double-entry core: complete.**
  pgx pool, migrations, accounts, transfers, ledger entries, database-enforced
  accounting invariants, PostgreSQL-backed readiness, integration tests, CI
  integration job.
- **Phase 2 — concurrency safety: complete.**
  Deterministic UUID-ordered row locking, bounded retry on SQLSTATE 40001 and
  40P01, reconciliation checks, concurrency and deadlock regression tests, a
  stress runner, and measured results in `docs/results/`.
- **Phase 3 — Redis-backed idempotency: complete.**
  Idempotency keys persisted with a UNIQUE constraint, request fingerprinting,
  Redis claim/completion records, duplicate recovery from PostgreSQL, and
  integration tests covering every Redis failure mode.
- **Phase 4 — gRPC API and JWT authentication: complete.**
  Versioned protobuf service, thin handlers over the existing service layer,
  RS256 token validation, scope-based authorization interceptors, deliberate
  error mapping, graceful shutdown of both listeners, and gRPC integration
  tests including twelve concurrent duplicate RPCs.
- **Phase 5 and later: not started.** Do not begin the next phase without an
  explicit instruction.

Anything not present in the tree is deliberately out of scope for the current
phase. Do not implement Kafka producers, the outbox, observability exporters or
chaos testing until the phase that owns them is requested.

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
- **Money.** Amounts are integer minor units (cents), stored as `BIGINT`.
  Floating point must never represent money — not `float32`, `float64`, `REAL`
  or `DOUBLE PRECISION`, anywhere.
- **The ledger is append-only.** Never add an update or delete path for
  `ledger_entries`, and never expose entry creation outside the transfer
  posting. Corrections are reversing entries.
- **Migrations are append-only** once applied beyond a local machine, and the
  application never migrates at start-up.
- **Tests.** Table-driven where practical. Concurrency-sensitive code must
  have a test that fails under `-race` if the synchronisation is removed.
- **Locking.** Multiple account rows are always locked in canonical UUID
  order, never in transfer direction. Lock acquisition order and business
  roles are separate concerns: the debit lands on the source regardless of
  which row was locked first.
- **Retries.** Only SQLSTATE 40001 and 40P01 are retried, classified through
  `pgconn.PgError` and never by matching error text. Business rejections must
  never be retried. Every retry loop is bounded and honours context
  cancellation.
- **Claims.** An executed test supports "observed X in this run", never "X is
  impossible". Record the machine, database version and command alongside any
  measurement, and keep results in `docs/results/`.
- **Redis is never a correctness boundary.** It coordinates and caches. Every
  guarantee it appears to provide must also hold with Redis flushed,
  unavailable or expired, and that fallback must be tested rather than assumed.
  Never use Redis to lock an account balance; PostgreSQL row locks are
  authoritative.
- **Idempotency.** A key is opaque and bounded. Reuse with a different request
  fingerprint is a conflict and must never execute. Completed results may be
  cached; business rejections must not be.
- **The transport is thin.** gRPC handlers validate, call one service method
  and convert the result. Never reimplement transfer posting, locking, retry or
  idempotency in a handler — there must be exactly one implementation.
- **Security checks precede business code.** Authentication and authorization
  are interceptors. Never move a scope check into a handler, and never add an
  RPC without a required-scope mapping: an unmapped RPC is denied, and it must
  stay that way.
- **Never hand-roll token cryptography.** Restrict signing algorithms
  explicitly; never let a token choose its own. Never log a token, an
  Authorization header or key material, and never tell a client *why*
  authentication failed.
- **Generated code is never edited by hand.** Change the `.proto` and run
  `make proto`; CI verifies the committed output matches.

## 5. Validation gate

Every change must pass, before commit:

```sh
make ci      # fmt-check, vet, build, test, race — no infrastructure needed
make verify  # the above plus compose config and PostgreSQL integration tests
```

which is equivalent to:

```sh
gofmt -s -l .                          # must print nothing
go vet ./...
go vet -tags=integration ./...
go build ./...
go test ./...
go test -race ./...
go test -tags=integration ./...        # needs PostgreSQL and Redis: make infra-up
go test -race -tags=integration ./...
make proto-check                       # generated code must be current
docker compose config                  # must parse
```

Concurrency changes additionally require `make test-concurrency-race` and, for
anything touching locking or retries, a stress run recorded in
`docs/results/`.

Integration tests must run against a real PostgreSQL, never a mock. They fail
loudly when the database is missing rather than skipping — a test that silently
skips is a test that never runs.

## 6. Reporting format

At the end of a phase, report: architecture created, directory tree, files
changed, tests created, commands executed, validation results, commits
created, known limitations, and the recommended scope of the next phase.
