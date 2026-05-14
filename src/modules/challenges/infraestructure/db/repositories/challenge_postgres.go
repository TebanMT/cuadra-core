//go:build server

// Package repositories implements the challenges (retos) GORM-backed
// repos for the cloud server build. The sidecar mirror lives in SQLite
// repos under the sidecar build tag (Session 2).
package repositories

import (
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/challenges/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type ChallengePostgresRepository struct{}

func NewChallengePostgresRepository() *ChallengePostgresRepository {
	return &ChallengePostgresRepository{}
}

func (r *ChallengePostgresRepository) Create(tx sharedDomain.Transaction, c *challengeDomain.Challenge) (*challengeDomain.Challenge, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := challengeToModel(c)
	if err := gormTx.Create(&m).Error; err != nil {
		return nil, err
	}
	return challengeToDomain(&m), nil
}

func (r *ChallengePostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*challengeDomain.Challenge, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var m models.ChallengeModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrChallengeNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return challengeToDomain(&m), nil
}

func (r *ChallengePostgresRepository) Update(tx sharedDomain.Transaction, c *challengeDomain.Challenge) (*challengeDomain.Challenge, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := challengeToModel(c)
	// Save (not Updates) so we overwrite zero-able fields like
	// inscription_refundable=false intentionally.
	if err := gormTx.Where("id = ?", c.ID).Save(&m).Error; err != nil {
		return nil, err
	}
	return challengeToDomain(&m), nil
}

func (r *ChallengePostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, statusFilter string) ([]*challengeDomain.Challenge, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	q := gormTx.Model(&models.ChallengeModel{}).
		Where("gym_id = ? AND deleted_at IS NULL", gymID).
		Order("created_at DESC")
	if statusFilter != "" {
		q = q.Where("status = ?", statusFilter)
	}
	var rows []models.ChallengeModel
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*challengeDomain.Challenge, len(rows))
	for i := range rows {
		out[i] = challengeToDomain(&rows[i])
	}
	return out, nil
}

// ─── mappers ───────────────────────────────────────────────────────────────

func challengeToModel(c *challengeDomain.Challenge) models.ChallengeModel {
	return models.ChallengeModel{
		ID:                    c.ID,
		GymID:                 c.GymID,
		Version:               c.Version,
		CreatedAt:             c.CreatedAt,
		UpdatedAt:             c.UpdatedAt,
		DeletedAt:             c.DeletedAt,
		Name:                  c.Name,
		Description:           c.Description,
		StartsAt:              c.StartsAt,
		MeasurementT0Deadline: c.MeasurementT0Deadline,
		MeasurementT1Start:    c.MeasurementT1Start,
		EndsAt:                c.EndsAt,
		Status:                c.Status,
		InscriptionFeeCents:   c.InscriptionFeeCents,
		InscriptionRefundable: c.InscriptionRefundable,
		MinWeeklyAttendance:   c.MinWeeklyAttendance,
		AttendanceGraceWeeks:  c.AttendanceGraceWeeks,
		StrengthCapPct:        c.StrengthCapPct,
		TieMarginIR:           c.TieMarginIR,
		BFFloorMalePct:        c.BFFloorMalePct,
		BFFloorFemalePct:      c.BFFloorFemalePct,
	}
}

func challengeToDomain(m *models.ChallengeModel) *challengeDomain.Challenge {
	return &challengeDomain.Challenge{
		ID:                    m.ID,
		GymID:                 m.GymID,
		Version:               m.Version,
		CreatedAt:             m.CreatedAt,
		UpdatedAt:             m.UpdatedAt,
		DeletedAt:             m.DeletedAt,
		Name:                  m.Name,
		Description:           m.Description,
		StartsAt:              m.StartsAt,
		MeasurementT0Deadline: m.MeasurementT0Deadline,
		MeasurementT1Start:    m.MeasurementT1Start,
		EndsAt:                m.EndsAt,
		Status:                m.Status,
		InscriptionFeeCents:   m.InscriptionFeeCents,
		InscriptionRefundable: m.InscriptionRefundable,
		MinWeeklyAttendance:   m.MinWeeklyAttendance,
		AttendanceGraceWeeks:  m.AttendanceGraceWeeks,
		StrengthCapPct:        m.StrengthCapPct,
		TieMarginIR:           m.TieMarginIR,
		BFFloorMalePct:        m.BFFloorMalePct,
		BFFloorFemalePct:      m.BFFloorFemalePct,
	}
}
