-- Accounts hold the current balance for a single currency.
--
-- Money is stored as BIGINT minor units (cents for USD). Floating point is
-- never used for monetary values: binary floating point cannot represent
-- decimal fractions exactly, so repeated arithmetic drifts. See
-- docs/LEDGER_DESIGN.md.

BEGIN;

CREATE TABLE accounts (
    id            UUID        PRIMARY KEY,

    -- ISO 4217 alphabetic code. VARCHAR(3) rather than CHAR(3): CHAR pads
    -- values to the declared width, which would make 'US ' and 'USD' compare
    -- in surprising ways. The regex forbids empty and lower-case values.
    currency      VARCHAR(3)  NOT NULL
                  CONSTRAINT accounts_currency_format
                  CHECK (currency ~ '^[A-Z]{3}$'),

    -- Balance in minor units. Non-negative: these are normal debit accounts
    -- and may not be overdrawn. This constraint is the database's last line of
    -- defence behind the guarded UPDATE in the transfer posting.
    balance_minor BIGINT      NOT NULL DEFAULT 0
                  CONSTRAINT accounts_balance_non_negative
                  CHECK (balance_minor >= 0),

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- updated_at is maintained by the database, not by callers, so that every
-- writer produces the same value regardless of which code path it took.
CREATE FUNCTION set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

CREATE TRIGGER accounts_set_updated_at
    BEFORE UPDATE ON accounts
    FOR EACH ROW
    EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE  accounts               IS 'Balances by account and currency, in minor units.';
COMMENT ON COLUMN accounts.balance_minor IS 'Current balance in minor units (e.g. cents). Never floating point.';
COMMENT ON COLUMN accounts.currency      IS 'ISO 4217 alphabetic currency code, upper case.';

COMMIT;
