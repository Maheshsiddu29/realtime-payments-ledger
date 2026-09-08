package transfer

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// PostgreSQL SQLSTATE codes that a transfer may safely be retried on.
//
// Both belong to class 40 — "Transaction Rollback". PostgreSQL guarantees the
// transaction was rolled back completely before either is reported, so a retry
// starts from a clean slate and cannot double-apply anything.
const (
	// sqlStateSerializationFailure (40001) is raised when SERIALIZABLE cannot
	// prove that concurrent transactions were equivalent to some serial order.
	// It is the expected, designed-for outcome of contention, not a defect.
	sqlStateSerializationFailure = "40001"

	// sqlStateDeadlockDetected (40P01) is raised when PostgreSQL breaks a lock
	// cycle by aborting one transaction.
	//
	// Deterministic lock ordering (see lockAccounts) is what prevents transfer
	// postings from forming such a cycle in the first place, so this code
	// should not appear for this workload. It is retried anyway because the
	// cost is nil and the alternative is a spurious failure: a concurrent
	// admin statement, a future code path, or maintenance touching the same
	// rows in a different order can still create a cycle that this transaction
	// merely lost. A deadlock victim is rolled back exactly like a
	// serialization failure, so retrying is equally safe.
	sqlStateDeadlockDetected = "40P01"
)

// ErrRetriesExhausted wraps the final error when every attempt failed with a
// retryable condition. It is distinct from a business rejection so that
// callers and load tooling can tell contention apart from a real refusal.
var ErrRetriesExhausted = errors.New("transfer: retries exhausted")

// RetryPolicy bounds how hard a transfer tries before giving up.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, including the first one.
	// It is never unbounded.
	MaxAttempts int
	// BaseDelay is the backoff ceiling after the first failure. It doubles
	// with each subsequent failure, up to MaxDelay.
	BaseDelay time.Duration
	// MaxDelay caps the backoff ceiling.
	MaxDelay time.Duration
}

// DefaultRetryPolicy is the policy used by NewService. Its values were chosen
// from measurement, not intuition; the runs are recorded in docs/results/.
//
// Twenty attempts. This is larger than a typical retry budget, for a reason
// specific to SERIALIZABLE. When several transactions contend for one account
// row, PostgreSQL does not queue them: a transaction that waits on a row lock
// and then finds the row was modified by a committed transaction is aborted
// with 40001 rather than allowed to re-read. Contention therefore produces an
// abort rate that rises with concurrency instead of a queue, and the retry
// budget has to absorb it.
//
// Measured on this workload (single hot source account, all attempts released
// simultaneously): 20 concurrent transfers need about 4.5 retries each, 120
// need about 5.6, and every attempt still completes. Smaller budgets fail
// well within the range of ordinary contention — at 5 attempts, only 9 of 20
// simultaneous transfers completed.
//
// It remains strictly bounded: at most 20 attempts and roughly 1.3s of
// cumulative backoff before a transfer is refused with ErrRetriesExhausted.
// Exhaustion is a refusal, never a correctness failure — the ledger is intact
// either way.
//
// The delays are deliberately short. This is a single database round trip on
// the same host, not a call across the internet; backing off for hundreds of
// milliseconds would serialise the whole workload and make contention worse
// than the conflict it is meant to relieve.
var DefaultRetryPolicy = RetryPolicy{
	MaxAttempts: 20,
	BaseDelay:   time.Millisecond,
	MaxDelay:    100 * time.Millisecond,
}

// delay returns how long to wait after a given failed attempt (1-based).
//
// This is "full jitter": a uniformly random duration in [0, ceiling], where
// the ceiling doubles per attempt up to MaxDelay. Randomising the whole
// interval rather than adding jitter to a fixed delay is what actually breaks
// up a thundering herd — without it, every transaction that conflicted at the
// same moment wakes up at the same moment and conflicts again.
func (p RetryPolicy) delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	ceiling := p.BaseDelay
	for range attempt - 1 {
		ceiling *= 2
		// Stop as soon as the cap is reached, which also prevents the shift
		// from overflowing on a large attempt count.
		if ceiling >= p.MaxDelay {
			ceiling = p.MaxDelay
			break
		}
	}
	if ceiling > p.MaxDelay {
		ceiling = p.MaxDelay
	}
	if ceiling <= 0 {
		return 0
	}

	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}

// wait sleeps for the backoff belonging to a failed attempt, or returns early
// if the caller's context is cancelled. A cancelled request must stop
// immediately rather than finish its backoff.
func (p RetryPolicy) wait(ctx context.Context, attempt int) error {
	d := p.delay(attempt)
	if d <= 0 {
		// Still honour an already-cancelled context.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryableCode reports the SQLSTATE of err when it is a condition worth
// retrying, and whether it is one.
//
// Classification is by SQLSTATE through pgconn.PgError, never by matching text
// in the error message: messages are localised, reworded between releases and
// vary by driver, whereas SQLSTATE codes are part of the SQL standard and
// stable. Business rejections such as ErrInsufficientFunds are not PgErrors at
// all and can never be mistaken for retryable ones.
func retryableCode(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return "", false
	}

	switch pgErr.Code {
	case sqlStateSerializationFailure, sqlStateDeadlockDetected:
		return pgErr.Code, true
	default:
		return pgErr.Code, false
	}
}

// Attempts records what one Post call cost. It lets tests and the load
// generator count contention without an observability stack, which is a later
// phase.
type Attempts struct {
	// Total is the number of attempts made, at least 1 for any call that
	// reached the database. A command rejected by validation makes none.
	Total int
	// SerializationFailures counts attempts lost to SQLSTATE 40001.
	SerializationFailures int
	// Deadlocks counts attempts lost to SQLSTATE 40P01.
	Deadlocks int
}

// Retries is the number of times the transfer had to be attempted again.
func (a Attempts) Retries() int {
	if a.Total <= 1 {
		return 0
	}
	return a.Total - 1
}

// record notes a retryable failure against the matching counter.
func (a *Attempts) record(code string) {
	switch code {
	case sqlStateSerializationFailure:
		a.SerializationFailures++
	case sqlStateDeadlockDetected:
		a.Deadlocks++
	}
}

func (a Attempts) String() string {
	return fmt.Sprintf("attempts=%d serialization_failures=%d deadlocks=%d",
		a.Total, a.SerializationFailures, a.Deadlocks)
}
