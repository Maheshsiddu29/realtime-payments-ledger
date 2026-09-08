//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/config"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/idempotency"
	"github.com/Maheshsiddu29/realtime-payments-ledger/internal/transfer"
)

func discardLog() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// serviceWithoutRedis returns a transfer service that has no coordination
// store at all.
//
// This is the deterministic stand-in for "the process committed to PostgreSQL
// and then died before writing to Redis": the transfer and its key are
// durable, and Redis knows nothing about it. It is also exactly the state
// after a Redis write failure, so one fixture covers both.
func serviceWithoutRedis(e *env) *transfer.Service {
	return transfer.NewService(e.pool, discardLog())
}

// serviceWithDeadRedis returns a service whose coordination store points at a
// port where nothing is listening, so every Redis call fails immediately.
func serviceWithDeadRedis(e *env) *transfer.Service {
	cfg := sharedConfig
	cfg.Redis.Addr = "127.0.0.1:1"
	cfg.Redis.DialTimeout = 250 * time.Millisecond
	cfg.Redis.CommandTimeout = 250 * time.Millisecond

	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Redis.Addr,
		DialTimeout:  cfg.Redis.DialTimeout,
		ReadTimeout:  cfg.Redis.CommandTimeout,
		WriteTimeout: cfg.Redis.CommandTimeout,
	})
	return transfer.NewService(e.pool, discardLog()).
		WithIdempotency(idempotency.NewStore(client, cfg))
}

// serviceWithConfig returns a service using the real Redis with an adjusted
// configuration, for tests that need a short lease.
func serviceWithConfig(e *env, cfg config.Config) *transfer.Service {
	return transfer.NewService(e.pool, discardLog()).
		WithIdempotency(idempotency.NewStore(sharedRedis, cfg))
}

// ---------------------------------------------------------------------------
// Redis unavailable
// ---------------------------------------------------------------------------

// Redis down before the transfer starts. The chosen policy is a PostgreSQL
// fallback rather than refusing the payment: the UNIQUE constraint provides
// the full deduplication guarantee without Redis, so refusing would trade
// availability for nothing.
//
// The request must still be deduplicated — that is the part being proven here.
func TestRedisUnavailableBeforeTransferFallsBackToPostgres(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 50_000)
	bob := newFundedAccount(t, e, USD, 0)

	svc := serviceWithDeadRedis(e)
	k := key(t, "pay_redis_down")
	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 1_000, Currency: USD,
	}

	first, err := svc.PostIdempotent(ctx, k, cmd)
	if err != nil {
		t.Fatalf("first request with redis down: %v", err)
	}
	if !first.CoordinationDegraded {
		t.Error("the result does not report degraded coordination")
	}

	// The crucial assertion: deduplication still holds with Redis gone.
	second, err := svc.PostIdempotent(ctx, k, cmd)
	if err != nil {
		t.Fatalf("retry with redis down: %v", err)
	}
	if second.Transfer.ID != first.Transfer.ID {
		t.Errorf("retry returned transfer %s, want the original %s", second.Transfer.ID, first.Transfer.ID)
	}
	if !second.RecoveredFromDatabase {
		t.Error("the retry did not report recovery from postgres")
	}

	if got := countTransfersForKey(t, e, k); got != 1 {
		t.Errorf("transfer rows = %d, want 1 with redis unavailable", got)
	}
	if got, want := balanceOf(t, e, alice.ID), int64(49_000); got != want {
		t.Errorf("source balance = %d, want %d", got, want)
	}
	assertCompletedTransferCount(t, e, 1)
	assertReconciled(t, e)
}

// A conflicting request must still be rejected while Redis is down, because
// the fingerprint is stored in PostgreSQL as well.
func TestRedisUnavailableStillDetectsConflict(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 50_000)
	bob := newFundedAccount(t, e, USD, 0)

	svc := serviceWithDeadRedis(e)
	k := key(t, "pay_conflict_no_redis")
	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 1_000, Currency: USD,
	}

	if _, err := svc.PostIdempotent(ctx, k, cmd); err != nil {
		t.Fatalf("first: %v", err)
	}

	conflicting := cmd
	conflicting.AmountMinor = 9_000

	if _, err := svc.PostIdempotent(ctx, k, conflicting); !errors.Is(err, transfer.ErrIdempotencyConflict) {
		t.Fatalf("conflicting request = %v, want ErrIdempotencyConflict", err)
	}
	if got := countTransfersForKey(t, e, k); got != 1 {
		t.Errorf("transfer rows = %d, want 1", got)
	}
	assertReconciled(t, e)
}

// ---------------------------------------------------------------------------
// Redis loses the completed record
// ---------------------------------------------------------------------------

// Redis is flushed, a key expires, or the cache is simply gone. PostgreSQL
// must still recognise the key and return the original transfer. This is what
// makes Redis an optimisation rather than the correctness boundary.
func TestRedisRecordLostStillRecoversFromPostgres(t *testing.T) {
	scenarios := []struct {
		name  string
		clear func(t *testing.T, e *env, k idempotency.Key)
	}{
		{
			name: "key deleted",
			clear: func(t *testing.T, e *env, k idempotency.Key) {
				if err := e.redis.Del(testContext(t), k.RedisKey()).Err(); err != nil {
					t.Fatalf("deleting the redis key: %v", err)
				}
			},
		},
		{
			name:  "whole database flushed",
			clear: func(t *testing.T, e *env, _ idempotency.Key) { flushRedis(t) },
		},
		{
			name: "key expired",
			clear: func(t *testing.T, e *env, k idempotency.Key) {
				// Expire immediately rather than waiting out a TTL.
				if err := e.redis.Expire(testContext(t), k.RedisKey(), time.Nanosecond).Err(); err != nil {
					t.Fatalf("expiring the redis key: %v", err)
				}
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					n, err := e.redis.Exists(testContext(t), k.RedisKey()).Result()
					if err != nil {
						t.Fatalf("checking expiry: %v", err)
					}
					if n == 0 {
						return
					}
				}
				t.Fatal("the redis key never expired")
			},
		},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			ctx := testContext(t)
			e := newEnv(t)

			alice := newFundedAccount(t, e, USD, 50_000)
			bob := newFundedAccount(t, e, USD, 0)

			k := key(t, "pay_cache_loss")
			cmd := transfer.Command{
				SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
				AmountMinor: 2_000, Currency: USD,
			}

			original, err := e.transfers.PostIdempotent(ctx, k, cmd)
			if err != nil {
				t.Fatalf("original: %v", err)
			}

			sc.clear(t, e, k)

			replay, err := e.transfers.PostIdempotent(ctx, k, cmd)
			if err != nil {
				t.Fatalf("replay after cache loss: %v", err)
			}
			if replay.Transfer.ID != original.Transfer.ID {
				t.Errorf("replay returned %s, want the original %s", replay.Transfer.ID, original.Transfer.ID)
			}
			if !replay.RecoveredFromDatabase {
				t.Error("the replay did not come from the postgres barrier")
			}

			if got := countTransfersForKey(t, e, k); got != 1 {
				t.Errorf("transfer rows = %d, want 1: redis expiry created a duplicate", got)
			}
			if got, want := balanceOf(t, e, alice.ID), int64(48_000); got != want {
				t.Errorf("source balance = %d, want %d", got, want)
			}
			assertCompletedTransferCount(t, e, 1)
			assertReconciled(t, e)
		})
	}
}

// ---------------------------------------------------------------------------
// Crash between COMMIT and the Redis update
// ---------------------------------------------------------------------------

// The transfer committed and the process died before Redis was told. Posting
// through a service with no coordination store reproduces that state exactly:
// the transfer and its key are durable, Redis knows nothing.
//
// The retry must find the existing transfer rather than creating a second one.
func TestCrashAfterCommitBeforeRedisUpdateRecovers(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 50_000)
	bob := newFundedAccount(t, e, USD, 0)

	k := key(t, "pay_crash_after_commit")
	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 4_000, Currency: USD,
	}

	// The doomed process: commits, then dies.
	crashed := serviceWithoutRedis(e)
	original, err := crashed.PostIdempotent(ctx, k, cmd)
	if err != nil {
		t.Fatalf("post before the simulated crash: %v", err)
	}

	// Redis must genuinely know nothing about it.
	if n, err := e.redis.Exists(ctx, k.RedisKey()).Result(); err != nil {
		t.Fatalf("checking redis: %v", err)
	} else if n != 0 {
		t.Fatalf("redis holds a record for %s; the crash was not simulated faithfully", k)
	}

	// A fresh process handles the client's retry.
	replay, err := e.transfers.PostIdempotent(ctx, k, cmd)
	if err != nil {
		t.Fatalf("retry after the simulated crash: %v", err)
	}
	if replay.Transfer.ID != original.Transfer.ID {
		t.Errorf("retry returned %s, want the original %s", replay.Transfer.ID, original.Transfer.ID)
	}
	if !replay.RecoveredFromDatabase {
		t.Error("the retry did not report recovery from postgres")
	}

	if got := countTransfersForKey(t, e, k); got != 1 {
		t.Errorf("transfer rows = %d, want 1", got)
	}
	if got, want := balanceOf(t, e, alice.ID), int64(46_000); got != want {
		t.Errorf("source balance = %d, want %d: the payment was applied twice", got, want)
	}
	if got := countRows(t, e, "ledger_entries"); got != 2 {
		t.Errorf("ledger entries = %d, want 2", got)
	}
	assertCompletedTransferCount(t, e, 1)
	assertReconciled(t, e)

	// The recovery should have repaired the cache, so the next replay is
	// answered without the database lookup.
	record, err := e.redis.Get(ctx, k.RedisKey()).Bytes()
	if err != nil {
		t.Fatalf("redis was not repaired after recovery: %v", err)
	}
	var decoded idempotency.Record
	if err := json.Unmarshal(record, &decoded); err != nil {
		t.Fatalf("decoding the repaired record: %v", err)
	}
	if decoded.State != idempotency.StateCompleted {
		t.Errorf("repaired record state = %q, want %q", decoded.State, idempotency.StateCompleted)
	}
	if decoded.TransferID != original.Transfer.ID.String() {
		t.Errorf("repaired record points at %s, want %s", decoded.TransferID, original.Transfer.ID)
	}
}

// ---------------------------------------------------------------------------
// Stale PROCESSING claim
// ---------------------------------------------------------------------------

// A claim whose holder died, with the transfer already committed. The retry
// must resolve against PostgreSQL rather than reporting the payment as
// perpetually in progress.
func TestStaleProcessingClaimResolvesAgainstPostgres(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 50_000)
	bob := newFundedAccount(t, e, USD, 0)

	k := key(t, "pay_stale_claim")
	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 1_250, Currency: USD,
	}

	// Commit the transfer without touching Redis, then plant the stale claim
	// the dead holder would have left behind.
	original, err := serviceWithoutRedis(e).PostIdempotent(ctx, k, cmd)
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	fingerprint := idempotency.Request{
		SourceAccountID:      cmd.SourceAccountID,
		DestinationAccountID: cmd.DestinationAccountID,
		AmountMinor:          cmd.AmountMinor,
		Currency:             cmd.Currency.String(),
	}.Fingerprint()

	stale, err := json.Marshal(idempotency.Record{
		State:       idempotency.StateProcessing,
		Fingerprint: fingerprint.String(),
		ClaimedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("encoding the stale claim: %v", err)
	}
	if err := e.redis.Set(ctx, k.RedisKey(), stale, time.Hour).Err(); err != nil {
		t.Fatalf("planting the stale claim: %v", err)
	}

	replay, err := e.transfers.PostIdempotent(ctx, k, cmd)
	if err != nil {
		t.Fatalf("retry against a stale PROCESSING claim: %v", err)
	}
	if replay.Transfer.ID != original.Transfer.ID {
		t.Errorf("retry returned %s, want %s", replay.Transfer.ID, original.Transfer.ID)
	}
	if got := countTransfersForKey(t, e, k); got != 1 {
		t.Errorf("transfer rows = %d, want 1", got)
	}
	assertReconciled(t, e)
}

// A claim whose holder died before committing anything must not block the key
// forever. The lease expires and a later request can take it.
func TestStaleProcessingClaimExpiresAndAllowsProgress(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 50_000)
	bob := newFundedAccount(t, e, USD, 0)

	// A deliberately tiny lease, so the expiry is observable without a long
	// wait. The production default is 30s.
	cfg := sharedConfig
	cfg.Idempotency.ProcessingTTL = 300 * time.Millisecond
	svc := serviceWithConfig(e, cfg)

	k := key(t, "pay_abandoned_claim")
	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 750, Currency: USD,
	}
	fingerprint := idempotency.Request{
		SourceAccountID:      cmd.SourceAccountID,
		DestinationAccountID: cmd.DestinationAccountID,
		AmountMinor:          cmd.AmountMinor,
		Currency:             cmd.Currency.String(),
	}.Fingerprint()

	// The abandoned claim: PROCESSING, nothing committed.
	abandoned, err := json.Marshal(idempotency.Record{
		State:       idempotency.StateProcessing,
		Fingerprint: fingerprint.String(),
		ClaimedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if err := e.redis.Set(ctx, k.RedisKey(), abandoned, cfg.Idempotency.ProcessingTTL).Err(); err != nil {
		t.Fatalf("planting the abandoned claim: %v", err)
	}

	// While the lease is held, the payment is reported in progress rather than
	// executed twice.
	if _, err := svc.PostIdempotent(ctx, k, cmd); !errors.Is(err, transfer.ErrIdempotencyInProgress) {
		t.Fatalf("while the claim is held = %v, want ErrIdempotencyInProgress", err)
	}
	if got := countTransfersForKey(t, e, k); got != 0 {
		t.Fatalf("transfer rows = %d, want 0 while the claim is held", got)
	}

	// Once the lease expires the key is claimable again.
	deadline := time.Now().Add(10 * time.Second)
	var result transfer.Result
	for {
		result, err = svc.PostIdempotent(ctx, k, cmd)
		if err == nil {
			break
		}
		if !errors.Is(err, transfer.ErrIdempotencyInProgress) {
			t.Fatalf("post after lease expiry: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the abandoned claim never expired; a dead holder blocks the key permanently")
		}
	}

	if result.Transfer.ID == uuid.Nil {
		t.Error("no transfer was returned after the lease expired")
	}
	if got := countTransfersForKey(t, e, k); got != 1 {
		t.Errorf("transfer rows = %d, want exactly 1", got)
	}
	assertCompletedTransferCount(t, e, 1)
	assertReconciled(t, e)
}

// ---------------------------------------------------------------------------
// PostgreSQL uniqueness with Redis bypassed entirely
// ---------------------------------------------------------------------------

// Redis coordination removed altogether, two requests racing for the same key.
// The UNIQUE constraint alone must decide: one inserts, the other recovers the
// original. This is the proof that Redis is not what makes deduplication work.
func TestPostgresUniquenessAloneDeduplicatesConcurrentRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext(t), 60*time.Second)
	defer cancel()

	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 100_000)
	bob := newFundedAccount(t, e, USD, 0)

	const callers = 8
	k := key(t, "pay_no_redis_race")
	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 5_000, Currency: USD,
	}

	// No coordination store at all: every caller goes straight to PostgreSQL.
	svc := serviceWithoutRedis(e)

	type outcome struct {
		result transfer.Result
		err    error
	}
	outcomes := make([]outcome, callers)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := svc.PostIdempotent(ctx, k, cmd)
			outcomes[i] = outcome{result: res, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var (
		created, recoveredCount, inProgress int
		ids                                 = map[uuid.UUID]struct{}{}
	)
	for _, o := range outcomes {
		switch {
		case o.err == nil:
			ids[o.result.Transfer.ID] = struct{}{}
			if o.result.RecoveredFromDatabase {
				recoveredCount++
			} else {
				created++
			}
		case errors.Is(o.err, transfer.ErrIdempotencyInProgress):
			inProgress++
		default:
			t.Errorf("unexpected failure: %v", o.err)
		}
	}

	t.Logf("callers=%d created=%d recovered=%d in_progress=%d distinct_ids=%d",
		callers, created, recoveredCount, inProgress, len(ids))

	if created != 1 {
		t.Errorf("callers that created a transfer = %d, want exactly 1", created)
	}
	if len(ids) != 1 {
		t.Errorf("distinct transfer IDs = %d, want exactly 1", len(ids))
	}
	if got := countTransfersForKey(t, e, k); got != 1 {
		t.Errorf("transfer rows = %d, want 1 with redis bypassed entirely", got)
	}
	if got, want := balanceOf(t, e, alice.ID), int64(95_000); got != want {
		t.Errorf("source balance = %d, want %d", got, want)
	}
	if got := countRows(t, e, "ledger_entries"); got != 2 {
		t.Errorf("ledger entries = %d, want 2", got)
	}
	assertCompletedTransferCount(t, e, 1)
	assertNoNegativeBalances(t, e)
	assertReconciled(t, e)
}

// ---------------------------------------------------------------------------
// Rejections and validation
// ---------------------------------------------------------------------------

// A business rejection must not be cached: the account may be funded later,
// and the same key should then be able to succeed.
func TestRejectedRequestReleasesTheKeyForRetry(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 100)
	bob := newFundedAccount(t, e, USD, 0)

	k := key(t, "pay_underfunded")
	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 5_000, Currency: USD,
	}

	if _, err := e.transfers.PostIdempotent(ctx, k, cmd); !errors.Is(err, transfer.ErrInsufficientFunds) {
		t.Fatalf("underfunded request = %v, want ErrInsufficientFunds", err)
	}
	if got := countTransfersForKey(t, e, k); got != 0 {
		t.Fatalf("transfer rows = %d, want 0 after a rejection", got)
	}

	// The money arrives, and the same key is retried.
	fund(t, e, alice.ID, 10_000)

	result, err := e.transfers.PostIdempotent(ctx, k, cmd)
	if err != nil {
		t.Fatalf("retry after funding: %v", err)
	}
	if result.Replayed {
		t.Error("the retry was treated as a replay; the rejection was cached")
	}
	if got := countTransfersForKey(t, e, k); got != 1 {
		t.Errorf("transfer rows = %d, want 1", got)
	}
	assertReconciled(t, e)
}

func TestInvalidIdempotencyKeysAreRejected(t *testing.T) {
	ctx := testContext(t)
	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 10_000)
	bob := newFundedAccount(t, e, USD, 0)

	cmd := transfer.Command{
		SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
		AmountMinor: 100, Currency: USD,
	}

	for _, bad := range []idempotency.Key{"", "with space", "line\nbreak"} {
		t.Run(string(bad), func(t *testing.T) {
			_, err := e.transfers.PostIdempotent(ctx, bad, cmd)
			if !errors.Is(err, idempotency.ErrInvalidKey) {
				t.Errorf("PostIdempotent(%q) = %v, want ErrInvalidKey", bad, err)
			}
		})
	}

	if n := countRows(t, e, "transfers"); n != 0 {
		t.Errorf("transfers table has %d rows after only invalid keys, want 0", n)
	}
}

// Phase 2 concurrency protection must be untouched by idempotency: distinct
// keys are distinct payments, and they must still not overspend the source.
func TestIdempotentPostsStillCannotOverspend(t *testing.T) {
	ctx, cancel := context.WithTimeout(testContext(t), 60*time.Second)
	defer cancel()

	e := newEnv(t)

	alice := newFundedAccount(t, e, USD, 1_000)
	bob := newFundedAccount(t, e, USD, 0)

	const (
		callers = 20
		amount  = 100
	)

	start := make(chan struct{})
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			k := key(t, "pay_overspend_"+uuid.New().String())
			_, err := e.transfers.PostIdempotent(ctx, k, transfer.Command{
				SourceAccountID: alice.ID, DestinationAccountID: bob.ID,
				AmountMinor: amount, Currency: USD,
			})
			if err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	t.Logf("callers=%d succeeded=%d", callers, succeeded)

	if succeeded != 10 {
		t.Errorf("successful transfers = %d, want exactly 10 (only ten are affordable)", succeeded)
	}
	if got := balanceOf(t, e, alice.ID); got != 0 {
		t.Errorf("source balance = %d, want 0", got)
	}
	if got, want := totalBalances(t, e), int64(1_000); got != want {
		t.Errorf("total balances = %d, want %d", got, want)
	}
	assertNoNegativeBalances(t, e)
	assertReconciled(t, e)
}
