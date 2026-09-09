// Package grpcapi is the gRPC transport for the payments ledger.
//
// It owns service registration, interceptors, error mapping and the server
// lifecycle. It owns no business logic: every handler calls the existing
// service layer, so there is exactly one implementation of a transfer.
package grpcapi

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/auth"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
	paymentsv1 "github.com/Maheshsiddu29/realtime-payments-ledger/internal/gen/payments/v1"
	healthreg "github.com/Maheshsiddu29/realtime-payments-ledger/internal/health"
)

// healthPollInterval is how often the gRPC health service re-reads readiness.
//
// The standard gRPC health protocol stores a status rather than computing one
// per request, so something has to keep it current. Polling avoids running the
// dependency checks once per probe, which would let a health checker generate
// database load.
const healthPollInterval = 5 * time.Second

// Server owns the gRPC listener and its lifecycle.
type Server struct {
	cfg    config.GRPC
	log    *slog.Logger
	server *grpc.Server
	health *health.Server

	// readiness supplies dependency state for the health service.
	readiness *healthreg.Registry

	mu       sync.Mutex
	listener net.Listener

	done chan struct{}
	// stopPolling ends the health poller when the server shuts down.
	stopPolling context.CancelFunc
}

// New builds a gRPC server with the payments service, health and — outside
// production — reflection registered.
//
// verifier may be nil only when authentication is unconfigured, which
// configuration validation already forbids in production. In that state the
// authentication interceptor refuses every payments RPC rather than serving an
// open API.
func New(
	cfg config.Config,
	log *slog.Logger,
	payments *PaymentsService,
	verifier *auth.Verifier,
	readiness *healthreg.Registry,
) *Server {
	// Order matters and is the whole point: log, then authenticate, then
	// authorize, then the handler. A request that fails authentication never
	// reaches authorization, and one that fails authorization never reaches
	// business code — so an unauthorized caller cannot claim an idempotency
	// key, open a transaction or move money.
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(
			logRequests(log),
			authenticate(verifier, log),
			authorize(log),
		),
	)

	paymentsv1.RegisterPaymentsServiceServer(server, payments)

	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(server, healthServer)

	if cfg.GRPC.Reflection {
		// Reflection lets grpcurl and similar tools discover the schema
		// without a .proto file. Convenient in development; in production it
		// is free reconnaissance for an attacker, so configuration validation
		// refuses to enable it there.
		reflection.Register(server)
		log.Info("grpc server reflection enabled", slog.String("environment", string(cfg.App.Environment)))
	}

	return &Server{
		cfg:       cfg.GRPC,
		log:       log,
		server:    server,
		health:    healthServer,
		readiness: readiness,
		done:      make(chan struct{}),
	}
}

// Start binds the listener and serves in the background. It returns once the
// socket is bound, so a caller can rely on Addr afterwards.
func (s *Server) Start(ctx context.Context) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Addr())
	if err != nil {
		return fmt.Errorf("grpc: listen on %s: %w", s.cfg.Addr(), err)
	}

	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	pollCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	s.stopPolling = stop
	go s.pollReadiness(pollCtx)

	s.log.InfoContext(ctx, "grpc server listening", slog.String("addr", ln.Addr().String()))

	go func() {
		defer close(s.done)
		if err := s.server.Serve(ln); err != nil && err != grpc.ErrServerStopped {
			s.log.ErrorContext(ctx, "grpc server stopped serving", slog.String("error", err.Error()))
		}
	}()

	return nil
}

// pollReadiness keeps the gRPC health status in step with dependency
// readiness.
//
// The distinction being maintained here matters: the process being alive is
// not the same as it being able to serve. A NOT_SERVING status means "do not
// send me traffic", which is the readiness question — it does not mean the
// process should be restarted. That is the same split the HTTP /healthz and
// /readyz endpoints make, and the two must not disagree.
func (s *Server) pollReadiness(ctx context.Context) {
	s.updateHealth(ctx)

	ticker := time.NewTicker(healthPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.updateHealth(ctx)
		}
	}
}

func (s *Server) updateHealth(ctx context.Context) {
	status := healthpb.HealthCheckResponse_SERVING
	if s.readiness != nil && !s.readiness.Check(ctx).Healthy() {
		status = healthpb.HealthCheckResponse_NOT_SERVING
	}

	// The empty service name is the conventional "whole server" entry that
	// generic health checkers probe.
	s.health.SetServingStatus("", status)
	s.health.SetServingStatus(serviceName, status)
}

// Addr returns the bound address, or an empty string before Start succeeds.
// With GRPC_PORT=0 this is how a test discovers the ephemeral port.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Shutdown stops the server, giving in-flight RPCs a bounded chance to finish.
//
// GracefulStop waits for active RPCs to complete, which for this service means
// letting a transfer finish committing rather than tearing its connection down
// mid-transaction. It has no timeout of its own, so it is raced against the
// configured budget: if the budget expires, Stop cuts the remaining
// connections. Shutdown therefore always terminates.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	started := s.listener != nil
	s.mu.Unlock()

	if !started {
		return nil
	}

	if s.stopPolling != nil {
		s.stopPolling()
	}

	// Report NOT_SERVING first, so anything watching health stops sending new
	// work while the drain runs.
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	s.health.SetServingStatus(serviceName, healthpb.HealthCheckResponse_NOT_SERVING)
	s.health.Shutdown()

	s.log.InfoContext(ctx, "grpc server shutting down",
		slog.Duration("grace_period", s.cfg.ShutdownTimeout))

	stopped := make(chan struct{})
	go func() {
		s.server.GracefulStop()
		close(stopped)
	}()

	select {
	case <-stopped:
		<-s.done
		s.log.InfoContext(ctx, "grpc server stopped gracefully")
		return nil

	case <-time.After(s.cfg.ShutdownTimeout):
		// The budget is spent. Cut what is left rather than hanging: an
		// unbounded wait here would block process shutdown indefinitely.
		s.log.WarnContext(ctx, "grpc graceful shutdown exceeded its budget; forcing stop",
			slog.Duration("grace_period", s.cfg.ShutdownTimeout))
		s.server.Stop()
		<-stopped
		<-s.done
		return fmt.Errorf("grpc: graceful shutdown exceeded %s and was forced", s.cfg.ShutdownTimeout)
	}
}

// Run starts the server and blocks until ctx is cancelled, then shuts down.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		return err
	}

	select {
	case <-s.done:
		// Serve returned on its own.
	case <-ctx.Done():
		s.log.InfoContext(ctx, "grpc shutdown signal received")
	}

	return s.Shutdown(context.WithoutCancel(ctx))
}
