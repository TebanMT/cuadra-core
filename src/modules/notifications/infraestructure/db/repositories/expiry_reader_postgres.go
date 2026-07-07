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

func (r *ExpiryPostgresReader) FindDueForStage(tx sharedDomain.Transaction, now time.Time, offsetDays int) ([]notiApp.ExpiryCandidate, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	type row struct {
		GymID          uuid.UUID
		GymName        *string
		GymTimezone    string
		MemberID       uuid.UUID
		FullName       string
		Phone          string
		MembershipType string
		ExpiryDate     time.Time
	}
	var rows []row
	// El "hoy" se evalúa POR GYM en su zona horaria: `now AT TIME ZONE tz`
	// da la hora de pared local y `::date` su día calendario. Etapa K:
	// expiry_date = hoy_local - K (para K=-3 → hoy+3, "vence en 3 días").
	// COALESCE/NULLIF: un gym sin tz configurada cae a UTC (comportamiento
	// previo) en vez de reventar la query de TODOS los gyms del tick.
	q := `
		SELECT
		    g.id   AS gym_id,
		    g.name AS gym_name,
		    COALESCE(NULLIF(g.timezone, ''), 'UTC') AS gym_timezone,
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
		  AND ms.expiry_date = ((?::timestamptz AT TIME ZONE COALESCE(NULLIF(g.timezone, ''), 'UTC'))::date - ?)
	`
	if err := gormTx.Raw(q, now.UTC(), offsetDays).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]notiApp.ExpiryCandidate, 0, len(rows))
	for _, r := range rows {
		c := notiApp.ExpiryCandidate{
			GymID:          r.GymID,
			GymTimezone:    r.GymTimezone,
			MemberID:       r.MemberID,
			MemberFullName: r.FullName,
			MemberPhone:    r.Phone,
			MembershipType: r.MembershipType,
			ExpiryDate:     r.ExpiryDate,
		}
		if r.GymName != nil {
			c.GymName = *r.GymName
		}
		out = append(out, c)
	}
	return out, nil
}
