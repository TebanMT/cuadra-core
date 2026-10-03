-- Pair of PG 047. The affected drift existed only in PostgreSQL databases
-- that had recorded evolving PG 039/040 files. SQLite already reconciles
-- these exact columns, triggers and constraints in ReconcileSQLiteSchema
-- after migrations; clean installs also received them in 034/035.
BEGIN;
INSERT INTO _migrations(version,name,applied_at)
SELECT 42,'042_repair_financial_schema_drift',CAST(strftime('%s','now') AS INTEGER)*1000
WHERE NOT EXISTS(SELECT 1 FROM _migrations WHERE version=42);
COMMIT;
