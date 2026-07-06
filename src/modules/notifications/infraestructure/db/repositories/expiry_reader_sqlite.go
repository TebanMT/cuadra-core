//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	notiApp "github.com/cuadra/cuadra-core/src/modules/notifications/app"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// ExpirySQLiteReader is the sidecar version of ExpiryReader. The sidecar
// rarely runs the scheduler (cloud owns it) but the implementation exists
// so dev / test paths share the same surface.
type ExpirySQLiteReader struct{}

func NewExpirySQLiteReader() *ExpirySQLiteReader { return &ExpirySQLiteReader{} }

func (r *ExpirySQLiteReader) FindDueForStage(tx sharedDomain.Transaction, now time.Time, offsetDays int) ([]notiApp.ExpiryCandidate, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	type row struct {
		GymID          string         `db:"gym_id"`
		GymName        sql.NullString `db:"gym_name"`
		GymTimezone    sql.NullString `db:"gym_timezone"`
		MemberID       string         `db:"member_id"`
		FullName       string         `db:"full_name"`
		Phone          string         `db:"phone"`
		MembershipType string         `db:"membership_type"`
		ExpiryDate     string         `db:"expiry_date"`
	}
	var rows []row
	// SQLite no trae base de zonas horarias; 'localtime' usa la tz de la
	// MÁQUINA — que en el sidecar es la PC del gym, o sea exactamente la
	// tz del gym. Etapa K: expiry = hoy_local - K días.
	const q = `
		SELECT
		    g.id   AS gym_id,
		    g.name AS gym_name,
		    g.timezone AS gym_timezone,
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
		  AND ms.expiry_date = date(?, 'localtime', ?)
	`
	modifier := fmt.Sprintf("%d days", -offsetDays)
	nowArg := now.UTC().Format("2006-01-02 15:04:05")
	if err := stx.Select(context.Background(), &rows, q, nowArg, modifier); err != nil {
		return nil, err
	}
	out := make([]notiApp.ExpiryCandidate, 0, len(rows))
	for _, x := range rows {
		gymID, _ := uuid.Parse(x.GymID)
		memberID, _ := uuid.Parse(x.MemberID)
		expiry, _ := time.Parse("2006-01-02", x.ExpiryDate)
		c := notiApp.ExpiryCandidate{
			GymID:          gymID,
			MemberID:       memberID,
			MemberFullName: x.FullName,
			MemberPhone:    x.Phone,
			MembershipType: x.MembershipType,
			ExpiryDate:     expiry,
		}
		if x.GymName.Valid {
			c.GymName = x.GymName.String
		}
		if x.GymTimezone.Valid {
			c.GymTimezone = x.GymTimezone.String
		}
		out = append(out, c)
	}
	return out, nil
}
