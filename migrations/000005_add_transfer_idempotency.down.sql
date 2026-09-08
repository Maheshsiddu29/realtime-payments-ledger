BEGIN;

ALTER TABLE transfers
    DROP CONSTRAINT IF EXISTS transfers_idempotency_pairing,
    DROP CONSTRAINT IF EXISTS transfers_request_fingerprint_length,
    DROP CONSTRAINT IF EXISTS transfers_idempotency_key_length,
    DROP CONSTRAINT IF EXISTS transfers_idempotency_key_unique;

ALTER TABLE transfers
    DROP COLUMN IF EXISTS request_fingerprint,
    DROP COLUMN IF EXISTS idempotency_key;

COMMIT;
