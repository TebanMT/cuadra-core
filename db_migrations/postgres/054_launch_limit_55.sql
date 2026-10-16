BEGIN;
-- Keep the same allocation lock used by running servers. The lock key is an
-- identifier, not the cohort size. Existing places retain their identity.
SELECT pg_advisory_xact_lock(20261014,50);
ALTER TABLE launch_benefits DROP CONSTRAINT launch_benefits_slot_check;
ALTER TABLE launch_benefits ADD CONSTRAINT launch_benefits_slot_check
 CHECK (slot BETWEEN 1 AND 55);

-- Include confirmed historical payments that did not fit the original cap.
WITH first_payments AS (
 SELECT e.gym_id,MIN(e.occurred_at) AS paid_at
 FROM subscription_events e JOIN gyms g ON g.id=e.gym_id
 WHERE NOT EXISTS (SELECT 1 FROM launch_benefits b WHERE b.gym_id=e.gym_id)
 AND e.type IN ('activated','renewed') AND e.amount>0
 AND e.plan IN ('standard_monthly','standard_annual','plus_monthly','plus_annual')
 AND (
   e.provider='manual'
   OR (e.provider='stripe' AND e.raw_payload->>'event' IN ('invoice.paid','invoice.payment_succeeded','payment_intent.succeeded'))
   OR (e.provider='stripe' AND e.raw_payload->>'event'='checkout.session.completed'
       AND e.raw_payload#>>'{data,object,payment_status}'='paid')
   OR (e.provider='mercadopago' AND e.raw_payload->>'payment_confirmed'='true')
 ) GROUP BY e.gym_id
), ranked AS (
 SELECT gym_id,paid_at,ROW_NUMBER() OVER(ORDER BY paid_at,gym_id)
   +(SELECT COALESCE(MAX(slot),0) FROM launch_benefits) AS slot
 FROM first_payments
), added AS (
 INSERT INTO launch_benefits(gym_id,slot,first_paid_at)
 SELECT gym_id,slot,paid_at FROM ranked WHERE slot<=55
 RETURNING gym_id
), granted AS (
 UPDATE gyms g SET all_features_access=TRUE,version=g.version+1,updated_at=clock_timestamp()
 FROM added WHERE g.id=added.gym_id
 RETURNING g.id,g.version
)
UPDATE sync_entities se SET server_updated_at=clock_timestamp(),version=GREATEST(se.version+1,g.version)
FROM granted g
WHERE se.entity_type='gyms' AND se.entity_id=g.id AND se.gym_id=g.id;
INSERT INTO _migrations(version,name,applied_at) VALUES(54,'054_launch_limit_55',NOW());
COMMIT;
