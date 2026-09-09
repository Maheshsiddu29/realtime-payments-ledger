# Authentication and authorization

## What this service is, precisely

**This service validates OAuth2-style JWT access tokens. It is not an OAuth2
authorization server.**

That distinction matters and is easy to blur:

| | |
| --- | --- |
| **OAuth2** | An authorization *framework*. Defines how an authorization server issues access tokens to clients on a resource owner's behalf. |
| **JWT** | A token *format*. A signed, self-contained set of claims. |
| **This service** | A **resource server**. It verifies tokens and enforces scopes. |

The payments ledger has **no** login flow, no user database, no password
storage, no consent screen, no refresh tokens, no token endpoint and no
revocation. It never issues a credential. In production, tokens are expected
from an external OAuth2/OIDC identity provider — Auth0, Okta, Entra ID,
Keycloak — and this service holds only that issuer's **public** key.

`cmd/devtoken` mints tokens for local development. It is not authentication
infrastructure: it generates a keypair and signs a claim set so a developer can
call the API with `grpcurl`. It is never deployed and is not built into the
service image.

Résumé-accurate phrasing: *"JWT (RS256) access-token validation with
scope-based authorization, designed for an external OAuth2/OIDC issuer."* Not
*"built an OAuth2 server"*.

## Signing: RS256, asymmetric

Tokens are signed with **RS256** and verified with an RSA **public** key.

The property worth having is that this service *cannot mint a token*. With a
symmetric scheme such as HS256, the verification secret and the signing secret
are the same value, so anyone who compromised the payments service — or read a
config dump, or an environment variable in a crash report — could forge tokens
for the entire estate. Verification-only failure is contained.

The cost is a little more configuration: a PEM public key rather than a shared
string. That is a good trade.

## What is validated

Every check is enabled **explicitly** rather than relying on library defaults,
so a library upgrade cannot quietly relax one:

| Check | Why |
| --- | --- |
| **Algorithm allow-list: RS256 only** | The single most important line. See below. |
| **Signature** | Against the configured public key. |
| **`exp`, and its presence is required** | A token without an expiry never stops working. |
| **`nbf`, when present** | A token is not usable before its time. |
| **`iss`** | Must equal `JWT_ISSUER`. A token from another issuer is not ours. |
| **`aud`** | Must contain `JWT_AUDIENCE`. A token minted for another service must not work here. |
| **`sub`, non-empty after trimming** | Every action must be attributable in the audit trail. |

### Why the algorithm allow-list matters most

Without an explicit list, a JWT nominates its own algorithm in its header, and
two attacks follow:

1. **`alg: none`.** The token declares itself unsigned. A parser that honours
   the header accepts anything anyone writes.
2. **Algorithm confusion.** An attacker takes the RSA **public** key — which is
   public — and uses it as an **HMAC secret** to sign an HS256 token. A parser
   that picks its verification method from the header will happily verify it.

`jwt.WithValidMethods([]string{"RS256"})` makes both impossible. Both are
covered by tests, at the verifier and again through the real gRPC transport.

The verification key is chosen by **configuration**, never by the token. A
`kid` supplied by the token is not used to select a key — that would be the
same class of mistake as trusting `alg`.

## Configuration

| Variable | Purpose |
| --- | --- |
| `JWT_ISSUER` | Expected `iss`. |
| `JWT_AUDIENCE` | Expected `aud`. |
| `JWT_PUBLIC_KEY` | PEM public key, inline. |
| `JWT_PUBLIC_KEY_FILE` | PEM public key, from a path — how a real deployment mounts a secret. |
| `JWT_LEEWAY` | Clock-skew tolerance for `exp`/`nbf`. Default 30s, capped at 5 minutes. |

Setting both key variables is rejected. `JWT_LEEWAY` is capped because it is
literally a window in which an expired token still works.

Key material is **never logged**. `Config.Redacted()` masks
`JWT_PUBLIC_KEY` even though a public key is not secret — the same field would
hold a private key if it were ever misconfigured, and the error for that case
deliberately does not echo its input either.

### Production fails loudly

With `APP_ENV=production`, start-up **refuses** unless the issuer, audience and
a verification key are all configured. There is no development fallback, no
default secret and no "authentication disabled" mode that a deployment could
reach by omission. `GRPC_REFLECTION=true` is rejected there too.

Outside production, running with no key configured is allowed — and the
authentication interceptor then **refuses every payments RPC** rather than
serving an open API. A missing key can only ever close the door, never open it.

## Authorization: scopes

Authentication asks *who is calling*. Authorization asks *what may they do*.

Scopes are used rather than roles because they map directly onto OAuth2 access
tokens: an identity provider issues a token with a `scope` claim, and this
service consumes it without needing to know anything about the caller.

| Scope | Grants |
| --- | --- |
| `accounts:read` | `GetAccount` |
| `accounts:write` | `CreateAccount` |
| `transfers:read` | `GetTransfer` |
| `transfers:write` | `CreateTransfer` |
| `ledger:read` | `ListLedgerEntriesForTransfer` |

Both claim spellings are accepted: `scope` (space-delimited, RFC 8693 and most
OAuth2 servers) and `scp` (an array, used by Entra ID among others).

## How it is enforced

Two unary interceptors, in a fixed order, both **before any handler**:

```
Client
  │  gRPC + Authorization: Bearer <jwt>
  ▼
┌─────────────────────────────┐
│ logging interceptor         │  one line per RPC; never the header
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│ authentication interceptor  │  verify token → Principal → context
│   fails ⇒ UNAUTHENTICATED   │
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│ authorization interceptor   │  method → required scope
│   fails ⇒ PERMISSION_DENIED │
└──────────────┬──────────────┘
               ▼
┌─────────────────────────────┐
│ PaymentsService handler     │  thin: validate, call, convert
└──────────────┬──────────────┘
               ▼
    application service layer
               ▼
    idempotency → PostgreSQL / Redis
```

The scope mapping is **one table**, not checks scattered through handlers, so
the whole policy can be read and tested in one place — and a handler cannot
forget to check, because it never runs otherwise. **An RPC absent from the
table is denied**, so adding an RPC without deciding its permission fails
closed. A unit test walks the generated service descriptor and fails if any RPC
lacks a mapping, or if the table names an RPC that no longer exists.

Only the standard health service and reflection skip authentication. A test
enumerates the service descriptor and fails if any payments RPC appears in that
exemption list.

### Authorization happens before any financial state

Because both checks are interceptors, a rejected request never reaches business
code. It cannot claim an idempotency key, cannot write a Redis `PROCESSING`
record, cannot open a transaction and cannot move money.

This is asserted directly: a valid token lacking `transfers:write` is refused,
and the test then verifies no transfer row exists, no Redis key exists, no
balance changed — and that the *same* idempotency key is still usable by a
properly authorized caller afterwards.

## Status codes

| Situation | Code |
| --- | --- |
| No token, malformed header, bad signature, expired, wrong issuer/audience | `UNAUTHENTICATED` |
| Valid token, missing scope | `PERMISSION_DENIED` |

An unauthenticated caller gets `UNAUTHENTICATED`, never `PERMISSION_DENIED`:
the distinction tells a client whether to obtain a token or to stop trying.

The message is uniform — `invalid or missing credentials` — regardless of which
check failed. A caller is never told *why*, because "expired" versus "wrong
audience" versus "bad signature" is a hint about which knob to turn. The
specific reason is logged.

## Logging

Authentication failures log the method, a coarse reason and the underlying
error. They never log the `Authorization` header, the raw token or key
material. Authorization denials log the subject, the required scope and the
granted scopes — safe metadata that makes a misconfigured client diagnosable.

## The principal

Handlers see a typed `auth.Principal` — subject, issuer, audience, scopes — and
never a `jwt.Token`, a claims map or a raw token string. The context key is
unexported, so nothing outside `internal/auth` can plant a principal and
impersonate a caller.

## Limitations

1. **Scope-based authorization only; no per-account ownership.** Any caller
   with `transfers:write` can move money between any two accounts. Accounts
   have no owner or tenant column, and inventing one would be a data-model
   redesign well beyond this phase. A real deployment needs per-account
   authorization or tenant isolation, and that requires schema work first.
2. **A single static verification key.** No JWKS endpoint, no `kid` selection,
   no rotation. Rotating the key means restarting with a new one. JWKS with
   caching and refresh is the natural next step, and it brings network
   dependency and cache-invalidation concerns that deserve their own tests.
3. **No revocation.** A JWT is valid until it expires; there is no
   introspection endpoint and no deny-list. Short token lifetimes are the
   mitigation.
4. **No mutual TLS and no transport encryption in the local setup.** The
   Compose stack serves plaintext gRPC. A real deployment terminates TLS.
5. **No rate limiting or brute-force protection** on the API.
6. **RS256 only.** ES256 would be a smaller, faster equivalent; supporting both
   is a one-line change to the allow-list, deliberately not made without a
   reason.
