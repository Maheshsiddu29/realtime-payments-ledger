// Package config loads and validates application configuration from the
// process environment.
//
// The environment is the single source of configuration truth: there are no
// config files and no command-line flags. Every other package receives an
// already-validated Config value, so configuration errors surface once, at
// startup, instead of at the point of first use.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment names the deployment environment the process is running in.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvStaging     Environment = "staging"
	EnvProduction  Environment = "production"
	EnvTest        Environment = "test"
)

// IsProduction reports whether the environment demands production safeguards.
func (e Environment) IsProduction() bool { return e == EnvProduction }

// redacted replaces every secret value in logs and diagnostic output.
const redacted = "[REDACTED]"

// Config is the fully validated configuration for the API process.
type Config struct {
	App         App
	HTTP        HTTP
	GRPC        GRPC
	JWT         JWT
	Postgres    Postgres
	Redis       Redis
	Kafka       Kafka
	Idempotency Idempotency
}

// App holds process-level identity and logging settings.
type App struct {
	Name        string
	Environment Environment
	LogLevel    string
	LogFormat   string
}

// HTTP holds the settings for the health/admin HTTP listener.
type HTTP struct {
	Host            string
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

// Addr returns the host:port the HTTP server binds to.
func (h HTTP) Addr() string {
	return net.JoinHostPort(h.Host, strconv.Itoa(h.Port))
}

// GRPC holds the settings for the application API listener.
//
// This is a separate listener from HTTP on purpose: HTTP serves operational
// probes for infrastructure, gRPC serves payments for clients. They have
// different audiences, different exposure and different failure consequences,
// so they get different ports.
type GRPC struct {
	Host string
	Port int
	// ShutdownTimeout bounds the graceful stop. In-flight transfers get this
	// long to finish before connections are cut.
	ShutdownTimeout time.Duration
	// Reflection enables the gRPC server reflection service, which lets tools
	// such as grpcurl discover the schema. Convenient in development,
	// unnecessary attack surface in production, so it defaults off there.
	Reflection bool
}

// Addr returns the host:port the gRPC server binds to.
func (g GRPC) Addr() string {
	return net.JoinHostPort(g.Host, strconv.Itoa(g.Port))
}

// JWT holds access-token verification settings.
//
// This service verifies tokens; it never issues them. Only a public key is
// configured, so a compromise here cannot forge tokens.
type JWT struct {
	// PublicKeyPEM is the PEM-encoded RSA public key. It may be supplied
	// inline or read from PublicKeyFile. Never logged.
	PublicKeyPEM string
	// PublicKeyFile is a path to read the key from, which is how a real
	// deployment mounts it.
	PublicKeyFile string
	Issuer        string
	Audience      string
	// Leeway tolerates small clock skew when checking exp and nbf.
	Leeway time.Duration
}

// Configured reports whether enough is present to verify tokens.
func (j JWT) Configured() bool {
	return j.PublicKeyPEM != "" && j.Issuer != "" && j.Audience != ""
}

// Postgres holds connection settings for the ledger database.
//
// No connection is opened in Phase 0; these values are validated so that the
// persistence phase inherits a known-good configuration surface.
type Postgres struct {
	Host            string
	Port            int
	User            string
	Password        string
	Database        string
	SSLMode         string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	// ConnectTimeout bounds the initial connection and its verifying ping, so
	// a wedged or unreachable database fails start-up promptly instead of
	// hanging the process.
	ConnectTimeout time.Duration
}

// DSN renders a libpq-style connection string, including the password.
// Never log the result; use RedactedDSN instead.
func (p Postgres) DSN() string { return p.dsn(p.Password) }

// RedactedDSN renders the connection string with the password masked.
func (p Postgres) RedactedDSN() string { return p.dsn(redacted) }

func (p Postgres) dsn(password string) string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s/%s?sslmode=%s",
		p.User, password, net.JoinHostPort(p.Host, strconv.Itoa(p.Port)), p.Database, p.SSLMode,
	)
}

// Redis holds connection settings for the idempotency coordination store.
type Redis struct {
	Addr     string
	Password string
	DB       int
	// DialTimeout bounds establishing a connection.
	DialTimeout time.Duration
	// CommandTimeout bounds a single command. Redis sits in the request path,
	// so a wedged server must fail fast rather than stall a payment.
	CommandTimeout time.Duration
}

// Idempotency holds the lifetimes of idempotency records.
//
// These are Redis-side lifetimes only. Expiry never weakens deduplication:
// the UNIQUE constraint on transfers.idempotency_key is the final barrier and
// has no TTL. See docs/IDEMPOTENCY.md.
type Idempotency struct {
	// TTL is how long a completed result stays cached in Redis. It bounds how
	// long a replay can be served without touching PostgreSQL, not how long
	// deduplication lasts.
	TTL time.Duration
	// ProcessingTTL is the lease on an in-flight claim. If the process holding
	// it dies, the claim expires after this long and another request may take
	// over; PostgreSQL uniqueness still prevents a second transfer.
	ProcessingTTL time.Duration
}

// Kafka holds connection settings for the audit event stream.
type Kafka struct {
	Brokers    []string
	AuditTopic string
}

// Redacted returns a copy of the configuration with every secret masked. Use
// it for any configuration dump that reaches logs or an operator's terminal.
func (c Config) Redacted() Config {
	out := c
	if out.Postgres.Password != "" {
		out.Postgres.Password = redacted
	}
	if out.Redis.Password != "" {
		out.Redis.Password = redacted
	}
	// Key material never reaches a log, even though a public key is not
	// secret: the same field would hold a private key if it were ever
	// misconfigured, and that must not be printable.
	if out.JWT.PublicKeyPEM != "" {
		out.JWT.PublicKeyPEM = redacted
	}
	return out
}

// validSSLModes are the sslmode values libpq accepts.
var validSSLModes = map[string]bool{
	"disable": true, "allow": true, "prefer": true,
	"require": true, "verify-ca": true, "verify-full": true,
}

var validLogLevels = map[string]bool{
	"debug": true, "info": true, "warn": true, "error": true,
}

var validLogFormats = map[string]bool{"json": true, "text": true}

var validEnvironments = map[Environment]bool{
	EnvDevelopment: true, EnvStaging: true, EnvProduction: true, EnvTest: true,
}

// Load reads configuration from the process environment, applies defaults for
// anything unset, and validates the result. It returns every problem it finds
// rather than only the first, so a misconfigured deployment can be fixed in
// one pass.
func Load() (Config, error) { return load(os.LookupEnv) }

// lookupFunc matches os.LookupEnv and lets tests supply an environment without
// mutating global process state.
type lookupFunc func(key string) (string, bool)

func load(lookup lookupFunc) (Config, error) {
	e := &env{lookup: lookup}

	// Read the environment first: several defaults depend on it, and a
	// production deployment must not inherit a development-shaped default.
	isProduction := Environment(e.str("APP_ENV", string(EnvDevelopment))).IsProduction()

	cfg := Config{
		App: App{
			Name:        e.str("APP_NAME", "payments-ledger"),
			Environment: Environment(e.str("APP_ENV", string(EnvDevelopment))),
			LogLevel:    strings.ToLower(e.str("LOG_LEVEL", "info")),
			LogFormat:   strings.ToLower(e.str("LOG_FORMAT", "json")),
		},
		HTTP: HTTP{
			Host:            e.str("HTTP_HOST", "0.0.0.0"),
			Port:            e.intVal("HTTP_PORT", 8080),
			ReadTimeout:     e.duration("HTTP_READ_TIMEOUT", 5*time.Second),
			WriteTimeout:    e.duration("HTTP_WRITE_TIMEOUT", 10*time.Second),
			IdleTimeout:     e.duration("HTTP_IDLE_TIMEOUT", 60*time.Second),
			ShutdownTimeout: e.duration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
		},
		GRPC: GRPC{
			Host:            e.str("GRPC_HOST", "0.0.0.0"),
			Port:            e.intVal("GRPC_PORT", 9090),
			ShutdownTimeout: e.duration("GRPC_SHUTDOWN_TIMEOUT", 15*time.Second),
			Reflection:      e.boolVal("GRPC_REFLECTION", !isProduction),
		},
		JWT: JWT{
			PublicKeyPEM:  e.str("JWT_PUBLIC_KEY", ""),
			PublicKeyFile: e.str("JWT_PUBLIC_KEY_FILE", ""),
			Issuer:        e.str("JWT_ISSUER", ""),
			Audience:      e.str("JWT_AUDIENCE", ""),
			Leeway:        e.duration("JWT_LEEWAY", 30*time.Second),
		},
		Postgres: Postgres{
			Host:            e.str("POSTGRES_HOST", "localhost"),
			Port:            e.intVal("POSTGRES_PORT", 5432),
			User:            e.str("POSTGRES_USER", "ledger"),
			Password:        e.str("POSTGRES_PASSWORD", "ledger"),
			Database:        e.str("POSTGRES_DB", "ledger"),
			SSLMode:         e.str("POSTGRES_SSLMODE", "disable"),
			MaxOpenConns:    e.intVal("POSTGRES_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    e.intVal("POSTGRES_MAX_IDLE_CONNS", 25),
			ConnMaxLifetime: e.duration("POSTGRES_CONN_MAX_LIFETIME", 30*time.Minute),
			ConnectTimeout:  e.duration("POSTGRES_CONNECT_TIMEOUT", 10*time.Second),
		},
		Redis: Redis{
			Addr:           e.str("REDIS_ADDR", "localhost:6379"),
			Password:       e.str("REDIS_PASSWORD", ""),
			DB:             e.intVal("REDIS_DB", 0),
			DialTimeout:    e.duration("REDIS_DIAL_TIMEOUT", 3*time.Second),
			CommandTimeout: e.duration("REDIS_COMMAND_TIMEOUT", time.Second),
		},
		Idempotency: Idempotency{
			TTL:           e.duration("IDEMPOTENCY_TTL", 24*time.Hour),
			ProcessingTTL: e.duration("IDEMPOTENCY_PROCESSING_TTL", 30*time.Second),
		},
		Kafka: Kafka{
			Brokers:    e.list("KAFKA_BROKERS", []string{"localhost:29092"}),
			AuditTopic: e.str("KAFKA_AUDIT_TOPIC", "ledger.audit.v1"),
		},
	}

	problems := append(e.errs, cfg.validate()...)
	if len(problems) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(problems...))
	}
	return cfg, nil
}

func (c Config) validate() []error {
	var errs []error

	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if strings.TrimSpace(c.App.Name) == "" {
		fail("APP_NAME must not be empty")
	}
	if !validEnvironments[c.App.Environment] {
		fail("APP_ENV %q must be one of development, staging, production, test", c.App.Environment)
	}
	if !validLogLevels[c.App.LogLevel] {
		fail("LOG_LEVEL %q must be one of debug, info, warn, error", c.App.LogLevel)
	}
	if !validLogFormats[c.App.LogFormat] {
		fail("LOG_FORMAT %q must be one of json, text", c.App.LogFormat)
	}

	if c.HTTP.Host == "" {
		fail("HTTP_HOST must not be empty")
	}
	// Port 0 is allowed: it asks the kernel for an ephemeral port, which the
	// integration tests rely on.
	if c.HTTP.Port < 0 || c.HTTP.Port > 65535 {
		fail("HTTP_PORT %d must be between 0 and 65535", c.HTTP.Port)
	}
	for _, d := range []struct {
		name  string
		value time.Duration
	}{
		{"HTTP_READ_TIMEOUT", c.HTTP.ReadTimeout},
		{"HTTP_WRITE_TIMEOUT", c.HTTP.WriteTimeout},
		{"HTTP_IDLE_TIMEOUT", c.HTTP.IdleTimeout},
		{"HTTP_SHUTDOWN_TIMEOUT", c.HTTP.ShutdownTimeout},
	} {
		if d.value <= 0 {
			fail("%s must be greater than zero, got %s", d.name, d.value)
		}
	}

	if c.Postgres.Host == "" {
		fail("POSTGRES_HOST must not be empty")
	}
	if c.Postgres.Port < 1 || c.Postgres.Port > 65535 {
		fail("POSTGRES_PORT %d must be between 1 and 65535", c.Postgres.Port)
	}
	if c.Postgres.User == "" {
		fail("POSTGRES_USER must not be empty")
	}
	if c.Postgres.Database == "" {
		fail("POSTGRES_DB must not be empty")
	}
	if !validSSLModes[c.Postgres.SSLMode] {
		fail("POSTGRES_SSLMODE %q is not a valid libpq sslmode", c.Postgres.SSLMode)
	}
	if c.Postgres.MaxOpenConns < 1 {
		fail("POSTGRES_MAX_OPEN_CONNS %d must be at least 1", c.Postgres.MaxOpenConns)
	}
	if c.Postgres.MaxIdleConns < 0 {
		fail("POSTGRES_MAX_IDLE_CONNS %d must not be negative", c.Postgres.MaxIdleConns)
	}
	if c.Postgres.MaxIdleConns > c.Postgres.MaxOpenConns {
		fail("POSTGRES_MAX_IDLE_CONNS %d must not exceed POSTGRES_MAX_OPEN_CONNS %d",
			c.Postgres.MaxIdleConns, c.Postgres.MaxOpenConns)
	}
	if c.Postgres.ConnMaxLifetime < 0 {
		fail("POSTGRES_CONN_MAX_LIFETIME must not be negative, got %s", c.Postgres.ConnMaxLifetime)
	}
	if c.Postgres.ConnectTimeout <= 0 {
		fail("POSTGRES_CONNECT_TIMEOUT must be greater than zero, got %s", c.Postgres.ConnectTimeout)
	}
	if c.App.Environment.IsProduction() && c.Postgres.SSLMode == "disable" {
		fail("POSTGRES_SSLMODE must not be 'disable' when APP_ENV is production")
	}

	if c.GRPC.Host == "" {
		fail("GRPC_HOST must not be empty")
	}
	if c.GRPC.Port < 0 || c.GRPC.Port > 65535 {
		fail("GRPC_PORT %d must be between 0 and 65535", c.GRPC.Port)
	}
	if c.GRPC.Port != 0 && c.GRPC.Port == c.HTTP.Port {
		fail("GRPC_PORT and HTTP_PORT are both %d; the two servers cannot share a port", c.GRPC.Port)
	}
	if c.GRPC.ShutdownTimeout <= 0 {
		fail("GRPC_SHUTDOWN_TIMEOUT must be greater than zero, got %s", c.GRPC.ShutdownTimeout)
	}

	if c.JWT.Leeway < 0 {
		fail("JWT_LEEWAY must not be negative, got %s", c.JWT.Leeway)
	}
	// Clock skew tolerance is a window in which an expired token still works.
	if c.JWT.Leeway > 5*time.Minute {
		fail("JWT_LEEWAY %s is too generous; it is a window in which expired tokens are accepted", c.JWT.Leeway)
	}
	if c.JWT.PublicKeyPEM != "" && c.JWT.PublicKeyFile != "" {
		fail("set JWT_PUBLIC_KEY or JWT_PUBLIC_KEY_FILE, not both")
	}
	// Production must never fall back to an unauthenticated API. Development
	// may run without tokens configured, which is loud in the logs and
	// rejected at start-up here for any real deployment.
	if c.App.Environment.IsProduction() {
		if c.JWT.Issuer == "" {
			fail("JWT_ISSUER is required when APP_ENV is production")
		}
		if c.JWT.Audience == "" {
			fail("JWT_AUDIENCE is required when APP_ENV is production")
		}
		if c.JWT.PublicKeyPEM == "" && c.JWT.PublicKeyFile == "" {
			fail("JWT_PUBLIC_KEY or JWT_PUBLIC_KEY_FILE is required when APP_ENV is production")
		}
		if c.GRPC.Reflection {
			fail("GRPC_REFLECTION must not be enabled when APP_ENV is production")
		}
	}

	if c.Redis.Addr == "" {
		fail("REDIS_ADDR must not be empty")
	}
	if c.Redis.DB < 0 {
		fail("REDIS_DB %d must not be negative", c.Redis.DB)
	}
	if c.Redis.DialTimeout <= 0 {
		fail("REDIS_DIAL_TIMEOUT must be greater than zero, got %s", c.Redis.DialTimeout)
	}
	if c.Redis.CommandTimeout <= 0 {
		fail("REDIS_COMMAND_TIMEOUT must be greater than zero, got %s", c.Redis.CommandTimeout)
	}

	if c.Idempotency.TTL <= 0 {
		fail("IDEMPOTENCY_TTL must be greater than zero, got %s", c.Idempotency.TTL)
	}
	if c.Idempotency.ProcessingTTL <= 0 {
		fail("IDEMPOTENCY_PROCESSING_TTL must be greater than zero, got %s", c.Idempotency.ProcessingTTL)
	}
	// A processing lease that outlives the cached result would leave a key
	// blocking payments after the result it guards has already expired.
	if c.Idempotency.ProcessingTTL > c.Idempotency.TTL {
		fail("IDEMPOTENCY_PROCESSING_TTL %s must not exceed IDEMPOTENCY_TTL %s",
			c.Idempotency.ProcessingTTL, c.Idempotency.TTL)
	}

	if len(c.Kafka.Brokers) == 0 {
		fail("KAFKA_BROKERS must list at least one broker")
	}
	if c.Kafka.AuditTopic == "" {
		fail("KAFKA_AUDIT_TOPIC must not be empty")
	}

	return errs
}

// env reads typed values from a lookup function, accumulating parse errors so
// that a single Load reports every malformed variable at once.
type env struct {
	lookup lookupFunc
	errs   []error
}

func (e *env) str(key, def string) string {
	if v, ok := e.lookup(key); ok {
		return v
	}
	return def
}

func (e *env) intVal(key string, def int) int {
	raw, ok := e.lookup(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %q is not an integer", key, raw))
		return def
	}
	return v
}

func (e *env) boolVal(key string, def bool) bool {
	raw, ok := e.lookup(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %q is not a boolean (true/false)", key, raw))
		return def
	}
	return v
}

func (e *env) duration(key string, def time.Duration) time.Duration {
	raw, ok := e.lookup(key)
	if !ok || raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %q is not a duration (e.g. 5s, 30m)", key, raw))
		return def
	}
	return v
}

// list splits a comma-separated variable, trimming whitespace and dropping
// empty entries.
func (e *env) list(key string, def []string) []string {
	raw, ok := e.lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}
