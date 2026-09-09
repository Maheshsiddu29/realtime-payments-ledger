# gRPC results — concurrent duplicate requests through the public API

Every number came from an executed run on the date below.

## Environment

| | |
| --- | --- |
| Date | 2026-09-09 (UTC) |
| Hardware | Apple M3 Pro, 12 CPUs, macOS (darwin/arm64) |
| Go | go1.26.5 |
| PostgreSQL | 16.14 (`postgres:16-alpine`, Docker Compose) |
| Redis | 7.4.11 (`redis:7-alpine`, Docker Compose) |
| gRPC | google.golang.org/grpc v1.83.2 |
| Transport | real gRPC server on an ephemeral port, real client, real interceptors |
| Auth | RS256, key generated per test run |

## How to reproduce

```sh
make infra-up
make test-grpc-race
```

## Twelve concurrent duplicate CreateTransfer RPCs

Twelve authenticated `CreateTransfer` calls, released together through a
closed-channel barrier, sharing one idempotency key, source, destination,
amount and currency. This is the Phase 3 scenario repeated through the actual
public API — real transport, real authentication, real interceptors.

Run **under the race detector**, five times:

| Run | Callers | Created | Replayed | `ABORTED` (in progress) | Unexpected | Distinct transfer IDs | Transfer rows | Data races |
| --: | --: | --: | --: | --: | --: | --: | --: | --: |
| 1 | 12 | **1** | 8 | 3 | 0 | **1** | **1** | 0 |
| 2 | 12 | **1** | 10 | 1 | 0 | **1** | **1** | 0 |
| 3 | 12 | **1** | 9 | 2 | 0 | **1** | **1** | 0 |
| 4 | 12 | **1** | 9 | 2 | 0 | **1** | **1** | 0 |
| 5 | 12 | **1** | 10 | 1 | 0 | **1** | **1** | 0 |

Invariants, every run:

| Check | Result |
| --- | --- |
| Transfer rows created | **1** |
| Distinct transfer IDs returned | **1** |
| Financial debits | **1** |
| Financial credits | **1** |
| Ledger entries (read back through the API) | **2**, summing to zero |
| Source balance | debited exactly once |
| Total balances | unchanged |
| Ledger invariant violations | **0** |
| Negative balances | **0** |
| Pending transfers left behind | **0** |
| Reconciliation | clean |
| Data races | **0** |

The created/replayed/aborted split varies with timing; the financial outcome
does not. Callers that received `ABORTED` were retried by the test and every
one resolved to the **same** transfer ID with no second posting.

## Phase 2 protections through the transport

Twenty concurrent `CreateTransfer` RPCs with **distinct** idempotency keys
against an account holding exactly ten transfers' worth:

| Callers | Succeeded | Refused | Final source balance | Total balances | Negative balances |
| --: | --: | --: | --: | --: | --: |
| 20 | **10** | 10 | 0 | unchanged | **0** |

Exposing the ledger over gRPC did not weaken account concurrency safety.

## Security results

All executed through the real transport, against `CreateAccount`:

| Case | Result |
| --- | --- |
| No `Authorization` header | `UNAUTHENTICATED` |
| Bare token, no `Bearer` scheme | `UNAUTHENTICATED` |
| `Basic` scheme | `UNAUTHENTICATED` |
| `Bearer` with no token | `UNAUTHENTICATED` |
| **Unsigned token (`alg: none`)** | `UNAUTHENTICATED` |
| Signed by an untrusted RSA key | `UNAUTHENTICATED` |
| **HS256 signed with the RSA public key as the HMAC secret** | `UNAUTHENTICATED` |
| Expired | `UNAUTHENTICATED` |
| No `exp` claim at all | `UNAUTHENTICATED` |
| `nbf` in the future | `UNAUTHENTICATED` |
| Wrong issuer | `UNAUTHENTICATED` |
| Wrong audience | `UNAUTHENTICATED` |
| Tampered payload | `UNAUTHENTICATED` |
| Not a JWT | `UNAUTHENTICATED` |
| Valid token, `nbf` in the past | **accepted** |

After all rejected credentials: **0 rows** in `accounts`, **0 rows** in
`transfers`.

Authorization, for each of the five RPCs: every scope *other* than the required
one → `PERMISSION_DENIED`; no scopes → `PERMISSION_DENIED`; the required scope
alone → success.

## Authorization creates no state

A valid token lacking `transfers:write`, then no token at all, both attempting
`CreateTransfer` with idempotency key `unauthorized-must-not-claim-this`:

| Check | Result |
| --- | --- |
| gRPC codes | `PERMISSION_DENIED`, then `UNAUTHENTICATED` |
| Transfer rows for the key | **0** |
| Redis key `idempotency:transfer:<key>` | **absent** |
| Source balance | unchanged |
| Same key used afterwards by an authorized caller | succeeded, **not** flagged as a replay |

Authorization runs in an interceptor, so a denied request never reaches the
idempotency layer at all.

This was also confirmed manually against a running server with `grpcurl`: a
read-only token returned `PermissionDenied`, and both the `transfers` table and
Redis reported zero for that key.

## Cancellation

| Case | Result |
| --- | --- |
| Context cancelled before the call | `CANCELED`, no money moved, 0 completed transfers |
| Deadline already expired | `DEADLINE_EXCEEDED`, no money moved |

## What these results support

Supported:

- **Observed exactly one financial transfer for 12 simultaneous same-key
  `CreateTransfer` RPCs, in five consecutive runs under the race detector**,
  with one distinct transfer ID, one debit, one credit, and no invariant
  violations.
- Observed zero data races across the whole gRPC suite under `-race`.
- Observed every listed credential attack rejected, and no state created by
  unauthenticated or unauthorized requests.

Not supported:

- That duplicates are mathematically impossible for every workload. These are
  executed tests, not a proof.
- Any throughput or latency claim; none was measured.
