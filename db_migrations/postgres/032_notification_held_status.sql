-- Migración 032 — agrega el estado `held` a notification_queue.
--
-- Fase 1 de la supresión de mensajes stale: cuando el gym estuvo mucho tiempo
-- sin internet, una notificación encolada localmente puede llegar al dispatcher
-- (cloud) días o semanas después del evento. En vez de enviarla tarde, el
-- dispatcher la deja en `held` si su antigüedad supera el TTL del template
-- (ver src/modules/notifications/app/stale.go). `held` = retenida, NO perdida
-- ni fallida; la Fase 2 (Plus) agrega la UI para recuperarla o descartarla.

BEGIN;

ALTER TABLE notification_queue DROP CONSTRAINT IF EXISTS chk_notification_queue_status;
ALTER TABLE notification_queue ADD CONSTRAINT chk_notification_queue_status
    CHECK (status IN ('pending','sent','failed','cancelled','held'));

INSERT INTO _migrations (version, name) VALUES (32, '032_notification_held_status')
ON CONFLICT (version) DO NOTHING;

COMMIT;
