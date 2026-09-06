-- Enforcement of the double-entry invariant:
--
--     for every transfer, SUM(ledger_entries.amount_minor) = 0
--
-- A plain CHECK constraint cannot express this. CHECK is evaluated per row
-- against that row's own values only; it cannot aggregate across rows, and
-- PostgreSQL rejects subqueries inside CHECK precisely because the result
-- would not be re-validated when other rows change.
--
-- The correct tool is a DEFERRABLE INITIALLY DEFERRED constraint trigger.
-- Deferred means the check runs at COMMIT rather than at statement time, so
-- the debit and the credit may be inserted one after the other inside a
-- transaction without the first insert tripping the check. If the pair does
-- not balance by the time the transaction commits, the COMMIT itself fails and
-- the whole transaction is rolled back.
--
-- This is genuine database-level enforcement: it holds for psql sessions,
-- future services and buggy application code alike, not just for the Go code
-- in this repository. See docs/LEDGER_DESIGN.md for the full statement of what
-- is and is not guaranteed.

BEGIN;

-- Shared assertion used by both triggers below.
CREATE FUNCTION assert_transfer_balanced(p_transfer_id UUID) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    v_status         TEXT;
    v_currency       VARCHAR(3);
    v_entry_count    INTEGER;
    v_imbalance      BIGINT;
    v_wrong_currency INTEGER;
BEGIN
    SELECT t.status, t.currency
      INTO v_status, v_currency
      FROM transfers t
     WHERE t.id = p_transfer_id;

    -- No transfer row: the foreign key on ledger_entries has already rejected
    -- this transaction, so there is nothing further to say.
    IF NOT FOUND THEN
        RETURN;
    END IF;

    SELECT COUNT(*),
           COALESCE(SUM(e.amount_minor), 0),
           COUNT(*) FILTER (WHERE e.currency <> v_currency)
      INTO v_entry_count, v_imbalance, v_wrong_currency
      FROM ledger_entries e
     WHERE e.transfer_id = p_transfer_id;

    -- A completed transfer that moved no money is a lie in the books.
    IF v_status = 'completed' AND v_entry_count = 0 THEN
        RAISE EXCEPTION
            'transfer % is completed but has no ledger entries', p_transfer_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;

    -- A transfer that has not posted anything yet is legitimately empty.
    IF v_entry_count = 0 THEN
        RETURN;
    END IF;

    IF v_imbalance <> 0 THEN
        RAISE EXCEPTION
            'ledger entries for transfer % do not balance: sum = %, expected 0',
            p_transfer_id, v_imbalance
            USING ERRCODE = 'integrity_constraint_violation',
                  HINT = 'Every posting must debit one account and credit another by the same amount.';
    END IF;

    IF v_wrong_currency > 0 THEN
        RAISE EXCEPTION
            'transfer % has % ledger entries in a currency other than %',
            p_transfer_id, v_wrong_currency, v_currency
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
END;
$$;

COMMENT ON FUNCTION assert_transfer_balanced(UUID) IS
    'Raises unless the ledger entries for the transfer sum to zero and share the transfer currency.';

-- Catches the case where entries are posted at all: any transfer that has
-- ledger entries must have balanced ones by commit time.
CREATE FUNCTION ledger_entries_assert_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM assert_transfer_balanced(NEW.transfer_id);
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_entries_transfer_must_balance
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION ledger_entries_assert_balanced();

-- Catches the opposite case: a transfer marked completed without any entries
-- at all, which the trigger above would never see because it only fires on
-- inserts into ledger_entries.
CREATE FUNCTION transfers_assert_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'completed' THEN
        PERFORM assert_transfer_balanced(NEW.id);
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER transfers_completed_must_balance
    AFTER INSERT OR UPDATE ON transfers
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION transfers_assert_balanced();

COMMIT;
