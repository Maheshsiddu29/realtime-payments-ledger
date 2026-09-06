// Package httpapi exposes the operational HTTP surface of the service:
// liveness, readiness and build information.
//
// This is deliberately not the business API. Payment operations are served
// over gRPC in a later phase; this listener exists so orchestrators can decide
// whether to route traffic to the process and when to restart it.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/health"
)

// BuildInfo describes the binary. Values are injected at link time; see the
// LDFLAGS in the Makefile.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Built   string `json:"built"`
}

// Server owns the operational HTTP listener and its lifecycle.
type Server struct {
	cfg    config.HTTP
	log    *slog.Logger
	health *health.Registry
	build  BuildInfo
	app    config.App

	http *http.Server

	mu       sync.Mutex
	listener net.Listener

	// done is closed when Serve returns; serveErr is written before the close,
	// so any receive on done happens-after the write.
	done     chan struct{}
	serveErr error
}

// New builds a server. It does not bind a socket; call Start or Run.
func New(cfg config.Config, log *slog.Logger, registry *health.Registry, build BuildInfo) *Server {
	s := &Server{
		cfg:    cfg.HTTP,
		app:    cfg.App,
		log:    log,
		health: registry,
		build:  build,
		done:   make(chan struct{}),
	}

	s.http = &http.Server{
		Handler:           s.routes(),
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s
}

// routes builds the operational mux. Method-qualified patterns mean anything
// other than GET is rejected with 405 by the router itself.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleLive)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /version", s.handleVersion)
	return s.withLogging(mux)
}

// handleLive answers the liveness probe. It reports only that the process is
// running and able to serve; it must never consult a dependency, because a
// failing dependency is not a reason to restart this process.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, s.log, http.StatusOK, map[string]string{"status": string(health.StatusHealthy)})
}

// handleReady answers the readiness probe by running the registered checks.
// It returns 503 when any dependency is unusable so the orchestrator stops
// routing traffic here without restarting the process.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	report := s.health.Check(r.Context())

	status := http.StatusOK
	if !report.Healthy() {
		status = http.StatusServiceUnavailable
		s.log.WarnContext(r.Context(), "readiness check failed", slog.Any("failed", report.Failed()))
	}
	writeJSON(w, r, s.log, status, report)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, s.log, http.StatusOK, map[string]any{
		"name":        s.app.Name,
		"environment": string(s.app.Environment),
		"build":       s.build,
	})
}

// withLogging records one structured line per request.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		s.log.LogAttrs(r.Context(), slog.LevelDebug, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func writeJSON(w http.ResponseWriter, r *http.Request, log *slog.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already on the wire, so this can only be logged.
		log.WarnContext(r.Context(), "writing response body failed", slog.String("error", err.Error()))
	}
}

// Start binds the listener and serves in the background. It returns once the
// socket is bound, so a caller (or a test) can rely on Addr afterwards.
func (s *Server) Start(ctx context.Context) error {
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "tcp", s.cfg.Addr())
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Addr(), err)
	}

	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	s.log.InfoContext(ctx, "http server listening", slog.String("addr", ln.Addr().String()))

	go func() {
		defer close(s.done)
		// Serve always returns a non-nil error; ErrServerClosed is the normal
		// outcome of a graceful shutdown.
		if err := s.http.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr = fmt.Errorf("http server: %w", err)
		}
	}()

	return nil
}

// Addr returns the bound address, or an empty string before Start succeeds.
// With HTTP_PORT=0 this is how the caller discovers the ephemeral port.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// Shutdown stops accepting connections and waits for in-flight requests to
// finish, bounded by the configured shutdown timeout. Requests still running
// when that budget expires are cut off and an error is returned.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	started := s.listener != nil
	s.mu.Unlock()

	// Nothing was ever bound, so there is nothing to drain and no serve
	// goroutine that will ever close done.
	if !started {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout)
	defer cancel()

	s.log.InfoContext(ctx, "http server shutting down",
		slog.Duration("grace_period", s.cfg.ShutdownTimeout))

	shutdownErr := s.http.Shutdown(ctx)
	if shutdownErr != nil {
		// Drop the remaining connections rather than leaking them.
		shutdownErr = fmt.Errorf("graceful shutdown: %w", shutdownErr)
		if closeErr := s.http.Close(); closeErr != nil {
			shutdownErr = errors.Join(shutdownErr, fmt.Errorf("force close: %w", closeErr))
		}
	}

	// Wait for Serve to return, then surface anything it reported. Shutdown is
	// safe to call more than once: receiving from a closed channel never blocks.
	<-s.done
	return errors.Join(shutdownErr, s.serveErr)
}

// Run starts the server and blocks until ctx is cancelled or the server fails,
// then shuts down gracefully. It is the entrypoint used by cmd/api.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		return err
	}

	select {
	case <-s.done:
		// The listener stopped on its own, without a shutdown signal.
	case <-ctx.Done():
		s.log.InfoContext(ctx, "shutdown signal received",
			slog.String("cause", context.Cause(ctx).Error()))
	}

	// ctx may already be cancelled, so the grace period is measured from a
	// live context derived from it.
	return s.Shutdown(context.WithoutCancel(ctx))
}
