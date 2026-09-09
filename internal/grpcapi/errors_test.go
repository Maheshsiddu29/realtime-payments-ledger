package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/account"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/auth"
	paymentsv1 "github.com/Maheshsiddu29/realtime-payments-ledger/internal/gen/payments/v1"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/idempotency"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/money"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// Every domain error a handler can surface must have a deliberate code. A
// wrong code is a contract bug: it tells a client to retry when it should not,
// or to give up when it should retry.
func TestErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		// Client mistakes.
		{"transport validation", fmt.Errorf("%w: id is not a valid UUID", ErrInvalidRequest), codes.InvalidArgument},
		{"invalid amount", transfer.ErrInvalidAmount, codes.InvalidArgument},
		{"same account", transfer.ErrSameAccount, codes.InvalidArgument},
		{"invalid currency", money.ErrInvalidCurrency, codes.InvalidArgument},
		{"invalid idempotency key", idempotency.ErrInvalidKey, codes.InvalidArgument},

		// Missing things.
		{"account not found", account.ErrNotFound, codes.NotFound},
		{"transfer not found", transfer.ErrNotFound, codes.NotFound},
		{"source missing", transfer.ErrSourceAccountNotFound, codes.NotFound},
		{"destination missing", transfer.ErrDestinationAccountNotFound, codes.NotFound},

		// Valid request, state refuses it. Not retryable until state changes.
		{"insufficient funds", transfer.ErrInsufficientFunds, codes.FailedPrecondition},
		{"currency mismatch", transfer.ErrCurrencyMismatch, codes.FailedPrecondition},

		// Idempotency. ALREADY_EXISTS is terminal — a new key is needed;
		// ABORTED asks the client to retry the whole operation.
		{"idempotency conflict", transfer.ErrIdempotencyConflict, codes.AlreadyExists},
		{"idempotency in progress", transfer.ErrIdempotencyInProgress, codes.Aborted},

		// Contention: nothing was written, retry later.
		{"retries exhausted", transfer.ErrRetriesExhausted, codes.Unavailable},

		// Credentials.
		{"no credentials", auth.ErrNoCredentials, codes.Unauthenticated},
		{"malformed credentials", auth.ErrMalformedCredentials, codes.Unauthenticated},
		{"invalid token", auth.ErrInvalidToken, codes.Unauthenticated},
		{"missing scope", auth.ErrMissingScope, codes.PermissionDenied},

		// The caller went away.
		{"cancelled", context.Canceled, codes.Canceled},
		{"deadline exceeded", context.DeadlineExceeded, codes.DeadlineExceeded},

		// Anything unanticipated is a bug, and says nothing to the client.
		{"unknown", errors.New("some internal failure"), codes.Internal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Wrapped, because that is how errors actually arrive.
			err := toStatus(context.Background(), discardLogger(), "Test",
				fmt.Errorf("transfer: doing something: %w", tt.err))

			if got := status.Code(err); got != tt.want {
				t.Errorf("code = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestToStatusPassesNilThrough(t *testing.T) {
	t.Parallel()

	if err := toStatus(context.Background(), discardLogger(), "Test", nil); err != nil {
		t.Errorf("toStatus(nil) = %v, want nil", err)
	}
}

// An unmapped error must reveal nothing. The detail belongs in the log.
func TestInternalErrorsRevealNothing(t *testing.T) {
	t.Parallel()

	leaky := errors.New(`pq: duplicate key value violates unique constraint "transfers_idempotency_key_unique" (SQLSTATE 23505)`)

	err := toStatus(context.Background(), discardLogger(), "CreateTransfer", leaky)
	message := status.Convert(err).Message()

	for _, forbidden := range []string{"SQLSTATE", "constraint", "pq:", "transfers_idempotency"} {
		if strings.Contains(message, forbidden) {
			t.Errorf("client message leaks %q: %q", forbidden, message)
		}
	}
	if message != "internal error" {
		t.Errorf("message = %q, want a fixed generic message", message)
	}
}

// A status produced by an interceptor must survive the mapping unchanged
// rather than being flattened to INTERNAL.
func TestExistingStatusIsPreserved(t *testing.T) {
	t.Parallel()

	original := status.Error(codes.PermissionDenied, "the token does not carry the required scope")

	if got := status.Code(toStatus(context.Background(), discardLogger(), "Test", original)); got != codes.PermissionDenied {
		t.Errorf("code = %s, want %s", got, codes.PermissionDenied)
	}
}

// Every RPC in the service must have a required scope. An RPC with no mapping
// is denied by the interceptor, but the mistake should be caught here, at
// build time, rather than in production.
func TestEveryRPCHasARequiredScope(t *testing.T) {
	t.Parallel()

	for _, method := range paymentsv1.PaymentsService_ServiceDesc.Methods {
		full := methodPath(method.MethodName)

		scope, ok := RequiredScope(full)
		if !ok {
			t.Errorf("RPC %s has no required scope; add it to requiredScopes", full)
			continue
		}
		if !slicesContains(auth.AllScopes, scope) {
			t.Errorf("RPC %s requires %q, which is not a known scope", full, scope)
		}
	}

	// And the table must not name RPCs that do not exist, which would be a
	// leftover from a rename.
	known := map[string]bool{}
	for _, method := range paymentsv1.PaymentsService_ServiceDesc.Methods {
		known[methodPath(method.MethodName)] = true
	}
	for path := range requiredScopes {
		if !known[path] {
			t.Errorf("requiredScopes names %s, which is not an RPC on the service", path)
		}
	}
}

// Only health and reflection may skip authentication. A payments RPC that
// slipped into that list would be an unauthenticated money API.
func TestNoPaymentsRPCSkipsAuthentication(t *testing.T) {
	t.Parallel()

	for _, method := range paymentsv1.PaymentsService_ServiceDesc.Methods {
		full := methodPath(method.MethodName)
		if isUnauthenticated(full) {
			t.Errorf("RPC %s skips authentication", full)
		}
	}

	// The intended exemptions still work.
	for _, exempt := range []string{
		"/grpc.health.v1.Health/Check",
		"/grpc.health.v1.Health/Watch",
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
	} {
		if !isUnauthenticated(exempt) {
			t.Errorf("%s requires authentication; probes and reflection carry no token", exempt)
		}
	}
}

func slicesContains(scopes []auth.Scope, want auth.Scope) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}
