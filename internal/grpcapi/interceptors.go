package grpcapi

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/auth"
	paymentsv1 "github.com/Maheshsiddu29/realtime-payments-ledger/internal/gen/payments/v1"
)

// serviceName is the fully qualified proto service name, taken from the
// generated descriptor so these paths cannot drift from the .proto.
var serviceName = paymentsv1.PaymentsService_ServiceDesc.ServiceName

// methodPath renders "/payments.v1.PaymentsService/<rpc>".
func methodPath(rpc string) string { return "/" + serviceName + "/" + rpc }

// requiredScopes maps every RPC to the scope a caller must hold.
//
// The mapping is a single table rather than checks scattered through handlers,
// so the whole authorization policy can be read — and tested — in one place. A
// handler cannot forget to check, because the interceptor consults this table
// before the handler runs at all.
//
// An RPC absent from this table is DENIED, not allowed. Adding an RPC without
// deciding its permission is a mistake, and the safe outcome of that mistake
// is a closed door.
var requiredScopes = map[string]auth.Scope{
	methodPath("CreateAccount"):                auth.ScopeAccountsWrite,
	methodPath("GetAccount"):                   auth.ScopeAccountsRead,
	methodPath("CreateTransfer"):               auth.ScopeTransfersWrite,
	methodPath("GetTransfer"):                  auth.ScopeTransfersRead,
	methodPath("ListLedgerEntriesForTransfer"): auth.ScopeLedgerRead,
}

// RequiredScope returns the scope an RPC needs, for documentation and tests.
func RequiredScope(fullMethod string) (auth.Scope, bool) {
	s, ok := requiredScopes[fullMethod]
	return s, ok
}

// unauthenticatedMethods are the only paths that skip authentication.
//
// The health service is here because an orchestrator probing readiness has no
// token and should not need one; it reveals nothing but serving status.
// Reflection is here because it is only ever enabled outside production.
// Every payments RPC requires a token — there is no exception, and a test
// asserts it by enumerating the service descriptor.
var unauthenticatedPrefixes = []string{
	"/grpc.health.v1.Health/",
	"/grpc.reflection.v1.ServerReflection/",
	"/grpc.reflection.v1alpha.ServerReflection/",
}

func isUnauthenticated(fullMethod string) bool {
	for _, prefix := range unauthenticatedPrefixes {
		if strings.HasPrefix(fullMethod, prefix) {
			return true
		}
	}
	return false
}

// authenticate validates the bearer token and puts the caller in the context.
//
// It runs before authorization and before any handler, so an unauthenticated
// request never reaches business code: it cannot claim an idempotency key,
// cannot open a transaction and cannot move money.
func authenticate(verifier *auth.Verifier, log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if isUnauthenticated(info.FullMethod) {
			return handler(ctx, req)
		}

		// No verifier means authentication is not configured. Refuse rather
		// than serve an open payments API: configuration validation already
		// makes this impossible in production, and this is the second line.
		if verifier == nil {
			log.ErrorContext(ctx, "rejecting a request because JWT verification is not configured",
				slog.String("method", info.FullMethod))
			return nil, status.Error(codes.Unauthenticated, "authentication is not configured")
		}

		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "invalid or missing credentials")
		}

		var header string
		if values := md.Get("authorization"); len(values) > 0 {
			header = values[0]
		}

		raw, err := auth.TokenFromAuthorization(header)
		if err != nil {
			// The header itself is never logged: it carries a live credential.
			logAuthFailure(ctx, log, info.FullMethod, "malformed credentials", err)
			return nil, status.Error(codes.Unauthenticated, "invalid or missing credentials")
		}

		principal, err := verifier.Verify(raw)
		if err != nil {
			// The specific reason — expired, wrong audience, bad signature —
			// is logged but never returned. Telling a caller which check
			// failed is free help for forging the next attempt.
			logAuthFailure(ctx, log, info.FullMethod, "token rejected", err)
			return nil, status.Error(codes.Unauthenticated, "invalid or missing credentials")
		}

		return handler(auth.WithPrincipal(ctx, principal), req)
	}
}

// logAuthFailure records why authentication failed, with nothing sensitive in
// it: no token, no header, no key material.
func logAuthFailure(ctx context.Context, log *slog.Logger, method, reason string, err error) {
	log.WarnContext(ctx, "authentication failed",
		slog.String("method", method),
		slog.String("reason", reason),
		slog.String("error", err.Error()))
}

// authorize enforces the scope table.
//
// It runs after authenticate and still before the handler, which is what makes
// "authorization happens before any financial state is created" structurally
// true rather than a convention each handler has to remember.
func authorize(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if isUnauthenticated(info.FullMethod) {
			return handler(ctx, req)
		}

		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			// Unreachable while authenticate runs first; kept because an
			// interceptor-ordering mistake must fail closed.
			return nil, status.Error(codes.Unauthenticated, "invalid or missing credentials")
		}

		required, known := requiredScopes[info.FullMethod]
		if !known {
			log.ErrorContext(ctx, "denying a method with no scope mapping",
				slog.String("method", info.FullMethod))
			return nil, status.Error(codes.PermissionDenied, "the token does not carry the required scope")
		}

		if !principal.HasScope(required) {
			log.WarnContext(ctx, "authorization denied",
				slog.String("method", info.FullMethod),
				slog.String("subject", principal.Subject),
				slog.String("required_scope", string(required)),
				slog.Any("granted_scopes", principal.ScopeStrings()))
			return nil, status.Error(codes.PermissionDenied, "the token does not carry the required scope")
		}

		return handler(ctx, req)
	}
}

// logRequests records one line per RPC.
//
// It deliberately logs no request payload: a CreateTransfer carries account
// ids and amounts, and an authorization header would carry a live token.
func logRequests(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()

		resp, err := handler(ctx, req)

		code := codes.OK
		if err != nil {
			code = status.Code(err)
		}

		attrs := []slog.Attr{
			slog.String("method", info.FullMethod),
			slog.String("code", code.String()),
			slog.Duration("duration", time.Since(start)),
		}
		if principal, ok := auth.PrincipalFrom(ctx); ok {
			attrs = append(attrs, slog.String("subject", principal.Subject))
		}

		level := slog.LevelDebug
		if code != codes.OK {
			level = slog.LevelInfo
		}
		log.LogAttrs(ctx, level, "grpc request", attrs...)

		return resp, err
	}
}
