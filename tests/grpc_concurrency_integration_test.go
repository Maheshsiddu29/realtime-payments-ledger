//go:build integration

package tests

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	paymentsv1 "github.com/Maheshsiddu29/realtime-payments-ledger/internal/gen/payments/v1"
)

// The Phase 3 headline scenario, repeated through the actual public API.
//
// Twelve authenticated CreateTransfer RPCs, released together, sharing one
// idempotency key. This is the end-to-end proof: real gRPC transport, real
// interceptors, real Redis coordination, real PostgreSQL barrier. Exactly one
// payment may result.
func TestGRPCTwelveConcurrentDuplicateRequestsCreateOneTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext(t), 60*time.Second)
	defer cancel()

	g := newGRPCEnv(t)
	authed := g.full(t, ctx)

	const (
		callers = 12
		amount  = 2_500
		funded  = 100_000
	)

	source := g.fundedAccountRPC(t, ctx, "USD", funded)
	destination := g.newAccountRPC(t, ctx, "USD")

	req := &paymentsv1.CreateTransferRequest{
		SourceAccountId: source, DestinationAccountId: destination,
		AmountMinor: amount, Currency: "USD",
		IdempotencyKey: "grpc_concurrent_01HQ8XZ4K9YN3TQF7WV2MC6RJD",
	}

	type outcome struct {
		resp *paymentsv1.CreateTransferResponse
		err  error
	}
	outcomes := make([]outcome, callers)

	// A closed-channel barrier releases every caller at the same instant, so
	// the contention is real rather than an artefact of staggered starts.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := g.client.CreateTransfer(authed, req)
			outcomes[i] = outcome{resp: resp, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var (
		succeeded  int
		replayed   int
		created    int
		inProgress int
		unexpected []error
		ids        = map[string]struct{}{}
	)
	for _, o := range outcomes {
		switch {
		case o.err == nil:
			succeeded++
			ids[o.resp.GetTransfer().GetId()] = struct{}{}
			if o.resp.GetIdempotentReplay() {
				replayed++
			} else {
				created++
			}
		case status.Code(o.err) == codes.Aborted:
			// The documented answer for "another request holds this key":
			// ABORTED tells the client to retry the whole operation.
			inProgress++
		default:
			unexpected = append(unexpected, o.err)
		}
	}

	t.Logf("callers=%d succeeded=%d created=%d replayed=%d aborted_in_progress=%d unexpected=%d distinct_transfer_ids=%d",
		callers, succeeded, created, replayed, inProgress, len(unexpected), len(ids))

	for _, err := range unexpected {
		t.Errorf("unexpected failure: %v (code %s)", err, status.Code(err))
	}

	// --- the assertions that matter ---------------------------------------

	if n := countTransfersForKey(t, g.env, key(t, req.IdempotencyKey)); n != 1 {
		t.Errorf("transfer rows created = %d, want exactly 1", n)
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

	// Exactly one payment's worth of money moved: no double debit.
	if got, want := balanceOf(t, g.env, uuidMust(t, source)), int64(funded-amount); got != want {
		t.Errorf("source balance = %d, want %d: more than one debit was applied", got, want)
	}
	if got, want := balanceOf(t, g.env, uuidMust(t, destination)), int64(amount); got != want {
		t.Errorf("destination balance = %d, want %d", got, want)
	}
	if got, want := totalBalances(t, g.env), int64(funded); got != want {
		t.Errorf("total balances = %d, want %d: money was created or destroyed", got, want)
	}

	// Exactly one debit and one credit, read back through the API.
	for id := range ids {
		entries, err := g.client.ListLedgerEntriesForTransfer(authed,
			&paymentsv1.ListLedgerEntriesForTransferRequest{TransferId: id})
		if err != nil {
			t.Fatalf("ListLedgerEntriesForTransfer: %v", err)
		}
		got := entries.GetEntries()
		if len(got) != 2 {
			t.Fatalf("transfer %s has %d ledger entries, want 2", id, len(got))
		}
		if got[0].GetAmountMinor() != -amount || got[1].GetAmountMinor() != amount {
			t.Errorf("entries = %d, %d; want one debit of %d and one credit of %d",
				got[0].GetAmountMinor(), got[1].GetAmountMinor(), -amount, amount)
		}
	}
	if n := countRows(t, g.env, "ledger_entries"); n != 2 {
		t.Errorf("ledger_entries has %d rows, want 2 for a single transfer", n)
	}

	assertCompletedTransferCount(t, g.env, 1)
	assertNoNegativeBalances(t, g.env)
	assertReconciled(t, g.env)

	// A caller told ABORTED retries, and must then get the same transfer.
	if inProgress > 0 {
		t.Run("callers told to retry resolve to the same transfer", func(t *testing.T) {
			for i := range inProgress {
				resp, err := g.client.CreateTransfer(authed, req)
				if err != nil {
					t.Fatalf("retry %d: %v", i, err)
				}
				ids[resp.GetTransfer().GetId()] = struct{}{}
			}
			if len(ids) != 1 {
				t.Errorf("distinct transfer IDs across all callers = %d, want 1", len(ids))
			}
			if n := countTransfersForKey(t, g.env, key(t, req.IdempotencyKey)); n != 1 {
				t.Errorf("transfer rows = %d, want 1 after every caller retried", n)
			}
			assertReconciled(t, g.env)
		})
	}
}

// Phase 2's protections must survive being driven through the transport:
// distinct keys are distinct payments, and they still cannot overspend.
func TestGRPCConcurrentDistinctTransfersCannotOverspend(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext(t), 60*time.Second)
	defer cancel()

	g := newGRPCEnv(t)
	authed := g.full(t, ctx)

	const (
		callers = 20
		amount  = 100
		funded  = 1_000 // exactly ten transfers' worth
	)

	source := g.fundedAccountRPC(t, ctx, "USD", funded)
	destination := g.newAccountRPC(t, ctx, "USD")

	start := make(chan struct{})
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
		refused   int
		other     []error
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			_, err := g.client.CreateTransfer(authed, &paymentsv1.CreateTransferRequest{
				SourceAccountId: source, DestinationAccountId: destination,
				AmountMinor: amount, Currency: "USD",
				IdempotencyKey: "overspend-" + newUUIDString(),
			})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case status.Code(err) == codes.FailedPrecondition: // insufficient funds
				refused++
			case status.Code(err) == codes.Unavailable: // retry budget exhausted
				refused++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	t.Logf("callers=%d succeeded=%d refused=%d unexpected=%d", callers, succeeded, refused, len(other))

	for _, err := range other {
		t.Errorf("unexpected failure: %v (code %s)", err, status.Code(err))
	}
	if succeeded != 10 {
		t.Errorf("successful transfers = %d, want exactly 10 (only ten are affordable)", succeeded)
	}
	if got := balanceOf(t, g.env, uuidMust(t, source)); got != 0 {
		t.Errorf("source balance = %d, want 0", got)
	}
	if got, want := totalBalances(t, g.env), int64(funded); got != want {
		t.Errorf("total balances = %d, want %d", got, want)
	}
	assertNoNegativeBalances(t, g.env)
	assertReconciled(t, g.env)
}

// An abandoned client must not leave work running. A cancelled request has to
// stop the retry loop and the Redis and database work behind it, not continue
// on a context nobody is waiting for.
func TestGRPCCancelledRequestStopsWork(t *testing.T) {
	ctx := testContext(t)
	g := newGRPCEnv(t)

	source := g.fundedAccountRPC(t, ctx, "USD", 10_000)
	destination := g.newAccountRPC(t, ctx, "USD")

	t.Run("cancelled before the call", func(t *testing.T) {
		callCtx, cancel := context.WithCancel(g.full(t, ctx))
		cancel()

		_, err := g.client.CreateTransfer(callCtx, &paymentsv1.CreateTransferRequest{
			SourceAccountId: source, DestinationAccountId: destination,
			AmountMinor: 100, Currency: "USD", IdempotencyKey: "cancelled-" + newUUIDString(),
		})
		if err == nil {
			t.Fatal("a cancelled request succeeded")
		}
		if code := status.Code(err); code != codes.Canceled {
			t.Errorf("code = %s, want %s", code, codes.Canceled)
		}
	})

	t.Run("deadline already expired", func(t *testing.T) {
		callCtx, cancel := context.WithDeadline(g.full(t, ctx), time.Now().Add(-time.Second))
		defer cancel()

		_, err := g.client.CreateTransfer(callCtx, &paymentsv1.CreateTransferRequest{
			SourceAccountId: source, DestinationAccountId: destination,
			AmountMinor: 100, Currency: "USD", IdempotencyKey: "expired-" + newUUIDString(),
		})
		if err == nil {
			t.Fatal("a request with an expired deadline succeeded")
		}
		if code := status.Code(err); code != codes.DeadlineExceeded {
			t.Errorf("code = %s, want %s", code, codes.DeadlineExceeded)
		}
	})

	// Neither abandoned request may have moved money.
	if got := balanceOf(t, g.env, uuidMust(t, source)); got != 10_000 {
		t.Errorf("source balance = %d, want 10000 unchanged", got)
	}
	assertCompletedTransferCount(t, g.env, 0)
	assertReconciled(t, g.env)
}
