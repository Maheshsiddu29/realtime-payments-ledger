BEGIN;

-- The append-only trigger blocks DELETE, but not DROP TABLE.
DROP TRIGGER IF EXISTS ledger_entries_are_append_only ON ledger_entries;
DROP TABLE IF EXISTS ledger_entries;
DROP FUNCTION IF EXISTS forbid_ledger_entry_mutation();

COMMIT;
