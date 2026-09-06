// Package ledger provides read access to the ledger entries that record every
// movement of money.
//
// This package deliberately exposes no way to create, modify or delete an
// entry. Entries are written only by the transfer posting operation, inside
// the same database transaction that moves the balances, so that a movement
// and its record can never come apart. The ledger_entries table is
// additionally append-only at the database level: a trigger rejects UPDATE and
// DELETE outright, so correcting a mistake means posting a reversing entry
// rather than editing history.
package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/money"
)

// Entry is one signed movement of money against one account.
//
// The sign carries the direction: a negative amount is a debit (money leaving
// the account), a positive amount is a credit (money arriving). For any one
// transfer the entries sum to zero.
type Entry struct {
	ID          uuid.UUID
	TransferID  uuid.UUID
	AccountID   uuid.UUID
	AmountMinor int64
	Currency    money.Currency
	CreatedAt   time.Time
}

// IsDebit reports whether the entry takes money out of the account.
func (e Entry) IsDebit() bool { return e.AmountMinor < 0 }

// IsCredit reports whether the entry puts money into the account.
func (e Entry) IsCredit() bool { return e.AmountMinor > 0 }

// Repository reads ledger entries. It has no write methods by design.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository returns a repository backed by the given pool.
func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// EntriesForTransfer returns every entry posted for a transfer, debits first,
// so that a caller reading the slice sees the movement in the order an
// accountant would write it.
//
// A transfer that has not posted returns an empty slice and no error; the
// caller decides whether that is unexpected.
func (r *Repository) EntriesForTransfer(ctx context.Context, transferID uuid.UUID) ([]Entry, error) {
	const query = `
		SELECT id, transfer_id, account_id, amount_minor, currency, created_at
		FROM ledger_entries
		WHERE transfer_id = $1
		ORDER BY amount_minor ASC`

	rows, err := r.pool.Query(ctx, query, transferID)
	if err != nil {
		return nil, fmt.Errorf("ledger: entries for transfer %s: %w", transferID, err)
	}
	defer rows.Close()

	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Entry, error) {
		var e Entry
		err := row.Scan(&e.ID, &e.TransferID, &e.AccountID, &e.AmountMinor, &e.Currency, &e.CreatedAt)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: scan entries for transfer %s: %w", transferID, err)
	}
	return entries, nil
}

// SumMinor returns the signed sum of the entries. The double-entry invariant
// is that this is zero for every posted transfer.
func SumMinor(entries []Entry) int64 {
	var total int64
	for _, e := range entries {
		total += e.AmountMinor
	}
	return total
}
