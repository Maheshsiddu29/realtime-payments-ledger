// Package redisclient owns the Redis connection.
//
// It mirrors internal/database: it is the only place that turns configuration
// into a live client and the only place that closes one. What Redis is *for*
// lives in internal/idempotency; this package only manages the connection.
package redisclient

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
)

// Client wraps a Redis client together with the logger used for lifecycle
// events.
type Client struct {
	client *redis.Client
	log    *slog.Logger
	addr   string
}

// New builds a Redis client from configuration.
//
// Like database.New it does not contact the server: go-redis connects lazily,
// and whether Redis is reachable right now is a readiness question rather than
// a start-up one. Call Verify for that.
func New(cfg config.Config, log *slog.Logger) *Client {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,

		// Bounded timeouts throughout. Redis sits in the request path, so a
		// wedged server must fail fast and let the caller fall back to
		// PostgreSQL rather than stall a payment.
		DialTimeout:  cfg.Redis.DialTimeout,
		ReadTimeout:  cfg.Redis.CommandTimeout,
		WriteTimeout: cfg.Redis.CommandTimeout,
	})

	// The address is safe to log; the password never is, and is not included.
	log.Info("redis client created",
		slog.String("addr", cfg.Redis.Addr),
		slog.Int("db", cfg.Redis.DB),
		slog.Duration("dial_timeout", cfg.Redis.DialTimeout),
		slog.Duration("command_timeout", cfg.Redis.CommandTimeout),
	)

	return &Client{client: client, log: log, addr: cfg.Redis.Addr}
}

// Redis returns the underlying client.
func (c *Client) Redis() *redis.Client { return c.client }

// Verify confirms the server answers, bounded by the configured timeouts.
func (c *Client) Verify(ctx context.Context) error {
	if err := c.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis at %s did not respond: %w", c.addr, err)
	}
	return nil
}

// Close releases every pooled connection.
func (c *Client) Close() {
	if err := c.client.Close(); err != nil {
		c.log.Error("closing redis client failed", slog.String("error", err.Error()))
		return
	}
	c.log.Info("redis client closed")
}
