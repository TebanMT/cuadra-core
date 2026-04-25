//go:build server

package repositories

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	"github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type MembershipPostgresRepository struct{}

func NewMembershipPostgresRepository() *MembershipPostgresRepository {
	return &MembershipPostgresRepository{}
}

func (r *MembershipPostgresRepository) Create(tx sharedDomain.Transaction, m *membershipDomain.Membership) (*membershipDomain.Membership, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := membershipToModel(m)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return membershipFromModel(&row), nil
}

func (r *MembershipPostgresRepository) Update(tx sharedDomain.Transaction, m *membershipDomain.Membership) (*membershipDomain.Membership, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m.UpdatedAt = time.Now().UTC()
	row := membershipToModel(m)
	if err := gormTx.Where("id = ?", m.ID).Omit("id", "created_at", "gym_id", "member_id").Save(&row).Error; err != nil {
		return nil, err
	}
	return membershipFromModel(&row), nil
}

func (r *MembershipPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*membershipDomain.Membership, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.MembershipModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMembershipNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return membershipFromModel(&row), nil
}

func (r *MembershipPostgresRepository) GetCurrentByMember(tx sharedDomain.Transaction, memberID uuid.UUID) (*membershipDomain.Membership, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.MembershipModel
	err := gormTx.Where("member_id = ? AND status = ? AND deleted_at IS NULL", memberID, membershipDomain.StatusActive).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrNoActiveMembership, "")
	}
	if err != nil {
		return nil, err
	}
	return membershipFromModel(&row), nil
}

// ---------------------------------------------------------------------------
// MembershipAdjustmentRepository
// ---------------------------------------------------------------------------

type MembershipAdjustmentPostgresRepository struct{}

func NewMembershipAdjustmentPostgresRepository() *MembershipAdjustmentPostgresRepository {
	return &MembershipAdjustmentPostgresRepository{}
}

func (r *MembershipAdjustmentPostgresRepository) Create(tx sharedDomain.Transaction, a *membershipDomain.MembershipAdjustment) (*membershipDomain.MembershipAdjustment, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := adjustmentToModel(a)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return adjustmentFromModel(&row), nil
}

func (r *MembershipAdjustmentPostgresRepository) ListByMembership(tx sharedDomain.Transaction, membershipID uuid.UUID) ([]*membershipDomain.MembershipAdjustment, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.MembershipAdjustmentModel
	if err := gormTx.Where("membership_id = ? AND deleted_at IS NULL", membershipID).
		Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*membershipDomain.MembershipAdjustment, len(rows))
	for i := range rows {
		out[i] = adjustmentFromModel(&rows[i])
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func membershipToModel(m *membershipDomain.Membership) models.MembershipModel {
	return models.MembershipModel{
		ID:                   m.ID,
		GymID:                m.GymID,
		Version:              m.Version,
		CreatedAt:            m.CreatedAt,
		UpdatedAt:            m.UpdatedAt,
		DeletedAt:            m.DeletedAt,
		MemberID:             m.MemberID,
		MembershipTypeID:     m.MembershipTypeID,
		TypeNameSnapshot:     m.TypeNameSnapshot,
		PriceSnapshot:        m.PriceSnapshot,
		DurationDaysSnapshot: m.DurationDaysSnapshot,
		StartDate:            m.StartDate,
		ExpiryDate:           m.ExpiryDate,
		Status:               m.Status,
		ReplacedBy:           m.ReplacedBy,
	}
}

func membershipFromModel(r *models.MembershipModel) *membershipDomain.Membership {
	return &membershipDomain.Membership{
		ID:                   r.ID,
		GymID:                r.GymID,
		Version:              r.Version,
		MemberID:             r.MemberID,
		MembershipTypeID:     r.MembershipTypeID,
		TypeNameSnapshot:     r.TypeNameSnapshot,
		PriceSnapshot:        r.PriceSnapshot,
		DurationDaysSnapshot: r.DurationDaysSnapshot,
		StartDate:            r.StartDate,
		ExpiryDate:           r.ExpiryDate,
		Status:               r.Status,
		ReplacedBy:           r.ReplacedBy,
		CreatedAt:            r.CreatedAt,
		UpdatedAt:            r.UpdatedAt,
		DeletedAt:            r.DeletedAt,
	}
}

func adjustmentToModel(a *membershipDomain.MembershipAdjustment) models.MembershipAdjustmentModel {
	return models.MembershipAdjustmentModel{
		ID:             a.ID,
		GymID:          a.GymID,
		Version:        a.Version,
		CreatedAt:      a.CreatedAt,
		UpdatedAt:      a.UpdatedAt,
		DeletedAt:      a.DeletedAt,
		MembershipID:   a.MembershipID,
		AdjustedBy:     a.AdjustedBy,
		Reason:         a.Reason,
		DaysAdded:      a.DaysAdded,
		PreviousExpiry: a.PreviousExpiry,
		NewExpiry:      a.NewExpiry,
	}
}

func adjustmentFromModel(r *models.MembershipAdjustmentModel) *membershipDomain.MembershipAdjustment {
	return &membershipDomain.MembershipAdjustment{
		ID:             r.ID,
		GymID:          r.GymID,
		Version:        r.Version,
		MembershipID:   r.MembershipID,
		AdjustedBy:     r.AdjustedBy,
		Reason:         r.Reason,
		DaysAdded:      r.DaysAdded,
		PreviousExpiry: r.PreviousExpiry,
		NewExpiry:      r.NewExpiry,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
		DeletedAt:      r.DeletedAt,
	}
}
