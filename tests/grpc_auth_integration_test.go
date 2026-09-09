//go:build integration

package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/auth"
	paymentsv1 "github.com/Maheshsiddu29/realtime-payments-ledger/internal/gen/payments/v1"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/grpcapi"
)

// assertCode checks the gRPC status code and that the message leaks nothing.
func assertCode(t *testing.T, err error, want codes.Code) {
	t.Helper()

	if err == nil {
		t.Fatalf("call succeeded, want %s", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error %v is not a gRPC status", err)
	}
	if st.Code() != want {
		t.Fatalf("code = %s, want %s (message %q)", st.Code(), want, st.Message())
	}
	assertNoLeakage(t, st.Message())
}

// assertNoLeakage fails if a client-facing message contains anything that
// belongs only in a log: SQLSTATEs, constraint names, driver text, key
// material or credentials.
func assertNoLeakage(t *testing.T, message string) {
	t.Helper()

	lowered := strings.ToLower(message)
	for _, forbidden := range []string{
		"sqlstate", "pq:", "pgx", "constraint", "postgres", "postgresql",
		"redis", "dial tcp", "relation ", "column ", "begin public key",
		"private key", "bearer ", "eyj", // eyJ… is the start of a JWT
	} {
		if strings.Contains(lowered, forbidden) {
			t.Errorf("client-facing message leaks internals (%q): %q", forbidden, message)
		}
	}
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

// Every payments RPC requires a valid token. This enumerates them from the
// generated service descriptor, so an RPC added later without authentication
// cannot slip through unnoticed.
func TestGRPCUnauthenticatedCallsAreRejected(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)

	calls := map[string]func() error{
		"CreateAccount": func() error {
			_, err := g.client.CreateAccount(ctx, &paymentsv1.CreateAccountRequest{Currency: "USD"})
			return err
		},
		"GetAccount": func() error {
			_, err := g.client.GetAccount(ctx, &paymentsv1.GetAccountRequest{Id: newUUIDString()})
			return err
		},
		"CreateTransfer": func() error {
			_, err := g.client.CreateTransfer(ctx, &paymentsv1.CreateTransferRequest{
				SourceAccountId: newUUIDString(), DestinationAccountId: newUUIDString(),
				AmountMinor: 100, Currency: "USD", IdempotencyKey: "unauthenticated-attempt",
			})
			return err
		},
		"GetTransfer": func() error {
			_, err := g.client.GetTransfer(ctx, &paymentsv1.GetTransferRequest{Id: newUUIDString()})
			return err
		},
		"ListLedgerEntriesForTransfer": func() error {
			_, err := g.client.ListLedgerEntriesForTransfer(ctx,
				&paymentsv1.ListLedgerEntriesForTransferRequest{TransferId: newUUIDString()})
			return err
		},
	}

	// Every RPC in the descriptor must be covered here.
	for _, method := range paymentsv1.PaymentsService_ServiceDesc.Methods {
		if _, ok := calls[method.MethodName]; !ok {
			t.Errorf("RPC %s has no unauthenticated-access test; every RPC must require a token",
				method.MethodName)
		}
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			assertCode(t, call(), codes.Unauthenticated)
		})
	}

	// Nothing may have been created by any of those attempts.
	if n := countRows(t, g.env, "transfers"); n != 0 {
		t.Errorf("transfers table has %d rows after unauthenticated attempts, want 0", n)
	}
	if n := countRows(t, g.env, "accounts"); n != 0 {
		t.Errorf("accounts table has %d rows after unauthenticated attempts, want 0", n)
	}
}

// The security cases, through the real transport. Each is a way in if the
// corresponding check is missing.
func TestGRPCRejectsBadCredentials(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)

	now := time.Now()
	expired := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	tests := []struct {
		name   string
		header func(t *testing.T) string
		why    string
	}{
		{
			name:   "no authorization header",
			header: func(*testing.T) string { return "" },
			why:    "an anonymous caller must not reach a payments RPC",
		},
		{
			name:   "authorization header with no scheme",
			header: func(t *testing.T) string { return g.keys.token(t, tokenOptions{}) },
			why:    "a bare token is not a Bearer credential",
		},
		{
			name:   "wrong scheme",
			header: func(*testing.T) string { return "Basic dXNlcjpwYXNzd29yZA==" },
			why:    "only Bearer is accepted",
		},
		{
			name:   "bearer with no token",
			header: func(*testing.T) string { return "Bearer " },
			why:    "an empty credential is not a credential",
		},
		{
			name: "unsigned token, alg none",
			header: func(t *testing.T) string {
				return "Bearer " + g.keys.token(t, tokenOptions{
					method: jwt.SigningMethodNone, signWith: jwt.UnsafeAllowNoneSignatureType,
				})
			},
			why: "an unsigned token would let anyone assert any identity",
		},
		{
			name: "signed by an untrusted key",
			header: func(t *testing.T) string {
				return "Bearer " + g.keys.token(t, tokenOptions{signWith: g.keys.untrusted})
			},
			why: "a signature from an unknown key must not verify",
		},
		{
			name: "HS256 signed with the public key as the secret",
			header: func(t *testing.T) string {
				return "Bearer " + g.keys.token(t, tokenOptions{
					method: jwt.SigningMethodHS256, signWith: []byte(g.keys.publicPEM),
				})
			},
			why: "algorithm confusion turns a public key into a signing secret",
		},
		{
			name: "expired",
			header: func(t *testing.T) string {
				return "Bearer " + g.keys.token(t, tokenOptions{expiresAt: &expired})
			},
			why: "an expired token must stop working",
		},
		{
			name: "no expiry claim",
			header: func(t *testing.T) string {
				return "Bearer " + g.keys.token(t, tokenOptions{omitExp: true})
			},
			why: "a token without exp never stops working",
		},
		{
			name: "not valid yet",
			header: func(t *testing.T) string {
				return "Bearer " + g.keys.token(t, tokenOptions{notBefore: &future})
			},
			why: "nbf in the future means the token is not usable yet",
		},
		{
			name: "wrong issuer",
			header: func(t *testing.T) string {
				return "Bearer " + g.keys.token(t, tokenOptions{issuer: "https://attacker.example"})
			},
			why: "a token from another issuer must not be accepted",
		},
		{
			name: "wrong audience",
			header: func(t *testing.T) string {
				return "Bearer " + g.keys.token(t, tokenOptions{audience: "some-other-service"})
			},
			why: "a token minted for a different service must not work here",
		},
		{
			name: "tampered payload",
			header: func(t *testing.T) string {
				raw := g.keys.token(t, tokenOptions{})
				parts := strings.Split(raw, ".")
				payload := []byte(parts[1])
				payload[0] ^= 'A' ^ 'B'
				return "Bearer " + parts[0] + "." + string(payload) + "." + parts[2]
			},
			why: "an edited payload invalidates the signature",
		},
		{
			name:   "not a token at all",
			header: func(*testing.T) string { return "Bearer not-a-jwt" },
			why:    "garbage must be rejected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callCtx := ctx
			if header := tt.header(t); header != "" {
				callCtx = g.withRawAuthorization(ctx, header)
			}

			_, err := g.client.CreateAccount(callCtx, &paymentsv1.CreateAccountRequest{Currency: "USD"})
			if err == nil {
				t.Fatalf("call succeeded: %s", tt.why)
			}
			assertCode(t, err, codes.Unauthenticated)
		})
	}

	if n := countRows(t, g.env, "accounts"); n != 0 {
		t.Errorf("accounts table has %d rows after only rejected credentials, want 0", n)
	}
}

// A valid token with a past nbf must be accepted, or the nbf rejection above
// would be passing for the wrong reason.
func TestGRPCAcceptsAValidToken(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)

	past := time.Now().Add(-time.Hour)
	authCtx := g.authed(t, ctx, tokenOptions{notBefore: &past})

	resp, err := g.client.CreateAccount(authCtx, &paymentsv1.CreateAccountRequest{Currency: "USD"})
	if err != nil {
		t.Fatalf("CreateAccount with a valid token: %v", err)
	}
	if resp.GetAccount().GetId() == "" {
		t.Error("CreateAccount returned no account id")
	}
}

// ---------------------------------------------------------------------------
// Authorization
// ---------------------------------------------------------------------------

// Authentication answers "who is calling"; authorization answers "what may
// they do". A caller with a perfectly valid token but the wrong scope must be
// refused — with PERMISSION_DENIED, not UNAUTHENTICATED, because the
// distinction tells the client whether to re-authenticate or give up.
func TestGRPCAuthorizationRequiresTheRightScope(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)

	// Prepared with a fully scoped token so the denial cases below fail for
	// authorization reasons and nothing else.
	source := g.fundedAccountRPC(t, ctx, "USD", 10_000)
	destination := g.newAccountRPC(t, ctx, "USD")

	posted, err := g.client.CreateTransfer(g.full(t, ctx), &paymentsv1.CreateTransferRequest{
		SourceAccountId: source, DestinationAccountId: destination,
		AmountMinor: 100, Currency: "USD", IdempotencyKey: "authz-setup",
	})
	if err != nil {
		t.Fatalf("setup transfer: %v", err)
	}
	transferID := posted.GetTransfer().GetId()

	tests := []struct {
		name     string
		required auth.Scope
		call     func(ctx context.Context) error
	}{
		{
			name:     "CreateAccount",
			required: auth.ScopeAccountsWrite,
			call: func(ctx context.Context) error {
				_, err := g.client.CreateAccount(ctx, &paymentsv1.CreateAccountRequest{Currency: "USD"})
				return err
			},
		},
		{
			name:     "GetAccount",
			required: auth.ScopeAccountsRead,
			call: func(ctx context.Context) error {
				_, err := g.client.GetAccount(ctx, &paymentsv1.GetAccountRequest{Id: source})
				return err
			},
		},
		{
			name:     "CreateTransfer",
			required: auth.ScopeTransfersWrite,
			call: func(ctx context.Context) error {
				_, err := g.client.CreateTransfer(ctx, &paymentsv1.CreateTransferRequest{
					SourceAccountId: source, DestinationAccountId: destination,
					AmountMinor: 100, Currency: "USD",
					IdempotencyKey: "authz-" + newUUIDString(),
				})
				return err
			},
		},
		{
			name:     "GetTransfer",
			required: auth.ScopeTransfersRead,
			call: func(ctx context.Context) error {
				_, err := g.client.GetTransfer(ctx, &paymentsv1.GetTransferRequest{Id: transferID})
				return err
			},
		},
		{
			name:     "ListLedgerEntriesForTransfer",
			required: auth.ScopeLedgerRead,
			call: func(ctx context.Context) error {
				_, err := g.client.ListLedgerEntriesForTransfer(ctx,
					&paymentsv1.ListLedgerEntriesForTransferRequest{TransferId: transferID})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The mapping the interceptor uses must agree with what this test
			// expects, so a change to one without the other is caught.
			method := "/" + paymentsv1.PaymentsService_ServiceDesc.ServiceName + "/" + tt.name
			mapped, ok := grpcapi.RequiredScope(method)
			if !ok {
				t.Fatalf("no scope mapped for %s", method)
			}
			if mapped != tt.required {
				t.Fatalf("%s requires %q, the test expects %q", method, mapped, tt.required)
			}

			t.Run("every other scope is refused", func(t *testing.T) {
				var others []auth.Scope
				for _, s := range auth.AllScopes {
					if s != tt.required {
						others = append(others, s)
					}
				}
				assertCode(t, tt.call(g.authed(t, ctx, tokenOptions{scopes: others})), codes.PermissionDenied)
			})

			t.Run("no scopes at all is refused", func(t *testing.T) {
				assertCode(t, tt.call(g.authed(t, ctx, tokenOptions{noScopes: true})), codes.PermissionDenied)
			})

			t.Run("the required scope alone succeeds", func(t *testing.T) {
				if err := tt.call(g.authed(t, ctx, tokenOptions{scopes: []auth.Scope{tt.required}})); err != nil {
					t.Errorf("call with %q = %v, want success", tt.required, err)
				}
			})
		})
	}
}

// An unauthorized request must not create any financial state. Authorization
// runs in an interceptor, before the handler, so a denied CreateTransfer never
// reaches the idempotency layer at all — it cannot claim a key, write a Redis
// PROCESSING record, open a transaction or move money.
func TestGRPCUnauthorizedRequestCreatesNoState(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)

	source := g.fundedAccountRPC(t, ctx, "USD", 10_000)
	destination := g.newAccountRPC(t, ctx, "USD")

	sourceBefore := balanceOf(t, g.env, uuidMust(t, source))
	const key = "unauthorized-must-not-claim-this"

	// Valid token, wrong scope.
	readOnly := g.authed(t, ctx, tokenOptions{
		scopes: []auth.Scope{auth.ScopeAccountsRead, auth.ScopeTransfersRead, auth.ScopeLedgerRead},
	})
	_, err := g.client.CreateTransfer(readOnly, &paymentsv1.CreateTransferRequest{
		SourceAccountId: source, DestinationAccountId: destination,
		AmountMinor: 500, Currency: "USD", IdempotencyKey: key,
	})
	assertCode(t, err, codes.PermissionDenied)

	// And with no token at all.
	_, err = g.client.CreateTransfer(ctx, &paymentsv1.CreateTransferRequest{
		SourceAccountId: source, DestinationAccountId: destination,
		AmountMinor: 500, Currency: "USD", IdempotencyKey: key,
	})
	assertCode(t, err, codes.Unauthenticated)

	// No transfer row.
	if n := countTransfersForKey(t, g.env, key); n != 0 {
		t.Errorf("transfer rows for the key = %d, want 0", n)
	}
	// No Redis claim: the key must still be free for a legitimate caller.
	exists, redisErr := g.env.redis.Exists(ctx, "idempotency:transfer:"+key).Result()
	if redisErr != nil {
		t.Fatalf("checking redis: %v", redisErr)
	}
	if exists != 0 {
		t.Errorf("an unauthorized request claimed the idempotency key in redis")
	}
	// No money moved.
	if got := balanceOf(t, g.env, uuidMust(t, source)); got != sourceBefore {
		t.Errorf("source balance = %d, want %d unchanged", got, sourceBefore)
	}

	// And the key is still usable by a properly authorized caller.
	authorized, err := g.client.CreateTransfer(g.full(t, ctx), &paymentsv1.CreateTransferRequest{
		SourceAccountId: source, DestinationAccountId: destination,
		AmountMinor: 500, Currency: "USD", IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("authorized call with the same key: %v", err)
	}
	if authorized.GetIdempotentReplay() {
		t.Error("the authorized call was treated as a replay; the denied request had reserved the key")
	}
	assertReconciled(t, g.env)
}
