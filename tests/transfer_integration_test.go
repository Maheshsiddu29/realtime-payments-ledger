//go:build integration

package tests

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/money"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

func TestPostTransferSucceeds(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 100000) // $1,000.00
	bob := newFundedAccount(t, e, USD, 50000)    //   $500.00

	posted, err := e.transfers.Post(ctx, transfer.Command{
		SourceAccountID:      alice.ID,
		DestinationAccountID: bob.ID,
		AmountMinor:          25000, // $250.00
		Currency:             USD,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	if posted.Status != transfer.StatusCompleted {
		t.Errorf("Status = %q, want %q", posted.Status, transfer.StatusCompleted)
	}
	if posted.CompletedAt == nil {
		t.Error("CompletedAt is nil on a completed transfer")
	}
	if posted.AmountMinor != 25000 {
		t.Errorf("AmountMinor = %d, want 25000", posted.AmountMinor)
	}

	if got, want := balanceOf(t, e, alice.ID), int64(75000); got != want {
		t.Errorf("source balance = %d, want %d", got, want)
	}
	if got, want := balanceOf(t, e, bob.ID), int64(75000); got != want {
		t.Errorf("destination balance = %d, want %d", got, want)
	}
}

func TestGetTransferReturnsWhatWasPosted(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	alice := newFundedAccount(t, e, USD, 10000)
	bob := newFundedAccount(t, e, USD, 0)

	posted, err := e.transfers.Post(ctx, transfer.Command{
		SourceAccountID:      alice.ID,
		DestinationAccountID: bob.ID,
		AmountMinor:          4200,
		Currency:             USD,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	got, err := e.transfers.Get(ctx, posted.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ID != posted.ID || got.AmountMinor != 4200 || got.Status != transfer.StatusCompleted {
		t.Errorf("Get returned %+v, want the posted transfer %+v", got, posted)
	}
	if got.SourceAccountID != alice.ID || got.DestinationAccountID != bob.ID {
		t.Errorf("accounts = %s -> %s, want %s -> %s",
			got.SourceAccountID, got.DestinationAccountID, alice.ID, bob.ID)
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt is nil after a completed transfer round-trip")
	}
}

func TestGetTransferUnknownID(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	if _, err := e.transfers.Get(ctx, uuid.New()); !errors.Is(err, transfer.ErrNotFound) {
		t.Errorf("Get(unknown) = %v, want it to wrap transfer.ErrNotFound", err)
	}
}

// Every rejection path must leave the database exactly as it was: no transfer
// row, no ledger entries, no balance movement.
func TestPostTransferRejections(t *testing.T) {
	tests := []struct {
		name string
		// build returns the command to post, given two prepared accounts.
		build   func(e *env, src, dst uuid.UUID) transfer.Command
		wantErr error
	}{
		{
			name: "insufficient funds",
			build: func(_ *env, src, dst uuid.UUID) transfer.Command {
				return transfer.Command{SourceAccountID: src, DestinationAccountID: dst, AmountMinor: 100001, Currency: USD}
			},
			wantErr: transfer.ErrInsufficientFunds,
		},
		{
			name: "source account missing",
			build: func(_ *env, _, dst uuid.UUID) transfer.Command {
				return transfer.Command{SourceAccountID: uuid.New(), DestinationAccountID: dst, AmountMinor: 100, Currency: USD}
			},
			wantErr: transfer.ErrSourceAccountNotFound,
		},
		{
			name: "destination account missing",
			build: func(_ *env, src, _ uuid.UUID) transfer.Command {
				return transfer.Command{SourceAccountID: src, DestinationAccountID: uuid.New(), AmountMinor: 100, Currency: USD}
			},
			wantErr: transfer.ErrDestinationAccountNotFound,
		},
		{
			name: "source equals destination",
			build: func(_ *env, src, _ uuid.UUID) transfer.Command {
				return transfer.Command{SourceAccountID: src, DestinationAccountID: src, AmountMinor: 100, Currency: USD}
			},
			wantErr: transfer.ErrSameAccount,
		},
		{
			name: "transfer currency does not match the accounts",
			build: func(_ *env, src, dst uuid.UUID) transfer.Command {
				return transfer.Command{SourceAccountID: src, DestinationAccountID: dst, AmountMinor: 100, Currency: money.Currency("EUR")}
			},
			wantErr: transfer.ErrCurrencyMismatch,
		},
		{
			name: "zero amount",
			build: func(_ *env, src, dst uuid.UUID) transfer.Command {
				return transfer.Command{SourceAccountID: src, DestinationAccountID: dst, AmountMinor: 0, Currency: USD}
			},
			wantErr: transfer.ErrInvalidAmount,
		},
		{
			name: "negative amount",
			build: func(_ *env, src, dst uuid.UUID) transfer.Command {
				return transfer.Command{SourceAccountID: src, DestinationAccountID: dst, AmountMinor: -5000, Currency: USD}
			},
			wantErr: transfer.ErrInvalidAmount,
		},
		{
			name: "invalid currency code",
			build: func(_ *env, src, dst uuid.UUID) transfer.Command {
				return transfer.Command{SourceAccountID: src, DestinationAccountID: dst, AmountMinor: 100, Currency: money.Currency("usd")}
			},
			wantErr: money.ErrInvalidCurrency,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := testContext(t)

			alice := newFundedAccount(t, e, USD, 100000)
			bob := newFundedAccount(t, e, USD, 50000)

			_, err := e.transfers.Post(ctx, tt.build(e, alice.ID, bob.ID))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Post = %v, want it to wrap %v", err, tt.wantErr)
			}

			// Nothing may have been left behind.
			if got, want := balanceOf(t, e, alice.ID), int64(100000); got != want {
				t.Errorf("source balance = %d, want %d unchanged", got, want)
			}
			if got, want := balanceOf(t, e, bob.ID), int64(50000); got != want {
				t.Errorf("destination balance = %d, want %d unchanged", got, want)
			}
			if n := countRows(t, e, "transfers"); n != 0 {
				t.Errorf("transfers table has %d rows after a rejected transfer, want 0", n)
			}
			if n := countRows(t, e, "ledger_entries"); n != 0 {
				t.Errorf("ledger_entries table has %d rows after a rejected transfer, want 0", n)
			}
		})
	}
}

// Accounts in different currencies cannot transfer to each other: there is no
// implicit conversion, because a conversion needs a rate and two more entries.
func TestPostTransferAcrossCurrenciesIsRejected(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	usd := newFundedAccount(t, e, USD, 100000)
	eur := newFundedAccount(t, e, money.Currency("EUR"), 100000)

	_, err := e.transfers.Post(ctx, transfer.Command{
		SourceAccountID:      usd.ID,
		DestinationAccountID: eur.ID,
		AmountMinor:          1000,
		Currency:             USD,
	})
	if !errors.Is(err, transfer.ErrCurrencyMismatch) {
		t.Fatalf("Post = %v, want it to wrap transfer.ErrCurrencyMismatch", err)
	}

	if got := balanceOf(t, e, usd.ID); got != 100000 {
		t.Errorf("USD balance = %d, want 100000 unchanged", got)
	}
	if got := balanceOf(t, e, eur.ID); got != 100000 {
		t.Errorf("EUR balance = %d, want 100000 unchanged", got)
	}
}

// An account may be emptied exactly, but not overdrawn by one minor unit.
func TestPostTransferBoundaryAmounts(t *testing.T) {
	t.Run("exact balance succeeds", func(t *testing.T) {
		e := newEnv(t)
		ctx := testContext(t)

		alice := newFundedAccount(t, e, USD, 25000)
		bob := newFundedAccount(t, e, USD, 0)

		if _, err := e.transfers.Post(ctx, transfer.Command{
			SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
			AmountMinor: 25000, Currency: USD,
		}); err != nil {
			t.Fatalf("Post of the exact balance: %v", err)
		}

		if got := balanceOf(t, e, alice.ID); got != 0 {
			t.Errorf("source balance = %d, want 0", got)
		}
	})

	t.Run("one minor unit over balance fails", func(t *testing.T) {
		e := newEnv(t)
		ctx := testContext(t)

		alice := newFundedAccount(t, e, USD, 25000)
		bob := newFundedAccount(t, e, USD, 0)

		_, err := e.transfers.Post(ctx, transfer.Command{
			SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
			AmountMinor: 25001, Currency: USD,
		})
		if !errors.Is(err, transfer.ErrInsufficientFunds) {
			t.Fatalf("Post = %v, want it to wrap transfer.ErrInsufficientFunds", err)
		}
		if got := balanceOf(t, e, alice.ID); got != 25000 {
			t.Errorf("source balance = %d, want 25000 unchanged", got)
		}
	})

	t.Run("one minor unit transfers", func(t *testing.T) {
		e := newEnv(t)
		ctx := testContext(t)

		alice := newFundedAccount(t, e, USD, 1)
		bob := newFundedAccount(t, e, USD, 0)

		if _, err := e.transfers.Post(ctx, transfer.Command{
			SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
			AmountMinor: 1, Currency: USD,
		}); err != nil {
			t.Fatalf("Post of one minor unit: %v", err)
		}
		if got := balanceOf(t, e, bob.ID); got != 1 {
			t.Errorf("destination balance = %d, want 1", got)
		}
	})
}

// A completed transfer is a financial record. The database refuses to let it
// be edited or deleted afterwards.
func TestCompletedTransferIsImmutable(t *testing.T) {
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
		`UPDATE transfers SET amount_minor = 1 WHERE id = $1`, posted.ID); err == nil {
		t.Error("updating a completed transfer succeeded, want the immutability trigger to reject it")
	}
	if _, err := e.pool.Exec(ctx,
		`DELETE FROM transfers WHERE id = $1`, posted.ID); err == nil {
		t.Error("deleting a completed transfer succeeded, want the immutability trigger to reject it")
	}

	got, err := e.transfers.Get(ctx, posted.ID)
	if err != nil {
		t.Fatalf("Get after rejected mutations: %v", err)
	}
	if got.AmountMinor != 1000 {
		t.Errorf("AmountMinor = %d, want 1000 unchanged", got.AmountMinor)
	}
}
