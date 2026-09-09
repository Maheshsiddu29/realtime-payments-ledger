# API

The payments ledger is exposed as a versioned gRPC service:
`payments.v1.PaymentsService`, defined in
[`api/proto/payments/v1/payments.proto`](../api/proto/payments/v1/payments.proto).

Every RPC requires a valid JWT access token and a specific scope. See
[AUTHENTICATION.md](AUTHENTICATION.md).

## Ports

The process serves two listeners, deliberately separate: they have different
audiences, different exposure and different consequences if reached by the
wrong party.

| Listener | Default | Purpose |
| --- | --- | --- |
| HTTP | `:8080` | Operational probes only — `/healthz`, `/readyz`, `/version`. No payments data. |
| gRPC | `:9090` | The application API. Everything below. |

Configured by `HTTP_PORT` and `GRPC_PORT`. Configuration validation rejects
the two being equal.

## Money

**Money is an integer count of minor units plus a currency code. There is no
floating point anywhere in this API.**

```protobuf
int64  amount_minor = 3;
string currency     = 4;
```

| Amount | `amount_minor` | `currency` |
| --- | ---: | --- |
| $10.25 | `1025` | `"USD"` |
| $0.01 | `1` | `"USD"` |
| €500.00 | `50000` | `"EUR"` |

`double` and `float` are not used and never will be: binary floating point
cannot represent most decimal fractions exactly, and in a ledger that error
compounds into money that does not exist.

One protobuf detail worth knowing: proto3 JSON encodes `int64` as a **string**,
so `grpcurl` shows `"amountMinor": "1025"`. That is the wire format, not a
change of type — it exists because JSON numbers cannot hold the full int64
range.

Currency codes are ISO 4217, exactly three upper-case letters, validated in Go
and again by a `CHECK` constraint in PostgreSQL. Lower case is rejected rather
than upper-cased.

## Timestamps

`google.protobuf.Timestamp`, UTC. A field that is not set is absent (`null`)
rather than the zero time — `completed_at` is absent until a transfer
completes.

## RPCs

| RPC | Required scope | Purpose |
| --- | --- | --- |
| `CreateAccount` | `accounts:write` | Open an account with a zero balance |
| `GetAccount` | `accounts:read` | Read an account |
| `CreateTransfer` | `transfers:write` | Move money, at most once per idempotency key |
| `GetTransfer` | `transfers:read` | Read a transfer |
| `ListLedgerEntriesForTransfer` | `ledger:read` | Read the entries a transfer posted |

### What the API deliberately does not expose

There is no RPC to set a balance, insert a ledger entry, or modify or delete
either. Money moves **only** through `CreateTransfer`, which is the single
write path to the ledger. A test asserts this against the generated service
descriptor, so adding such an RPC fails the build's tests.

### CreateAccount

```
CreateAccountRequest  { currency }
CreateAccountResponse { account }
```

Accounts always start at `balance_minor = 0`, and there is **no starting
balance field**. An account created already holding money would be a credit
with no matching debit, and the books would not balance from the first row.
Funding is a transfer.

### GetAccount

```
GetAccountRequest  { id }
GetAccountResponse { account { id, currency, balance_minor, created_at, updated_at } }
```

A malformed id is `INVALID_ARGUMENT`; an unknown one is `NOT_FOUND`.

### CreateTransfer

```
CreateTransferRequest  { source_account_id, destination_account_id,
                         amount_minor, currency, idempotency_key }
CreateTransferResponse { transfer, idempotent_replay }
```

`idempotency_key` is **required**. Accepting a transfer without one would mean
a retried payment could post twice.

It is a request field rather than gRPC metadata because it is business data,
not transport data: it belongs in the audit trail, it is stored on the transfer
row, and it is part of the contract a client codes against. Hiding a key that
decides whether money moves twice inside a metadata header would make it easy
to drop by accident.

Repeating a request with the same key returns the **same transfer id** with
`idempotent_replay = true`, and no money moves. Reusing the key for a
materially different payment is `ALREADY_EXISTS` and is **not executed**. Full
behaviour: [IDEMPOTENCY.md](IDEMPOTENCY.md).

The RPC calls the same posting path everything else does, so it keeps
`SERIALIZABLE` isolation, canonical-order row locking, bounded serialization
retry, Redis coordination, the PostgreSQL uniqueness barrier and the ledger
invariants. The transport adds validation and conversion, nothing else.

### GetTransfer

```
GetTransferRequest  { id }
GetTransferResponse { transfer }
```

### ListLedgerEntriesForTransfer

```
ListLedgerEntriesForTransferRequest  { transfer_id }
ListLedgerEntriesForTransferResponse { entries[] }
```

Read-only. Entries come back debits first; a completed transfer has exactly
two, summing to zero. Negative is a debit, positive a credit.

An unknown transfer is `NOT_FOUND` rather than an empty list — "no entries"
and "no such transfer" are different answers, and a client reconciling its own
records needs to tell them apart.

**No pagination.** The response is inherently bounded at two entries, so
pagination would be an abstraction with no caller. An account statement
endpoint, which is genuinely unbounded, would require it.

## Error codes

Mapped deliberately from domain errors. The choices that could reasonably go
either way are explained.

| Condition | Code | Retry? |
| --- | --- | --- |
| Malformed UUID, bad currency, non-positive amount, missing/invalid idempotency key, source == destination | `INVALID_ARGUMENT` | No — fix the request |
| Account or transfer not found | `NOT_FOUND` | No |
| Insufficient funds | `FAILED_PRECONDITION` | Only after the balance changes |
| Currency mismatch | `FAILED_PRECONDITION` | No |
| Idempotency key reused for a different payment | `ALREADY_EXISTS` | No — use a new key |
| A request with this key is already in progress | `ABORTED` | Yes, shortly |
| Retry budget exhausted under contention | `UNAVAILABLE` | Yes, later |
| Missing, malformed, expired or invalid token | `UNAUTHENTICATED` | Re-authenticate |
| Valid token without the required scope | `PERMISSION_DENIED` | No |
| Client cancelled | `CANCELED` | — |
| Client deadline expired | `DEADLINE_EXCEEDED` | Maybe |
| Anything unanticipated | `INTERNAL` | — |

**`ALREADY_EXISTS` for an idempotency conflict**, not `FAILED_PRECONDITION`:
the entity the caller tried to create — a payment under this key — already
exists with different content, and no change of state will make the same call
succeed. `FAILED_PRECONDITION` would wrongly suggest it might.

**`ABORTED` for a request already in progress**, not `UNAVAILABLE`: gRPC
reserves `ABORTED` for concurrency conflicts where the client should retry the
whole operation, which is exactly the situation. `UNAVAILABLE` would imply the
service is down; it is not, one key is busy.

**Messages never leak internals.** No SQLSTATEs, constraint names, driver
text, Redis errors or key material reach a client. An unmapped error returns
the fixed message `internal error` and the detail goes to the log. A test
asserts this by feeding a realistic PostgreSQL error through the mapper.

Authentication failures are deliberately uniform — a client is never told
whether the token was expired, wrongly signed, or minted for another audience,
because that tells an attacker which knob to turn next. The reason is logged.

## Calling the service locally

Verified commands. `grpcurl` needs no `.proto` file because reflection is
enabled outside production.

```sh
# 1. infrastructure and schema
make infra-up
make migrate-up

# 2. a development keypair and token
#    Writes ./.devkeys and prints the export lines to copy.
make devtoken

# 3. start the API with the matching public key
export JWT_ISSUER="https://dev-issuer.local"
export JWT_AUDIENCE="payments-api"
export JWT_PUBLIC_KEY_FILE="$PWD/.devkeys/public.pem"
make run

# 4. in another shell, mint a token and call
export TOKEN=$(go run ./cmd/devtoken -key ./.devkeys/private.pem -quiet)

grpcurl -plaintext localhost:9090 list
grpcurl -plaintext localhost:9090 describe payments.v1.PaymentsService

grpcurl -plaintext -H "authorization: Bearer $TOKEN" \
  -d '{"currency":"USD"}' \
  localhost:9090 payments.v1.PaymentsService/CreateAccount

grpcurl -plaintext -H "authorization: Bearer $TOKEN" \
  -d '{"id":"<account-id>"}' \
  localhost:9090 payments.v1.PaymentsService/GetAccount

grpcurl -plaintext -H "authorization: Bearer $TOKEN" \
  -d '{"source_account_id":"<src>","destination_account_id":"<dst>",
       "amount_minor":2500,"currency":"USD","idempotency_key":"demo-1"}' \
  localhost:9090 payments.v1.PaymentsService/CreateTransfer

grpcurl -plaintext -H "authorization: Bearer $TOKEN" \
  -d '{"transfer_id":"<transfer-id>"}' \
  localhost:9090 payments.v1.PaymentsService/ListLedgerEntriesForTransfer

# health needs no token
grpcurl -plaintext localhost:9090 grpc.health.v1.Health/Check
```

A new account has a zero balance, so a demo transfer needs the source funded.
There is no production way to do that — see
[LEDGER_DESIGN.md](LEDGER_DESIGN.md#accounts-start-at-zero). For a local demo,
write the balance directly:

```sh
make psql
# UPDATE accounts SET balance_minor = 100000 WHERE id = '<src>';
```

## Health checking

The standard gRPC health service is registered and needs no token — an
orchestrator probing readiness has none, and the response reveals only serving
status.

| | |
| --- | --- |
| `SERVING` | Required dependencies are healthy |
| `NOT_SERVING` | A required dependency is failing, or the server is draining |

The status mirrors the HTTP `/readyz` view: it answers "should traffic come
here", not "is the process alive". A `NOT_SERVING` reply is not a reason to
restart the process. PostgreSQL is a required dependency; Redis is optional and
its failure degrades rather than unreadies the service.

Status is refreshed every five seconds rather than computed per request, so a
health checker cannot generate database load.

## Server reflection

Enabled outside production, off in production, and configuration validation
**refuses** `GRPC_REFLECTION=true` when `APP_ENV=production`. Reflection lets
anyone enumerate the API and its message shapes; that is a convenience in
development and free reconnaissance in production.

## Protobuf generation

Generated `.pb.go` files **are committed**, so cloning and building need no
protobuf toolchain. CI regenerates and diffs them, so they cannot drift from
the `.proto`.

```sh
make proto-tools   # install the pinned plugins
make proto         # regenerate into internal/gen/
make proto-check   # fail if the committed output is stale
```

Pinned versions, which must match the modules in `go.mod`:

| Tool | Version |
| --- | --- |
| `protoc` | 3.20+ (CI uses 25.1) |
| `protoc-gen-go` | v1.36.12 |
| `protoc-gen-go-grpc` | v1.5.1 |

Buf was considered and not adopted: with one `.proto` file, no external
consumers and no breaking-change surface yet, it would be tool sprawl without
benefit. It becomes worthwhile when the API has published clients.
