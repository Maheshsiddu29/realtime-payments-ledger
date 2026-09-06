package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
)

func TestParseLevel(t *testing.T) {
	t.Parallel()

	for level, want := range map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"error":   slog.LevelError,
		"unknown": slog.LevelInfo, // safety net: config rejects this first
	} {
		if got := parseLevel(level); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", level, got, want)
		}
	}
}

// The logger must emit JSON carrying the service identity, because the log
// pipeline keys on those fields.
func TestNewLoggerEmitsStructuredJSON(t *testing.T) {
	t.Parallel()

	out := captureStdout(t, func(f *os.File) {
		newLogger(config.App{
			Name: "payments-ledger", Environment: config.EnvTest,
			LogLevel: "info", LogFormat: "json",
		}, f).Info("hello", slog.String("k", "v"))
	})

	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &entry); err != nil {
		t.Fatalf("log line %q is not JSON: %v", out, err)
	}
	for key, want := range map[string]string{
		"service": "payments-ledger",
		"env":     "test",
		"msg":     "hello",
		"k":       "v",
	} {
		if got, _ := entry[key].(string); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestNewLoggerTextFormatAndLevelFiltering(t *testing.T) {
	t.Parallel()

	out := captureStdout(t, func(f *os.File) {
		log := newLogger(config.App{
			Name: "payments-ledger", Environment: config.EnvTest,
			LogLevel: "warn", LogFormat: "text",
		}, f)
		log.Debug("suppressed-debug")
		log.Info("suppressed-info")
		log.Warn("emitted-warning")
	})

	if strings.Contains(out, "suppressed") {
		t.Errorf("output %q contains a record below the configured level", out)
	}
	if !strings.Contains(out, "emitted-warning") {
		t.Errorf("output %q is missing the warning", out)
	}
	if strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Errorf("output %q is JSON, want text format", out)
	}
}

// captureStdout runs fn with a pipe standing in for a log destination and
// returns everything written to it.
func captureStdout(t *testing.T, fn func(*os.File)) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()

	fn(w)
	if err := w.Close(); err != nil {
		t.Fatalf("closing pipe: %v", err)
	}
	out := <-done
	if err := r.Close(); err != nil {
		t.Fatalf("closing pipe reader: %v", err)
	}
	return out
}
