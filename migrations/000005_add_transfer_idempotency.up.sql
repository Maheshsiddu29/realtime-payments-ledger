-- Idempotency: at most one financial transfer per idempotency key.
--
-- This is the FINAL deduplication barrier, and it lives in PostgreSQL rather
-- than in Redis. Redis coordinates requests and caches results — it is fast,
-- but it can be flushed, can expire a key, and can be unavailable at exactly
-- the wrong moment. A UNIQUE constraint cannot.
--
-- The key is stored on transfers rather than in a separate table on purpose.
-- What has to be unique is "at most one *transfer* per key", and putting the
-- column on the transfer row makes that a single UNIQUE constraint enforced by
-- the same INSERT that creates the transfer. There is no window between
-- reserving a key and creating the transfer it belongs to, and no second table
-- to keep consistent. Recovering the original transfer after a duplicate is a
-- primary-key lookup, not a join.
--
-- The column is nullable, and PostgreSQL's default UNIQUE treats NULLs as
-- distinct, so transfers posted without an idempotency key — the internal
-- posting path, the load generator, everything from Phase 1 and 2 — are
-- unaffected and may exist in any number.

BEGIN;

ALTER TABLE transfers
    ADD COLUMN idempotency_key    TEXT,
    ADD COLUMN request_fingerprint BYTEA;

-- The barrier itself. Named explicitly rather than left to PostgreSQL, because
-- the application matches on the constraint name to tell a duplicate
-- idempotency key apart from any other unique violation.
ALTER TABLE transfers
    ADD CONSTRAINT transfers_idempotency_key_unique UNIQUE (idempotency_key);

ALTER TABLE transfers
    -- Bounded length. Keys are opaque to this system, but unbounded input in
    -- a unique index is a denial-of-service surface.
    ADD CONSTRAINT transfers_idempotency_key_length
        CHECK (idempotency_key IS NULL OR length(idempotency_key) BETWEEN 1 AND 255),

    -- SHA-256 of the canonical request. Exactly 32 bytes when present.
    ADD CONSTRAINT transfers_request_fingerprint_length
        CHECK (request_fingerprint IS NULL OR octet_length(request_fingerprint) = 32),

    -- A key without a fingerprint could not detect a mismatched replay, and a
    -- fingerprint without a key has nothing to identify. Both or neither.
    ADD CONSTRAINT transfers_idempotency_pairing
        CHECK ((idempotency_key IS NULL) = (request_fingerprint IS NULL));

COMMENT ON COLUMN transfers.idempotency_key IS
    'Client-supplied key. UNIQUE: the final barrier against duplicate financial postings. NULL for internally originated transfers.';
COMMENT ON COLUMN transfers.request_fingerprint IS
    'SHA-256 over the canonical financial identity of the request, used to reject reuse of a key for a different payment.';

COMMIT;
