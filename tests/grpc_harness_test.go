//go:build integration

package tests

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/account"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/auth"
	paymentsv1 "github.com/Maheshsiddu29/realtime-payments-ledger/internal/gen/payments/v1"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/grpcapi"
	healthreg "github.com/Maheshsiddu29/realtime-payments-ledger/internal/health"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/idempotency"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/ledger"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

// The gRPC tests run against a real server on an ephemeral port, spoken to by
// a real gRPC client. Handlers are never called directly: the point is to test
// the transport contract — interceptors, metadata, status codes, protobuf
// conversion — and calling a handler in-process would skip all of it.
//
// All key material is GENERATED AT RUN TIME. Nothing in this repository is a
// private key.

const (
	grpcTestIssuer   = "https://issuer.test"
	grpcTestAudience = "payments-api"
)

// testKeys holds the signing key the test issuer uses, plus a second key that
// the server does not trust, for signature-mismatch cases.
type testKeys struct {
	signer    *rsa.PrivateKey
	publicPEM string
	untrusted *rsa.PrivateKey
}

func newTestKeys(t *testing.T) testKeys {
	t.Helper()

	signer, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating signing key: %v", err)
	}
	untrusted, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating untrusted key: %v", err)
	}

	der, err := x509.MarshalPKIXPublicKey(&signer.PublicKey)
	if err != nil {
		t.Fatalf("marshalling public key: %v", err)
	}

	return testKeys{
		signer:    signer,
		publicPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		untrusted: untrusted,
	}
}

// tokenOptions describes a token to mint. The zero value produces a valid
// token carrying every scope.
type tokenOptions struct {
	subject   string
	issuer    string
	audience  string
	scopes    []auth.Scope
	noScopes  bool
	expiresAt *time.Time
	notBefore *time.Time
	omitExp   bool

	// signWith overrides the signing key, for forgery cases.
	signWith any
	// method overrides the signing algorithm, for algorithm-confusion cases.
	method jwt.SigningMethod
}

func (k testKeys) token(t *testing.T, opts tokenOptions) string {
	t.Helper()

	now := time.Now()

	subject := opts.subject
	if subject == "" {
		subject = "integration-test-client"
	}
	issuer := opts.issuer
	if issuer == "" {
		issuer = grpcTestIssuer
	}
	audience := opts.audience
	if audience == "" {
		audience = grpcTestAudience
	}

	scopes := opts.scopes
	if scopes == nil && !opts.noScopes {
		scopes = auth.AllScopes
	}
	rendered := make([]string, 0, len(scopes))
	for _, s := range scopes {
		rendered = append(rendered, string(s))
	}

	claims := jwt.MapClaims{
		"sub": subject,
		"iss": issuer,
		"aud": audience,
		"iat": now.Unix(),
	}
	if len(rendered) > 0 {
		claims["scope"] = strings.Join(rendered, " ")
	}
	if !opts.omitExp {
		exp := now.Add(time.Hour)
		if opts.expiresAt != nil {
			exp = *opts.expiresAt
		}
		claims["exp"] = exp.Unix()
	}
	if opts.notBefore != nil {
		claims["nbf"] = opts.notBefore.Unix()
	}

	method := opts.method
	if method == nil {
		method = jwt.SigningMethodRS256
	}
	var key any = k.signer
	if opts.signWith != nil {
		key = opts.signWith
	}

	raw, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("signing test token: %v", err)
	}
	return raw
}

// grpcEnv is a running server plus a client that talks to it.
type grpcEnv struct {
	*env

	keys   testKeys
	addr   string
	client paymentsv1.PaymentsServiceClient
	conn   *grpc.ClientConn
	server *grpcapi.Server
}

// newGRPCEnv starts a real gRPC server on an ephemeral port against the shared
// PostgreSQL and Redis, and dials it.
func newGRPCEnv(t *testing.T) *grpcEnv {
	t.Helper()

	base := newEnv(t)
	keys := newTestKeys(t)

	cfg := sharedConfig
	cfg.GRPC.Host = "127.0.0.1"
	cfg.GRPC.Port = 0 // ephemeral: tests must not fight over a fixed port
	cfg.GRPC.ShutdownTimeout = 5 * time.Second
	cfg.GRPC.Reflection = false
	cfg.JWT.PublicKeyPEM = keys.publicPEM
	cfg.JWT.Issuer = grpcTestIssuer
	cfg.JWT.Audience = grpcTestAudience
	cfg.JWT.Leeway = 0

	verifier, err := auth.NewVerifier(auth.VerifierConfig{
		PublicKeyPEM: cfg.JWT.PublicKeyPEM,
		Issuer:       cfg.JWT.Issuer,
		Audience:     cfg.JWT.Audience,
		Leeway:       cfg.JWT.Leeway,
	})
	if err != nil {
		t.Fatalf("auth.NewVerifier: %v", err)
	}

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	store := idempotency.NewStore(sharedRedis, cfg)

	payments := grpcapi.NewPaymentsService(
		account.NewRepository(base.pool),
		transfer.NewService(base.pool, log).WithIdempotency(store),
		ledger.NewRepository(base.pool),
		log,
	)

	registry := healthreg.New(2 * time.Second)
	server := grpcapi.New(cfg, log, payments, verifier, registry)

	if err := server.Start(testContext(t)); err != nil {
		t.Fatalf("starting the grpc server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting down the grpc server: %v", err)
		}
	})

	conn, err := grpc.NewClient(server.Addr(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dialling %s: %v", server.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return &grpcEnv{
		env:    base,
		keys:   keys,
		addr:   server.Addr(),
		client: paymentsv1.NewPaymentsServiceClient(conn),
		conn:   conn,
		server: server,
	}
}

// authed returns a context carrying a bearer token with the given options.
func (g *grpcEnv) authed(t *testing.T, ctx context.Context, opts tokenOptions) context.Context {
	t.Helper()
	return g.withToken(ctx, g.keys.token(t, opts))
}

// full returns a context authenticated with every scope.
func (g *grpcEnv) full(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	return g.authed(t, ctx, tokenOptions{})
}

// withToken attaches a raw token, for cases that need a malformed header.
func (g *grpcEnv) withToken(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// withRawAuthorization attaches an arbitrary authorization header value.
func (g *grpcEnv) withRawAuthorization(ctx context.Context, header string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", header)
}

// newAccountRPC creates an account through the API and returns its id.
func (g *grpcEnv) newAccountRPC(t *testing.T, ctx context.Context, currency string) string {
	t.Helper()

	resp, err := g.client.CreateAccount(g.full(t, ctx), &paymentsv1.CreateAccountRequest{Currency: currency})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	return resp.GetAccount().GetId()
}

// fundRPC creates an account through the API and funds it with the test-only
// fixture, which writes a balance directly. THAT IS NOT PRODUCTION
// FUNCTIONALITY — see the note on fund() in the harness.
func (g *grpcEnv) fundedAccountRPC(t *testing.T, ctx context.Context, currency string, minor int64) string {
	t.Helper()

	id := g.newAccountRPC(t, ctx, currency)
	if minor > 0 {
		fund(t, g.env, uuidMust(t, id), minor)
	}
	return id
}

// uuidMust parses a UUID returned by the API, failing the test if the API
// produced something unparseable.
func uuidMust(t *testing.T, raw string) uuid.UUID {
	t.Helper()

	id, err := uuid.Parse(raw)
	if err != nil {
		t.Fatalf("the API returned an unparseable UUID %q: %v", raw, err)
	}
	return id
}

// newUUIDString returns a fresh UUID, for tests that need a well-formed but
// unknown identifier.
func newUUIDString() string { return uuid.New().String() }
