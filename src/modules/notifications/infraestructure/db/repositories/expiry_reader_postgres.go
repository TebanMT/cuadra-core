//go:build server

package repositories

import (
	"time"

	"github.com/google/uuid"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ExpiryPostgresReader implements notifications.ExpiryReader via a single
// JOIN over members + memberships + gyms. Indexed by
// `idx_memberships_gym_expiry` so the per-day query stays sub-second even
// at 1000 socios.
type ExpiryPostgresReader struct{}

func NewExpiryPostgresReader() *ExpiryPostgresReader { return &ExpiryPostgresReader{} }

func (r *ExpiryPostgresReader) FindExpiringOn(tx sharedDomain.Transaction, targetDate time.Time) ([]notiApp.ExpiryCandidate, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		GymID                 uuid.UUID
		GymName               *string
		WhatsAppConnectedAt   *time.Time
		WhatsAppBusinessPhone *string
		MemberID              uuid.UUID
		FullName              string
		Phone                 string
		MembershipType        string
		ExpiryDate            time.Time
	}
	var rows []row
	q := `
		SELECT
		    g.id   AS gym_id,
		    g.name AS gym_name,
		    g.whatsapp_connected_at,
		    g.whatsapp_business_phone,
		    m.id   AS member_id,
		    m.full_name,
		    m.phone,
		    ms.type_name_snapshot AS membership_type,
		    ms.expiry_date
		FROM members m
		JOIN memberships ms ON ms.member_id = m.id AND ms.deleted_at IS NULL AND ms.status = 'active'
		JOIN gyms g ON g.id = m.gym_id AND g.deleted_at IS NULL
		WHERE m.deleted_at IS NULL
		  AND m.status = 'active'
		  AND ms.expiry_date = ?
	`
	if err := gormTx.Raw(q, targetDate.UTC().Format("2006-01-02")).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]notiApp.ExpiryCandidate, 0, len(rows))
	for _, r := range rows {
		c := notiApp.ExpiryCandidate{
			GymID:            r.GymID,
			MemberID:         r.MemberID,
			MemberFullName:   r.FullName,
			MemberPhone:      r.Phone,
			MembershipType:   r.MembershipType,
			ExpiryDate:       r.ExpiryDate,
			GymWhatsAppReady: r.WhatsAppConnectedAt != nil && r.WhatsAppBusinessPhone != nil && *r.WhatsAppBusinessPhone != "",
		}
		if r.GymName != nil {
			c.GymName = *r.GymName
		}
		out = append(out, c)
	}
	return out, nil
}
