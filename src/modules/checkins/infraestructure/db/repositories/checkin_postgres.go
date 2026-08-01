//go:build server

package repositories

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	checkinDomain "github.com/cuadra/cuadra-core/src/modules/checkins/domain/checkin"
	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	chkRepo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/checkins/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

type CheckinPostgresRepository struct{}

func NewCheckinPostgresRepository() *CheckinPostgresRepository {
	return &CheckinPostgresRepository{}
}

func (r *CheckinPostgresRepository) Create(tx sharedDomain.Transaction, c *checkinDomain.Checkin) (*checkinDomain.Checkin, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := checkinToModel(c)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return checkinFromModel(&row), nil
}

func (r *CheckinPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*checkinDomain.Checkin, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.CheckinModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(chkErrors.ErrCheckinNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return checkinFromModel(&row), nil
}

func (r *CheckinPostgresRepository) ListByMember(tx sharedDomain.Transaction, memberID uuid.UUID, since time.Time, limit int) ([]*checkinDomain.Checkin, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var rows []models.CheckinModel
	q := gormTx.Where("member_id = ? AND deleted_at IS NULL", memberID).
		Order("checkin_at DESC").Limit(limit)
	if !since.IsZero() {
		q = q.Where("checkin_at >= ?", since)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*checkinDomain.Checkin, 0, len(rows))
	for i := range rows {
		out = append(out, checkinFromModel(&rows[i]))
	}
	return out, nil
}

func (r *CheckinPostgresRepository) CountTodayByGym(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, today time.Time) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	// Día LOCAL del gym → rango de instantes. Con medianoche UTC, el
	// contador se reiniciaba a las 6 PM de CDMX.
	start, end := tz.DayBounds(tzName, today, today)
	var n int64
	err := gormTx.Model(&models.CheckinModel{}).
		Where("gym_id = ? AND deleted_at IS NULL AND result LIKE 'allowed%' AND checkin_at >= ? AND checkin_at < ?",
			gymID, start, end).
		Count(&n).Error
	return int(n), err
}

func (r *CheckinPostgresRepository) ListRecentByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]chkRepo.RecentCheckinRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	type row struct {
		ID             uuid.UUID
		MemberID       *uuid.UUID
		MemberName     *string
		Method         string
		Result         string
		ManualOverride bool
		OverrideReason *string
		OperatorName   *string
		CheckinAt      time.Time
		ExpiryDate     *time.Time
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT c.id, c.member_id,
		       m.full_name AS member_name,
		       c.method, c.result, c.manual_override, c.override_reason,
		       u.full_name AS operator_name,
		       c.checkin_at,
		       (SELECT ms.expiry_date FROM memberships ms
		        WHERE ms.member_id = c.member_id
		          AND ms.status = 'active' AND ms.deleted_at IS NULL
		        LIMIT 1) AS expiry_date
		FROM checkins c
		LEFT JOIN members m ON m.id = c.member_id AND m.deleted_at IS NULL
		LEFT JOIN users u ON u.id = c.operator_id AND u.deleted_at IS NULL
		WHERE c.gym_id = ? AND c.deleted_at IS NULL
		ORDER BY c.checkin_at DESC
		LIMIT ?`, gymID, limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]chkRepo.RecentCheckinRow, len(rows))
	for i, x := range rows {
		out[i] = chkRepo.RecentCheckinRow(x)
	}
	return out, nil
}

// ListByGymBetween — drill-down: check-ins en el rango [from,to] al día
// LOCAL del gym (la barra del chart se cuenta por día local; con
// `checkin_at::date` la lista iba por día UTC y nunca cuadraba con la
// barra — además el cast inutilizaba el índice).
// Misma forma de salida que ListRecentByGym para que el FE renderee igual.
func (r *CheckinPostgresRepository) ListByGymBetween(tx sharedDomain.Transaction, gymID uuid.UUID, tzName string, from, to time.Time, limit int) ([]chkRepo.RecentCheckinRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	start, end := tz.DayBounds(tzName, from, to)
	type row struct {
		ID             uuid.UUID
		MemberID       *uuid.UUID
		MemberName     *string
		Method         string
		Result         string
		ManualOverride bool
		OverrideReason *string
		OperatorName   *string
		CheckinAt      time.Time
		ExpiryDate     *time.Time
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT c.id, c.member_id,
		       m.full_name AS member_name,
		       c.method, c.result, c.manual_override, c.override_reason,
		       u.full_name AS operator_name,
		       c.checkin_at,
		       (SELECT ms.expiry_date FROM memberships ms
		        WHERE ms.member_id = c.member_id
		          AND ms.status = 'active' AND ms.deleted_at IS NULL
		        LIMIT 1) AS expiry_date
		FROM checkins c
		LEFT JOIN members m ON m.id = c.member_id AND m.deleted_at IS NULL
		LEFT JOIN users u ON u.id = c.operator_id AND u.deleted_at IS NULL
		WHERE c.gym_id = ? AND c.deleted_at IS NULL
		  AND c.checkin_at >= ? AND c.checkin_at < ?
		ORDER BY c.checkin_at DESC
		LIMIT ?`,
		gymID, start, end, limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]chkRepo.RecentCheckinRow, len(rows))
	for i, x := range rows {
		out[i] = chkRepo.RecentCheckinRow(x)
	}
	return out, nil
}

func (r *CheckinPostgresRepository) ListByMemberDetailed(tx sharedDomain.Transaction, gymID, memberID uuid.UUID, limit int) ([]chkRepo.RecentCheckinRow, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	type row struct {
		ID             uuid.UUID
		MemberID       *uuid.UUID
		MemberName     *string
		Method         string
		Result         string
		ManualOverride bool
		OverrideReason *string
		OperatorName   *string
		CheckinAt      time.Time
		ExpiryDate     *time.Time
	}
	var rows []row
	if err := gormTx.Raw(`
		SELECT c.id, c.member_id,
		       c.method, c.result, c.manual_override, c.override_reason,
		       u.full_name AS operator_name,
		       c.checkin_at
		FROM checkins c
		LEFT JOIN users u ON u.id = c.operator_id AND u.deleted_at IS NULL
		WHERE c.gym_id = ? AND c.member_id = ? AND c.deleted_at IS NULL
		ORDER BY c.checkin_at DESC
		LIMIT ?`, gymID, memberID, limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]chkRepo.RecentCheckinRow, len(rows))
	for i, x := range rows {
		out[i] = chkRepo.RecentCheckinRow(x)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func checkinToModel(c *checkinDomain.Checkin) models.CheckinModel {
	return models.CheckinModel{
		ID:             c.ID,
		GymID:          c.GymID,
		Version:        c.Version,
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
		DeletedAt:      c.DeletedAt,
		MemberID:       c.MemberID,
		CheckinAt:      c.CheckinAt,
		Method:         c.Method,
		Result:         c.Result,
		OperatorID:     c.OperatorID,
		ManualOverride: c.ManualOverride,
		OverrideReason: c.OverrideReason,
	}
}

func checkinFromModel(r *models.CheckinModel) *checkinDomain.Checkin {
	return &checkinDomain.Checkin{
		ID:             r.ID,
		GymID:          r.GymID,
		Version:        r.Version,
		MemberID:       r.MemberID,
		CheckinAt:      r.CheckinAt,
		Method:         r.Method,
		Result:         r.Result,
		OperatorID:     r.OperatorID,
		ManualOverride: r.ManualOverride,
		OverrideReason: r.OverrideReason,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
		DeletedAt:      r.DeletedAt,
	}
}
