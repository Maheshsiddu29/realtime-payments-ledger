// Command api is the entrypoint for the payments ledger API process.
//
// Its only job is wiring: load configuration, build a logger, assemble the
// health registry and the HTTP server, then hand control to the server and
// shut down cleanly on SIGINT or SIGTERM. All behaviour lives in internal/.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/health"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/httpapi"
)

// Build metadata, injected at link time by the Makefile.
var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	if err := run(context.Background()); err != nil {
		// The logger may not exist yet if configuration failed, so report on
		// stderr and exit non-zero for the supervisor.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg.App, os.Stdout)
	slog.SetDefault(log)

	build := httpapi.BuildInfo{Version: version, Commit: commit, Built: buildDate}

	// service and env are already attached to every record by newLogger.
	log.InfoContext(ctx, "starting payments ledger api",
		slog.String("version", build.Version),
		slog.String("commit", build.Commit),
		slog.String("addr", cfg.HTTP.Addr()),
	)
	// Secrets are masked; this is safe to emit.
	log.DebugContext(ctx, "configuration loaded",
		slog.String("postgres_dsn", cfg.Postgres.RedactedDSN()),
		slog.String("redis_addr", cfg.Redis.Addr),
		slog.Any("kafka_brokers", cfg.Kafka.Brokers),
	)

	// SIGTERM is what container orchestrators send; SIGINT is Ctrl-C locally.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Phase 0 has no dependencies to probe, so readiness passes as soon as the
	// process is serving. Later phases register PostgreSQL, Redis and Kafka
	// here.
	registry := health.New(health.DefaultTimeout)

	if err := httpapi.New(cfg, log, registry, build).Run(ctx); err != nil {
		return fmt.Errorf("api server: %w", err)
	}

	log.InfoContext(ctx, "shutdown complete")
	return nil
}

// newLogger builds the process logger from configuration. Output is injected
// so tests can capture it.
func newLogger(cfg config.App, out *os.File) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}

	var handler slog.Handler
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(out, opts)
	} else {
		// JSON by default: logs are shipped to Loki in a later phase and must
		// be machine-parseable.
		handler = slog.NewJSONHandler(out, opts)
	}

	return slog.New(handler).With(
		slog.String("service", cfg.Name),
		slog.String("env", string(cfg.Environment)),
	)
}

// parseLevel maps a validated configuration value to a slog level. Config
// rejects anything unknown, so the default here is only a safety net.
func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
