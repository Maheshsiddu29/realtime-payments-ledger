// Package reconcile verifies that the stored account balances and the ledger
// agree with each other.
//
// The ledger is the record of what happened; the balances are a cached
// summary of it (see docs/LEDGER_DESIGN.md). Nothing in PostgreSQL ties the
// two together — the balance columns and the entry rows are updated in the
// same transaction, but no constraint says they must correspond. This package
// is the check that they do.
//
// It reads only committed state through ordinary queries, so it can be run
// after a test, after a stress run, or eventually as a scheduled job.
package reconcile

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Baseline records money that exists in an account without ledger entries
// explaining it.
//
// This exists solely because of the test-only funding fixture. Phase 1
// deliberately has no production code path that puts money into the system:
// accounts are created at zero, and the only way to reach a funded state is a
// direct balance write from test or development tooling, which creates money
// from nothing and writes no entries.
//
// Reconciliation must therefore be told about that money, or every funded
// account would look like a discrepancy. This is an honest exception, not a
// property of the ledger: for a baseline account the check proves only that
// *movements since funding* are fully explained by the ledger, not that the
// whole balance is. An account absent from the baseline is reconciled
// strictly, with an expected contribution of zero.
//
// When a later phase models funding as a real deposit with its own ledger
// entries, this type and the exception it represents can be deleted.
type Baseline map[uuid.UUID]int64

// NegativeBalance is an account that holds less than nothing.
type NegativeBalance struct {
	AccountID    uuid.UUID
	BalanceMinor int64
}

// BalanceMismatch is an account whose stored balance is not explained by its
// baseline plus its ledger entries.
type BalanceMismatch struct {
	AccountID     uuid.UUID
	StoredMinor   int64
	BaselineMinor int64
	LedgerMinor   int64
	ExpectedMinor int64
}

// Difference is how far the stored balance is from the expected one.
func (m BalanceMismatch) Difference() int64 { return m.StoredMinor - m.ExpectedMinor }

// TransferProblem is a completed transfer whose ledger entries do not describe
// it correctly.
type TransferProblem struct {
	TransferID         uuid.UUID
	EntryCount         int
	EntrySumMinor      int64
	DebitCount         int
	CreditCount        int
	CurrencyMismatches int
	ForeignAccounts    int
	WrongDebitAmount   int
	WrongCreditAmount  int
}

func (p TransferProblem) String() string {
	return fmt.Sprintf(
		"transfer %s: entries=%d sum=%d debits=%d credits=%d currency_mismatches=%d foreign_accounts=%d wrong_debit=%d wrong_credit=%d",
		p.TransferID, p.EntryCount, p.EntrySumMinor, p.DebitCount, p.CreditCount,
		p.CurrencyMismatches, p.ForeignAccounts, p.WrongDebitAmount, p.WrongCreditAmount)
}

// Report is the outcome of a reconciliation pass.
type Report struct {
	Accounts           int   `json:"accounts"`
	CompletedTransfers int   `json:"completed_transfers"`
	PendingTransfers   int   `json:"pending_transfers"`
	LedgerEntries      int   `json:"ledger_entries"`
	TotalBalanceMinor  int64 `json:"total_balance_minor"`
	TotalLedgerMinor   int64 `json:"total_ledger_minor"`

	NegativeBalances    []NegativeBalance `json:"negative_balances,omitempty"`
	BalanceMismatches   []BalanceMismatch `json:"balance_mismatches,omitempty"`
	UnbalancedTransfers []TransferProblem `json:"unbalanced_transfers,omitempty"`
}

// OK reports whether every invariant held.
func (r Report) OK() bool {
	return len(r.NegativeBalances) == 0 &&
		len(r.BalanceMismatches) == 0 &&
		len(r.UnbalancedTransfers) == 0 &&
		r.TotalLedgerMinor == 0
}

// Violations counts the individual problems found, for load-tooling output.
func (r Report) Violations() int {
	n := len(r.NegativeBalances) + len(r.BalanceMismatches) + len(r.UnbalancedTransfers)
	if r.TotalLedgerMinor != 0 {
		n++
	}
	return n
}

// Problems returns one human-readable line per violation, empty when clean.
func (r Report) Problems() []string {
	var out []string

	if r.TotalLedgerMinor != 0 {
		out = append(out, fmt.Sprintf(
			"the whole ledger sums to %d, want 0", r.TotalLedgerMinor))
	}
	for _, n := range r.NegativeBalances {
		out = append(out, fmt.Sprintf("account %s has a negative balance of %d",
			n.AccountID, n.BalanceMinor))
	}
	for _, m := range r.BalanceMismatches {
		out = append(out, fmt.Sprintf(
			"account %s holds %d but baseline %d plus ledger %d expects %d (difference %d)",
			m.AccountID, m.StoredMinor, m.BaselineMinor, m.LedgerMinor, m.ExpectedMinor, m.Difference()))
	}
	for _, p := range r.UnbalancedTransfers {
		out = append(out, p.String())
	}
	return out
}

// Querier is satisfied by *pgxpool.Pool and by pgx.Tx, so a reconciliation can
// run against the pool or inside an existing transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var _ Querier = (*pgxpool.Pool)(nil)

// Check runs every reconciliation query and returns what it found.
//
// It reports all problems rather than stopping at the first, because after a
// stress run the shape of the damage matters more than its first instance.
func Check(ctx context.Context, db Querier, baseline Baseline) (Report, error) {
	var report Report

	if err := countTotals(ctx, db, &report); err != nil {
		return Report{}, err
	}
	if err := findNegativeBalances(ctx, db, &report); err != nil {
		return Report{}, err
	}
	if err := findUnbalancedTransfers(ctx, db, &report); err != nil {
		return Report{}, err
	}
	if err := findBalanceMismatches(ctx, db, baseline, &report); err != nil {
		return Report{}, err
	}
	return report, nil
}

func countTotals(ctx context.Context, db Querier, report *Report) error {
	const query = `
		SELECT
			(SELECT COUNT(*) FROM accounts),
			(SELECT COALESCE(SUM(balance_minor), 0) FROM accounts),
			(SELECT COUNT(*) FROM transfers WHERE status = 'completed'),
			(SELECT COUNT(*) FROM transfers WHERE status = 'pending'),
			(SELECT COUNT(*) FROM ledger_entries),
			(SELECT COALESCE(SUM(amount_minor), 0) FROM ledger_entries)`

	err := db.QueryRow(ctx, query).Scan(
		&report.Accounts, &report.TotalBalanceMinor,
		&report.CompletedTransfers, &report.PendingTransfers,
		&report.LedgerEntries, &report.TotalLedgerMinor,
	)
	if err != nil {
		return fmt.Errorf("reconcile: totals: %w", err)
	}
	return nil
}

// findNegativeBalances asks the database directly rather than trusting any
// Go-side accounting. A committed negative balance would mean both the
// application checks and the CHECK constraint had failed.
func findNegativeBalances(ctx context.Context, db Querier, report *Report) error {
	const query = `
		SELECT id, balance_minor
		FROM accounts
		WHERE balance_minor < 0
		ORDER BY id`

	rows, err := db.Query(ctx, query)
	if err != nil {
		return fmt.Errorf("reconcile: negative balances: %w", err)
	}
	defer rows.Close()

	report.NegativeBalances, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (NegativeBalance, error) {
		var n NegativeBalance
		err := row.Scan(&n.AccountID, &n.BalanceMinor)
		return n, err
	})
	if err != nil {
		return fmt.Errorf("reconcile: scan negative balances: %w", err)
	}
	return nil
}

// findUnbalancedTransfers checks the persisted rows for every completed
// transfer: that there are exactly two entries, that they sum to zero, that
// one is a debit and one a credit, that the currencies match the transfer, and
// that the entries hit the source and destination accounts for the transfer's
// amount.
//
// Only offending transfers are returned, so this stays usable on a large
// ledger rather than pulling every row into memory.
func findUnbalancedTransfers(ctx context.Context, db Querier, report *Report) error {
	const query = `
		WITH per_transfer AS (
			SELECT
				t.id,
				COUNT(e.id)                                                        AS entry_count,
				COALESCE(SUM(e.amount_minor), 0)                                   AS entry_sum,
				COUNT(*) FILTER (WHERE e.amount_minor < 0)                         AS debit_count,
				COUNT(*) FILTER (WHERE e.amount_minor > 0)                         AS credit_count,
				COUNT(*) FILTER (WHERE e.currency IS DISTINCT FROM t.currency)     AS currency_mismatches,
				COUNT(*) FILTER (
					WHERE e.account_id <> t.source_account_id
					  AND e.account_id <> t.destination_account_id
				)                                                                  AS foreign_accounts,
				COUNT(*) FILTER (
					WHERE e.account_id = t.source_account_id
					  AND e.amount_minor <> -t.amount_minor
				)                                                                  AS wrong_debit,
				COUNT(*) FILTER (
					WHERE e.account_id = t.destination_account_id
					  AND e.amount_minor <> t.amount_minor
				)                                                                  AS wrong_credit
			FROM transfers t
			LEFT JOIN ledger_entries e ON e.transfer_id = t.id
			WHERE t.status = 'completed'
			GROUP BY t.id
		)
		SELECT id, entry_count, entry_sum, debit_count, credit_count,
		       currency_mismatches, foreign_accounts, wrong_debit, wrong_credit
		FROM per_transfer
		WHERE entry_count <> 2
		   OR entry_sum <> 0
		   OR debit_count <> 1
		   OR credit_count <> 1
		   OR currency_mismatches > 0
		   OR foreign_accounts > 0
		   OR wrong_debit > 0
		   OR wrong_credit > 0
		ORDER BY id`

	rows, err := db.Query(ctx, query)
	if err != nil {
		return fmt.Errorf("reconcile: unbalanced transfers: %w", err)
	}
	defer rows.Close()

	report.UnbalancedTransfers, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (TransferProblem, error) {
		var p TransferProblem
		err := row.Scan(&p.TransferID, &p.EntryCount, &p.EntrySumMinor,
			&p.DebitCount, &p.CreditCount, &p.CurrencyMismatches,
			&p.ForeignAccounts, &p.WrongDebitAmount, &p.WrongCreditAmount)
		return p, err
	})
	if err != nil {
		return fmt.Errorf("reconcile: scan unbalanced transfers: %w", err)
	}
	return nil
}

// findBalanceMismatches checks that every stored balance equals its baseline
// funding plus the sum of its ledger entries.
func findBalanceMismatches(ctx context.Context, db Querier, baseline Baseline, report *Report) error {
	const query = `
		SELECT a.id, a.balance_minor, COALESCE(SUM(e.amount_minor), 0) AS ledger_minor
		FROM accounts a
		LEFT JOIN ledger_entries e ON e.account_id = a.id
		GROUP BY a.id, a.balance_minor
		ORDER BY a.id`

	rows, err := db.Query(ctx, query)
	if err != nil {
		return fmt.Errorf("reconcile: balance mismatches: %w", err)
	}
	defer rows.Close()

	mismatches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BalanceMismatch, error) {
		var m BalanceMismatch
		if err := row.Scan(&m.AccountID, &m.StoredMinor, &m.LedgerMinor); err != nil {
			return BalanceMismatch{}, err
		}
		// An account with no baseline entry is expected to be explained by the
		// ledger alone.
		m.BaselineMinor = baseline[m.AccountID]
		m.ExpectedMinor = m.BaselineMinor + m.LedgerMinor
		return m, nil
	})
	if err != nil {
		return fmt.Errorf("reconcile: scan balance mismatches: %w", err)
	}

	for _, m := range mismatches {
		if m.StoredMinor != m.ExpectedMinor {
			report.BalanceMismatches = append(report.BalanceMismatches, m)
		}
	}
	return nil
}

// SnapshotBaseline derives the current unexplained balance of every account:
// its stored balance minus the sum of its ledger entries.
//
// It is how development tooling establishes a starting point without being
// told how accounts came to be funded. Reconciling against such a snapshot
// proves that the workload run afterwards explained every movement it made;
// it says nothing about how the balances got there beforehand, which is the
// same exception described on Baseline.
func SnapshotBaseline(ctx context.Context, db Querier) (Baseline, error) {
	const query = `
		SELECT a.id, a.balance_minor - COALESCE(SUM(e.amount_minor), 0)
		FROM accounts a
		LEFT JOIN ledger_entries e ON e.account_id = a.id
		GROUP BY a.id, a.balance_minor`

	rows, err := db.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("reconcile: snapshot baseline: %w", err)
	}
	defer rows.Close()

	baseline := Baseline{}
	for rows.Next() {
		var (
			id          uuid.UUID
			unexplained int64
		)
		if err := rows.Scan(&id, &unexplained); err != nil {
			return nil, fmt.Errorf("reconcile: scan baseline: %w", err)
		}
		baseline[id] = unexplained
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reconcile: read baseline: %w", err)
	}
	return baseline, nil
}
