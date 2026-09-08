package transfer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Only class-40 rollback codes may be retried. Everything else — business
// rejections above all — must be returned on the first attempt, or a caller's
// refusal could be retried into a success.
func TestRetryableCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		wantCode  string
		wantRetry bool
	}{
		{"serialization failure", &pgconn.PgError{Code: "40001"}, "40001", true},
		{"deadlock detected", &pgconn.PgError{Code: "40P01"}, "40P01", true},
		{"wrapped serialization failure",
			fmt.Errorf("transfer: commit: %w", &pgconn.PgError{Code: "40001"}), "40001", true},
		{"check constraint violation", &pgconn.PgError{Code: "23514"}, "23514", false},
		{"unique violation", &pgconn.PgError{Code: "23505"}, "23505", false},
		{"foreign key violation", &pgconn.PgError{Code: "23503"}, "23503", false},
		{"undefined table", &pgconn.PgError{Code: "42P01"}, "42P01", false},
		{"insufficient funds is not a pg error", ErrInsufficientFunds, "", false},
		{"currency mismatch is not a pg error", ErrCurrencyMismatch, "", false},
		{"source missing is not a pg error", ErrSourceAccountNotFound, "", false},
		{"invalid amount is not a pg error", ErrInvalidAmount, "", false},
		{"context cancelled", context.Canceled, "", false},
		{"nil", nil, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, retry := retryableCode(tt.err)
			if retry != tt.wantRetry {
				t.Errorf("retryable = %v, want %v", retry, tt.wantRetry)
			}
			if code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
		})
	}
}

// A wrapped business error must never become retryable.
func TestBusinessErrorsAreNeverRetryable(t *testing.T) {
	t.Parallel()

	for _, err := range []error{
		ErrInsufficientFunds, ErrCurrencyMismatch, ErrSameAccount,
		ErrInvalidAmount, ErrSourceAccountNotFound, ErrDestinationAccountNotFound,
		ErrNotBalanced, ErrNotFound,
	} {
		wrapped := fmt.Errorf("transfer: %w: account 123 holds 5", err)
		if _, retry := retryableCode(wrapped); retry {
			t.Errorf("%v was classified as retryable", err)
		}
	}
}

// The backoff ceiling doubles per attempt and then stops at MaxDelay; the
// actual delay is uniformly random below that ceiling (full jitter), which is
// what stops every conflicting transaction from waking at the same instant.
func TestRetryPolicyDelayBoundsAndCap(t *testing.T) {
	t.Parallel()

	policy := RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: 8 * time.Millisecond}

	ceilings := map[int]time.Duration{
		1: 1 * time.Millisecond,
		2: 2 * time.Millisecond,
		3: 4 * time.Millisecond,
		4: 8 * time.Millisecond,
		5: 8 * time.Millisecond, // capped
		9: 8 * time.Millisecond, // still capped, no overflow
	}

	for attempt, ceiling := range ceilings {
		for range 200 {
			got := policy.delay(attempt)
			if got < 0 || got > ceiling {
				t.Fatalf("delay(%d) = %s, want within [0, %s]", attempt, got, ceiling)
			}
		}
	}
}

// Full jitter must actually vary, otherwise the herd stays synchronised.
func TestRetryPolicyDelayIsJittered(t *testing.T) {
	t.Parallel()

	policy := RetryPolicy{MaxAttempts: 5, BaseDelay: 10 * time.Millisecond, MaxDelay: time.Second}

	seen := make(map[time.Duration]struct{})
	for range 100 {
		seen[policy.delay(3)] = struct{}{}
	}
	if len(seen) < 10 {
		t.Errorf("delay produced only %d distinct values in 100 draws; jitter is not working", len(seen))
	}
}

// A cancelled request must abandon its backoff immediately rather than sleep
// out the remainder.
func TestRetryPolicyWaitHonoursContextCancellation(t *testing.T) {
	t.Parallel()

	policy := RetryPolicy{MaxAttempts: 5, BaseDelay: 30 * time.Second, MaxDelay: time.Minute}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := policy.wait(ctx, 1)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("wait = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Errorf("wait took %s after cancellation, want it to return immediately", elapsed)
	}
}

func TestRetryPolicyWaitReturnsAfterBackoff(t *testing.T) {
	t.Parallel()

	policy := RetryPolicy{MaxAttempts: 5, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}

	if err := policy.wait(context.Background(), 1); err != nil {
		t.Errorf("wait = %v, want nil", err)
	}
}

// The policy must be bounded by construction: no configuration produces an
// unbounded loop, and the worst-case tail must stay acceptable for a payment.
func TestDefaultRetryPolicyIsBounded(t *testing.T) {
	t.Parallel()

	if DefaultRetryPolicy.MaxAttempts < 1 {
		t.Fatalf("MaxAttempts = %d, want at least 1", DefaultRetryPolicy.MaxAttempts)
	}
	// The budget is generous because SERIALIZABLE aborts rather than queues
	// (see DefaultRetryPolicy), but it must stay bounded and its worst-case
	// backoff must stay within a few seconds.
	if DefaultRetryPolicy.MaxAttempts > 50 {
		t.Errorf("MaxAttempts = %d, which is not a bounded retry budget", DefaultRetryPolicy.MaxAttempts)
	}
	if DefaultRetryPolicy.MaxDelay > time.Second {
		t.Errorf("MaxDelay = %s; long backoffs serialise the workload they are meant to relieve",
			DefaultRetryPolicy.MaxDelay)
	}
	if DefaultRetryPolicy.BaseDelay <= 0 {
		t.Errorf("BaseDelay = %s, want a positive duration", DefaultRetryPolicy.BaseDelay)
	}
	worstCase := time.Duration(DefaultRetryPolicy.MaxAttempts) * DefaultRetryPolicy.MaxDelay
	if worstCase > 5*time.Second {
		t.Errorf("worst-case cumulative backoff is %s, which is too long a tail for a payment request", worstCase)
	}
}

// NewServiceWithPolicy must refuse a zero attempt budget, which would post
// nothing at all.
func TestNewServiceWithPolicyClampsAttempts(t *testing.T) {
	t.Parallel()

	svc := NewServiceWithPolicy(nil, nil, RetryPolicy{MaxAttempts: 0})
	if got := svc.RetryPolicy().MaxAttempts; got != 1 {
		t.Errorf("MaxAttempts = %d, want it clamped to 1", got)
	}
}

func TestAttemptsAccounting(t *testing.T) {
	t.Parallel()

	var a Attempts
	if got := a.Retries(); got != 0 {
		t.Errorf("Retries() on zero value = %d, want 0", got)
	}

	a.Total = 1
	if got := a.Retries(); got != 0 {
		t.Errorf("Retries() after one attempt = %d, want 0", got)
	}

	a.Total = 4
	a.record(sqlStateSerializationFailure)
	a.record(sqlStateSerializationFailure)
	a.record(sqlStateDeadlockDetected)
	a.record("23514") // not retryable, must not be counted

	if got := a.Retries(); got != 3 {
		t.Errorf("Retries() = %d, want 3", got)
	}
	if a.SerializationFailures != 2 {
		t.Errorf("SerializationFailures = %d, want 2", a.SerializationFailures)
	}
	if a.Deadlocks != 1 {
		t.Errorf("Deadlocks = %d, want 1", a.Deadlocks)
	}
}
