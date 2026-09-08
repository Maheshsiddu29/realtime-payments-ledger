package transfer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/idempotency"
)

// Idempotency errors. They are sentinels so that an API layer in a later phase
// can map them to status codes without parsing strings, and so that no raw
// Redis or PostgreSQL text ever reaches a client.
var (
	// ErrIdempotencyConflict means the key was reused for a materially
	// different payment. The second request is refused; it is not executed.
	ErrIdempotencyConflict = errors.New("transfer: idempotency key reused for a different request")
	// ErrIdempotencyInProgress means another request holds the claim for this
	// key and has not finished. The caller should retry shortly.
	ErrIdempotencyInProgress = errors.New("transfer: request with this idempotency key is already in progress")
)

// PostgreSQL identifiers used to recognise the deduplication barrier firing.
const (
	sqlStateUniqueViolation = "23505"
	// idempotencyKeyConstraint is named explicitly in migration 000005 so this
	// can distinguish a duplicate key from any other unique violation.
	idempotencyKeyConstraint = "transfers_idempotency_key_unique"
)

// idempotencyStamp is the pair of values persisted with a transfer to arm the
// UNIQUE constraint. A nil stamp means the transfer has no idempotency key,
// which is how internally originated transfers are posted.
type idempotencyStamp struct {
	key         string
	fingerprint []byte
}

func (s *idempotencyStamp) keyOrNil() any {
	if s == nil {
		return nil
	}
	return s.key
}

func (s *idempotencyStamp) fingerprintOrNil() any {
	if s == nil {
		return nil
	}
	return s.fingerprint
}

// Result describes the outcome of an idempotent post, including how the answer
// was arrived at. Tests and operators need that distinction; a caller that
// only wants the money moved can ignore everything but Transfer.
type Result struct {
	Transfer Transfer
	Attempts Attempts

	// Replayed is true when this is the original transfer for a repeated
	// request rather than a newly created one. No money moved.
	Replayed bool
	// RecoveredFromDatabase is true when the replay came from the PostgreSQL
	// uniqueness barrier rather than from the Redis cache — the path that
	// covers a lost Redis record, an expired key, or a crash between COMMIT
	// and the Redis update.
	RecoveredFromDatabase bool
	// CoordinationDegraded is true when Redis could not be used and the
	// request relied on PostgreSQL alone. Deduplication still held.
	CoordinationDegraded bool
}

// PostIdempotent posts a transfer at most once for a given idempotency key.
//
// The guarantee is: same key + same request => at most one financial transfer,
// no matter how many times the request arrives, how they overlap, or what
// Redis is doing.
//
// Redis coordinates: it detects duplicates in one round trip and lets a replay
// be answered without re-running the posting. PostgreSQL decides: the UNIQUE
// constraint on transfers.idempotency_key is what actually makes a second
// transfer impossible. Every Redis failure mode below therefore degrades
// performance, never correctness.
//
// The flow:
//
//  1. validate the command and compute the request fingerprint
//  2. try to claim the key in Redis with SET NX EX
//  3. claim held by someone else and COMPLETED   -> return the original
//  4. claim held by someone else and PROCESSING  -> check PostgreSQL, then
//     report in-progress
//  5. fingerprint differs from the record        -> conflict, do not execute
//  6. claim acquired (or Redis unusable)         -> post the transfer
//  7. UNIQUE violation on the key                -> load and return the
//     original transfer
//  8. success                                    -> cache the result
func (s *Service) PostIdempotent(ctx context.Context, key idempotency.Key, cmd Command) (Result, error) {
	if _, err := idempotency.ParseKey(key.String()); err != nil {
		return Result{}, err
	}
	if err := cmd.Validate(); err != nil {
		return Result{}, err
	}

	fingerprint := idempotency.Request{
		SourceAccountID:      cmd.SourceAccountID,
		DestinationAccountID: cmd.DestinationAccountID,
		AmountMinor:          cmd.AmountMinor,
		Currency:             cmd.Currency.String(),
	}.Fingerprint()

	degraded := !s.idem.Available()

	if !degraded {
		claim, err := s.idem.Acquire(ctx, key, fingerprint)
		switch {
		case errors.Is(err, idempotency.ErrUnavailable):
			// Redis is down. Fall through to the PostgreSQL-only path rather
			// than refusing the payment: the UNIQUE constraint provides the
			// full deduplication guarantee without Redis. See
			// docs/IDEMPOTENCY.md for why this is a fallback and not a
			// weakening.
			degraded = true
			s.log.WarnContext(ctx, "idempotency coordination unavailable, relying on postgres uniqueness",
				slog.String("idempotency_key", key.String()),
				slog.String("error", err.Error()))

		case errors.Is(err, idempotency.ErrInProgress):
			// One sentinel for callers: the store's in-progress condition and
			// this package's are the same thing to anyone above.
			return Result{}, fmt.Errorf("%w: key %s", ErrIdempotencyInProgress, key)

		case err != nil:
			return Result{}, err

		case !claim.Acquired:
			return s.resolveExisting(ctx, key, fingerprint, cmd, claim.Existing)
		}
	}

	return s.postClaimed(ctx, key, fingerprint, cmd, degraded)
}

// resolveExisting answers a request whose key is already held by another.
func (s *Service) resolveExisting(ctx context.Context, key idempotency.Key, fingerprint idempotency.Fingerprint, cmd Command, existing idempotency.Record) (Result, error) {
	// A key reused for a different payment is refused before anything else is
	// considered. Returning the first result would silently swallow the second
	// payment; executing it would break the guarantee the key exists to give.
	if !existing.Matches(fingerprint) {
		return Result{}, fmt.Errorf("%w: key %s was first used for a different payment", ErrIdempotencyConflict, key)
	}

	// Whatever Redis says, the answer comes from PostgreSQL. Redis holds a
	// transfer id; PostgreSQL holds the transfer. Reading through means a
	// stale or wrong cache entry cannot make this return the wrong payment.
	result, found, err := s.recoverFromDatabase(ctx, key, fingerprint)
	if err != nil {
		return Result{}, err
	}
	if found {
		return result, nil
	}

	switch existing.State {
	case idempotency.StateCompleted:
		// Redis believes this completed but PostgreSQL has no such transfer.
		// The record cannot be trusted — a flushed database, a stale
		// environment — so fall through and post. The UNIQUE constraint keeps
		// that safe: at worst it creates the one transfer that should exist.
		s.log.WarnContext(ctx, "redis reports a completed transfer that postgres does not have; reposting",
			slog.String("idempotency_key", key.String()),
			slog.String("redis_transfer_id", existing.TransferID))
		return s.postClaimed(ctx, key, fingerprint, cmd, false)

	default:
		// PROCESSING, and PostgreSQL has nothing yet. The other request is
		// still working.
		//
		// The policy is to report in progress immediately rather than poll.
		// Polling would hold a goroutine and a connection for the duration of
		// somebody else's transaction, and it would still have to give up
		// eventually. An explicit, immediate answer lets the caller decide.
		// The claim carries a lease, so a crashed holder cannot block the key
		// beyond ProcessingTTL.
		return Result{}, fmt.Errorf("%w: key %s", ErrIdempotencyInProgress, key)
	}
}

// postClaimed posts the transfer with the idempotency key attached, and deals
// with the UNIQUE constraint firing.
func (s *Service) postClaimed(ctx context.Context, key idempotency.Key, fingerprint idempotency.Fingerprint, cmd Command, degraded bool) (Result, error) {
	stamp := &idempotencyStamp{key: key.String(), fingerprint: fingerprint.Bytes()}

	posted, attempts, err := s.postWithRetry(ctx, cmd, stamp)
	if err != nil {
		// The deduplication barrier fired: a transfer for this key already
		// exists. This is the path that recovers from a lost Redis record, an
		// expired key, a crash between COMMIT and the Redis update, and two
		// requests racing past Redis entirely.
		if isDuplicateIdempotencyKey(err) {
			result, found, recErr := s.recoverFromDatabase(ctx, key, fingerprint)
			if recErr != nil {
				return Result{}, recErr
			}
			if !found {
				// The constraint fired but the row is not visible. The only
				// way this happens is a concurrent transaction that has not
				// committed yet, so report in progress rather than inventing
				// an answer.
				return Result{}, fmt.Errorf("%w: key %s", ErrIdempotencyInProgress, key)
			}

			result.Attempts = attempts
			result.CoordinationDegraded = degraded
			// Repair the cache so the next replay does not need this lookup.
			s.cacheCompletion(ctx, key, fingerprint, result.Transfer.ID)
			return result, nil
		}

		// A business rejection: no transfer was created and no money moved, so
		// the key must not stay locked for the rest of its lease. Releasing it
		// lets a later retry — after the account is funded, say — proceed.
		// Only rejections are released; a completed transfer never is.
		s.releaseClaim(ctx, key)
		return Result{Attempts: attempts, CoordinationDegraded: degraded}, err
	}

	if !s.cacheCompletion(ctx, key, fingerprint, posted.ID) && s.idem.Available() {
		degraded = true
	}

	return Result{Transfer: posted, Attempts: attempts, CoordinationDegraded: degraded}, nil
}

// recoverFromDatabase loads the transfer already recorded for a key.
//
// It verifies the stored fingerprint rather than trusting the caller's: if the
// key was first used for a different payment, this is a conflict even though
// the barrier is what surfaced it.
func (s *Service) recoverFromDatabase(ctx context.Context, key idempotency.Key, fingerprint idempotency.Fingerprint) (Result, bool, error) {
	const query = `
		SELECT id, source_account_id, destination_account_id,
		       amount_minor, currency, status, created_at, completed_at,
		       request_fingerprint
		FROM transfers
		WHERE idempotency_key = $1`

	var (
		t      Transfer
		stored []byte
	)
	err := s.pool.QueryRow(ctx, query, key.String()).Scan(
		&t.ID, &t.SourceAccountID, &t.DestinationAccountID,
		&t.AmountMinor, &t.Currency, &t.Status, &t.CreatedAt, &t.CompletedAt,
		&stored,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Result{}, false, nil
	case err != nil:
		return Result{}, false, fmt.Errorf("transfer: recover by idempotency key: %w", err)
	}

	storedFingerprint, err := idempotency.FingerprintFromBytes(stored)
	if err != nil {
		return Result{}, false, fmt.Errorf("transfer: stored fingerprint for key %s: %w", key, err)
	}
	if storedFingerprint != fingerprint {
		return Result{}, false, fmt.Errorf("%w: key %s was first used for a different payment", ErrIdempotencyConflict, key)
	}

	return Result{Transfer: t, Replayed: true, RecoveredFromDatabase: true}, true, nil
}

// cacheCompletion records the result in Redis. It reports whether that
// succeeded, and never returns an error: the money has already moved and
// PostgreSQL holds the key, so a cache write failure cannot fail the payment.
// A later replay recovers from the database instead.
func (s *Service) cacheCompletion(ctx context.Context, key idempotency.Key, fingerprint idempotency.Fingerprint, transferID uuid.UUID) bool {
	if !s.idem.Available() {
		return false
	}

	if err := s.idem.Complete(ctx, key, fingerprint, transferID.String()); err != nil {
		s.log.WarnContext(ctx, "transfer committed but the idempotency result was not cached; a replay will recover it from postgres",
			slog.String("idempotency_key", key.String()),
			slog.String("transfer_id", transferID.String()),
			slog.String("error", err.Error()))
		return false
	}
	return true
}

// releaseClaim drops a claim after a business rejection.
func (s *Service) releaseClaim(ctx context.Context, key idempotency.Key) {
	if !s.idem.Available() {
		return
	}

	if err := s.idem.Release(ctx, key); err != nil {
		// Harmless: the claim expires with its lease.
		s.log.WarnContext(ctx, "releasing idempotency claim failed; it will expire with its lease",
			slog.String("idempotency_key", key.String()),
			slog.String("error", err.Error()))
	}
}

// isDuplicateIdempotencyKey reports whether the error is the deduplication
// barrier firing, as opposed to any other unique violation. Matching on the
// constraint name — not on message text — is what keeps that distinction
// reliable.
func isDuplicateIdempotencyKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == sqlStateUniqueViolation &&
		pgErr.ConstraintName == idempotencyKeyConstraint
}

// GetByIdempotencyKey returns the transfer recorded for a key, if any.
func (s *Service) GetByIdempotencyKey(ctx context.Context, key idempotency.Key) (Transfer, error) {
	const query = `
		SELECT id, source_account_id, destination_account_id,
		       amount_minor, currency, status, created_at, completed_at
		FROM transfers
		WHERE idempotency_key = $1`

	var t Transfer
	err := s.pool.QueryRow(ctx, query, key.String()).Scan(
		&t.ID, &t.SourceAccountID, &t.DestinationAccountID,
		&t.AmountMinor, &t.Currency, &t.Status, &t.CreatedAt, &t.CompletedAt,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Transfer{}, fmt.Errorf("transfer for idempotency key %s: %w", key, ErrNotFound)
	case err != nil:
		return Transfer{}, fmt.Errorf("transfer: get by idempotency key %s: %w", key, err)
	}
	return t, nil
}
