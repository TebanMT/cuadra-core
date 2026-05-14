//go:build server

package repositories

import (
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	participantDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/participant"
	"github.com/cuadra/cuadra-core/src/modules/challenges/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type ParticipantPostgresRepository struct{}

func NewParticipantPostgresRepository() *ParticipantPostgresRepository {
	return &ParticipantPostgresRepository{}
}

func (r *ParticipantPostgresRepository) Create(tx sharedDomain.Transaction, p *participantDomain.Participant) (*participantDomain.Participant, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := participantToModel(p)
	if err := gormTx.Create(&m).Error; err != nil {
		return nil, err
	}
	return participantToDomain(&m), nil
}

func (r *ParticipantPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*participantDomain.Participant, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var m models.ParticipantModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrParticipantNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return participantToDomain(&m), nil
}

func (r *ParticipantPostgresRepository) Update(tx sharedDomain.Transaction, p *participantDomain.Participant) (*participantDomain.Participant, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	m := participantToModel(p)
	if err := gormTx.Where("id = ?", p.ID).Save(&m).Error; err != nil {
		return nil, err
	}
	return participantToDomain(&m), nil
}

func (r *ParticipantPostgresRepository) SoftDelete(tx sharedDomain.Transaction, id uuid.UUID) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	return gormTx.Exec(
		`UPDATE challenge_participants SET deleted_at = NOW(), updated_at = NOW(), version = version + 1
		 WHERE id = ? AND deleted_at IS NULL`, id).Error
}

func (r *ParticipantPostgresRepository) ListByChallenge(tx sharedDomain.Transaction, challengeID uuid.UUID, statusFilter string, categoryFilter *uuid.UUID) ([]*participantDomain.Participant, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	q := gormTx.Model(&models.ParticipantModel{}).
		Where("challenge_id = ? AND deleted_at IS NULL", challengeID).
		Order("created_at ASC")
	if statusFilter != "" {
		q = q.Where("status = ?", statusFilter)
	}
	if categoryFilter != nil {
		q = q.Where("category_id = ?", *categoryFilter)
	}
	var rows []models.ParticipantModel
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*participantDomain.Participant, len(rows))
	for i := range rows {
		out[i] = participantToDomain(&rows[i])
	}
	return out, nil
}

func (r *ParticipantPostgresRepository) ExistsByMember(tx sharedDomain.Transaction, challengeID, memberID uuid.UUID) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.ParticipantModel{}).
		Where("challenge_id = ? AND member_id = ? AND deleted_at IS NULL", challengeID, memberID).
		Count(&n).Error
	return n > 0, err
}

func (r *ParticipantPostgresRepository) HasAnyMeasurement(tx sharedDomain.Transaction, participantID uuid.UUID) (bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Model(&models.MeasurementModel{}).
		Where("participant_id = ? AND deleted_at IS NULL", participantID).
		Count(&n).Error
	return n > 0, err
}

// ─── mappers ───────────────────────────────────────────────────────────────

func participantToModel(p *participantDomain.Participant) models.ParticipantModel {
	return models.ParticipantModel{
		ID:                     p.ID,
		GymID:                  p.GymID,
		ChallengeID:            p.ChallengeID,
		MemberID:               p.MemberID,
		CategoryID:             p.CategoryID,
		Version:                p.Version,
		CreatedAt:              p.CreatedAt,
		UpdatedAt:              p.UpdatedAt,
		DeletedAt:              p.DeletedAt,
		ExerciseLegs:           p.ExerciseLegs,
		ExercisePush:           p.ExercisePush,
		ExercisePull:           p.ExercisePull,
		InscriptionFeePaid:     p.InscriptionFeePaid,
		InscriptionPaidAt:      p.InscriptionPaidAt,
		InscriptionRefundedAt:  p.InscriptionRefundedAt,
		Status:                 p.Status,
		DisqualificationReason: p.DisqualificationReason,
		DisqualifiedAt:         p.DisqualifiedAt,
	}
}

func participantToDomain(m *models.ParticipantModel) *participantDomain.Participant {
	return &participantDomain.Participant{
		ID:                     m.ID,
		GymID:                  m.GymID,
		ChallengeID:            m.ChallengeID,
		MemberID:               m.MemberID,
		CategoryID:             m.CategoryID,
		Version:                m.Version,
		CreatedAt:              m.CreatedAt,
		UpdatedAt:              m.UpdatedAt,
		DeletedAt:              m.DeletedAt,
		ExerciseLegs:           m.ExerciseLegs,
		ExercisePush:           m.ExercisePush,
		ExercisePull:           m.ExercisePull,
		InscriptionFeePaid:     m.InscriptionFeePaid,
		InscriptionPaidAt:      m.InscriptionPaidAt,
		InscriptionRefundedAt:  m.InscriptionRefundedAt,
		Status:                 m.Status,
		DisqualificationReason: m.DisqualificationReason,
		DisqualifiedAt:         m.DisqualifiedAt,
	}
}
