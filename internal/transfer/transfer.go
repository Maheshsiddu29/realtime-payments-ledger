// Package transfer implements the atomic double-entry posting operation.
//
// Posting a transfer is the only way money moves in this system. It debits one
// account, credits another, and writes the two ledger entries that record the
// movement — all inside a single PostgreSQL transaction. Either every one of
// those effects is durable, or none of them is.
package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/money"
)

// Status is the lifecycle state of a transfer. The set is closed and mirrored
// by a CHECK constraint on the transfers table.
type Status string

const (
	// StatusPending is the state of a transfer that has been recorded but not
	// yet posted. It is only ever observed inside the posting transaction.
	StatusPending Status = "pending"
	// StatusCompleted means the money moved and the ledger entries exist.
	// Completed transfers are immutable, enforced by a database trigger.
	StatusCompleted Status = "completed"
	// StatusFailed is reserved for transfers that are recorded as attempted
	// and rejected. Phase 1 never writes it: a failed posting rolls back
	// entirely and leaves no row at all.
	StatusFailed Status = "failed"
)

// Errors returned by Post. They are sentinels so that a caller — an HTTP or
// gRPC layer in a later phase — can map them to status codes without parsing
// strings.
var (
	// ErrNotFound is returned by Get for an unknown transfer id.
	ErrNotFound = errors.New("transfer: not found")
	// ErrSourceAccountNotFound means the debited account does not exist.
	ErrSourceAccountNotFound = errors.New("transfer: source account not found")
	// ErrDestinationAccountNotFound means the credited account does not exist.
	ErrDestinationAccountNotFound = errors.New("transfer: destination account not found")
	// ErrSameAccount means source and destination are the same account.
	ErrSameAccount = errors.New("transfer: source and destination must differ")
	// ErrInvalidAmount means the amount was not strictly positive.
	ErrInvalidAmount = errors.New("transfer: amount must be greater than zero")
	// ErrCurrencyMismatch means the transfer currency and the two account
	// currencies are not all identical. Currencies are never converted
	// implicitly.
	ErrCurrencyMismatch = errors.New("transfer: currency mismatch")
	// ErrInsufficientFunds means the source account balance is below the
	// amount. Accounts may not be overdrawn.
	ErrInsufficientFunds = errors.New("transfer: insufficient funds")
	// ErrNotBalanced means the posted entries did not sum to zero. It should
	// be unreachable; if it is ever returned, the posting logic is broken and
	// the transaction has been rolled back.
	ErrNotBalanced = errors.New("transfer: ledger entries do not balance")
)

// Transfer is the record of an attempt to move money between two accounts.
type Transfer struct {
	ID                   uuid.UUID
	SourceAccountID      uuid.UUID
	DestinationAccountID uuid.UUID
	// AmountMinor is always positive; direction comes from the account fields.
	AmountMinor int64
	Currency    money.Currency
	Status      Status
	CreatedAt   time.Time
	// CompletedAt is set if and only if Status is StatusCompleted.
	CompletedAt *time.Time
}

// Command describes a transfer to post.
type Command struct {
	SourceAccountID      uuid.UUID
	DestinationAccountID uuid.UUID
	AmountMinor          int64
	Currency             money.Currency
}

// Validate checks the parts of a command that need no database access.
func (c Command) Validate() error {
	if c.AmountMinor <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidAmount, c.AmountMinor)
	}
	if err := c.Currency.Validate(); err != nil {
		return err
	}
	if c.SourceAccountID == c.DestinationAccountID {
		return fmt.Errorf("%w: both are %s", ErrSameAccount, c.SourceAccountID)
	}
	if c.SourceAccountID == uuid.Nil || c.DestinationAccountID == uuid.Nil {
		return fmt.Errorf("%w: account id must not be the zero uuid", ErrSourceAccountNotFound)
	}
	return nil
}

// Service posts transfers and reads them back.
type Service struct {
	pool  *pgxpool.Pool
	log   *slog.Logger
	retry RetryPolicy
}

// NewService returns a transfer service using DefaultRetryPolicy.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return NewServiceWithPolicy(pool, log, DefaultRetryPolicy)
}

// NewServiceWithPolicy returns a transfer service with an explicit retry
// policy. Tests use it to make retry behaviour observable without waiting on
// the production backoff.
func NewServiceWithPolicy(pool *pgxpool.Pool, log *slog.Logger, policy RetryPolicy) *Service {
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}
	return &Service{pool: pool, log: log, retry: policy}
}

// RetryPolicy returns the policy this service applies.
func (s *Service) RetryPolicy() RetryPolicy { return s.retry }

// Post moves money atomically and returns the completed transfer.
//
// It runs postOnce inside a bounded retry loop. Keeping the retry policy out
// of the transaction body means transactional correctness and retry policy can
// be reviewed — and tested — separately, and that the transfer logic exists in
// exactly one place.
func (s *Service) Post(ctx context.Context, cmd Command) (Transfer, error) {
	posted, _, err := s.PostWithAttempts(ctx, cmd)
	return posted, err
}

// PostWithAttempts is Post, additionally reporting how much contention the
// call encountered. Tests and the load generator use it to count retries;
// ordinary callers should use Post.
//
// Only SQLSTATE 40001 and 40P01 are retried. Business rejections —
// insufficient funds, unknown account, currency mismatch, invalid amount — are
// never PgErrors and are returned on the first attempt, so a caller can never
// have a refusal silently retried into a success.
func (s *Service) PostWithAttempts(ctx context.Context, cmd Command) (Transfer, Attempts, error) {
	var attempts Attempts

	// Validation needs no database, so a malformed command costs no attempt.
	if err := cmd.Validate(); err != nil {
		return Transfer{}, attempts, err
	}

	for attempt := 1; ; attempt++ {
		attempts.Total = attempt

		posted, err := s.postOnce(ctx, cmd)
		if err == nil {
			if attempt > 1 {
				s.log.InfoContext(ctx, "transfer posted after contention",
					slog.String("transfer_id", posted.ID.String()),
					slog.Int("attempts", attempt),
					slog.Int("serialization_failures", attempts.SerializationFailures),
					slog.Int("deadlocks", attempts.Deadlocks),
				)
			}
			return posted, attempts, nil
		}

		code, retryable := retryableCode(err)
		if !retryable {
			return Transfer{}, attempts, err
		}
		attempts.record(code)

		if attempt >= s.retry.MaxAttempts {
			// Bounded: the loop always terminates here, whatever the load.
			return Transfer{}, attempts, fmt.Errorf("%w after %d attempts (last SQLSTATE %s): %w",
				ErrRetriesExhausted, attempt, code, err)
		}

		// A cancelled request stops immediately rather than finishing its
		// backoff.
		if err := s.retry.wait(ctx, attempt); err != nil {
			return Transfer{}, attempts, fmt.Errorf("transfer: retry cancelled after %d attempts: %w", attempt, err)
		}
	}
}

// postOnce performs exactly one attempt: one transaction, from BEGIN to
// COMMIT. It contains no retry logic of any kind.
//
// Everything happens inside one SERIALIZABLE transaction:
//
//  1. lock both account rows in a canonical order
//  2. read the locked source and destination state
//  3. check both exist, share a currency, and match the transfer currency
//  4. check the source has enough money
//  5. insert the transfer as pending
//  6. debit the source with a guarded UPDATE
//  7. credit the destination
//  8. insert the debit and credit ledger entries
//  9. verify in Go that the entries balance
//  10. mark the transfer completed
//  11. commit — at which point PostgreSQL re-checks the balance invariant
//
// Any error at any step rolls the whole thing back, so there is no state in
// which the source is debited without the destination being credited, or a
// transfer is completed without its ledger entries.
func (s *Service) postOnce(ctx context.Context, cmd Command) (Transfer, error) {
	// SERIALIZABLE is the strongest isolation PostgreSQL offers: concurrent
	// transactions behave as if they had run one after another. It is kept
	// even though explicit row locking was added, because the two protect
	// different things — see docs/CONCURRENCY.md.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return Transfer{}, fmt.Errorf("transfer: begin: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this defer is safe on
	// every path and guarantees no transaction is ever left open.
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			s.log.ErrorContext(ctx, "rolling back transfer failed",
				slog.String("error", err.Error()))
		}
	}()

	posted, err := s.post(ctx, tx, cmd)
	if err != nil {
		return Transfer{}, err
	}

	// The deferred constraint triggers run here. If the entries somehow did
	// not balance, the COMMIT itself fails and nothing is durable. A
	// serialization failure also surfaces here rather than at a statement,
	// which is why the commit error is returned unwrapped by any business
	// error type — the retry loop needs to see its SQLSTATE.
	if err := tx.Commit(ctx); err != nil {
		return Transfer{}, fmt.Errorf("transfer: commit: %w", err)
	}

	s.log.InfoContext(ctx, "transfer posted",
		slog.String("transfer_id", posted.ID.String()),
		slog.String("source_account_id", posted.SourceAccountID.String()),
		slog.String("destination_account_id", posted.DestinationAccountID.String()),
		slog.Int64("amount_minor", posted.AmountMinor),
		slog.String("currency", posted.Currency.String()),
	)
	return posted, nil
}

// post carries out the work inside an open transaction. Splitting it from
// postOnce keeps the commit/rollback handling in one place and lets every step
// here simply return an error.
func (s *Service) post(ctx context.Context, tx pgx.Tx, cmd Command) (Transfer, error) {
	source, destination, err := lockAccounts(ctx, tx, cmd.SourceAccountID, cmd.DestinationAccountID)
	if err != nil {
		return Transfer{}, err
	}

	// All three currencies must agree. There is no implicit conversion: a
	// cross-currency transfer needs an exchange rate and two more ledger
	// entries, which this phase does not model.
	if source.currency != cmd.Currency {
		return Transfer{}, fmt.Errorf("%w: source account is %s, transfer is %s",
			ErrCurrencyMismatch, source.currency, cmd.Currency)
	}
	if destination.currency != cmd.Currency {
		return Transfer{}, fmt.Errorf("%w: destination account is %s, transfer is %s",
			ErrCurrencyMismatch, destination.currency, cmd.Currency)
	}

	// The balance was read under FOR UPDATE, so no concurrent transfer can
	// change it before this transaction ends: this check is now authoritative
	// as well as producing a precise error. The guarded UPDATE below and the
	// non-negative CHECK constraint are kept as defence in depth — they are
	// what would still stop an overdraft if this locking were ever weakened,
	// which was verified by removing this check and observing the constraint
	// hold.
	if source.balanceMinor < cmd.AmountMinor {
		return Transfer{}, fmt.Errorf("%w: account %s holds %d, needs %d",
			ErrInsufficientFunds, source.id, source.balanceMinor, cmd.AmountMinor)
	}

	transferID := uuid.New()
	created := Transfer{
		ID:                   transferID,
		SourceAccountID:      cmd.SourceAccountID,
		DestinationAccountID: cmd.DestinationAccountID,
		AmountMinor:          cmd.AmountMinor,
		Currency:             cmd.Currency,
		Status:               StatusPending,
	}

	const insertTransfer = `
		INSERT INTO transfers (id, source_account_id, destination_account_id, amount_minor, currency, status)
		VALUES ($1, $2, $3, $4, $5, 'pending')
		RETURNING created_at`
	err = tx.QueryRow(ctx, insertTransfer,
		created.ID, created.SourceAccountID, created.DestinationAccountID,
		created.AmountMinor, created.Currency,
	).Scan(&created.CreatedAt)
	if err != nil {
		return Transfer{}, fmt.Errorf("transfer: insert: %w", err)
	}

	// Guarded debit: the WHERE clause re-checks the balance as part of the
	// same statement. With the row locked this cannot fail, which is exactly
	// why it stays — if it ever does fail, the locking is broken and the
	// transfer must be rejected rather than proceed on a stale read.
	const debit = `
		UPDATE accounts
		SET balance_minor = balance_minor - $2
		WHERE id = $1 AND balance_minor >= $2`
	tag, err := tx.Exec(ctx, debit, cmd.SourceAccountID, cmd.AmountMinor)
	if err != nil {
		return Transfer{}, fmt.Errorf("transfer: debit source: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return Transfer{}, fmt.Errorf("%w: account %s no longer holds %d",
			ErrInsufficientFunds, cmd.SourceAccountID, cmd.AmountMinor)
	}

	const credit = `
		UPDATE accounts
		SET balance_minor = balance_minor + $2
		WHERE id = $1`
	tag, err = tx.Exec(ctx, credit, cmd.DestinationAccountID, cmd.AmountMinor)
	if err != nil {
		return Transfer{}, fmt.Errorf("transfer: credit destination: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return Transfer{}, fmt.Errorf("transfer: credit destination %s affected %d rows, want 1",
			cmd.DestinationAccountID, tag.RowsAffected())
	}

	// The two halves of the double entry. Written together, in the same
	// transaction as the balance changes.
	const insertEntries = `
		INSERT INTO ledger_entries (id, transfer_id, account_id, amount_minor, currency)
		VALUES ($1, $2, $3, $4, $5), ($6, $2, $7, $8, $5)`
	_, err = tx.Exec(ctx, insertEntries,
		uuid.New(), transferID, cmd.SourceAccountID, -cmd.AmountMinor, cmd.Currency,
		uuid.New(), cmd.DestinationAccountID, cmd.AmountMinor,
	)
	if err != nil {
		return Transfer{}, fmt.Errorf("transfer: insert ledger entries: %w", err)
	}

	// Application-side verification of the invariant, before the transfer is
	// marked completed. PostgreSQL re-checks this at COMMIT; doing it here too
	// means a bug is caught with a precise error rather than an opaque commit
	// failure.
	if err := assertBalanced(ctx, tx, transferID); err != nil {
		return Transfer{}, err
	}

	const complete = `
		UPDATE transfers
		SET status = 'completed', completed_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING status, completed_at`
	var completedAt time.Time
	err = tx.QueryRow(ctx, complete, transferID).Scan(&created.Status, &completedAt)
	if err != nil {
		return Transfer{}, fmt.Errorf("transfer: mark completed: %w", err)
	}
	created.CompletedAt = &completedAt

	return created, nil
}

// account is the minimal projection the posting needs. The full account type
// lives in the account package; duplicating three fields here avoids an import
// cycle and keeps the posting query explicit about what it reads.
type account struct {
	id           uuid.UUID
	currency     money.Currency
	balanceMinor int64
}

// lockOrder returns the two account ids in canonical order: the numerically
// smaller UUID first, comparing the raw 16 bytes.
//
// This is the whole of the deadlock-prevention strategy. Two transfers in
// opposite directions over the same pair of accounts — A to B and B to A —
// would, if each locked its own source first, take the two row locks in
// opposite orders and could form a cycle: each holds one row and waits for the
// other. PostgreSQL breaks such a cycle by aborting one transaction with
// SQLSTATE 40P01.
//
// Sorting by UUID makes the acquisition order a property of the *pair* of
// accounts rather than of the direction of the transfer, so every transaction
// touching the same two rows requests them in the same sequence and no cycle
// can form. Any total order would do; UUID byte order is used because it is
// already available, stable, and free of ties.
func lockOrder(a, b uuid.UUID) (first, second uuid.UUID) {
	if bytes.Compare(a[:], b[:]) <= 0 {
		return a, b
	}
	return b, a
}

// lockAccounts takes a row-level write lock on both accounts in canonical
// order and returns them mapped back to their business roles.
//
// Lock acquisition order and business meaning are deliberately kept apart. The
// rows are locked lowest-UUID-first, but the returned source and destination
// are resolved by matching ids, never by which row happened to be locked
// first. Debiting "whichever row was locked first" would silently reverse half
// of all transfers.
func lockAccounts(ctx context.Context, tx pgx.Tx, sourceID, destinationID uuid.UUID) (source, destination account, err error) {
	first, second := lockOrder(sourceID, destinationID)

	// Two statements rather than one `WHERE id = ANY(...) ORDER BY id FOR
	// UPDATE`: with a single statement the order in which rows are locked
	// depends on the query plan, and the ordering guarantee is exactly what
	// this function exists to provide. Two round trips is a small price for an
	// order that is obvious from the code.
	lockedFirst, err := lockAccount(ctx, tx, first)
	if err != nil {
		return account{}, account{}, missingAccountError(err, first, sourceID)
	}

	lockedSecond, err := lockAccount(ctx, tx, second)
	if err != nil {
		return account{}, account{}, missingAccountError(err, second, sourceID)
	}

	// Map back by identity, not by lock position.
	if lockedFirst.id == sourceID {
		return lockedFirst, lockedSecond, nil
	}
	return lockedSecond, lockedFirst, nil
}

// missingAccountError attributes a missing row to the correct business role,
// so the caller learns which side of the transfer was wrong.
func missingAccountError(err error, lockedID, sourceID uuid.UUID) error {
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if lockedID == sourceID {
		return fmt.Errorf("%w: %s", ErrSourceAccountNotFound, lockedID)
	}
	return fmt.Errorf("%w: %s", ErrDestinationAccountNotFound, lockedID)
}

// lockAccount reads one account row and holds a write lock on it until the
// transaction ends.
//
// FOR UPDATE is what serialises the read-modify-write on a balance: a
// concurrent transfer touching the same account blocks here instead of reading
// a balance that is about to change. Under SERIALIZABLE alone the conflict
// would still be caught, but only at COMMIT and only by aborting one
// transaction — blocking briefly is far cheaper than doing the whole transfer
// twice.
func lockAccount(ctx context.Context, tx pgx.Tx, id uuid.UUID) (account, error) {
	const query = `
		SELECT id, currency, balance_minor
		FROM accounts
		WHERE id = $1
		FOR UPDATE`

	var a account
	if err := tx.QueryRow(ctx, query, id).Scan(&a.id, &a.currency, &a.balanceMinor); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return account{}, err
		}
		return account{}, fmt.Errorf("transfer: lock account %s: %w", id, err)
	}
	return a, nil
}

// assertBalanced verifies the double-entry invariant for one transfer:
// the entries must sum to zero, and this phase's model posts exactly two.
func assertBalanced(ctx context.Context, tx pgx.Tx, transferID uuid.UUID) error {
	const query = `
		SELECT COALESCE(SUM(amount_minor), 0), COUNT(*)
		FROM ledger_entries
		WHERE transfer_id = $1`

	var (
		sum   int64
		count int
	)
	if err := tx.QueryRow(ctx, query, transferID).Scan(&sum, &count); err != nil {
		return fmt.Errorf("transfer: verify invariant: %w", err)
	}
	if count != 2 {
		return fmt.Errorf("%w: transfer %s has %d entries, want 2", ErrNotBalanced, transferID, count)
	}
	if sum != 0 {
		return fmt.Errorf("%w: transfer %s entries sum to %d, want 0", ErrNotBalanced, transferID, sum)
	}
	return nil
}

// Get returns a transfer by id, or ErrNotFound.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Transfer, error) {
	const query = `
		SELECT id, source_account_id, destination_account_id,
		       amount_minor, currency, status, created_at, completed_at
		FROM transfers
		WHERE id = $1`

	var t Transfer
	err := s.pool.QueryRow(ctx, query, id).Scan(
		&t.ID, &t.SourceAccountID, &t.DestinationAccountID,
		&t.AmountMinor, &t.Currency, &t.Status, &t.CreatedAt, &t.CompletedAt,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Transfer{}, fmt.Errorf("transfer %s: %w", id, ErrNotFound)
	case err != nil:
		return Transfer{}, fmt.Errorf("transfer: get %s: %w", id, err)
	}
	return t, nil
}
