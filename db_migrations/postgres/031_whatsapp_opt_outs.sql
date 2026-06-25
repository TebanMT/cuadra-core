-- Opt-out de marketing por WhatsApp (cumplimiento Meta). Cuando un socio
-- responde STOP/BAJA al número maestro de Tinta, registramos su teléfono aquí
-- y el dispatcher OMITE los mensajes Marketing (broadcast) a ese número.
--
-- Es por TELÉFONO, no por gym (excepción deliberada a la convención gym_id):
-- el opt-out aplica al número de WhatsApp del que el socio recibió, que es el
-- maestro compartido de Tinta. Si alguien dice STOP, no debe recibir marketing
-- de NINGÚN gym por ese número. Cloud-only: el webhook de Twilio y el
-- dispatcher viven en el cloud; no se sincroniza al sidecar.
CREATE TABLE IF NOT EXISTS whatsapp_opt_outs (
    phone        TEXT PRIMARY KEY,
    opted_out_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
