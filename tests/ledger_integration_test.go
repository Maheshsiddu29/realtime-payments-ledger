//go:build integration

package tests

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/ledger"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

// A posted transfer produces exactly one debit and one credit, and they cancel
// out. This is the double-entry invariant in its most direct form.
func TestPostedTransferWritesBalancedLedgerEntries(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 100000)
	bob := newFundedAccount(t, e, USD, 50000)

	posted, err := e.transfers.Post(ctx, transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 25000, Currency: USD,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	entries, err := e.entries.EntriesForTransfer(ctx, posted.ID)
	if err != nil {
		t.Fatalf("EntriesForTransfer: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("got %d ledger entries, want exactly 2", len(entries))
	}

	// EntriesForTransfer orders by amount ascending, so the debit comes first.
	debit, credit := entries[0], entries[1]

	if !debit.IsDebit() {
		t.Errorf("first entry amount = %d, want a negative debit", debit.AmountMinor)
	}
	if !credit.IsCredit() {
		t.Errorf("second entry amount = %d, want a positive credit", credit.AmountMinor)
	}
	if debit.AmountMinor != -25000 {
		t.Errorf("debit = %d, want -25000", debit.AmountMinor)
	}
	if credit.AmountMinor != 25000 {
		t.Errorf("credit = %d, want +25000", credit.AmountMinor)
	}
	if debit.AccountID != alice.ID {
		t.Errorf("debit is against account %s, want the source %s", debit.AccountID, alice.ID)
	}
	if credit.AccountID != bob.ID {
		t.Errorf("credit is against account %s, want the destination %s", credit.AccountID, bob.ID)
	}

	if sum := ledger.SumMinor(entries); sum != 0 {
		t.Errorf("ledger entries sum to %d, want 0", sum)
	}

	for _, entry := range entries {
		if entry.TransferID != posted.ID {
			t.Errorf("entry %s belongs to transfer %s, want %s", entry.ID, entry.TransferID, posted.ID)
		}
		if entry.Currency != USD {
			t.Errorf("entry currency = %q, want %q", entry.Currency, USD)
		}
		if entry.CreatedAt.IsZero() {
			t.Errorf("entry %s has no created_at", entry.ID)
		}
	}

	// And the balances agree with the entries.
	if got, want := balanceOf(t, e, alice.ID), int64(75000); got != want {
		t.Errorf("source balance = %d, want %d", got, want)
	}
	if got, want := balanceOf(t, e, bob.ID), int64(75000); got != want {
		t.Errorf("destination balance = %d, want %d", got, want)
	}
}

func TestEntriesForTransferUnknownID(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	entries, err := e.entries.EntriesForTransfer(ctx, uuid.New())
	if err != nil {
		t.Fatalf("EntriesForTransfer(unknown) = %v, want no error", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries for an unknown transfer, want 0", len(entries))
	}
}

// The ledger is the audit trail: history is never rewritten. A correction is a
// new, reversing entry, so UPDATE and DELETE are refused by the database.
func TestLedgerEntriesAreAppendOnly(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 10000)
	bob := newFundedAccount(t, e, USD, 0)

	posted, err := e.transfers.Post(ctx, transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 1000, Currency: USD,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	if _, err := e.pool.Exec(ctx,
		`UPDATE ledger_entries SET amount_minor = 1 WHERE transfer_id = $1`, posted.ID); err == nil {
		t.Error("updating a ledger entry succeeded, want the append-only trigger to reject it")
	}
	if _, err := e.pool.Exec(ctx,
		`DELETE FROM ledger_entries WHERE transfer_id = $1`, posted.ID); err == nil {
		t.Error("deleting a ledger entry succeeded, want the append-only trigger to reject it")
	}

	entries, err := e.entries.EntriesForTransfer(ctx, posted.ID)
	if err != nil {
		t.Fatalf("EntriesForTransfer: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries after rejected mutations, want 2", len(entries))
	}
	if sum := ledger.SumMinor(entries); sum != 0 {
		t.Errorf("entries sum to %d after rejected mutations, want 0", sum)
	}
}

// Several transfers between the same accounts must each keep their own books.
func TestMultipleTransfersEachBalance(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 100000)
	bob := newFundedAccount(t, e, USD, 0)

	amounts := []int64{1, 999, 25000, 74000}
	var moved int64

	for _, amount := range amounts {
		posted, err := e.transfers.Post(ctx, transfer.Command{
			SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
			AmountMinor: amount, Currency: USD,
		})
		if err != nil {
			t.Fatalf("Post(%d): %v", amount, err)
		}
		moved += amount

		entries, err := e.entries.EntriesForTransfer(ctx, posted.ID)
		if err != nil {
			t.Fatalf("EntriesForTransfer: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("transfer of %d produced %d entries, want 2", amount, len(entries))
		}
		if sum := ledger.SumMinor(entries); sum != 0 {
			t.Errorf("transfer of %d has entries summing to %d, want 0", amount, sum)
		}
	}

	if got, want := balanceOf(t, e, alice.ID), 100000-moved; got != want {
		t.Errorf("source balance = %d, want %d", got, want)
	}
	if got, want := balanceOf(t, e, bob.ID), moved; got != want {
		t.Errorf("destination balance = %d, want %d", got, want)
	}
	if n := countRows(t, e, "ledger_entries"); n != 2*len(amounts) {
		t.Errorf("ledger has %d entries, want %d", n, 2*len(amounts))
	}
}
