//go:build integration

package tests

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/database"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/health"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/httpapi"
)

// startAPI wires the process the way cmd/api does — pool, health registry with
// the PostgreSQL check registered, HTTP server — and returns its base URL.
func startAPI(t *testing.T, cfg config.Config) string {
	t.Helper()

	cfg.HTTP.Host = "127.0.0.1"
	cfg.HTTP.Port = 0
	cfg.HTTP.ShutdownTimeout = 5 * time.Second

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))

	db, err := database.New(cfg, log)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(db.Close)

	registry := health.New(2 * time.Second)
	registry.Register("postgres", db.Ping)

	srv := httpapi.New(cfg, log, registry, httpapi.BuildInfo{Version: "integration"})
	if err := srv.Start(testContext(t)); err != nil {
		t.Fatalf("starting the server: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Shutdown(testContext(t)); err != nil {
			t.Errorf("shutting down: %v", err)
		}
	})

	return "http://" + srv.Addr()
}

func probe(t *testing.T, url string) (int, health.Report) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}

	var report health.Report
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("decoding %s body %q: %v", url, body, err)
	}
	return resp.StatusCode, report
}

// With PostgreSQL reachable, readiness passes and names the check that passed.
func TestReadyzIsHealthyWhenPostgresIsUp(t *testing.T) {
	cfg, err := testConfig()
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}

	base := startAPI(t, cfg)

	status, report := probe(t, base+"/readyz")
	if status != http.StatusOK {
		t.Fatalf("/readyz = %d, want %d (report %+v)", status, http.StatusOK, report)
	}
	if !report.Healthy() {
		t.Errorf("report status = %q, want healthy", report.Status)
	}

	check, ok := report.Checks["postgres"]
	if !ok {
		t.Fatalf("no 'postgres' check in the readiness report: %+v", report.Checks)
	}
	if check.Status != health.StatusHealthy {
		t.Errorf("postgres check = %q (%s), want healthy", check.Status, check.Error)
	}
}

// The requirement this phase exists to satisfy: when PostgreSQL cannot be
// reached, readiness fails so traffic stops being routed here — but liveness
// keeps passing, so the orchestrator does not restart a process whose only
// problem is a dependency outage.
func TestReadyzFailsButHealthzSurvivesWhenPostgresIsDown(t *testing.T) {
	cfg, err := testConfig()
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}

	// Port 1 is reserved and has nothing listening, so every connection
	// attempt is refused immediately. This is a genuinely unreachable
	// database, not a stubbed-out check.
	cfg.Postgres.Host = "127.0.0.1"
	cfg.Postgres.Port = 1
	cfg.Postgres.ConnectTimeout = time.Second

	base := startAPI(t, cfg)

	t.Run("readyz reports 503", func(t *testing.T) {
		status, report := probe(t, base+"/readyz")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("/readyz = %d, want %d (report %+v)", status, http.StatusServiceUnavailable, report)
		}
		if report.Healthy() {
			t.Errorf("report status = %q, want unhealthy", report.Status)
		}
		if failed := report.Failed(); len(failed) != 1 || failed[0] != "postgres" {
			t.Errorf("failed checks = %v, want [postgres]", failed)
		}
		if got := report.Checks["postgres"].Error; got == "" {
			t.Error("the postgres check reported no error message")
		}
	})

	t.Run("healthz stays 200", func(t *testing.T) {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("/healthz = %d, want %d: a database outage must not make the process look dead",
				resp.StatusCode, http.StatusOK)
		}
	})
}

// A malformed DSN is a configuration error, not a transient outage, so it
// fails immediately and visibly instead of degrading to unready.
func TestInvalidPostgresConfigurationFailsFast(t *testing.T) {
	cfg, err := testConfig()
	if err != nil {
		t.Fatalf("testConfig: %v", err)
	}
	cfg.Postgres.Host = "not a valid host:with colons"

	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if _, err := database.New(cfg, log); err == nil {
		t.Error("database.New accepted a malformed DSN, want a clear configuration error")
	}
}
