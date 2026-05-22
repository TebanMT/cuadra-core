//go:build server

package db

import (
	"time"

	"github.com/google/uuid"

	subApp "github.com/cuadra/cuadra-core/src/modules/subscriptions/app"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// OXXORenewalPostgresReader implementa subApp.OXXORenewalReader. Las dos
// consultas comparten la misma "lateral pick" sobre subscription_events:
// el último evento de tipo activated/renewed del gym, y se filtra a que
// haya sido OXXO (raw_payload->>'payment_method' = 'oxxo'). Sin ese marker
// el gym no entra al cron — los gyms de TARJETA renuevan vía Stripe
// Subscription y los reconcilia el webhook invoice.paid.
type OXXORenewalPostgresReader struct{}

func NewOXXORenewalPostgresReader() *OXXORenewalPostgresReader {
	return &OXXORenewalPostgresReader{}
}

// FindRenewalCandidates retorna gyms anuales OXXO con SubscriptionEndsAt en
// la ventana [windowStart, windowEnd]. Está prefiltado a status=active —
// no queremos volver a tocar gyms ya cancelados (el job CancelExpiredOXXO
// los mueve a cancelled tras la gracia).
func (r *OXXORenewalPostgresReader) FindRenewalCandidates(tx sharedDomain.Transaction, windowStart, windowEnd time.Time) ([]subApp.OXXORenewalCandidate, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		GymID                 uuid.UUID
		GymName               *string
		WhatsAppConnectedAt   *time.Time
		WhatsAppBusinessPhone *string
		SubscriptionEndsAt    time.Time
	}
	var rows []row
	q := `
		SELECT
		    g.id   AS gym_id,
		    g.name AS gym_name,
		    g.whatsapp_connected_at,
		    g.whatsapp_business_phone,
		    g.subscription_ends_at
		FROM gyms g
		WHERE g.deleted_at IS NULL
		  AND g.subscription_plan = 'standard_annual'
		  AND g.subscription_status = 'active'
		  AND g.subscription_ends_at IS NOT NULL
		  AND g.subscription_ends_at >= ?
		  AND g.subscription_ends_at <= ?
		  AND EXISTS (
		      SELECT 1
		      FROM subscription_events e
		      WHERE e.gym_id = g.id
		        AND e.type IN ('activated', 'renewed')
		        AND e.raw_payload->>'payment_method' = 'oxxo'
		        AND e.occurred_at = (
		            SELECT MAX(e2.occurred_at)
		            FROM subscription_events e2
		            WHERE e2.gym_id = g.id
		              AND e2.type IN ('activated', 'renewed')
		        )
		  )
	`
	if err := gormTx.Raw(q, windowStart.UTC(), windowEnd.UTC()).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]subApp.OXXORenewalCandidate, 0, len(rows))
	for _, r := range rows {
		c := subApp.OXXORenewalCandidate{
			GymID:              r.GymID,
			SubscriptionEndsAt: r.SubscriptionEndsAt,
			GymWhatsAppReady:   r.WhatsAppConnectedAt != nil && r.WhatsAppBusinessPhone != nil && *r.WhatsAppBusinessPhone != "",
		}
		if r.GymName != nil {
			c.GymName = *r.GymName
		}
		out = append(out, c)
	}
	return out, nil
}

// FindExpiredOXXO retorna gyms anuales OXXO con SubscriptionEndsAt < before
// Y sin activated/renewed posterior. El caller pasa `now - grace` como
// `before` para implementar la ventana de 7 días post-vencimiento.
func (r *OXXORenewalPostgresReader) FindExpiredOXXO(tx sharedDomain.Transaction, before time.Time) ([]subApp.OXXORenewalCandidate, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		GymID                 uuid.UUID
		GymName               *string
		WhatsAppConnectedAt   *time.Time
		WhatsAppBusinessPhone *string
		SubscriptionEndsAt    time.Time
	}
	var rows []row
	q := `
		SELECT
		    g.id   AS gym_id,
		    g.name AS gym_name,
		    g.whatsapp_connected_at,
		    g.whatsapp_business_phone,
		    g.subscription_ends_at
		FROM gyms g
		WHERE g.deleted_at IS NULL
		  AND g.subscription_plan = 'standard_annual'
		  AND g.subscription_status = 'active'
		  AND g.subscription_ends_at IS NOT NULL
		  AND g.subscription_ends_at < ?
		  AND NOT EXISTS (
		      SELECT 1 FROM subscription_events e
		      WHERE e.gym_id = g.id
		        AND e.type IN ('activated', 'renewed')
		        AND e.occurred_at >= g.subscription_ends_at
		  )
		  AND EXISTS (
		      SELECT 1
		      FROM subscription_events e
		      WHERE e.gym_id = g.id
		        AND e.type IN ('activated', 'renewed')
		        AND e.raw_payload->>'payment_method' = 'oxxo'
		        AND e.occurred_at = (
		            SELECT MAX(e2.occurred_at)
		            FROM subscription_events e2
		            WHERE e2.gym_id = g.id
		              AND e2.type IN ('activated', 'renewed')
		        )
		  )
	`
	if err := gormTx.Raw(q, before.UTC()).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]subApp.OXXORenewalCandidate, 0, len(rows))
	for _, r := range rows {
		c := subApp.OXXORenewalCandidate{
			GymID:              r.GymID,
			SubscriptionEndsAt: r.SubscriptionEndsAt,
			GymWhatsAppReady:   r.WhatsAppConnectedAt != nil && r.WhatsAppBusinessPhone != nil && *r.WhatsAppBusinessPhone != "",
		}
		if r.GymName != nil {
			c.GymName = *r.GymName
		}
		out = append(out, c)
	}
	return out, nil
}
