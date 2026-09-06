// Package tests holds end-to-end tests that exercise the process the way an
// operator does: real environment variables, a real listener, real HTTP
// requests and a real shutdown signal.
//
// These tests need no external infrastructure. Tests that require PostgreSQL,
// Redis or Kafka arrive with the phases that introduce those dependencies and
// will be guarded by a build tag.
package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/health"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/httpapi"
)

// bootstrap loads configuration from the environment exactly as the binary
// does, starts the server on an ephemeral port, and returns its base URL, a
// memoised wait for the server's exit status, and the shutdown trigger. The
// server is always shut down when the test finishes.
func bootstrap(t *testing.T, registry *health.Registry) (baseURL string, wait func() error, stop context.CancelFunc) {
	t.Helper()

	// t.Setenv also restores the previous values, so these tests cannot leak
	// configuration into each other. It forbids t.Parallel, which is why the
	// tests in this package run sequentially.
	t.Setenv("APP_ENV", "test")
	t.Setenv("APP_NAME", "payments-ledger")
	t.Setenv("LOG_LEVEL", "error")
	t.Setenv("HTTP_HOST", "127.0.0.1")
	t.Setenv("HTTP_PORT", "0")
	t.Setenv("HTTP_SHUTDOWN_TIMEOUT", "5s")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := httpapi.New(cfg, log, registry, httpapi.BuildInfo{Version: "e2e", Commit: "test"})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	addr := ""
	for time.Now().Before(deadline) && addr == "" {
		if addr = srv.Addr(); addr == "" {
			time.Sleep(time.Millisecond)
		}
	}
	if addr == "" {
		cancel()
		t.Fatal("server never bound an address")
	}

	// Both the test body and the cleanup may want the exit status, but only
	// one receive can succeed, so memoise it.
	var (
		once      sync.Once
		runResult error
	)
	wait = func() error {
		once.Do(func() {
			select {
			case runResult = <-errCh:
			case <-time.After(10 * time.Second):
				runResult = errors.New("server did not shut down within 10s")
			}
		})
		return runResult
	}

	t.Cleanup(func() {
		cancel()
		if err := wait(); err != nil {
			t.Errorf("server exited with %v, want a clean shutdown", err)
		}
	})

	return "http://" + addr, wait, cancel
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s body: %v", url, err)
	}
	return resp.StatusCode, body
}

// The process must boot from environment configuration alone and answer every
// operational probe.
func TestAPIStartsAndServesHealthEndpoints(t *testing.T) {
	base, _, _ := bootstrap(t, health.New(time.Second))

	t.Run("healthz", func(t *testing.T) {
		status, body := get(t, base+"/healthz")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %s)", status, http.StatusOK, body)
		}
	})

	t.Run("readyz", func(t *testing.T) {
		status, body := get(t, base+"/readyz")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %s)", status, http.StatusOK, body)
		}

		var report health.Report
		if err := json.Unmarshal(body, &report); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if !report.Healthy() {
			t.Errorf("report = %+v, want healthy", report)
		}
	})

	t.Run("version", func(t *testing.T) {
		status, body := get(t, base+"/version")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %s)", status, http.StatusOK, body)
		}

		var v struct {
			Name        string `json:"name"`
			Environment string `json:"environment"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if v.Environment != "test" {
			t.Errorf("environment = %q, want %q (configuration did not reach the server)", v.Environment, "test")
		}
	})
}

// A failing dependency must take the process out of the load-balancer rotation
// (readiness) without making it look dead (liveness).
func TestReadinessFailsIndependentlyOfLiveness(t *testing.T) {
	registry := health.New(time.Second)
	registry.Register("postgres", func(context.Context) error {
		return errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	})

	base, _, _ := bootstrap(t, registry)

	if status, body := get(t, base+"/readyz"); status != http.StatusServiceUnavailable {
		t.Errorf("/readyz status = %d, want %d (body %s)", status, http.StatusServiceUnavailable, body)
	}
	if status, body := get(t, base+"/healthz"); status != http.StatusOK {
		t.Errorf("/healthz status = %d, want %d (body %s)", status, http.StatusOK, body)
	}
}

// Graceful shutdown means a request already being served is allowed to finish
// after the signal arrives, rather than having its connection cut.
func TestShutdownDrainsInFlightRequests(t *testing.T) {
	started := make(chan struct{})
	registry := health.New(3 * time.Second)
	registry.Register("slow", func(ctx context.Context) error {
		close(started)
		select {
		case <-time.After(400 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	base, wait, stop := bootstrap(t, registry)

	type result struct {
		status int
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Get(base + "/readyz")
		if err != nil {
			resCh <- result{err: err}
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		resCh <- result{status: resp.StatusCode}
	}()

	// Signal shutdown only once the request is genuinely being handled.
	<-started
	stop()

	select {
	case res := <-resCh:
		if res.err != nil {
			t.Fatalf("in-flight request was cut off during shutdown: %v", res.err)
		}
		if res.status != http.StatusOK {
			t.Errorf("in-flight request status = %d, want %d", res.status, http.StatusOK)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	if err := wait(); err != nil {
		t.Errorf("Run returned %v, want nil for a clean shutdown", err)
	}
}

// Invalid configuration must stop the process at start-up rather than
// surfacing later as a runtime failure.
func TestInvalidConfigurationIsRejectedAtStartup(t *testing.T) {
	t.Setenv("HTTP_PORT", "not-a-port")

	if _, err := config.Load(); err == nil {
		t.Fatal("config.Load succeeded with an invalid HTTP_PORT, want an error")
	}
}
