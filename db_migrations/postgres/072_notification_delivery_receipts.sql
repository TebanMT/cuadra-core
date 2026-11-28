-- Acuses de entrega de WhatsApp en notification_queue (historial de
-- mensajes en el perfil del socio).
--
-- `status = 'sent'` sólo significa que Twilio ACEPTÓ el mensaje. La
-- confirmación de que llegó al teléfono (delivered) y de que el socio lo
-- abrió (read) llega después por el StatusCallback de Twilio — sin costo
-- extra — y hasta hoy sólo se guardaba en whatsapp_events (cloud-only).
-- Para un reclamo del tipo "nunca me llegó el aviso" la evidencia real es
-- delivered_at; read_at sólo existe si el socio tiene activas las
-- confirmaciones de lectura de WhatsApp.
--
-- Cloud-authoritative: sólo el webhook del cloud escribe estas columnas. El
-- sidecar las recibe por pull (están en SyncedTables) y nunca las empuja
-- (el projector las descarta de los pushes). Gemela sqlite/051.
--
-- Backfill: los acuses que ya están en whatsapp_events se copian a la
-- fila, y la fila espejo de sync_entities se actualiza con versión nueva
-- para que los desktops los bajen en su próximo pull, sin importar cuánto
-- tiempo hayan estado sin internet. Sólo toca filas `sent` sin acuse, así
-- que re-ejecutar este archivo no vuelve a bumpear versiones.
BEGIN;

ALTER TABLE public.notification_queue
  ADD COLUMN IF NOT EXISTS delivered_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS read_at TIMESTAMPTZ;

-- Historial por socio: GET /members/:id/notifications ordena por
-- created_at DESC dentro de (gym, destinatario).
CREATE INDEX IF NOT EXISTS idx_notification_queue_recipient
  ON public.notification_queue (gym_id, recipient_id, created_at DESC)
  WHERE deleted_at IS NULL;

WITH acks AS (
  SELECT e.notification_id,
         MIN(e.received_at) AS delivered_at,
         MIN(e.received_at) FILTER (WHERE e.status = 'read') AS read_at
    FROM public.whatsapp_events e
   WHERE e.event_type = 'status'
     AND e.notification_id IS NOT NULL
     AND e.status IN ('delivered', 'read')
   GROUP BY e.notification_id
), touched AS (
  UPDATE public.notification_queue n
     SET delivered_at = a.delivered_at,
         read_at      = a.read_at,
         version      = n.version + 1,
         updated_at   = NOW()
    FROM acks a
   WHERE n.id = a.notification_id
     AND n.status = 'sent'
     AND n.delivered_at IS NULL
  RETURNING n.id, n.gym_id, n.version, n.updated_at, n.delivered_at, n.read_at
)
UPDATE public.sync_entities s
   SET payload = s.payload || jsonb_build_object(
         'version',      t.version,
         'updated_at',   (EXTRACT(EPOCH FROM t.updated_at) * 1000)::bigint,
         'delivered_at', (EXTRACT(EPOCH FROM t.delivered_at) * 1000)::bigint,
         'read_at',      CASE WHEN t.read_at IS NULL THEN NULL
                              ELSE (EXTRACT(EPOCH FROM t.read_at) * 1000)::bigint END),
       version = t.version,
       server_updated_at = NOW()
  FROM touched t
 WHERE s.gym_id = t.gym_id
   AND s.entity_type = 'notification_queue'
   AND s.entity_id = t.id;

INSERT INTO _migrations(version, name)
VALUES (72, '072_notification_delivery_receipts')
ON CONFLICT (version) DO NOTHING;

COMMIT;
