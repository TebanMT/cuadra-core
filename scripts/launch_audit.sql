-- Read-only check after migrations 052–054. Does not call payment providers.
-- A paid SKU is not proof of a successful charge. Review the second result
-- against receipts before recording missing historical payments.
-- New manual payments use cmd/admin-subscribe with --amount (actual MXN received).
-- TINTA_LAUNCH_OFFER_ENABLED=false closes admissions without revoking benefits.
BEGIN TRANSACTION READ ONLY;
SELECT COUNT(*) AS assigned_places, GREATEST(0,55-COUNT(*)) AS remaining_places
FROM launch_benefits;

SELECT g.id AS gym_id,g.name,g.subscription_plan,g.subscription_status,
       MIN(e.occurred_at) AS first_recorded_event
FROM gyms g
LEFT JOIN launch_benefits b ON b.gym_id=g.id
LEFT JOIN subscription_events e ON e.gym_id=g.id
WHERE g.deleted_at IS NULL AND b.gym_id IS NULL
  AND g.subscription_plan IN ('standard_monthly','standard_annual','plus_monthly','plus_annual')
GROUP BY g.id,g.name,g.subscription_plan,g.subscription_status
ORDER BY first_recorded_event NULLS LAST,g.id;

-- Must return no rows: cohort flag and grant should agree.
SELECT b.slot,b.gym_id FROM launch_benefits b
JOIN gyms g ON g.id=b.gym_id
WHERE NOT g.all_features_access;
COMMIT;
