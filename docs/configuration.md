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

Not connected to yet — validated only, for a later phase.

| Variable         | Default          | Description                                  |
| ---------------- | ---------------- | -------------------------------------------- |
| `REDIS_ADDR`     | `localhost:6379` | `host:port`. `redis:6379` inside Compose.    |
| `REDIS_PASSWORD` | *(empty)*        | Optional. Never logged.                      |
| `REDIS_DB`       | `0`              | Logical database index.                      |

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
  `POSTGRES_CONNECT_TIMEOUT`.
- `POSTGRES_SSLMODE` is not a valid libpq mode.
- `POSTGRES_MAX_IDLE_CONNS` exceeds `POSTGRES_MAX_OPEN_CONNS`.
- `APP_ENV=production` and `POSTGRES_SSLMODE=disable`.
- `KAFKA_BROKERS` resolves to an empty list.

Errors are joined, so one start-up attempt reveals every misconfiguration:

```
fatal: invalid configuration: APP_ENV "prod" must be one of development, staging, production, test
LOG_LEVEL "loud" must be one of debug, info, warn, error
HTTP_PORT 99999 must be between 0 and 65535
```

## Secret handling

Passwords are never written to logs. `Config.Redacted()` returns a copy with
every secret replaced by `[REDACTED]`, and `Postgres.RedactedDSN()` renders a
connection string safe to print. An empty password stays empty rather than
becoming the literal redaction marker.
