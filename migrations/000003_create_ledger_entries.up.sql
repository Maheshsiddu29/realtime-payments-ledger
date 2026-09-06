-- Ledger entries are the actual movements of money. Each entry is signed:
-- a negative amount is a debit (money leaving an account), a positive amount
-- is a credit (money arriving).
--
--   Alice sends Bob $100.00 USD
--     Alice  -10000 USD   (debit)
--     Bob    +10000 USD   (credit)
--     sum         0       <- the double-entry invariant
--
-- The table is append-only: entries are the audit trail, so correcting a
-- mistake means posting a reversing entry, never editing history.

BEGIN;

CREATE TABLE ledger_entries (
    id           UUID        PRIMARY KEY,

    transfer_id  UUID        NOT NULL REFERENCES transfers (id),
    account_id   UUID        NOT NULL REFERENCES accounts (id),

    -- Signed movement in minor units. Zero is rejected: an entry that moves
    -- nothing is not a movement. Together with the balanced-transfer trigger
    -- in migration 000004, this also guarantees that a balanced transfer has
    -- at least two entries, since a single non-zero entry cannot sum to zero.
    amount_minor BIGINT      NOT NULL
                 CONSTRAINT ledger_entries_amount_non_zero
                 CHECK (amount_minor <> 0),

    currency     VARCHAR(3)  NOT NULL
                 CONSTRAINT ledger_entries_currency_format
                 CHECK (currency ~ '^[A-Z]{3}$'),

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The Phase 1 model is a two-legged transfer: one debit and one credit,
    -- each against a different account. This encodes that a transfer posts at
    -- most one entry per account, which makes a duplicated leg impossible.
    CONSTRAINT ledger_entries_one_leg_per_account UNIQUE (transfer_id, account_id)
);

-- ledger_entries_one_leg_per_account already provides an index led by
-- transfer_id, which serves lookups of all entries for a transfer. A separate
-- transfer_id index would be redundant and is deliberately omitted.

-- Indexes the account_id foreign key, and serves the per-account statement
-- query ("entries for this account, newest first") that later phases add.
CREATE INDEX ledger_entries_account_id_created_at_idx
    ON ledger_entries (account_id, created_at DESC);

-- Append-only enforcement. Posting happens through the transfer operation;
-- nothing may rewrite the audit trail afterwards.
CREATE FUNCTION forbid_ledger_entry_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only: % on entry % is not permitted',
        TG_OP, OLD.id
        USING ERRCODE = 'restrict_violation',
              HINT = 'Post a reversing entry instead of editing history.';
END;
$$;

CREATE TRIGGER ledger_entries_are_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW
    EXECUTE FUNCTION forbid_ledger_entry_mutation();

COMMENT ON TABLE  ledger_entries              IS 'Append-only signed money movements. Debits are negative, credits positive.';
COMMENT ON COLUMN ledger_entries.amount_minor IS 'Signed minor units. Negative = debit, positive = credit. Never zero.';

COMMIT;
