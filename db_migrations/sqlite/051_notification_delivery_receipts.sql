-- Acuses de entrega de WhatsApp (gemela de postgres/072). El cloud las
-- escribe desde el StatusCallback de Twilio y las baja por pull; el sidecar
-- sólo las lee para el historial de mensajes del perfil del socio, así que
-- funciona sin internet con lo último que se sincronizó. Epoch-ms, como el
-- resto de timestamps del sidecar.
BEGIN;
ALTER TABLE notification_queue ADD COLUMN delivered_at INTEGER;
ALTER TABLE notification_queue ADD COLUMN read_at INTEGER;
CREATE INDEX IF NOT EXISTS idx_notification_queue_recipient
  ON notification_queue(gym_id, recipient_id, created_at DESC)
  WHERE deleted_at IS NULL;
INSERT INTO _migrations(version,name,applied_at) VALUES(51,'051_notification_delivery_receipts',CAST(strftime('%s','now') AS INTEGER)*1000);
COMMIT;
