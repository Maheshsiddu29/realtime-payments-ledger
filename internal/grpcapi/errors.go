package grpcapi

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/account"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/auth"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/idempotency"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/money"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

// ErrInvalidRequest marks a request the transport rejected before it reached
// the domain: a malformed UUID, a missing required field. It maps to
// INVALID_ARGUMENT.
//
// It exists so transport-level validation does not have to borrow a domain
// sentinel that means something else — a bad UUID is not an invalid amount.
var ErrInvalidRequest = errors.New("invalid request")

// toStatus maps a domain error onto a gRPC status.
//
// Two rules govern this function:
//
//  1. Every mapping is deliberate. A domain error that reaches the default
//     branch becomes INTERNAL with a generic message, which is the correct
//     answer for "the service has a bug it did not anticipate".
//  2. Nothing internal leaks. SQLSTATEs, constraint names, Redis errors,
//     connection strings and JWT failure reasons stay in the logs. A client
//     learns the code and a short, stable message and nothing else — an error
//     message is an information channel, and for an unauthenticated caller it
//     is the only one they have.
func toStatus(ctx context.Context, log *slog.Logger, method string, err error) error {
	if err == nil {
		return nil
	}

	// Already a status (produced by an interceptor, say): pass it through
	// rather than re-wrapping it as INTERNAL.
	if _, ok := status.FromError(err); ok && errors.As(err, new(interface{ GRPCStatus() *status.Status })) {
		return err
	}

	switch {
	// ---- Client mistakes -------------------------------------------------
	case errors.Is(err, ErrInvalidRequest),
		errors.Is(err, transfer.ErrInvalidAmount),
		errors.Is(err, transfer.ErrSameAccount),
		errors.Is(err, money.ErrInvalidCurrency),
		errors.Is(err, idempotency.ErrInvalidKey):
		return status.Error(codes.InvalidArgument, publicMessage(err))

	// ---- Missing things --------------------------------------------------
	case errors.Is(err, account.ErrNotFound):
		return status.Error(codes.NotFound, "account not found")
	case errors.Is(err, transfer.ErrNotFound):
		return status.Error(codes.NotFound, "transfer not found")
	case errors.Is(err, transfer.ErrSourceAccountNotFound):
		return status.Error(codes.NotFound, "source account not found")
	case errors.Is(err, transfer.ErrDestinationAccountNotFound):
		return status.Error(codes.NotFound, "destination account not found")

	// ---- The request is well formed but the ledger will not allow it -----
	//
	// FAILED_PRECONDITION rather than INVALID_ARGUMENT: the request is valid,
	// the system state is what refuses it. Per the gRPC guidance, the client
	// should not retry until that state changes.
	case errors.Is(err, transfer.ErrInsufficientFunds):
		return status.Error(codes.FailedPrecondition, "insufficient funds")
	case errors.Is(err, transfer.ErrCurrencyMismatch):
		return status.Error(codes.FailedPrecondition, "currency mismatch between transfer and accounts")

	// ---- Idempotency -----------------------------------------------------
	//
	// ALREADY_EXISTS: the entity the caller tried to create — a payment under
	// this key — already exists with different content. It is not retryable;
	// the caller must use a new key. FAILED_PRECONDITION would suggest the
	// same call could succeed once state changes, which is never true here.
	case errors.Is(err, transfer.ErrIdempotencyConflict):
		return status.Error(codes.AlreadyExists,
			"idempotency key was already used for a different payment")

	// ABORTED: gRPC reserves this for concurrency conflicts where the client
	// should retry the whole operation, which is exactly the situation.
	// UNAVAILABLE would imply the service is down, and it is not — one
	// specific key is busy.
	case errors.Is(err, transfer.ErrIdempotencyInProgress):
		return status.Error(codes.Aborted,
			"a request with this idempotency key is already in progress; retry shortly")

	// ---- Contention ------------------------------------------------------
	//
	// The transfer exhausted its bounded retry budget under contention.
	// Nothing was written, and retrying later is the right response.
	case errors.Is(err, transfer.ErrRetriesExhausted):
		return status.Error(codes.Unavailable, "the ledger is busy; retry shortly")

	// ---- Authentication and authorization --------------------------------
	case errors.Is(err, auth.ErrNoCredentials), errors.Is(err, auth.ErrMalformedCredentials),
		errors.Is(err, auth.ErrInvalidToken):
		return status.Error(codes.Unauthenticated, "invalid or missing credentials")
	case errors.Is(err, auth.ErrMissingScope):
		return status.Error(codes.PermissionDenied, "the token does not carry the required scope")

	// ---- The caller went away --------------------------------------------
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, "request cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded")
	}

	// Anything unmapped is a bug. Log it in full for us; tell the client
	// nothing.
	log.ErrorContext(ctx, "unmapped error from a gRPC handler",
		slog.String("method", method),
		slog.String("error", err.Error()))
	return status.Error(codes.Internal, "internal error")
}

// publicMessage returns a message safe to hand a client.
//
// Validation errors are written to be client-facing already — they describe
// the request, not the system — so they are returned as-is. Everything else
// goes through the mappings above with a fixed message.
func publicMessage(err error) string { return err.Error() }
