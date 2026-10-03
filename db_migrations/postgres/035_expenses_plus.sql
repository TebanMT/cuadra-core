-- Expenses Plus: paid expenses, physical cash movements and deterministic dues.
BEGIN;

ALTER TABLE expenses DROP CONSTRAINT IF EXISTS chk_expenses_category;
UPDATE expenses SET category='nomina' WHERE category='sueldos';
UPDATE expenses SET category='insumos_no_inventariables' WHERE category='mercaderia_externa';
UPDATE sync_entities SET payload = jsonb_set(
 jsonb_set(jsonb_set(payload,'{category}',to_jsonb(CASE payload->>'category' WHEN 'sueldos' THEN 'nomina' WHEN 'mercaderia_externa' THEN 'insumos_no_inventariables' ELSE payload->>'category' END),false),
 '{classification}','"variable"'::jsonb,true),'{source}','"manual"'::jsonb,true)
WHERE entity_type='expenses';
ALTER TABLE expenses
  ADD COLUMN IF NOT EXISTS payee_name TEXT,
  ADD COLUMN IF NOT EXISTS reference TEXT,
  ADD COLUMN IF NOT EXISTS classification TEXT NOT NULL DEFAULT 'variable',
  ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'manual',
  ADD COLUMN IF NOT EXISTS recurring_occurrence_id UUID,
  ADD COLUMN IF NOT EXISTS cash_movement_id UUID;
-- Refresh the canonical journal with the normalized values so a later full
-- sync cannot reintroduce a legacy category.
UPDATE sync_entities se SET payload = se.payload || jsonb_build_object(
 'payee_name',e.payee_name,'reference',e.reference,'classification',e.classification,
 'source',e.source,'recurring_occurrence_id',e.recurring_occurrence_id,'cash_movement_id',e.cash_movement_id)
FROM expenses e WHERE se.entity_type='expenses' AND se.entity_id=e.id AND se.gym_id=e.gym_id;
ALTER TABLE expenses ADD CONSTRAINT chk_expenses_category CHECK (category IN
 ('renta','servicios','nomina','mantenimiento','marketing','insumos_no_inventariables','impuestos_y_permisos','otros'));
ALTER TABLE expenses ADD CONSTRAINT chk_expenses_classification CHECK (classification IN ('fixed','variable'));
ALTER TABLE expenses ADD CONSTRAINT chk_expenses_source CHECK (source IN ('manual','recurring','cash_movement'));
ALTER TABLE expenses ADD CONSTRAINT chk_expenses_payee_len CHECK (payee_name IS NULL OR char_length(payee_name)<=120);
ALTER TABLE expenses ADD CONSTRAINT chk_expenses_reference_len CHECK (reference IS NULL OR char_length(reference)<=120);

CREATE TABLE cash_movements (
 id UUID PRIMARY KEY, gym_id UUID NOT NULL REFERENCES gyms(id), version INTEGER NOT NULL DEFAULT 1,
 created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, deleted_at TIMESTAMPTZ,
 movement_on DATE NOT NULL, amount NUMERIC(12,2) NOT NULL CHECK(amount>0),
 movement_type TEXT NOT NULL CHECK(movement_type IN ('cash_in','cash_out')),
 reason TEXT NOT NULL CHECK(char_length(reason) BETWEEN 1 AND 200), operator_id UUID NOT NULL REFERENCES users(id),
 expense_id UUID UNIQUE, classification_status TEXT NOT NULL DEFAULT 'unclassified'
   CHECK(classification_status IN ('unclassified','expense','non_operating'))
);

CREATE TABLE recurring_expense_templates (
 id UUID PRIMARY KEY, gym_id UUID NOT NULL REFERENCES gyms(id), version INTEGER NOT NULL DEFAULT 1,
 created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, deleted_at TIMESTAMPTZ,
 name TEXT NOT NULL CHECK(char_length(name) BETWEEN 1 AND 120), payee_name TEXT,
 category TEXT NOT NULL, expected_amount NUMERIC(12,2) NOT NULL CHECK(expected_amount>0),
 usual_payment_method TEXT NOT NULL CHECK(usual_payment_method IN ('cash','transfer','card')),
 classification TEXT NOT NULL CHECK(classification IN ('fixed','variable')),
 frequency TEXT NOT NULL CHECK(frequency IN ('weekly','every_14_days','semimonthly','monthly','bimonthly','quarterly','semiannual','annual')),
 starts_on DATE NOT NULL, ends_on DATE, next_due_on DATE NOT NULL, active BOOLEAN NOT NULL DEFAULT TRUE,
 created_by UUID NOT NULL REFERENCES users(id), CHECK(ends_on IS NULL OR ends_on>=starts_on),
 CHECK(category IN ('renta','servicios','nomina','mantenimiento','marketing','insumos_no_inventariables','impuestos_y_permisos','otros'))
);

CREATE TABLE expense_occurrences (
 id UUID PRIMARY KEY, gym_id UUID NOT NULL REFERENCES gyms(id), version INTEGER NOT NULL DEFAULT 1,
 created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, deleted_at TIMESTAMPTZ,
 template_id UUID NOT NULL REFERENCES recurring_expense_templates(id), due_on DATE NOT NULL,
 expected_amount NUMERIC(12,2) NOT NULL CHECK(expected_amount>0), category TEXT NOT NULL,
 payee_name TEXT, payment_method TEXT NOT NULL CHECK(payment_method IN ('cash','transfer','card')),
 classification TEXT NOT NULL CHECK(classification IN ('fixed','variable')),
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','paid','skipped')),
 expense_id UUID UNIQUE, resolved_by UUID REFERENCES users(id), resolved_at TIMESTAMPTZ, skip_reason TEXT,
 UNIQUE(gym_id,template_id,due_on)
);
ALTER TABLE expense_occurrences ADD CONSTRAINT chk_occurrence_category CHECK(category IN ('renta','servicios','nomina','mantenimiento','marketing','insumos_no_inventariables','impuestos_y_permisos','otros'));
ALTER TABLE expenses ADD CONSTRAINT fk_expenses_occurrence FOREIGN KEY(recurring_occurrence_id) REFERENCES expense_occurrences(id);
ALTER TABLE expenses ADD CONSTRAINT fk_expenses_cash_movement FOREIGN KEY(cash_movement_id) REFERENCES cash_movements(id);
ALTER TABLE expenses ADD CONSTRAINT uq_expenses_occurrence UNIQUE(recurring_occurrence_id);
ALTER TABLE expenses ADD CONSTRAINT uq_expenses_cash_movement UNIQUE(cash_movement_id);
-- expense_id on movements/occurrences is the idempotency backlink. The
-- enforced FK lives in expenses -> source entity to keep sync acyclic.

CREATE INDEX idx_cash_movements_gym_day ON cash_movements(gym_id,movement_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_occurrences_gym_due ON expense_occurrences(gym_id,status,due_on,id) WHERE deleted_at IS NULL;
CREATE INDEX idx_templates_gym_active ON recurring_expense_templates(gym_id,active,next_due_on,id) WHERE deleted_at IS NULL;
INSERT INTO _migrations(version,name) VALUES(35,'035_expenses_plus') ON CONFLICT(version) DO NOTHING;
COMMIT;
