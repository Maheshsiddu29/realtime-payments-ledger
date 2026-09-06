BEGIN;

DROP TRIGGER IF EXISTS accounts_set_updated_at ON accounts;
DROP TABLE IF EXISTS accounts;
DROP FUNCTION IF EXISTS set_updated_at();

COMMIT;
