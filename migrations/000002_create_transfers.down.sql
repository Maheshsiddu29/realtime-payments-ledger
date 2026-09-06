BEGIN;

DROP TRIGGER IF EXISTS transfers_completed_are_immutable ON transfers;
DROP TABLE IF EXISTS transfers;
DROP FUNCTION IF EXISTS forbid_completed_transfer_mutation();

COMMIT;
