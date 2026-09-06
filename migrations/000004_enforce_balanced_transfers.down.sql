BEGIN;

DROP TRIGGER IF EXISTS transfers_completed_must_balance ON transfers;
DROP TRIGGER IF EXISTS ledger_entries_transfer_must_balance ON ledger_entries;

DROP FUNCTION IF EXISTS transfers_assert_balanced();
DROP FUNCTION IF EXISTS ledger_entries_assert_balanced();
DROP FUNCTION IF EXISTS assert_transfer_balanced(UUID);

COMMIT;
