// Package account holds account records and their persistence.
//
// An account is a balance in exactly one currency. Balances are stored as
// BIGINT minor units and may never go negative: these are ordinary debit
// accounts, not credit lines.
package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/money"
)

// ErrNotFound is returned when no account exists with the requested id.
var ErrNotFound = errors.New("account: not found")

// Account is a balance held in a single currency.
type Account struct {
	ID           uuid.UUID
	Currency     money.Currency
	BalanceMinor int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Repository reads and writes account rows.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository returns a repository backed by the given pool.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Create opens a new account with a zero balance.
//
// The zero balance is deliberate and not a placeholder. In double-entry
// accounting every credit needs a matching debit, so an account that came into
// existence already holding money would be money with no source — the books
// would not balance from the very first row. Funding an account is a transfer
// from somewhere else, which later phases will model explicitly as a deposit
// against a funding account. Until then there is no production code path that
// puts money into the system.
func (r *Repository) Create(ctx context.Context, currency money.Currency) (Account, error) {
	if err := currency.Validate(); err != nil {
		return Account{}, err
	}

	const query = `
		INSERT INTO accounts (id, currency, balance_minor)
		VALUES ($1, $2, 0)
		RETURNING id, currency, balance_minor, created_at, updated_at`

	var a Account
	err := r.pool.QueryRow(ctx, query, uuid.New(), currency).
		Scan(&a.ID, &a.Currency, &a.BalanceMinor, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		return Account{}, fmt.Errorf("account: create: %w", err)
	}
	return a, nil
}

// Get returns the account with the given id, or ErrNotFound.
func (r *Repository) Get(ctx context.Context, id uuid.UUID) (Account, error) {
	const query = `
		SELECT id, currency, balance_minor, created_at, updated_at
		FROM accounts
		WHERE id = $1`

	var a Account
	err := r.pool.QueryRow(ctx, query, id).
		Scan(&a.ID, &a.Currency, &a.BalanceMinor, &a.CreatedAt, &a.UpdatedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Account{}, fmt.Errorf("account %s: %w", id, ErrNotFound)
	case err != nil:
		return Account{}, fmt.Errorf("account: get %s: %w", id, err)
	}
	return a, nil
}
