# Configuration

All configuration is read from the process environment by `internal/config`.
There are no config files and no command-line flags. Values are parsed,
defaulted and validated once at start-up; the process refuses to start if
anything is invalid, and reports every problem at once.

For local development, copy `.env.example` to `.env` and edit it. Docker
Compose reads `.env` automatically.

## Application

| Variable     | Default            | Description                                       |
| ------------ | ------------------ | ------------------------------------------------- |
| `APP_NAME`   | `payments-ledger`  | Service identity attached to every log record.    |
| `APP_ENV`    | `development`      | One of `development`, `staging`, `production`, `test`. |
| `LOG_LEVEL`  | `info`             | One of `debug`, `info`, `warn`, `error`.          |
| `LOG_FORMAT` | `json`             | `json` for shipped logs, `text` for local reading.|

## HTTP (operational endpoints)

| Variable                | Default   | Description                                        |
| ----------------------- | --------- | -------------------------------------------------- |
| `HTTP_HOST`             | `0.0.0.0` | Bind address.                                      |
| `HTTP_PORT`             | `8080`    | Bind port. `0` requests an ephemeral port (tests). |
| `HTTP_READ_TIMEOUT`     | `5s`      | Request read deadline.                             |
| `HTTP_WRITE_TIMEOUT`    | `10s`     | Response write deadline.                           |
| `HTTP_IDLE_TIMEOUT`     | `60s`     | Keep-alive idle deadline.                          |
| `HTTP_SHUTDOWN_TIMEOUT` | `15s`     | Grace period for draining in-flight requests.      |

Durations use Go syntax: `5s`, `500ms`, `30m`.

## gRPC (application API)

| Variable | Default | Description |
| --- | --- | --- |
| `GRPC_HOST` | `0.0.0.0` | Bind address. |
| `GRPC_PORT` | `9090` | Bind port. `0` requests an ephemeral port (tests). Must differ from `HTTP_PORT`. |
| `GRPC_SHUTDOWN_TIMEOUT` | `15s` | Budget for draining in-flight RPCs before connections are cut. |
| `GRPC_REFLECTION` | `true` outside production | Server reflection, for `grpcurl`. **Rejected** when `APP_ENV=production`. |

## JWT verification

This service **verifies** access tokens; it never issues them. Only a public
key is configured, so a compromise here cannot forge a token. See
[AUTHENTICATION.md](AUTHENTICATION.md).

| Variable | Default | Description |
| --- | --- | --- |
| `JWT_ISSUER` | *(empty)* | Expected `iss` claim. |
| `JWT_AUDIENCE` | *(empty)* | Expected `aud` claim. |
| `JWT_PUBLIC_KEY` | *(empty)* | PEM RSA public key, inline. Never logged. |
| `JWT_PUBLIC_KEY_FILE` | *(empty)* | Path to the PEM key. Mutually exclusive with the above. |
| `JWT_LEEWAY` | `30s` | Clock-skew tolerance for `exp`/`nbf`. Capped at 5 minutes. |

With no key configured the process still starts, and the authentication
interceptor **refuses every payments RPC** — a missing key can only close the
door, never open it.

## PostgreSQL

Live: the application connects to PostgreSQL using these settings and reports
itself unready while the database is unreachable.

| Variable                     | Default     | Description                                  |
| ---------------------------- | ----------- | -------------------------------------------- |
| `POSTGRES_HOST`              | `localhost` | Hostname. `postgres` inside Compose.         |
| `POSTGRES_PORT`              | `5432`      | Port.                                        |
| `POSTGRES_USER`              | `ledger`    | Role.                                        |
| `POSTGRES_PASSWORD`          | `ledger`    | Password. Never logged; masked in all output.|
| `POSTGRES_DB`                | `ledger`    | Database name.                               |
| `POSTGRES_SSLMODE`           | `disable`   | libpq sslmode. **Rejected as `disable` when `APP_ENV=production`.** |
| `POSTGRES_MAX_OPEN_CONNS`    | `25`        | Pool ceiling.                                |
| `POSTGRES_MAX_IDLE_CONNS`    | `25`        | Idle ceiling. Must not exceed the open ceiling. |
| `POSTGRES_CONN_MAX_LIFETIME` | `30m`       | Connection recycle age.                      |
| `POSTGRES_CONNECT_TIMEOUT`   | `10s`       | Bounds the initial connection and its verifying ping. |

`POSTGRES_MAX_IDLE_CONNS` has no `pgxpool` equivalent and is applied by the
migration runner, which uses `database/sql`. See
[DATABASE.md](DATABASE.md#connection-pooling).

### Test-only

| Variable           | Default       | Description                                                |
| ------------------ | ------------- | ---------------------------------------------------------- |
| `POSTGRES_TEST_DB` | `ledger_test` | Database used by the integration tests. Created and migrated automatically, so a test run never touches development data. |

## Redis

Live: Redis coordinates idempotent requests. It is **not** part of the
financial source of truth — see [IDEMPOTENCY.md](IDEMPOTENCY.md).

| Variable                | Default          | Description                                  |
| ----------------------- | ---------------- | -------------------------------------------- |
| `REDIS_ADDR`            | `localhost:6379` | `host:port`. `redis:6379` inside Compose.    |
| `REDIS_PASSWORD`        | *(empty)*        | Optional. Never logged.                      |
| `REDIS_DB`              | `0`              | Logical database index.                      |
| `REDIS_DIAL_TIMEOUT`    | `3s`             | Bounds establishing a connection.            |
| `REDIS_COMMAND_TIMEOUT` | `1s`             | Bounds a single command, so a wedged Redis fails fast rather than stalling a payment. |

## Idempotency

Redis-side lifetimes only. Expiry never weakens deduplication: the UNIQUE
constraint on `transfers.idempotency_key` is the final barrier and has no TTL.

| Variable                     | Default | Description                                        |
| ---------------------------- | ------- | -------------------------------------------------- |
| `IDEMPOTENCY_TTL`            | `24h`   | How long a completed result stays cached.          |
| `IDEMPOTENCY_PROCESSING_TTL` | `30s`   | Lease on an in-flight claim. A crashed holder releases the key after this. |

`IDEMPOTENCY_PROCESSING_TTL` must not exceed `IDEMPOTENCY_TTL`; a lease
outliving the result it guards would leave a key blocking payments.

### Test-only

| Variable        | Default | Description                                              |
| --------------- | ------- | -------------------------------------------------------- |
| `REDIS_TEST_DB` | `15`    | Logical Redis database the integration tests flush.      |

## Kafka

Not connected to yet — validated only, for a later phase.

| Variable            | Default            | Description                                        |
| ------------------- | ------------------ | -------------------------------------------------- |
| `KAFKA_BROKERS`     | `localhost:29092`  | Comma-separated bootstrap list. `kafka:9092` inside Compose. |
| `KAFKA_AUDIT_TOPIC` | `ledger.audit.v1`  | Topic that audit events are published to.          |

## Validation rules

`config.Load` fails when any of these hold:

- `APP_ENV`, `LOG_LEVEL` or `LOG_FORMAT` is outside its allowed set.
- A port is outside its valid range, or a numeric variable is not a number.
- A duration is unparseable or not greater than zero, including
  `POSTGRES_CONNECT_TIMEOUT`, the Redis timeouts and the idempotency TTLs.
- `IDEMPOTENCY_PROCESSING_TTL` exceeds `IDEMPOTENCY_TTL`.
- `POSTGRES_SSLMODE` is not a valid libpq mode.
- `POSTGRES_MAX_IDLE_CONNS` exceeds `POSTGRES_MAX_OPEN_CONNS`.
- `APP_ENV=production` and `POSTGRES_SSLMODE=disable`.
- `APP_ENV=production` without `JWT_ISSUER`, `JWT_AUDIENCE` and a verification
  key. There is no development fallback and no default secret.
- `APP_ENV=production` with `GRPC_REFLECTION=true`.
- `GRPC_PORT` equal to `HTTP_PORT`.
- Both `JWT_PUBLIC_KEY` and `JWT_PUBLIC_KEY_FILE` set.
- `JWT_LEEWAY` greater than 5 minutes — it is a window in which expired tokens
  are accepted.
- `KAFKA_BROKERS` resolves to an empty list.

Errors are joined, so one start-up attempt reveals every misconfiguration:

```
fatal: invalid configuration: APP_ENV "prod" must be one of development, staging, production, test
LOG_LEVEL "loud" must be one of debug, info, warn, error
HTTP_PORT 99999 must be between 0 and 65535
```

## Secret handling

Passwords and key material are never written to logs. `Config.Redacted()`
returns a copy with every secret — including `JWT_PUBLIC_KEY`, because the same
field would hold a private key if misconfigured — replaced by `[REDACTED]`, and `Postgres.RedactedDSN()` renders a
connection string safe to print. An empty password stays empty rather than
becoming the literal redaction marker.
