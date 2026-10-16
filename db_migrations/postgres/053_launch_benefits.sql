BEGIN;
-- Immutable cohort ledger. Soft-deleting a gym never frees a launch place.
CREATE TABLE launch_benefits (
 gym_id UUID PRIMARY KEY REFERENCES gyms(id),
 slot INTEGER NOT NULL UNIQUE CHECK(slot BETWEEN 1 AND 50),
 first_paid_at TIMESTAMPTZ NOT NULL,
 created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Only recorded payments qualify. Subscription creation/activation alone and
-- unpaid vouchers are not evidence of money received.
WITH first_payments AS (
 SELECT e.gym_id, MIN(e.occurred_at) AS paid_at
 FROM subscription_events e JOIN gyms g ON g.id=e.gym_id
 WHERE e.type IN ('activated','renewed') AND e.amount>0
 AND e.plan IN ('standard_monthly','standard_annual','plus_monthly','plus_annual')
 AND (
   e.provider='manual'
   OR (e.provider='stripe' AND e.raw_payload->>'event' IN ('invoice.paid','invoice.payment_succeeded','payment_intent.succeeded'))
   OR (e.provider='stripe' AND e.raw_payload->>'event'='checkout.session.completed'
       AND e.raw_payload#>>'{data,object,payment_status}'='paid')
   OR (e.provider='mercadopago' AND e.raw_payload->>'payment_confirmed'='true')
 ) GROUP BY e.gym_id
), ranked AS (
 SELECT gym_id,paid_at,ROW_NUMBER() OVER(ORDER BY paid_at,gym_id) AS slot FROM first_payments
)
INSERT INTO launch_benefits(gym_id,slot,first_paid_at)
SELECT gym_id,slot,paid_at FROM ranked WHERE slot<=50;
UPDATE gyms SET all_features_access=TRUE,version=version+1,updated_at=NOW()
WHERE id IN (SELECT gym_id FROM launch_benefits);
UPDATE sync_entities se SET server_updated_at=NOW(),version=GREATEST(se.version+1,g.version)
FROM gyms g JOIN launch_benefits b ON b.gym_id=g.id
WHERE se.entity_type='gyms' AND se.entity_id=g.id AND se.gym_id=g.id;
INSERT INTO _migrations(version,name,applied_at) VALUES(53,'053_launch_benefits',NOW());
COMMIT;
