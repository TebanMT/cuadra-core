-- Migration 024 — extender chk_sub_type con los nuevos event types de OXXO.
--
-- voucher_emitted = Stripe Checkout (mode=payment, OXXO) emitió la ficha
--                   pero el cliente todavía no la paga en tienda. Visibilidad
--                   para el dashboard ("ficha pendiente"); no muta el gym.
-- voucher_expired = el voucher venció sin pagarse. Tampoco muta el gym
--                   (un voucher vencido en trial no acaba el trial; uno que
--                   intentaba renovar el anual se maneja por SubscriptionEndsAt
--                   + cron, no por past_due).

BEGIN;

ALTER TABLE subscription_events DROP CONSTRAINT IF EXISTS chk_sub_type;

ALTER TABLE subscription_events
    ADD CONSTRAINT chk_sub_type CHECK (
        type IN (
            'activated',
            'renewed',
            'past_due',
            'cancelled',
            'trial_extended',
            'voucher_emitted',
            'voucher_expired'
        )
    );

INSERT INTO _migrations (version, name) VALUES (24, '024_subscription_events_voucher_types')
ON CONFLICT (version) DO NOTHING;

COMMIT;
