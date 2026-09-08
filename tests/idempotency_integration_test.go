//go:build integration

package tests

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/idempotency"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

// key builds a validated idempotency key, failing the test if it is malformed.
func key(t *testing.T, s string) idempotency.Key {
	t.Helper()

	k, err := idempotency.ParseKey(s)
	if err != nil {
		t.Fatalf("ParseKey(%q): %v", s, err)
	}
	return k
}

// countTransfersForKey reports how many transfer rows carry an idempotency
// key, straight from the database.
func countTransfersForKey(t *testing.T, e *env, k idempotency.Key) int {
	t.Helper()

	var n int
	err := e.pool.QueryRow(testContext(t),
		`SELECT COUNT(*) FROM transfers WHERE idempotency_key = $1`, k.String()).Scan(&n)
	if err != nil {
		t.Fatalf("counting transfers for key %s: %v", k, err)
	}
	return n
}

// ---------------------------------------------------------------------------
// The headline scenario
// ---------------------------------------------------------------------------

// Twelve identical requests, one idempotency key, all released together.
// Exactly one financial transfer may exist, and every successful caller must
// be told about that same transfer.
func TestSameKeyConcurrentRequestsCreateExactlyOneTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext(t), 60*time.Second)
	defer cancel()

	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 100_000)
	bob := newFundedAccount(t, e, USD, 0)

	const (
		callers = 12
		amount  = 2_500
	)
	k := key(t, "pay_01HQ8XZ4K9YN3TQF7WV2MC6RJD")
	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: amount, Currency: USD,
	}

	type outcome struct {
		result transfer.Result
		err    error
	}
	outcomes := make([]outcome, callers)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release every caller at the same instant
			res, err := e.transfers.PostIdempotent(ctx, k, cmd)
			outcomes[i] = outcome{result: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var (
		succeeded  int
		inProgress int
		unexpected []error
		ids        = map[uuid.UUID]struct{}{}
		created    int
		replayed   int
		recovered  int
	)
	for _, o := range outcomes {
		switch {
		case o.err == nil:
			succeeded++
			ids[o.result.Transfer.ID] = struct{}{}
			if o.result.Replayed {
				replayed++
			} else {
				created++
			}
			if o.result.RecoveredFromDatabase {
				recovered++
			}
		case errors.Is(o.err, transfer.ErrIdempotencyInProgress):
			inProgress++
		default:
			unexpected = append(unexpected, o.err)
		}
	}

	t.Logf("callers=%d succeeded=%d in_progress=%d unexpected=%d distinct_transfer_ids=%d "+
		"created=%d replayed=%d recovered_from_postgres=%d",
		callers, succeeded, inProgress, len(unexpected), len(ids), created, replayed, recovered)

	for _, err := range unexpected {
		t.Errorf("unexpected failure: %v", err)
	}

	// The assertions that matter.
	if got := countTransfersForKey(t, e, k); got != 1 {
		t.Errorf("transfer rows for the key = %d, want exactly 1", got)
	}
	if len(ids) != 1 {
		t.Errorf("distinct transfer IDs returned = %d, want exactly 1", len(ids))
	}
	if created != 1 {
		t.Errorf("callers that created a transfer = %d, want exactly 1", created)
	}
	if succeeded == 0 {
		t.Fatal("no caller succeeded")
	}

	// Exactly one payment's worth of money moved.
	if got, want := balanceOf(t, e, alice.ID), int64(100_000-amount); got != want {
		t.Errorf("source balance = %d, want %d: more than one debit was applied", got, want)
	}
	if got, want := balanceOf(t, e, bob.ID), int64(amount); got != want {
		t.Errorf("destination balance = %d, want %d", got, want)
	}
	if got, want := totalBalances(t, e), int64(100_000); got != want {
		t.Errorf("total balances = %d, want %d", got, want)
	}

	// Exactly one debit and one credit.
	for id := range ids {
		entries, err := e.entries.EntriesForTransfer(ctx, id)
		if err != nil {
			t.Fatalf("EntriesForTransfer: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("transfer %s has %d ledger entries, want 2", id, len(entries))
		}
		if !entries[0].IsDebit() || !entries[1].IsCredit() {
			t.Errorf("entries are not one debit and one credit: %+v", entries)
		}
	}
	if n := countRows(t, e, "ledger_entries"); n != 2 {
		t.Errorf("ledger_entries has %d rows, want 2 for a single transfer", n)
	}

	assertCompletedTransferCount(t, e, 1)
	assertNoNegativeBalances(t, e)
	assertReconciled(t, e)

	// The rest of the client story. A caller told "in progress" retries, and
	// the policy is only useful if that retry resolves. Every one of them must
	// now receive the same transfer, and still no second posting may occur.
	t.Run("callers told in progress resolve on retry", func(t *testing.T) {
		if inProgress == 0 {
			t.Skip("no caller was told in progress in this run")
		}

		for i := range inProgress {
			res, err := e.transfers.PostIdempotent(ctx, k, cmd)
			if err != nil {
				t.Fatalf("retry %d: %v", i, err)
			}
			if !res.Replayed {
				t.Errorf("retry %d created a new transfer", i)
			}
			ids[res.Transfer.ID] = struct{}{}
		}

		if len(ids) != 1 {
			t.Errorf("distinct transfer IDs across all callers = %d, want exactly 1", len(ids))
		}
		if got := countTransfersForKey(t, e, k); got != 1 {
			t.Errorf("transfer rows = %d, want 1 after every caller retried", got)
		}
		if got, want := balanceOf(t, e, alice.ID), int64(100_000-amount); got != want {
			t.Errorf("source balance = %d, want %d", got, want)
		}
		assertCompletedTransferCount(t, e, 1)
		assertReconciled(t, e)
	})
}

// ---------------------------------------------------------------------------
// Same key, different request
// ---------------------------------------------------------------------------

// A key must not silently accept a different payment. The second request is
// refused and, critically, never executed.
func TestSameKeyDifferentRequestIsRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*transfer.Command, uuid.UUID)
	}{
		{"different amount", func(c *transfer.Command, _ uuid.UUID) { c.AmountMinor = 2_000 }},
		{"different destination", func(c *transfer.Command, other uuid.UUID) { c.DestinationAccountID = other }},
		{"different currency", func(c *transfer.Command, _ uuid.UUID) { c.Currency = "EUR" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := testContext(t)
			e := newEnv(t)

			alice := newFundedAccount(t, e, USD, 50_000)
			bob := newFundedAccount(t, e, USD, 0)
			carol := newFundedAccount(t, e, USD, 0)

			k := key(t, "abc")
			first := transfer.Command{
				SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
				AmountMinor: 1_000, Currency: USD,
			}

			if _, err := e.transfers.PostIdempotent(ctx, k, first); err != nil {
				t.Fatalf("first request: %v", err)
			}
			balanceAfterFirst := balanceOf(t, e, alice.ID)

			second := first
			tt.mutate(&second, carol.ID)

			_, err := e.transfers.PostIdempotent(ctx, k, second)
			if !errors.Is(err, transfer.ErrIdempotencyConflict) {
				t.Fatalf("second request = %v, want ErrIdempotencyConflict", err)
			}

			// The second payment must not have executed.
			if got := countTransfersForKey(t, e, k); got != 1 {
				t.Errorf("transfer rows for the key = %d, want 1", got)
			}
			if got := balanceOf(t, e, alice.ID); got != balanceAfterFirst {
				t.Errorf("source balance = %d, want %d unchanged: the conflicting request executed",
					got, balanceAfterFirst)
			}
			assertCompletedTransferCount(t, e, 1)
			assertReconciled(t, e)
		})
	}
}

// The conflict must be detected from PostgreSQL too, not only from the Redis
// record — otherwise a flushed cache would let a key be reused for a different
// payment.
func TestSameKeyDifferentRequestIsRejectedAfterRedisIsFlushed(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 50_000)
	bob := newFundedAccount(t, e, USD, 0)

	k := key(t, "abc")
	first := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 1_000, Currency: USD,
	}
	if _, err := e.transfers.PostIdempotent(ctx, k, first); err != nil {
		t.Fatalf("first request: %v", err)
	}

	flushRedis(t)

	second := first
	second.AmountMinor = 2_000

	_, err := e.transfers.PostIdempotent(ctx, k, second)
	if !errors.Is(err, transfer.ErrIdempotencyConflict) {
		t.Fatalf("second request = %v, want ErrIdempotencyConflict from the stored fingerprint", err)
	}
	if got := countTransfersForKey(t, e, k); got != 1 {
		t.Errorf("transfer rows = %d, want 1", got)
	}
	assertReconciled(t, e)
}

// ---------------------------------------------------------------------------
// Different keys, same request
// ---------------------------------------------------------------------------

// Idempotency must not become content deduplication. Two distinct keys are two
// distinct logical payments, even for identical amounts.
func TestDifferentKeysSameRequestBothExecute(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 50_000)
	bob := newFundedAccount(t, e, USD, 0)

	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 100, Currency: USD,
	}

	first, err := e.transfers.PostIdempotent(ctx, key(t, "key-1"), cmd)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := e.transfers.PostIdempotent(ctx, key(t, "key-2"), cmd)
	if err != nil {
		t.Fatalf("second: %v", err)
	}

	if first.Transfer.ID == second.Transfer.ID {
		t.Error("two distinct keys resolved to the same transfer")
	}
	if second.Replayed {
		t.Error("the second request was treated as a replay")
	}
	if got, want := balanceOf(t, e, bob.ID), int64(200); got != want {
		t.Errorf("destination balance = %d, want %d: both payments should have executed", got, want)
	}
	assertCompletedTransferCount(t, e, 2)
	assertReconciled(t, e)
}

// ---------------------------------------------------------------------------
// Retry after completion, and after a lost response
// ---------------------------------------------------------------------------

// The plain replay: the caller got its answer and asks again.
func TestRetryAfterCompletionReturnsTheOriginalTransfer(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 50_000)
	bob := newFundedAccount(t, e, USD, 0)

	k := key(t, "pay_retry")
	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 1_500, Currency: USD,
	}

	original, err := e.transfers.PostIdempotent(ctx, k, cmd)
	if err != nil {
		t.Fatalf("original: %v", err)
	}
	balanceAfter := balanceOf(t, e, alice.ID)
	entriesAfter := countRows(t, e, "ledger_entries")

	for attempt := range 3 {
		replay, err := e.transfers.PostIdempotent(ctx, k, cmd)
		if err != nil {
			t.Fatalf("replay %d: %v", attempt, err)
		}
		if replay.Transfer.ID != original.Transfer.ID {
			t.Errorf("replay %d returned transfer %s, want %s",
				attempt, replay.Transfer.ID, original.Transfer.ID)
		}
		if !replay.Replayed {
			t.Errorf("replay %d was not marked as a replay", attempt)
		}
	}

	if got := balanceOf(t, e, alice.ID); got != balanceAfter {
		t.Errorf("source balance = %d, want %d unchanged by replays", got, balanceAfter)
	}
	if got := countRows(t, e, "ledger_entries"); got != entriesAfter {
		t.Errorf("ledger entries = %d, want %d unchanged by replays", got, entriesAfter)
	}
	if got := countTransfersForKey(t, e, k); got != 1 {
		t.Errorf("transfer rows = %d, want 1", got)
	}
	assertReconciled(t, e)
}

// The scenario idempotency exists for: the transfer committed but the caller
// never saw the response, so it retries. One logical payment, one posting.
func TestLostResponseRetryDoesNotDoublePost(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 50_000)
	bob := newFundedAccount(t, e, USD, 0)

	k := key(t, "pay_lost_response")
	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 3_000, Currency: USD,
	}

	// The server commits, and the response is dropped on the way back: the
	// caller keeps no record of it at all.
	if _, err := e.transfers.PostIdempotent(ctx, k, cmd); err != nil {
		t.Fatalf("first attempt: %v", err)
	}

	// The client, knowing nothing, retries.
	retry, err := e.transfers.PostIdempotent(ctx, k, cmd)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !retry.Replayed {
		t.Error("the retry created a new transfer instead of replaying the original")
	}

	if got, want := balanceOf(t, e, alice.ID), int64(47_000); got != want {
		t.Errorf("source balance = %d, want %d: the payment was applied twice", got, want)
	}
	if got := countTransfersForKey(t, e, k); got != 1 {
		t.Errorf("transfer rows = %d, want 1", got)
	}
	assertCompletedTransferCount(t, e, 1)
	assertReconciled(t, e)
}
