//go:build integration

package tests

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/ledger"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

// TestMoneyIsConservedAcrossATransfer is the headline property of a ledger:
// a transfer moves money, it never creates or destroys it. The total held by
// all accounts before the transfer must equal the total after.
//
//	before:  Alice 100000 + Bob 50000 = 150000
//	transfer: Alice -> Bob, 25000
//	after:   Alice  75000 + Bob 75000 = 150000
//	ledger:  -25000 + 25000 = 0
func TestMoneyIsConservedAcrossATransfer(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 100000)
	bob := newFundedAccount(t, e, USD, 50000)

	totalBefore := totalBalances(t, e)
	if totalBefore != 150000 {
		t.Fatalf("total before = %d, want 150000", totalBefore)
	}

	posted, err := e.transfers.Post(ctx, transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 25000, Currency: USD,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	totalAfter := totalBalances(t, e)
	if totalAfter != totalBefore {
		t.Errorf("money was created or destroyed: total before = %d, after = %d",
			totalBefore, totalAfter)
	}

	if got := balanceOf(t, e, alice.ID); got != 75000 {
		t.Errorf("Alice = %d, want 75000", got)
	}
	if got := balanceOf(t, e, bob.ID); got != 75000 {
		t.Errorf("Bob = %d, want 75000", got)
	}

	entries, err := e.entries.EntriesForTransfer(ctx, posted.ID)
	if err != nil {
		t.Fatalf("EntriesForTransfer: %v", err)
	}
	if sum := ledger.SumMinor(entries); sum != 0 {
		t.Errorf("ledger entries sum to %d, want 0", sum)
	}
}

// Conservation must survive a whole sequence of transfers in both directions,
// not just one.
func TestMoneyIsConservedAcrossManyTransfers(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 100000)
	bob := newFundedAccount(t, e, USD, 50000)
	carol := newFundedAccount(t, e, USD, 25000)

	totalBefore := totalBalances(t, e)

	moves := []struct {
		from, to uuid.UUID
		amount   int64
	}{
		{alice.ID, bob.ID, 25000},
		{bob.ID, carol.ID, 30000},
		{carol.ID, alice.ID, 55000},
		{alice.ID, carol.ID, 1},
		{bob.ID, alice.ID, 12345},
	}

	for i, mv := range moves {
		if _, err := e.transfers.Post(ctx, transfer.Command{
			SourceAccountID: mv.from, DestinationAccountID: mv.to,
			AmountMinor: mv.amount, Currency: USD,
		}); err != nil {
			t.Fatalf("Post move %d (%d minor units): %v", i, mv.amount, err)
		}

		if got := totalBalances(t, e); got != totalBefore {
			t.Fatalf("after move %d the total is %d, want %d", i, got, totalBefore)
		}
	}

	// The whole ledger, across every transfer, must also sum to zero.
	var ledgerSum int64
	if err := e.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries`).Scan(&ledgerSum); err != nil {
		t.Fatalf("summing the ledger: %v", err)
	}
	if ledgerSum != 0 {
		t.Errorf("the whole ledger sums to %d, want 0", ledgerSum)
	}
}

// The invariant is enforced by PostgreSQL, not only by the Go code. This test
// bypasses the application entirely and writes a one-sided entry with raw SQL:
// the deferred constraint trigger must reject it at COMMIT, and nothing may
// survive.
func TestDatabaseRejectsUnbalancedLedgerEntriesAtCommit(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 100000)
	bob := newFundedAccount(t, e, USD, 50000)
	totalBefore := totalBalances(t, e)

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	transferID := uuid.New()

	if _, err := tx.Exec(ctx, `
		INSERT INTO transfers (id, source_account_id, destination_account_id, amount_minor, currency)
		VALUES ($1, $2, $3, $4, 'USD')`,
		transferID, alice.ID, bob.ID, int64(5000)); err != nil {
		t.Fatalf("inserting the transfer: %v", err)
	}

	// Only the debit. The matching credit is deliberately missing.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (id, transfer_id, account_id, amount_minor, currency)
		VALUES ($1, $2, $3, $4, 'USD')`,
		uuid.New(), transferID, alice.ID, int64(-5000)); err != nil {
		t.Fatalf("inserting the one-sided entry: %v", err)
	}

	// Statement-level checks pass; the imbalance is only detected at COMMIT,
	// which is exactly what DEFERRABLE INITIALLY DEFERRED buys us.
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("COMMIT of a one-sided ledger posting succeeded, want it rejected")
	} else {
		t.Logf("COMMIT correctly rejected: %v", err)
	}

	if n := countRows(t, e, "transfers"); n != 0 {
		t.Errorf("transfers table has %d rows after the rejected commit, want 0", n)
	}
	if n := countRows(t, e, "ledger_entries"); n != 0 {
		t.Errorf("ledger_entries table has %d rows after the rejected commit, want 0", n)
	}
	if got := totalBalances(t, e); got != totalBefore {
		t.Errorf("total balances = %d after the rejected commit, want %d", got, totalBefore)
	}
}

// The complementary hole: a transfer marked completed with no entries at all.
// The trigger on ledger_entries cannot catch this, because nothing was ever
// inserted there, so a second trigger on transfers covers it.
func TestDatabaseRejectsCompletedTransferWithNoEntries(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 100000)
	bob := newFundedAccount(t, e, USD, 50000)

	_, err := e.pool.Exec(ctx, `
		INSERT INTO transfers (id, source_account_id, destination_account_id,
		                       amount_minor, currency, status, completed_at)
		VALUES ($1, $2, $3, $4, 'USD', 'completed', now())`,
		uuid.New(), alice.ID, bob.ID, int64(5000))
	if err == nil {
		t.Fatal("a completed transfer with no ledger entries was accepted, want it rejected")
	}
	t.Logf("correctly rejected: %v", err)

	if n := countRows(t, e, "transfers"); n != 0 {
		t.Errorf("transfers table has %d rows, want 0", n)
	}
}

// Entries whose currency differs from the transfer's are rejected, so a
// balanced-looking pair cannot mix denominations.
func TestDatabaseRejectsMixedCurrencyEntries(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 100000)
	bob := newFundedAccount(t, e, USD, 50000)

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	transferID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO transfers (id, source_account_id, destination_account_id, amount_minor, currency)
		VALUES ($1, $2, $3, $4, 'USD')`,
		transferID, alice.ID, bob.ID, int64(5000)); err != nil {
		t.Fatalf("inserting the transfer: %v", err)
	}

	// Sums to zero, but one leg is denominated in EUR.
	if _, err := tx.Exec(ctx, `
		INSERT INTO ledger_entries (id, transfer_id, account_id, amount_minor, currency)
		VALUES ($1, $2, $3, -5000, 'USD'), ($4, $2, $5, 5000, 'EUR')`,
		uuid.New(), transferID, alice.ID, uuid.New(), bob.ID); err != nil {
		t.Fatalf("inserting the entries: %v", err)
	}

	if err := tx.Commit(ctx); err == nil {
		t.Fatal("COMMIT of a mixed-currency posting succeeded, want it rejected")
	} else {
		t.Logf("COMMIT correctly rejected: %v", err)
	}

	if n := countRows(t, e, "ledger_entries"); n != 0 {
		t.Errorf("ledger_entries table has %d rows, want 0", n)
	}
}

// A transfer may not post two entries against the same account, which is what
// the unique constraint on (transfer_id, account_id) encodes.
func TestDatabaseRejectsDuplicateLegForOneAccount(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 100000)
	bob := newFundedAccount(t, e, USD, 50000)

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	transferID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO transfers (id, source_account_id, destination_account_id, amount_minor, currency)
		VALUES ($1, $2, $3, $4, 'USD')`,
		transferID, alice.ID, bob.ID, int64(5000)); err != nil {
		t.Fatalf("inserting the transfer: %v", err)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO ledger_entries (id, transfer_id, account_id, amount_minor, currency)
		VALUES ($1, $2, $3, -5000, 'USD'), ($4, $2, $3, 5000, 'USD')`,
		uuid.New(), transferID, alice.ID, uuid.New())
	if err == nil {
		t.Fatal("two entries against the same account were accepted, want the unique constraint to reject them")
	}
	t.Logf("correctly rejected: %v", err)
}

// Atomicity, forced from inside the posting transaction: if any statement
// after the debit fails, the debit must not survive.
func TestFailureAfterDebitRollsBackEverything(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 100000)
	bob := newFundedAccount(t, e, USD, 50000)
	totalBefore := totalBalances(t, e)

	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	transferID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO transfers (id, source_account_id, destination_account_id, amount_minor, currency)
		VALUES ($1, $2, $3, $4, 'USD')`,
		transferID, alice.ID, bob.ID, int64(25000)); err != nil {
		t.Fatalf("inserting the transfer: %v", err)
	}

	// Debit the source, exactly as the real posting does.
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET balance_minor = balance_minor - 25000 WHERE id = $1`, alice.ID); err != nil {
		t.Fatalf("debiting the source: %v", err)
	}

	// Now force a failure before the credit: a violation of the non-negative
	// balance constraint, which aborts the transaction.
	if _, err := tx.Exec(ctx,
		`UPDATE accounts SET balance_minor = -1 WHERE id = $1`, bob.ID); err == nil {
		t.Fatal("driving the balance negative succeeded, want a constraint violation")
	}

	if err := tx.Commit(ctx); err == nil {
		t.Fatal("COMMIT after an aborted statement succeeded, want it rejected")
	}

	// Nothing at all may have survived.
	if got := balanceOf(t, e, alice.ID); got != 100000 {
		t.Errorf("source balance = %d, want 100000: the debit survived a rolled-back transaction", got)
	}
	if got := balanceOf(t, e, bob.ID); got != 50000 {
		t.Errorf("destination balance = %d, want 50000", got)
	}
	if got := totalBalances(t, e); got != totalBefore {
		t.Errorf("total = %d, want %d", got, totalBefore)
	}
	if n := countRows(t, e, "transfers"); n != 0 {
		t.Errorf("transfers table has %d rows, want 0", n)
	}
	if n := countRows(t, e, "ledger_entries"); n != 0 {
		t.Errorf("ledger_entries table has %d rows, want 0", n)
	}
}
