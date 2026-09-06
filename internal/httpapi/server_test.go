package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/health"
)

// discardLogger keeps test output readable while still exercising every
// logging path in the server.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func testConfig() config.Config {
	return config.Config{
		App: config.App{Name: "payments-ledger", Environment: config.EnvTest},
		HTTP: config.HTTP{
			Host:            "127.0.0.1",
			Port:            0, // ephemeral: tests must not fight over a fixed port
			ReadTimeout:     time.Second,
			WriteTimeout:    time.Second,
			IdleTimeout:     time.Second,
			ShutdownTimeout: 2 * time.Second,
		},
	}
}

func newTestServer(t *testing.T, registry *health.Registry) *Server {
	t.Helper()
	if registry == nil {
		registry = health.New(time.Second)
	}
	return New(testConfig(), discardLogger(), registry, BuildInfo{Version: "test", Commit: "abc123"})
}

func TestHealthzIsAlwaysOK(t *testing.T) {
	t.Parallel()

	// Liveness must ignore dependency state: a broken database is not a reason
	// to restart the process.
	registry := health.New(time.Second)
	registry.Register("postgres", func(context.Context) error { return errors.New("down") })

	rec := httptest.NewRecorder()
	newTestServer(t, registry).routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if got, want := rec.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if got, want := body["status"], string(health.StatusHealthy); got != want {
		t.Errorf("status field = %q, want %q", got, want)
	}
}

func TestReadyzReflectsDependencies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		register   func(*health.Registry)
		wantStatus int
	}{
		{
			name:       "no dependencies registered",
			register:   func(*health.Registry) {},
			wantStatus: http.StatusOK,
		},
		{
			name: "all dependencies healthy",
			register: func(r *health.Registry) {
				r.Register("postgres", func(context.Context) error { return nil })
				r.Register("redis", func(context.Context) error { return nil })
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "one dependency unhealthy",
			register: func(r *health.Registry) {
				r.Register("postgres", func(context.Context) error { return nil })
				r.Register("redis", func(context.Context) error { return errors.New("connection refused") })
			},
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			registry := health.New(time.Second)
			tt.register(registry)

			rec := httptest.NewRecorder()
			newTestServer(t, registry).routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if got := rec.Code; got != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", got, tt.wantStatus, rec.Body.String())
			}

			var report health.Report
			if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if want := tt.wantStatus == http.StatusOK; report.Healthy() != want {
				t.Errorf("report.Healthy() = %v, want %v", report.Healthy(), want)
			}
		})
	}
}

func TestVersionEndpoint(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	newTestServer(t, nil).routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/version", nil))

	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}

	var body struct {
		Name        string    `json:"name"`
		Environment string    `json:"environment"`
		Build       BuildInfo `json:"build"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	if got, want := body.Name, "payments-ledger"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if got, want := body.Build.Commit, "abc123"; got != want {
		t.Errorf("build.commit = %q, want %q", got, want)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	t.Parallel()

	handler := newTestServer(t, nil).routes()

	tests := []struct {
		method, path string
		wantStatus   int
	}{
		{http.MethodGet, "/does-not-exist", http.StatusNotFound},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/readyz", http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if got := rec.Code; got != tt.wantStatus {
				t.Errorf("status = %d, want %d", got, tt.wantStatus)
			}
		})
	}
}

// Health responses must never be cached: a stale 200 would keep traffic
// flowing to a process that has already gone unready.
func TestHealthResponsesAreNotCacheable(t *testing.T) {
	t.Parallel()

	handler := newTestServer(t, nil).routes()

	for _, path := range []string{"/healthz", "/readyz", "/version"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if got, want := rec.Header().Get("Cache-Control"), "no-store"; got != want {
			t.Errorf("%s Cache-Control = %q, want %q", path, got, want)
		}
	}
}

func TestStartAddrAndShutdown(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, nil)

	if got := srv.Addr(); got != "" {
		t.Errorf("Addr() before Start = %q, want empty", got)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	addr := srv.Addr()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("Addr() = %q, want a 127.0.0.1 address", addr)
	}

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	// Shutdown is idempotent: the signal handler and a deferred call may both
	// reach it.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

func TestShutdownBeforeStartIsNoOp(t *testing.T) {
	t.Parallel()

	if err := newTestServer(t, nil).Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown before Start: %v", err)
	}
}

// Run must return only after the listener is closed, and must not report an
// error for an ordinary signal-driven shutdown.
func TestRunStopsOnContextCancellation(t *testing.T) {
	t.Parallel()

	srv := newTestServer(t, nil)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	// Wait for the socket to be bound before signalling shutdown.
	addr := waitForAddr(t, srv)
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned %v, want nil for a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation")
	}

	if _, err := http.Get("http://" + addr + "/healthz"); err == nil {
		t.Error("server still accepting connections after Run returned")
	}
}

func TestStartFailsOnBusyPort(t *testing.T) {
	t.Parallel()

	first := newTestServer(t, nil)
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = first.Shutdown(context.Background()) })

	_, port, ok := strings.Cut(first.Addr(), ":")
	if !ok {
		t.Fatalf("unexpected address %q", first.Addr())
	}

	cfg := testConfig()
	cfg.HTTP.Port = atoi(t, port)
	second := New(cfg, discardLogger(), health.New(time.Second), BuildInfo{})

	err := second.Start(context.Background())
	if err == nil {
		_ = second.Shutdown(context.Background())
		t.Fatal("Start on an occupied port succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "listen on") {
		t.Errorf("error = %q, want it to name the failing listen", err)
	}
}

func waitForAddr(t *testing.T, srv *Server) string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := srv.Addr(); addr != "" {
			return addr
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("server never bound an address")
	return ""
}

func atoi(t *testing.T, s string) int {
	t.Helper()

	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("port %q is not numeric", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}
