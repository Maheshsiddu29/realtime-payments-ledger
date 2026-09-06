//go:build integration

package tests

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/account"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/money"
)

// A new account starts at zero. This is the central rule of account creation:
// an account that came into existence already holding money would be money
// with no source, and the books would not balance from the first row.
func TestCreateAccountStartsAtZeroBalance(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	acct, err := e.accounts.Create(ctx, USD)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if acct.BalanceMinor != 0 {
		t.Errorf("BalanceMinor = %d, want 0: a new account must not hold unexplained money", acct.BalanceMinor)
	}
	if acct.Currency != USD {
		t.Errorf("Currency = %q, want %q", acct.Currency, USD)
	}
	if acct.ID == uuid.Nil {
		t.Error("ID is the zero UUID, want a generated identifier")
	}
	if acct.CreatedAt.IsZero() || acct.UpdatedAt.IsZero() {
		t.Errorf("timestamps not populated: created_at=%v updated_at=%v", acct.CreatedAt, acct.UpdatedAt)
	}
	if since := time.Since(acct.CreatedAt).Abs(); since > time.Minute {
		t.Errorf("created_at is %s away from now; the database clock is not being used", since)
	}
}

func TestGetAccountReturnsWhatWasCreated(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	created, err := e.accounts.Create(ctx, money.Currency("EUR"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := e.accounts.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ID != created.ID {
		t.Errorf("ID = %s, want %s", got.ID, created.ID)
	}
	if got.Currency != money.Currency("EUR") {
		t.Errorf("Currency = %q, want EUR", got.Currency)
	}
	if got.BalanceMinor != 0 {
		t.Errorf("BalanceMinor = %d, want 0", got.BalanceMinor)
	}
}

func TestGetAccountUnknownID(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	_, err := e.accounts.Get(ctx, uuid.New())
	if !errors.Is(err, account.ErrNotFound) {
		t.Errorf("Get(unknown) = %v, want it to wrap account.ErrNotFound", err)
	}
}

// Invalid currencies are rejected before they reach the database, and would be
// rejected by the accounts_currency_format CHECK constraint even if they did.
func TestCreateAccountRejectsInvalidCurrency(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	for _, currency := range []money.Currency{"", "US", "USDD", "usd", "U5D"} {
		t.Run(string(currency), func(t *testing.T) {
			_, err := e.accounts.Create(ctx, currency)
			if !errors.Is(err, money.ErrInvalidCurrency) {
				t.Errorf("Create(%q) = %v, want it to wrap money.ErrInvalidCurrency", currency, err)
			}
		})
	}

	if n := countRows(t, e, "accounts"); n != 0 {
		t.Errorf("accounts table has %d rows after only invalid creates, want 0", n)
	}
}

// The database refuses a negative balance regardless of what the application
// does. This is the constraint that makes overdraft structurally impossible.
func TestDatabaseRejectsNegativeBalance(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	acct := newFundedAccount(t, e, USD, 500)

	_, err := e.pool.Exec(ctx,
		`UPDATE accounts SET balance_minor = -1 WHERE id = $1`, acct.ID)
	if err == nil {
		t.Fatal("setting a negative balance succeeded, want a check constraint violation")
	}

	if got := balanceOf(t, e, acct.ID); got != 500 {
		t.Errorf("balance = %d after the rejected update, want 500", got)
	}
}

// updated_at is maintained by a database trigger so that every writer produces
// a consistent value, whatever code path it took.
func TestAccountUpdatedAtIsMaintainedByTheDatabase(t *testing.T) {
	e := newEnv(t)
	ctx := testContext(t)

	acct := newAccount(t, e, USD)
	before, err := e.accounts.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	fund(t, e, acct.ID, 100)

	after, err := e.accounts.Get(ctx, acct.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at did not advance on update: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("created_at changed on update: before=%v after=%v", before.CreatedAt, after.CreatedAt)
	}
}
