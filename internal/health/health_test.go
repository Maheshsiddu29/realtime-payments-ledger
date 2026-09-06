package health

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEmptyRegistryIsHealthy(t *testing.T) {
	t.Parallel()

	r := New(0)

	if got, want := r.Len(), 0; got != want {
		t.Errorf("Len() = %d, want %d", got, want)
	}

	report := r.Check(context.Background())
	if !report.Healthy() {
		t.Errorf("Check() = %q, want %q", report.Status, StatusHealthy)
	}
	if len(report.Checks) != 0 {
		t.Errorf("Checks = %v, want empty", report.Checks)
	}
	if got := report.Failed(); len(got) != 0 {
		t.Errorf("Failed() = %v, want empty", got)
	}
}

func TestCheckAggregatesResults(t *testing.T) {
	t.Parallel()

	r := New(time.Second)
	r.Register("postgres", func(context.Context) error { return nil })
	r.Register("redis", func(context.Context) error { return errors.New("dial tcp: connection refused") })
	r.Register("kafka", func(context.Context) error { return nil })

	report := r.Check(context.Background())

	if report.Healthy() {
		t.Errorf("Status = %q, want %q", report.Status, StatusUnhealthy)
	}
	if got, want := len(report.Checks), 3; got != want {
		t.Fatalf("len(Checks) = %d, want %d", got, want)
	}
	if got, want := report.Checks["postgres"].Status, StatusHealthy; got != want {
		t.Errorf("postgres status = %q, want %q", got, want)
	}
	if got := report.Checks["redis"].Error; !strings.Contains(got, "connection refused") {
		t.Errorf("redis error = %q, want it to mention the dial failure", got)
	}
	if got, want := strings.Join(report.Failed(), ","), "redis"; got != want {
		t.Errorf("Failed() = %q, want %q", got, want)
	}
}

func TestRegisterReplacesByName(t *testing.T) {
	t.Parallel()

	r := New(time.Second)
	r.Register("postgres", func(context.Context) error { return errors.New("boom") })
	r.Register("postgres", func(context.Context) error { return nil })

	if got, want := r.Len(), 1; got != want {
		t.Fatalf("Len() = %d, want %d (re-registration must replace, not append)", got, want)
	}
	if report := r.Check(context.Background()); !report.Healthy() {
		t.Errorf("Status = %q, want %q after replacement", report.Status, StatusHealthy)
	}
}

func TestNamesPreservesRegistrationOrder(t *testing.T) {
	t.Parallel()

	r := New(0)
	for _, name := range []string{"postgres", "redis", "kafka"} {
		r.Register(name, func(context.Context) error { return nil })
	}

	if got, want := strings.Join(r.Names(), ","), "postgres,redis,kafka"; got != want {
		t.Errorf("Names() = %q, want %q", got, want)
	}
}

// A dependency that never answers must not hang the readiness probe: the
// registry-level timeout has to cancel it.
func TestCheckHonoursTimeout(t *testing.T) {
	t.Parallel()

	r := New(50 * time.Millisecond)
	r.Register("wedged", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	start := time.Now()
	report := r.Check(context.Background())
	elapsed := time.Since(start)

	if report.Healthy() {
		t.Errorf("Status = %q, want %q", report.Status, StatusUnhealthy)
	}
	if elapsed > time.Second {
		t.Errorf("Check took %s, want it bounded by the registry timeout", elapsed)
	}
}

// Cancelling the caller's context must abort the run even when the registry
// timeout is long.
func TestCheckHonoursCallerCancellation(t *testing.T) {
	t.Parallel()

	r := New(30 * time.Second)
	r.Register("wedged", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	if report := r.Check(ctx); report.Healthy() {
		t.Errorf("Status = %q, want %q", report.Status, StatusUnhealthy)
	}
}

// A panicking probe must be contained: readiness reports unhealthy instead of
// killing the process.
func TestCheckContainsPanics(t *testing.T) {
	t.Parallel()

	r := New(time.Second)
	r.Register("exploding", func(context.Context) error { panic("dependency driver bug") })

	report := r.Check(context.Background())

	if report.Healthy() {
		t.Fatalf("Status = %q, want %q", report.Status, StatusUnhealthy)
	}
	if got := report.Checks["exploding"].Error; !strings.Contains(got, "panicked") {
		t.Errorf("error = %q, want it to record the panic", got)
	}
}

func TestRegisterNilCheckPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("Register(nil) did not panic; a nil check would crash the readiness probe later")
		}
	}()
	New(0).Register("bad", nil)
}

// Registration during start-up overlaps with readiness probes already being
// served. This test fails under -race if the registry's locking is removed.
func TestRegistryConcurrentAccess(t *testing.T) {
	t.Parallel()

	r := New(time.Second)

	const goroutines = 16
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := range goroutines {
		wg.Add(2)

		go func(i int) {
			defer wg.Done()
			<-start
			r.Register(string(rune('a'+i)), func(context.Context) error { return nil })
		}(i)

		go func() {
			defer wg.Done()
			<-start
			_ = r.Check(context.Background())
			_ = r.Names()
			_ = r.Len()
		}()
	}

	close(start)
	wg.Wait()

	if got, want := r.Len(), goroutines; got != want {
		t.Errorf("Len() = %d, want %d", got, want)
	}
	if report := r.Check(context.Background()); !report.Healthy() {
		t.Errorf("Status = %q, want %q", report.Status, StatusHealthy)
	}
}
