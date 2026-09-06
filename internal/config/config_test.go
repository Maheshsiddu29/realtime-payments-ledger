package config

import (
	"strings"
	"testing"
	"time"
)

// envMap turns a map into a lookupFunc so tests never mutate process state and
// can therefore run in parallel.
func envMap(m map[string]string) lookupFunc {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := load(envMap(nil))
	if err != nil {
		t.Fatalf("load with empty environment: %v", err)
	}

	if got, want := cfg.App.Name, "payments-ledger"; got != want {
		t.Errorf("App.Name = %q, want %q", got, want)
	}
	if got, want := cfg.App.Environment, EnvDevelopment; got != want {
		t.Errorf("App.Environment = %q, want %q", got, want)
	}
	if got, want := cfg.HTTP.Port, 8080; got != want {
		t.Errorf("HTTP.Port = %d, want %d", got, want)
	}
	if got, want := cfg.HTTP.ShutdownTimeout, 15*time.Second; got != want {
		t.Errorf("HTTP.ShutdownTimeout = %s, want %s", got, want)
	}
	if got, want := cfg.Postgres.Database, "ledger"; got != want {
		t.Errorf("Postgres.Database = %q, want %q", got, want)
	}
	if got, want := cfg.Postgres.ConnectTimeout, 10*time.Second; got != want {
		t.Errorf("Postgres.ConnectTimeout = %s, want %s", got, want)
	}
	if got, want := len(cfg.Kafka.Brokers), 1; got != want {
		t.Fatalf("len(Kafka.Brokers) = %d, want %d", got, want)
	}
	if got, want := cfg.Kafka.Brokers[0], "localhost:29092"; got != want {
		t.Errorf("Kafka.Brokers[0] = %q, want %q", got, want)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := load(envMap(map[string]string{
		"APP_NAME":              "ledger-api",
		"APP_ENV":               "staging",
		"LOG_LEVEL":             "DEBUG",
		"LOG_FORMAT":            "TEXT",
		"HTTP_HOST":             "127.0.0.1",
		"HTTP_PORT":             "9090",
		"HTTP_READ_TIMEOUT":     "2s",
		"HTTP_SHUTDOWN_TIMEOUT": "45s",
		"POSTGRES_SSLMODE":      "require",
		"REDIS_DB":              "3",
		"KAFKA_BROKERS":         " kafka-1:9092 , kafka-2:9092 ,, ",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got, want := cfg.App.Environment, EnvStaging; got != want {
		t.Errorf("App.Environment = %q, want %q", got, want)
	}
	// Log level and format are normalised to lower case.
	if got, want := cfg.App.LogLevel, "debug"; got != want {
		t.Errorf("App.LogLevel = %q, want %q", got, want)
	}
	if got, want := cfg.App.LogFormat, "text"; got != want {
		t.Errorf("App.LogFormat = %q, want %q", got, want)
	}
	if got, want := cfg.HTTP.Addr(), "127.0.0.1:9090"; got != want {
		t.Errorf("HTTP.Addr() = %q, want %q", got, want)
	}
	if got, want := cfg.HTTP.ReadTimeout, 2*time.Second; got != want {
		t.Errorf("HTTP.ReadTimeout = %s, want %s", got, want)
	}
	if got, want := cfg.Redis.DB, 3; got != want {
		t.Errorf("Redis.DB = %d, want %d", got, want)
	}
	if got, want := strings.Join(cfg.Kafka.Brokers, "|"), "kafka-1:9092|kafka-2:9092"; got != want {
		t.Errorf("Kafka.Brokers = %q, want %q", got, want)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     map[string]string
		wantMsg string
	}{
		{
			name:    "unknown environment",
			env:     map[string]string{"APP_ENV": "prod"},
			wantMsg: "APP_ENV",
		},
		{
			name:    "unknown log level",
			env:     map[string]string{"LOG_LEVEL": "verbose"},
			wantMsg: "LOG_LEVEL",
		},
		{
			name:    "port out of range",
			env:     map[string]string{"HTTP_PORT": "70000"},
			wantMsg: "HTTP_PORT",
		},
		{
			name:    "port not an integer",
			env:     map[string]string{"HTTP_PORT": "eighty"},
			wantMsg: "is not an integer",
		},
		{
			name:    "malformed duration",
			env:     map[string]string{"HTTP_READ_TIMEOUT": "5 seconds"},
			wantMsg: "is not a duration",
		},
		{
			name:    "non-positive timeout",
			env:     map[string]string{"HTTP_SHUTDOWN_TIMEOUT": "0s"},
			wantMsg: "HTTP_SHUTDOWN_TIMEOUT",
		},
		{
			name:    "empty database name",
			env:     map[string]string{"POSTGRES_DB": ""},
			wantMsg: "POSTGRES_DB",
		},
		{
			name:    "invalid sslmode",
			env:     map[string]string{"POSTGRES_SSLMODE": "maybe"},
			wantMsg: "POSTGRES_SSLMODE",
		},
		{
			name: "idle connections exceed open connections",
			env: map[string]string{
				"POSTGRES_MAX_OPEN_CONNS": "5",
				"POSTGRES_MAX_IDLE_CONNS": "10",
			},
			wantMsg: "must not exceed",
		},
		{
			name: "production forbids disabled tls",
			env: map[string]string{
				"APP_ENV":          "production",
				"POSTGRES_SSLMODE": "disable",
			},
			wantMsg: "must not be 'disable' when APP_ENV is production",
		},
		{
			name:    "non-positive connect timeout",
			env:     map[string]string{"POSTGRES_CONNECT_TIMEOUT": "0s"},
			wantMsg: "POSTGRES_CONNECT_TIMEOUT",
		},
		{
			name:    "negative redis database",
			env:     map[string]string{"REDIS_DB": "-1"},
			wantMsg: "REDIS_DB",
		},
		{
			name:    "empty audit topic",
			env:     map[string]string{"KAFKA_AUDIT_TOPIC": ""},
			wantMsg: "KAFKA_AUDIT_TOPIC",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := load(envMap(tt.env))
			if err == nil {
				t.Fatalf("load(%v) succeeded, want error mentioning %q", tt.env, tt.wantMsg)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

// Load must report every problem at once so a broken deployment can be fixed
// in a single pass rather than one restart per typo.
func TestLoadReportsAllProblems(t *testing.T) {
	t.Parallel()

	_, err := load(envMap(map[string]string{
		"APP_ENV":   "nope",
		"LOG_LEVEL": "loud",
		"HTTP_PORT": "99999",
	}))
	if err == nil {
		t.Fatal("load succeeded, want aggregated error")
	}
	for _, want := range []string{"APP_ENV", "LOG_LEVEL", "HTTP_PORT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregated error %q is missing %q", err, want)
		}
	}
}

func TestPostgresDSNRedaction(t *testing.T) {
	t.Parallel()

	p := Postgres{
		Host: "db.internal", Port: 5432, User: "ledger",
		Password: "sup3r-s3cret", Database: "ledger", SSLMode: "require",
	}

	if got, want := p.DSN(), "postgres://ledger:sup3r-s3cret@db.internal:5432/ledger?sslmode=require"; got != want {
		t.Errorf("DSN() = %q, want %q", got, want)
	}
	if got := p.RedactedDSN(); strings.Contains(got, "sup3r-s3cret") {
		t.Errorf("RedactedDSN() = %q, must not leak the password", got)
	}
}

func TestConfigRedacted(t *testing.T) {
	t.Parallel()

	cfg, err := load(envMap(map[string]string{
		"POSTGRES_PASSWORD": "pg-secret",
		"REDIS_PASSWORD":    "redis-secret",
	}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	safe := cfg.Redacted()
	if safe.Postgres.Password != redacted {
		t.Errorf("Postgres.Password = %q, want %q", safe.Postgres.Password, redacted)
	}
	if safe.Redis.Password != redacted {
		t.Errorf("Redis.Password = %q, want %q", safe.Redis.Password, redacted)
	}
	// Redacted must not mutate the receiver: the process still needs the real
	// credentials to connect.
	if cfg.Postgres.Password != "pg-secret" {
		t.Errorf("Redacted() mutated the original config: %q", cfg.Postgres.Password)
	}
}

// An unset password must stay empty rather than becoming the literal redaction
// marker, which would otherwise be sent to Redis as a real password.
func TestRedactedLeavesEmptySecretsEmpty(t *testing.T) {
	t.Parallel()

	cfg, err := load(envMap(nil))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Redacted().Redis.Password; got != "" {
		t.Errorf("Redis.Password = %q, want empty", got)
	}
}

func TestEnvironmentIsProduction(t *testing.T) {
	t.Parallel()

	for env, want := range map[Environment]bool{
		EnvProduction:  true,
		EnvStaging:     false,
		EnvDevelopment: false,
		EnvTest:        false,
	} {
		if got := env.IsProduction(); got != want {
			t.Errorf("Environment(%q).IsProduction() = %v, want %v", env, got, want)
		}
	}
}
