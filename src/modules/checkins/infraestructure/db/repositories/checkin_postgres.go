//go:build server

package repositories

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	checkinDomain "github.com/cuadra/cuadra-core/src/modules/checkins/domain/checkin"
	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/checkins/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
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
