-- SQLite mirror for Expenses Plus. Business dates remain YYYY-MM-DD text and money integer cents.
BEGIN;

CREATE TABLE cash_movements (
 id TEXT PRIMARY KEY, gym_id TEXT NOT NULL REFERENCES gyms(id), version INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, deleted_at INTEGER, synced_at INTEGER,
 movement_on TEXT NOT NULL, amount INTEGER NOT NULL CHECK(amount>0),
 movement_type TEXT NOT NULL CHECK(movement_type IN ('cash_in','cash_out')),
 reason TEXT NOT NULL CHECK(length(reason) BETWEEN 1 AND 200), operator_id TEXT NOT NULL REFERENCES users(id),
 expense_id TEXT UNIQUE, classification_status TEXT NOT NULL DEFAULT 'unclassified'
 CHECK(classification_status IN ('unclassified','expense','non_operating'))
);
CREATE TABLE recurring_expense_templates (
 id TEXT PRIMARY KEY, gym_id TEXT NOT NULL REFERENCES gyms(id), version INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, deleted_at INTEGER, synced_at INTEGER,
 name TEXT NOT NULL CHECK(length(name) BETWEEN 1 AND 120), payee_name TEXT, category TEXT NOT NULL CHECK(category IN ('renta','servicios','nomina','mantenimiento','marketing','insumos_no_inventariables','impuestos_y_permisos','otros')),
 expected_amount INTEGER NOT NULL CHECK(expected_amount>0), usual_payment_method TEXT NOT NULL CHECK(usual_payment_method IN ('cash','transfer','card')),
 classification TEXT NOT NULL CHECK(classification IN ('fixed','variable')), frequency TEXT NOT NULL CHECK(frequency IN
 ('weekly','every_14_days','semimonthly','monthly','bimonthly','quarterly','semiannual','annual')),
 starts_on TEXT NOT NULL, ends_on TEXT, next_due_on TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1,
 created_by TEXT NOT NULL REFERENCES users(id), CHECK(ends_on IS NULL OR ends_on>=starts_on)
);
CREATE TABLE expense_occurrences (
 id TEXT PRIMARY KEY, gym_id TEXT NOT NULL REFERENCES gyms(id), version INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, deleted_at INTEGER, synced_at INTEGER,
 template_id TEXT NOT NULL REFERENCES recurring_expense_templates(id), due_on TEXT NOT NULL,
 expected_amount INTEGER NOT NULL CHECK(expected_amount>0), category TEXT NOT NULL CHECK(category IN ('renta','servicios','nomina','mantenimiento','marketing','insumos_no_inventariables','impuestos_y_permisos','otros')), payee_name TEXT,
 payment_method TEXT NOT NULL CHECK(payment_method IN ('cash','transfer','card')),
 classification TEXT NOT NULL CHECK(classification IN ('fixed','variable')),
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','paid','skipped')),
 expense_id TEXT UNIQUE, resolved_by TEXT REFERENCES users(id), resolved_at INTEGER, skip_reason TEXT,
 UNIQUE(gym_id,template_id,due_on)
);
CREATE TABLE expenses_plus_new (
 id TEXT PRIMARY KEY, gym_id TEXT NOT NULL REFERENCES gyms(id), version INTEGER NOT NULL DEFAULT 1,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, deleted_at INTEGER, synced_at INTEGER,
 expense_date TEXT NOT NULL, amount INTEGER NOT NULL CHECK(amount>0),
 category TEXT NOT NULL CHECK(category IN ('renta','servicios','nomina','mantenimiento','marketing','insumos_no_inventariables','impuestos_y_permisos','otros')),
 description TEXT, payment_method TEXT NOT NULL CHECK(payment_method IN ('cash','transfer','card')),
 created_by TEXT NOT NULL REFERENCES users(id), payee_name TEXT, reference TEXT,
 classification TEXT NOT NULL DEFAULT 'variable' CHECK(classification IN ('fixed','variable')),
 source TEXT NOT NULL DEFAULT 'manual' CHECK(source IN ('manual','recurring','cash_movement')),
 recurring_occurrence_id TEXT UNIQUE REFERENCES expense_occurrences(id),
 cash_movement_id TEXT UNIQUE REFERENCES cash_movements(id),
 CHECK(description IS NULL OR length(description)<=200), CHECK(payee_name IS NULL OR length(payee_name)<=120),
 CHECK(reference IS NULL OR length(reference)<=120)
);
INSERT INTO expenses_plus_new(id,gym_id,version,created_at,updated_at,deleted_at,synced_at,expense_date,amount,category,description,payment_method,created_by)
 SELECT id,gym_id,version,created_at,updated_at,deleted_at,synced_at,expense_date,amount,
 CASE category WHEN 'sueldos' THEN 'nomina' WHEN 'mercaderia_externa' THEN 'insumos_no_inventariables' ELSE category END,
 description,payment_method,created_by FROM expenses;
DROP TABLE expenses;
ALTER TABLE expenses_plus_new RENAME TO expenses;
UPDATE sync_queue SET payload=json_set(payload,
 '$.category',CASE json_extract(payload,'$.category') WHEN 'sueldos' THEN 'nomina' WHEN 'mercaderia_externa' THEN 'insumos_no_inventariables' ELSE json_extract(payload,'$.category') END,
 '$.classification','variable','$.source','manual')
WHERE entity_type='expenses' AND synced_at IS NULL AND json_valid(payload);
CREATE INDEX idx_expenses_gym_date ON expenses(gym_id,expense_date DESC) WHERE deleted_at IS NULL;
CREATE INDEX idx_expenses_gym_category ON expenses(gym_id,category) WHERE deleted_at IS NULL;
CREATE INDEX idx_expenses_sync ON expenses(gym_id,updated_at);
CREATE INDEX idx_cash_movements_gym_day ON cash_movements(gym_id,movement_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_occurrences_gym_due ON expense_occurrences(gym_id,status,due_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_templates_gym_active ON recurring_expense_templates(gym_id,active,next_due_on,id) WHERE deleted_at IS NULL;
INSERT INTO _migrations(version,name,applied_at)
 SELECT 32,'032_expenses_plus',CAST(strftime('%s','now') AS INTEGER)*1000
 WHERE NOT EXISTS(SELECT 1 FROM _migrations WHERE version=32);
COMMIT;
