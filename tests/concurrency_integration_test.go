//go:build integration

package tests

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/account"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

// outcome is what one concurrent attempt produced.
type outcome struct {
	transfer transfer.Transfer
	attempts transfer.Attempts
	err      error
}

// workload is the aggregate of a concurrent run.
type workload struct {
	outcomes []outcome

	Successful         int
	InsufficientFunds  int
	ExhaustedRetries   int
	Unexpected         int
	SerializationRetry int
	DeadlockRetry      int
	TotalAttempts      int
	Duration           time.Duration
}

// unexpectedErrors returns the errors that were neither a success nor an
// accepted business rejection, so a failure message can name them.
func (w workload) unexpectedErrors() []error {
	var errs []error
	for _, o := range w.outcomes {
		if o.err == nil || errors.Is(o.err, transfer.ErrInsufficientFunds) ||
			errors.Is(o.err, transfer.ErrRetriesExhausted) {
			continue
		}
		errs = append(errs, o.err)
	}
	return errs
}

// runConcurrently posts every command at once and classifies the results.
//
// All goroutines block on a barrier channel and are released together, so the
// contention is real rather than an artefact of staggered start times. There
// are no sleeps anywhere in the synchronisation: the barrier is a closed
// channel and completion is a WaitGroup, so the test is deterministic and
// cannot flake on timing.
func runConcurrently(t *testing.T, ctx context.Context, e *env, cmds []transfer.Command) workload {
	t.Helper()

	outcomes := make([]outcome, len(cmds))
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i, cmd := range cmds {
		wg.Add(1)
		go func(i int, cmd transfer.Command) {
			defer wg.Done()

			<-start // release everyone at the same moment

			posted, attempts, err := e.transfers.PostWithAttempts(ctx, cmd)
			outcomes[i] = outcome{transfer: posted, attempts: attempts, err: err}
		}(i, cmd)
	}

	began := time.Now()
	close(start)

	// Wait with a timeout so a genuine deadlock fails the test instead of
	// hanging CI forever.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("concurrent workload did not finish before the context expired (%v); "+
			"this is what an unresolved deadlock looks like", ctx.Err())
	}

	w := workload{outcomes: outcomes, Duration: time.Since(began)}
	for _, o := range outcomes {
		w.TotalAttempts += o.attempts.Total
		w.SerializationRetry += o.attempts.SerializationFailures
		w.DeadlockRetry += o.attempts.Deadlocks

		switch {
		case o.err == nil:
			w.Successful++
		case errors.Is(o.err, transfer.ErrInsufficientFunds):
			w.InsufficientFunds++
		case errors.Is(o.err, transfer.ErrRetriesExhausted):
			w.ExhaustedRetries++
		default:
			w.Unexpected++
		}
	}
	return w
}

// oneWay builds n identical transfers from src to dst.
func oneWay(src, dst uuid.UUID, amount int64, n int) []transfer.Command {
	cmds := make([]transfer.Command, n)
	for i := range cmds {
		cmds[i] = transfer.Command{
			SourceAccountID: src, DestinationAccountID: dst,
			AmountMinor: amount, Currency: USD,
		}
	}
	return cmds
}

// ---------------------------------------------------------------------------
// Double spend
// ---------------------------------------------------------------------------

// The central concurrency claim: a source account cannot be overspent, no
// matter how many transfers race for the same money.
//
// A holds exactly ten transfers' worth. Twenty attempts run at once. Exactly
// ten may succeed — not "some", exactly ten — and the money must land in B
// undamaged.
func TestConcurrentTransfersCannotOverspend(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext(t), 60*time.Second)
	defer cancel()

	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 1000)
	bob := newFundedAccount(t, e, USD, 0)

	const (
		amount     = 100
		attempts   = 20
		affordable = 10
	)

	w := runConcurrently(t, ctx, e, oneWay(alice.ID, bob.ID, amount, attempts))

	t.Logf("attempts=%d successful=%d insufficient_funds=%d exhausted=%d unexpected=%d "+
		"serialization_retries=%d deadlock_retries=%d duration=%s",
		attempts, w.Successful, w.InsufficientFunds, w.ExhaustedRetries, w.Unexpected,
		w.SerializationRetry, w.DeadlockRetry, w.Duration)

	for _, err := range w.unexpectedErrors() {
		t.Errorf("unexpected failure: %v", err)
	}

	if w.Successful != affordable {
		t.Errorf("successful transfers = %d, want exactly %d", w.Successful, affordable)
	}
	if w.Successful+w.InsufficientFunds+w.ExhaustedRetries+w.Unexpected != attempts {
		t.Errorf("outcomes do not add up to %d attempts", attempts)
	}

	if got := balanceOf(t, e, alice.ID); got != 0 {
		t.Errorf("source balance = %d, want 0", got)
	}
	if got := balanceOf(t, e, bob.ID); got != affordable*amount {
		t.Errorf("destination balance = %d, want %d", got, affordable*amount)
	}
	if got, want := totalBalances(t, e), int64(1000); got != want {
		t.Errorf("total balances = %d, want %d: money was created or destroyed", got, want)
	}

	assertCompletedTransferCount(t, e, affordable)
	assertReconciled(t, e)
}

// Retry exhaustion is a legitimate outcome under contention, but it must not
// be how the ledger stays correct. This run gives every attempt enough money,
// so all of them must succeed.
func TestConcurrentTransfersAllAffordableSucceed(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext(t), 90*time.Second)
	defer cancel()

	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 100_000)
	bob := newFundedAccount(t, e, USD, 0)

	const attempts = 50

	w := runConcurrently(t, ctx, e, oneWay(alice.ID, bob.ID, 100, attempts))

	t.Logf("attempts=%d successful=%d exhausted=%d unexpected=%d serialization_retries=%d deadlock_retries=%d duration=%s",
		attempts, w.Successful, w.ExhaustedRetries, w.Unexpected,
		w.SerializationRetry, w.DeadlockRetry, w.Duration)

	for _, err := range w.unexpectedErrors() {
		t.Errorf("unexpected failure: %v", err)
	}
	if w.Successful != attempts {
		t.Errorf("successful = %d, want all %d to succeed when funds are ample", w.Successful, attempts)
	}
	if got := balanceOf(t, e, alice.ID); got != 100_000-attempts*100 {
		t.Errorf("source balance = %d, want %d", got, 100_000-attempts*100)
	}
	if got, want := totalBalances(t, e), int64(100_000); got != want {
		t.Errorf("total = %d, want %d", got, want)
	}

	assertCompletedTransferCount(t, e, attempts)
	assertReconciled(t, e)
}

// ---------------------------------------------------------------------------
// Opposing transfers: the deadlock regression
// ---------------------------------------------------------------------------

// A to B and B to A running at once is the scenario deterministic lock
// ordering exists to survive. Without it, the two directions take the same two
// row locks in opposite orders and PostgreSQL breaks the cycle by aborting a
// transaction with SQLSTATE 40P01.
//
// The test asserts the workload finishes inside a bounded timeout, that no
// deadlock was reported at all, and that the money is conserved exactly.
func TestOpposingTransfersDoNotDeadlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext(t), 120*time.Second)
	defer cancel()

	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 50_000)
	bob := newFundedAccount(t, e, USD, 50_000)
	const total = 100_000

	// Interleaved so the two directions are genuinely simultaneous.
	const pairs = 60
	cmds := make([]transfer.Command, 0, pairs*2)
	for range pairs {
		cmds = append(cmds,
			transfer.Command{SourceAccountID: alice.ID, DestinationAccountID: bob.ID, AmountMinor: 100, Currency: USD},
			transfer.Command{SourceAccountID: bob.ID, DestinationAccountID: alice.ID, AmountMinor: 100, Currency: USD},
		)
	}

	w := runConcurrently(t, ctx, e, cmds)

	t.Logf("attempts=%d successful=%d insufficient_funds=%d exhausted=%d unexpected=%d "+
		"serialization_retries=%d deadlock_retries=%d duration=%s",
		len(cmds), w.Successful, w.InsufficientFunds, w.ExhaustedRetries, w.Unexpected,
		w.SerializationRetry, w.DeadlockRetry, w.Duration)

	for _, err := range w.unexpectedErrors() {
		t.Errorf("unexpected failure: %v", err)
	}

	// The specific regression: with canonical lock ordering no transfer should
	// ever be chosen as a deadlock victim.
	if w.DeadlockRetry != 0 {
		t.Errorf("observed %d deadlock retries (SQLSTATE 40P01); lock ordering is not canonical", w.DeadlockRetry)
	}
	// Both accounts are funded well beyond the workload, so nothing should be
	// refused for lack of funds and nothing should exhaust its retry budget.
	if w.InsufficientFunds != 0 {
		t.Errorf("insufficient funds = %d, want 0: both accounts were funded for the whole workload", w.InsufficientFunds)
	}
	if w.Successful != len(cmds) {
		t.Errorf("successful = %d, want all %d", w.Successful, len(cmds))
	}

	if got := totalBalances(t, e); got != total {
		t.Errorf("total balances = %d, want %d", got, total)
	}
	// Equal numbers each way, so the balances must come back to where they started.
	if got := balanceOf(t, e, alice.ID); got != 50_000 {
		t.Errorf("alice = %d, want 50000 after equal flows in both directions", got)
	}
	if got := balanceOf(t, e, bob.ID); got != 50_000 {
		t.Errorf("bob = %d, want 50000 after equal flows in both directions", got)
	}

	assertCompletedTransferCount(t, e, len(cmds))
	assertReconciled(t, e)
}

// ---------------------------------------------------------------------------
// Lock order must not change transfer semantics
// ---------------------------------------------------------------------------

// Rows are locked lowest-UUID-first, which for half of all transfers means the
// destination is locked before the source. The debit must still land on the
// source. Getting this wrong would silently reverse half the transfers while
// keeping every balance invariant intact, so no other test would catch it.
func TestLockOrderDoesNotSwapSourceAndDestination(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	// Create several accounts and pick a deterministic low/high pair, so the
	// test does not depend on which UUIDs happened to be generated.
	accounts := make([]account.Account, 0, 6)
	for range 6 {
		accounts = append(accounts, newAccount(t, e, USD))
	}
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].ID.String() < accounts[j].ID.String()
	})
	low, high := accounts[0], accounts[len(accounts)-1]

	t.Run("source has the higher uuid so it is locked second", func(t *testing.T) {
		fund(t, e, high.ID, 5_000)

		posted, err := e.transfers.Post(ctx, transfer.Command{
			SourceAccountID: high.ID, DestinationAccountID: low.ID,
			AmountMinor: 1_200, Currency: USD,
		})
		if err != nil {
			t.Fatalf("Post: %v", err)
		}

		if got := balanceOf(t, e, high.ID); got != 3_800 {
			t.Errorf("source (higher uuid, locked second) balance = %d, want 3800: the debit did not land on the source", got)
		}
		if got := balanceOf(t, e, low.ID); got != 1_200 {
			t.Errorf("destination (lower uuid, locked first) balance = %d, want 1200", got)
		}

		assertEntrySides(t, e, posted.ID, high.ID, low.ID, 1_200)
	})

	t.Run("source has the lower uuid so it is locked first", func(t *testing.T) {
		fund(t, e, low.ID, 5_000)
		before := balanceOf(t, e, low.ID)

		posted, err := e.transfers.Post(ctx, transfer.Command{
			SourceAccountID: low.ID, DestinationAccountID: high.ID,
			AmountMinor: 700, Currency: USD,
		})
		if err != nil {
			t.Fatalf("Post: %v", err)
		}

		if got := balanceOf(t, e, low.ID); got != before-700 {
			t.Errorf("source balance = %d, want %d", got, before-700)
		}
		assertEntrySides(t, e, posted.ID, low.ID, high.ID, 700)
	})

	assertReconciled(t, e)
}

// assertEntrySides checks that the debit is against the source and the credit
// against the destination, with the right signs.
func assertEntrySides(t *testing.T, e *env, transferID, sourceID, destinationID uuid.UUID, amount int64) {
	t.Helper()

	entries, err := e.entries.EntriesForTransfer(testContext(t), transferID)
	if err != nil {
		t.Fatalf("EntriesForTransfer: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	debit, credit := entries[0], entries[1] // ordered by amount ascending
	if debit.AccountID != sourceID {
		t.Errorf("debit is against %s, want the source %s", debit.AccountID, sourceID)
	}
	if debit.AmountMinor != -amount {
		t.Errorf("debit = %d, want %d", debit.AmountMinor, -amount)
	}
	if credit.AccountID != destinationID {
		t.Errorf("credit is against %s, want the destination %s", credit.AccountID, destinationID)
	}
	if credit.AmountMinor != amount {
		t.Errorf("credit = %d, want %d", credit.AmountMinor, amount)
	}
}

// ---------------------------------------------------------------------------
// Multi-account contention
// ---------------------------------------------------------------------------

// Four accounts in a ring, plus two chords. Every pair is touched from both
// directions across the workload, which is where a lock-ordering mistake shows
// up as a cycle of three or more transactions rather than a simple pair.
//
// The scenario is fixed rather than randomised so a CI failure is reproducible.
func TestMultiAccountConcurrentTransfersConserveMoney(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext(t), 120*time.Second)
	defer cancel()

	e := newEnv(t)

	a := newFundedAccount(t, e, USD, 25_000)
	b := newFundedAccount(t, e, USD, 25_000)
	c := newFundedAccount(t, e, USD, 25_000)
	d := newFundedAccount(t, e, USD, 25_000)
	const total = 100_000

	edges := []struct{ from, to uuid.UUID }{
		{a.ID, b.ID}, {b.ID, c.ID}, {c.ID, d.ID}, {d.ID, a.ID}, {a.ID, c.ID}, {b.ID, d.ID},
	}

	const rounds = 25
	cmds := make([]transfer.Command, 0, len(edges)*rounds)
	for range rounds {
		for _, edge := range edges {
			cmds = append(cmds, transfer.Command{
				SourceAccountID: edge.from, DestinationAccountID: edge.to,
				AmountMinor: 100, Currency: USD,
			})
		}
	}

	w := runConcurrently(t, ctx, e, cmds)

	t.Logf("attempts=%d successful=%d insufficient_funds=%d exhausted=%d unexpected=%d "+
		"serialization_retries=%d deadlock_retries=%d duration=%s",
		len(cmds), w.Successful, w.InsufficientFunds, w.ExhaustedRetries, w.Unexpected,
		w.SerializationRetry, w.DeadlockRetry, w.Duration)

	for _, err := range w.unexpectedErrors() {
		t.Errorf("unexpected failure: %v", err)
	}
	if w.DeadlockRetry != 0 {
		t.Errorf("observed %d deadlock retries across a 4-account cycle; lock ordering is not canonical", w.DeadlockRetry)
	}

	// Conservation is the invariant that must hold whatever the mix of
	// successes and rejections turned out to be.
	if got := totalBalances(t, e); got != total {
		t.Errorf("total balances = %d, want %d", got, total)
	}
	assertNoNegativeBalances(t, e)
	assertCompletedTransferCount(t, e, w.Successful)
	assertReconciled(t, e)
}

// ---------------------------------------------------------------------------
// Progressive concurrency
// ---------------------------------------------------------------------------

// Correctness must not depend on the level of contention, so the same
// invariants are asserted at increasing concurrency. Larger runs (500, 1000)
// live in cmd/stress rather than here, to keep CI fast and stable.
func TestProgressiveConcurrencyPreservesInvariants(t *testing.T) {
	for _, attempts := range []int{10, 25, 50, 100} {
		t.Run(scale(attempts), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(testContext(t), 120*time.Second)
			defer cancel()

			e := newEnv(t)

			// Fund exactly half the attempts, so every run exercises both the
			// success path and the insufficient-funds path under contention.
			affordable := attempts / 2
			alice := newFundedAccount(t, e, USD, int64(affordable)*100)
			bob := newFundedAccount(t, e, USD, 0)
			total := int64(affordable) * 100

			w := runConcurrently(t, ctx, e, oneWay(alice.ID, bob.ID, 100, attempts))

			t.Logf("attempts=%d successful=%d insufficient_funds=%d exhausted=%d unexpected=%d "+
				"serialization_retries=%d deadlock_retries=%d duration=%s",
				attempts, w.Successful, w.InsufficientFunds, w.ExhaustedRetries, w.Unexpected,
				w.SerializationRetry, w.DeadlockRetry, w.Duration)

			for _, err := range w.unexpectedErrors() {
				t.Errorf("unexpected failure: %v", err)
			}
			if w.Successful != affordable {
				t.Errorf("successful = %d, want exactly %d", w.Successful, affordable)
			}
			if got := balanceOf(t, e, alice.ID); got != 0 {
				t.Errorf("source balance = %d, want 0", got)
			}
			if got := balanceOf(t, e, bob.ID); got != total {
				t.Errorf("destination balance = %d, want %d", got, total)
			}
			if got := totalBalances(t, e); got != total {
				t.Errorf("total = %d, want %d", got, total)
			}

			assertNoNegativeBalances(t, e)
			assertCompletedTransferCount(t, e, affordable)
			assertReconciled(t, e)
		})
	}
}

// ---------------------------------------------------------------------------
// Retry behaviour under contention
// ---------------------------------------------------------------------------

// A business rejection must be returned immediately, never retried. If
// insufficient funds were retried, a refusal could eventually be granted.
func TestBusinessRejectionsAreNotRetried(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 100)
	bob := newFundedAccount(t, e, USD, 0)

	_, attempts, err := e.transfers.PostWithAttempts(ctx, transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 5_000, Currency: USD,
	})
	if !errors.Is(err, transfer.ErrInsufficientFunds) {
		t.Fatalf("err = %v, want ErrInsufficientFunds", err)
	}
	if attempts.Total != 1 {
		t.Errorf("attempts = %d, want exactly 1: a business rejection must not be retried", attempts.Total)
	}
	if attempts.Retries() != 0 {
		t.Errorf("retries = %d, want 0", attempts.Retries())
	}
}

// A command rejected by validation must not reach the database at all.
func TestValidationFailuresCostNoAttempt(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 1_000)

	_, attempts, err := e.transfers.PostWithAttempts(ctx, transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: alice.ID,
		AmountMinor: 100, Currency: USD,
	})
	if !errors.Is(err, transfer.ErrSameAccount) {
		t.Fatalf("err = %v, want ErrSameAccount", err)
	}
	if attempts.Total != 0 {
		t.Errorf("attempts = %d, want 0: validation runs before any database work", attempts.Total)
	}
}

// A cancelled request must stop rather than continue retrying.
func TestCancelledContextStopsPosting(t *testing.T) {
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 1_000)
	bob := newFundedAccount(t, e, USD, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := e.transfers.PostWithAttempts(ctx, transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 100, Currency: USD,
	})
	if err == nil {
		t.Fatal("Post succeeded with a cancelled context, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}

	if got := balanceOf(t, e, alice.ID); got != 1_000 {
		t.Errorf("source balance = %d, want 1000 unchanged", got)
	}
	assertCompletedTransferCount(t, e, 0)
}

// ---------------------------------------------------------------------------
// Shared assertions
// ---------------------------------------------------------------------------

// assertNoNegativeBalances queries the database directly rather than trusting
// any Go-side bookkeeping.
func assertNoNegativeBalances(t *testing.T, e *env) {
	t.Helper()

	var n int
	err := e.pool.QueryRow(testContext(t),
		`SELECT COUNT(*) FROM accounts WHERE balance_minor < 0`).Scan(&n)
	if err != nil {
		t.Fatalf("counting negative balances: %v", err)
	}
	if n != 0 {
		t.Errorf("%d accounts have a negative balance", n)
	}
}

// assertCompletedTransferCount checks that exactly the expected number of
// transfers committed, and that no transfer was left pending — a pending row
// would mean a transaction committed halfway.
func assertCompletedTransferCount(t *testing.T, e *env, want int) {
	t.Helper()

	var completed, pending, failed int
	err := e.pool.QueryRow(testContext(t), `
		SELECT
			COUNT(*) FILTER (WHERE status = 'completed'),
			COUNT(*) FILTER (WHERE status = 'pending'),
			COUNT(*) FILTER (WHERE status = 'failed')
		FROM transfers`).Scan(&completed, &pending, &failed)
	if err != nil {
		t.Fatalf("counting transfers: %v", err)
	}

	if completed != want {
		t.Errorf("completed transfers = %d, want %d", completed, want)
	}
	if pending != 0 {
		t.Errorf("%d transfers are still pending; a posting transaction committed partially", pending)
	}
	if failed != 0 {
		t.Errorf("%d transfers are marked failed, which this phase never writes", failed)
	}
}

func scale(n int) string {
	switch n {
	case 10:
		return "10_attempts"
	case 25:
		return "25_attempts"
	case 50:
		return "50_attempts"
	case 100:
		return "100_attempts"
	default:
		return "attempts"
	}
}
