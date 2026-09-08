package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
)

// State is where a request has got to.
//
// There are deliberately only two states. A FAILED state was considered and
// rejected: caching a failure would mean a request refused for insufficient
// funds stays refused even after the account is topped up, which is wrong for
// a payments system. Business rejections are therefore never cached — the
// claim is released and the key becomes available again — while a *completed*
// transfer is cached because it is a fact that cannot change.
type State string

const (
	// StateProcessing means some request holds the claim and is working. The
	// record carries a lease: if the holder dies, it expires.
	StateProcessing State = "PROCESSING"
	// StateCompleted means a transfer exists in PostgreSQL for this key.
	StateCompleted State = "COMPLETED"
)

// Errors surfaced by the store. They are classified rather than raw Redis
// errors so that callers — and, in a later phase, an API boundary — never see
// a driver string.
var (
	// ErrConflict means the key was reused for a materially different
	// payment.
	ErrConflict = errors.New("idempotency: key reused for a different request")
	// ErrInProgress means another request holds the claim and has not
	// finished.
	ErrInProgress = errors.New("idempotency: request already in progress")
	// ErrUnavailable means Redis could not be reached or answered in time.
	// It is informational: the transfer path falls back to PostgreSQL rather
	// than refusing the payment.
	ErrUnavailable = errors.New("idempotency: coordination store unavailable")
)

// Record is what is stored in Redis for one idempotency key.
type Record struct {
	State State `json:"state"`
	// Fingerprint is hex-encoded so the record stays human-readable in
	// redis-cli, which matters when debugging a stuck payment at 3am.
	Fingerprint string `json:"fingerprint"`
	// TransferID is set once the transfer has committed.
	TransferID  string `json:"transfer_id,omitempty"`
	ClaimedAt   string `json:"claimed_at"`
	CompletedAt string `json:"completed_at,omitempty"`
}

// Matches reports whether the record describes the same payment.
func (r Record) Matches(f Fingerprint) bool { return r.Fingerprint == f.String() }

// Store is the Redis-backed coordination store.
//
// A nil *Store is valid and behaves as if Redis were permanently unavailable:
// every operation reports ErrUnavailable and the caller falls back to
// PostgreSQL. That makes "Redis not configured" and "Redis down" the same code
// path, so the fallback is exercised by ordinary unit tests rather than only
// by an outage.
type Store struct {
	client        redis.UniversalClient
	ttl           time.Duration
	processingTTL time.Duration
	// commandTimeout bounds every individual call, so a wedged Redis fails
	// fast instead of stalling a payment.
	commandTimeout time.Duration
}

// NewStore returns a store over an existing client.
func NewStore(client redis.UniversalClient, cfg config.Config) *Store {
	return &Store{
		client:         client,
		ttl:            cfg.Idempotency.TTL,
		processingTTL:  cfg.Idempotency.ProcessingTTL,
		commandTimeout: cfg.Redis.CommandTimeout,
	}
}

// Available reports whether the store has a client at all.
func (s *Store) Available() bool { return s != nil && s.client != nil }

// TTL returns the completed-record lifetime.
func (s *Store) TTL() time.Duration { return s.ttl }

// ProcessingTTL returns the lease held by an in-flight request.
func (s *Store) ProcessingTTL() time.Duration { return s.processingTTL }

// withTimeout bounds a single command without overriding an earlier caller
// deadline.
func (s *Store) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.commandTimeout)
}

// Claim is the outcome of attempting to take ownership of a key.
type Claim struct {
	// Acquired is true when this caller now owns the claim and must go on to
	// post the transfer.
	Acquired bool
	// Existing is the record already present when Acquired is false.
	Existing Record
}

// Acquire atomically takes ownership of a key, or reports what is already
// there.
//
// The claim is a single SET ... NX EX. A GET followed by a SET would be a
// race: two callers could both observe a missing key, both decide they own it,
// and both post a transfer. SET NX decides a winner inside Redis, in one
// round trip, with no lock to release and no Lua required.
//
// The EX lease is what stops a crashed holder from blocking the key forever.
// If the process that claimed it dies, the claim expires after ProcessingTTL
// and another request may take over — and PostgreSQL uniqueness still prevents
// that takeover from producing a second transfer.
func (s *Store) Acquire(ctx context.Context, key Key, f Fingerprint) (Claim, error) {
	if !s.Available() {
		return Claim{}, ErrUnavailable
	}

	record := Record{
		State:       StateProcessing,
		Fingerprint: f.String(),
		ClaimedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return Claim{}, fmt.Errorf("idempotency: encode claim: %w", err)
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	// Bounded loop, not a spin. There is one narrow race: SET NX can fail
	// because a claim exists, and the following GET can find nothing because
	// that claim's lease expired in between. The key is genuinely free at that
	// point, so the correct response is to try to take it — reporting
	// "in progress" would stall a payment on a claim that no longer exists.
	// A handful of attempts is ample: each lost race requires another holder's
	// lease to expire in the microseconds between two commands.
	const maxAttempts = 3

	for attempt := range maxAttempts {
		ok, err := s.client.SetNX(ctx, key.RedisKey(), encoded, s.processingTTL).Result()
		if err != nil {
			return Claim{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		if ok {
			return Claim{Acquired: true}, nil
		}

		// Somebody else owns it. Read what they left.
		existing, found, err := s.get(ctx, key)
		if err != nil {
			return Claim{}, err
		}
		if found {
			return Claim{Existing: existing}, nil
		}

		_ = attempt // the lease expired between the two commands; try again
	}

	// Losing the race this many times in a row means the key is being
	// contended hard. Report it as in progress; the caller retries and
	// PostgreSQL remains the barrier regardless.
	return Claim{}, fmt.Errorf("%w: could not settle the claim for %s after %d attempts",
		ErrInProgress, key, maxAttempts)
}

// Get returns the current record for a key.
func (s *Store) Get(ctx context.Context, key Key) (Record, bool, error) {
	if !s.Available() {
		return Record{}, false, ErrUnavailable
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	return s.get(ctx, key)
}

func (s *Store) get(ctx context.Context, key Key) (Record, bool, error) {
	raw, err := s.client.Get(ctx, key.RedisKey()).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
		return Record{}, false, nil
	case err != nil:
		return Record{}, false, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	var record Record
	if err := json.Unmarshal(raw, &record); err != nil {
		// A corrupt record must not wedge a payment. Report it as absent so
		// the caller proceeds; PostgreSQL still deduplicates.
		return Record{}, false, fmt.Errorf("idempotency: decode record for %s: %w", key, err)
	}
	return record, true, nil
}

// Complete records that a transfer exists for this key.
//
// It overwrites whatever was there, because the caller reaching this point has
// a committed transfer in PostgreSQL — a fact that outranks any claim record.
// The completed record is what lets a later replay be answered without
// touching the database.
//
// A failure here is not fatal to the payment: the money has already moved and
// PostgreSQL holds the key. The caller logs it and carries on, and a
// subsequent replay recovers from the database instead of the cache.
func (s *Store) Complete(ctx context.Context, key Key, f Fingerprint, transferID string) error {
	if !s.Available() {
		return ErrUnavailable
	}

	record := Record{
		State:       StateCompleted,
		Fingerprint: f.String(),
		TransferID:  transferID,
		ClaimedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		CompletedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("idempotency: encode completion: %w", err)
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	if err := s.client.Set(ctx, key.RedisKey(), encoded, s.ttl).Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// Release drops a claim this caller holds, so a rejected request does not keep
// the key locked for the rest of its lease.
//
// It is only ever called after a business rejection, where no transfer was
// created and no money moved. A completed transfer is never released — that
// record is a fact, and dropping it would only cost a database lookup on
// replay, never correctness.
func (s *Store) Release(ctx context.Context, key Key) error {
	if !s.Available() {
		return ErrUnavailable
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	if err := s.client.Del(ctx, key.RedisKey()).Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// Ping reports whether Redis is reachable. It backs the readiness check.
func (s *Store) Ping(ctx context.Context) error {
	if !s.Available() {
		return ErrUnavailable
	}

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()

	if err := s.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}
