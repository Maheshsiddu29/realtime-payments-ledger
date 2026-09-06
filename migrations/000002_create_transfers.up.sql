-- A transfer is the intent to move money between two accounts, plus the
-- record of whether that intent was carried out.
--
-- The ledger entries that actually move the money live in ledger_entries and
-- are written in the same database transaction that completes the transfer.

BEGIN;

CREATE TABLE transfers (
    id                     UUID        PRIMARY KEY,

    source_account_id      UUID        NOT NULL REFERENCES accounts (id),
    destination_account_id UUID        NOT NULL REFERENCES accounts (id),

    -- Strictly positive: direction is carried by the source/destination
    -- columns, not by the sign. Signed values belong in ledger_entries.
    amount_minor           BIGINT      NOT NULL
                           CONSTRAINT transfers_amount_positive
                           CHECK (amount_minor > 0),

    currency               VARCHAR(3)  NOT NULL
                           CONSTRAINT transfers_currency_format
                           CHECK (currency ~ '^[A-Z]{3}$'),

    -- Explicit, closed set of states. TEXT with a CHECK rather than an ENUM:
    -- adding a state later is an ordinary migration instead of an ALTER TYPE,
    -- which cannot run inside a transaction block on older servers.
    status                 TEXT        NOT NULL DEFAULT 'pending'
                           CONSTRAINT transfers_status_valid
                           CHECK (status IN ('pending', 'completed', 'failed')),

    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at           TIMESTAMPTZ,

    -- Self-transfers are meaningless and would let a caller construct a
    -- balanced pair of entries that moves nothing.
    CONSTRAINT transfers_distinct_accounts
        CHECK (source_account_id <> destination_account_id),

    -- completed_at is set if and only if the transfer completed, so the two
    -- columns can never disagree.
    CONSTRAINT transfers_completed_at_matches_status
        CHECK (
            (status = 'completed' AND completed_at IS NOT NULL)
            OR
            (status <> 'completed' AND completed_at IS NULL)
        )
);

-- PostgreSQL does not index foreign key columns automatically. These indexes
-- serve both the referential integrity checks and the per-account transfer
-- lookups the API will need.
CREATE INDEX transfers_source_account_id_idx      ON transfers (source_account_id);
CREATE INDEX transfers_destination_account_id_idx ON transfers (destination_account_id);

-- A completed transfer is a financial record: it must never be edited or
-- deleted afterwards. Enforced in the database so that this holds for psql
-- sessions and future services too, not only for the Go code.
CREATE FUNCTION forbid_completed_transfer_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'transfer % is completed and cannot be deleted', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    RAISE EXCEPTION 'transfer % is completed and cannot be modified', OLD.id
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER transfers_completed_are_immutable
    BEFORE UPDATE OR DELETE ON transfers
    FOR EACH ROW
    WHEN (OLD.status = 'completed')
    EXECUTE FUNCTION forbid_completed_transfer_mutation();

COMMENT ON TABLE  transfers              IS 'Intent to move money between two accounts, and its outcome.';
COMMENT ON COLUMN transfers.amount_minor IS 'Always positive; direction comes from the account columns.';
COMMENT ON COLUMN transfers.status       IS 'pending -> completed, or pending -> failed. Completed rows are immutable.';

COMMIT;
