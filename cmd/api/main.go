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
	"sync"
	"syscall"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/account"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/auth"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/database"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/grpcapi"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/health"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/httpapi"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/idempotency"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/ledger"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/redisclient"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
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

	// A malformed DSN is a configuration error and is fatal.
	db, err := database.New(cfg, log)
	if err != nil {
		return err
	}
	// Closed after the HTTP server has drained, so in-flight requests keep
	// their connections until they finish.
	defer db.Close()

	// An unreachable database is deliberately not fatal. The process starts and
	// reports itself unready, so an orchestrator withholds traffic instead of
	// restarting the pod in a crash loop, and the service recovers on its own
	// once PostgreSQL comes back. The schema is applied by cmd/migrate, never
	// from here.
	if err := db.Verify(ctx); err != nil {
		log.ErrorContext(ctx, "postgres is not reachable at start-up; /readyz will report unready until it recovers",
			slog.String("error", err.Error()))
	}

	// Redis coordinates idempotent requests. It is deliberately NOT part of
	// the financial source of truth: the UNIQUE constraint on
	// transfers.idempotency_key is what prevents duplicate transfers, and it
	// keeps working while Redis is down.
	redisClient := redisclient.New(cfg, log)
	defer redisClient.Close()

	if err := redisClient.Verify(ctx); err != nil {
		log.ErrorContext(ctx, "redis is not reachable at start-up; idempotency will fall back to postgres uniqueness",
			slog.String("error", err.Error()))
	}

	store := idempotency.NewStore(redisClient.Redis(), cfg)

	registry := health.New(health.DefaultTimeout)
	registry.Register("postgres", db.Ping)
	// Optional: a Redis outage degrades the service — duplicate detection gets
	// slower and replays hit the database — but it does not make the process
	// unable to serve, and deduplication still holds. Marking it required
	// would withdraw traffic from every replica over a dependency that is not
	// needed for correctness, turning a partial outage into a total one.
	registry.RegisterOptional("redis", store.Ping)

	// Authentication. A nil verifier means no key is configured, which
	// configuration validation permits only outside production; the
	// interceptor then refuses every payments RPC rather than serving an open
	// API.
	verifier, err := auth.VerifierFromConfig(cfg)
	if err != nil {
		return err
	}
	if verifier == nil {
		log.WarnContext(ctx, "no JWT verification key configured; every gRPC payments call will be refused",
			slog.String("environment", string(cfg.App.Environment)))
	} else {
		log.InfoContext(ctx, "jwt verification configured",
			slog.String("issuer", cfg.JWT.Issuer),
			slog.String("audience", cfg.JWT.Audience),
			slog.String("algorithm", "RS256"))
	}

	// The gRPC transport calls exactly the same service layer everything else
	// does. There is one transfer implementation.
	payments := grpcapi.NewPaymentsService(
		account.NewRepository(db.Pool()),
		transfer.NewService(db.Pool(), log).WithIdempotency(store),
		ledger.NewRepository(db.Pool()),
		log,
	)

	grpcServer := grpcapi.New(cfg, log, payments, verifier, registry)
	httpServer := httpapi.New(cfg, log, registry, build)

	// Two listeners, one lifecycle. Both are started, both are given the
	// shutdown signal, and the process waits for both to drain before the
	// pools close.
	errs := make(chan error, 2)
	var servers sync.WaitGroup

	servers.Add(2)
	go func() {
		defer servers.Done()
		if err := grpcServer.Run(ctx); err != nil {
			errs <- fmt.Errorf("grpc server: %w", err)
		}
	}()
	go func() {
		defer servers.Done()
		if err := httpServer.Run(ctx); err != nil {
			errs <- fmt.Errorf("http server: %w", err)
		}
	}()

	log.InfoContext(ctx, "servers started",
		slog.String("http_addr", cfg.HTTP.Addr()),
		slog.String("grpc_addr", cfg.GRPC.Addr()))

	servers.Wait()
	close(errs)

	// Report the first failure, if either server failed.
	for err := range errs {
		if err != nil {
			return err
		}
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
