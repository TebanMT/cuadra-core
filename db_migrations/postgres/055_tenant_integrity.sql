-- Defense in depth for every writer: a global UUID does not authorize moving
-- an entity or linking it to another tenant. NOT VALID avoids pretending that
-- historical data has been audited; new/changed rows are enforced immediately.
CREATE OR REPLACE FUNCTION tinta_immutable_tenant() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.gym_id IS DISTINCT FROM OLD.gym_id THEN
    RAISE EXCEPTION 'tenant identity is immutable' USING ERRCODE='23514', CONSTRAINT='tenant_identity_immutable';
  END IF;
  RETURN NEW;
END $$;

DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT table_name FROM information_schema.columns
           WHERE table_schema='public' AND column_name='gym_id'
  LOOP
    EXECUTE format('CREATE TRIGGER tenant_identity_immutable BEFORE UPDATE OF gym_id ON %I FOR EACH ROW EXECUTE FUNCTION tinta_immutable_tenant()', r.table_name);
  END LOOP;
END $$;

ALTER TABLE gyms ADD CONSTRAINT tenant_gym_identity CHECK(id=gym_id) NOT VALID;

-- Extend existing simple id FKs to (gym_id,id). Preserve deferred semantics
-- for cyclic financial graphs; never weaken their original constraints.
DO $$
DECLARE r record;
BEGIN
  FOR r IN
    SELECT c.oid, child.relname AS child_table, parent.relname AS parent_table,
           ca.attname AS child_column, c.condeferrable, c.condeferred
      FROM pg_constraint c
      JOIN pg_class child ON child.oid=c.conrelid
      JOIN pg_namespace ns ON ns.oid=child.relnamespace AND ns.nspname='public'
      JOIN pg_class parent ON parent.oid=c.confrelid
      JOIN pg_attribute ca ON ca.attrelid=c.conrelid AND ca.attnum=c.conkey[1]
      JOIN pg_attribute pa ON pa.attrelid=c.confrelid AND pa.attnum=c.confkey[1]
     WHERE c.contype='f' AND cardinality(c.conkey)=1 AND cardinality(c.confkey)=1
       AND ca.attname<>'gym_id' AND pa.attname='id'
       AND EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=child.oid AND attname='gym_id' AND NOT attisdropped)
       AND EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=parent.oid AND attname='gym_id' AND NOT attisdropped)
  LOOP
    EXECUTE format('CREATE UNIQUE INDEX IF NOT EXISTS %I ON %I(gym_id,id)', 'tenant_key_'||r.parent_table,r.parent_table);
    EXECUTE format('ALTER TABLE %I ADD CONSTRAINT %I FOREIGN KEY(gym_id,%I) REFERENCES %I(gym_id,id) %s NOT VALID',
      r.child_table, 'tenant_ref_'||r.oid, r.child_column,r.parent_table,
      CASE WHEN r.condeferrable THEN CASE WHEN r.condeferred THEN 'DEFERRABLE INITIALLY DEFERRED' ELSE 'DEFERRABLE INITIALLY IMMEDIATE' END ELSE '' END);
  END LOOP;
END $$;
