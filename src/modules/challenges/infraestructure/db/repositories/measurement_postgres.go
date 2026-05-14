//go:build server

package repositories

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	measurementDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/measurement"
	"github.com/cuadra/cuadra-core/src/modules/challenges/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type MeasurementPostgresRepository struct{}

func NewMeasurementPostgresRepository() *MeasurementPostgresRepository {
	return &MeasurementPostgresRepository{}
}

func (r *MeasurementPostgresRepository) Create(tx sharedDomain.Transaction, m *measurementDomain.Measurement) (*measurementDomain.Measurement, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	row := measurementToModel(m)
	if err := gormTx.Create(&row).Error; err != nil {
		return nil, err
	}
	return measurementToDomain(&row), nil
}

func (r *MeasurementPostgresRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*measurementDomain.Measurement, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.MeasurementModel
	err := gormTx.Where("id = ? AND deleted_at IS NULL", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrMeasurementNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return measurementToDomain(&row), nil
}

// Supersede flips the prior measurement's superseded_at + superseded_by_id in
// place. We bump version + updated_at so the sync projector + delta-pull pick
// the change up — the prior row's mutation must propagate to clients.
func (r *MeasurementPostgresRepository) Supersede(tx sharedDomain.Transaction, priorID, replacementID uuid.UUID, at time.Time) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	return gormTx.Exec(
		`UPDATE challenge_measurements
		 SET superseded_at = ?, superseded_by_id = ?, updated_at = ?, version = version + 1
		 WHERE id = ? AND deleted_at IS NULL AND superseded_at IS NULL`,
		at, replacementID, at, priorID).Error
}

func (r *MeasurementPostgresRepository) ListByParticipant(tx sharedDomain.Transaction, participantID uuid.UUID) ([]*measurementDomain.Measurement, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var rows []models.MeasurementModel
	if err := gormTx.
		Where("participant_id = ? AND deleted_at IS NULL", participantID).
		Order("created_at DESC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*measurementDomain.Measurement, len(rows))
	for i := range rows {
		out[i] = measurementToDomain(&rows[i])
	}
	return out, nil
}

func (r *MeasurementPostgresRepository) GetActiveByMoment(tx sharedDomain.Transaction, participantID uuid.UUID, moment string) (*measurementDomain.Measurement, bool, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var row models.MeasurementModel
	err := gormTx.
		Where("participant_id = ? AND moment = ? AND deleted_at IS NULL AND superseded_at IS NULL",
			participantID, moment).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return measurementToDomain(&row), true, nil
}

func (r *MeasurementPostgresRepository) CountByChallenge(tx sharedDomain.Transaction, challengeID uuid.UUID) (int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	var n int64
	err := gormTx.Raw(
		`SELECT COUNT(1) FROM challenge_measurements m
		 JOIN challenge_participants p ON p.id = m.participant_id
		 WHERE p.challenge_id = ? AND m.deleted_at IS NULL`, challengeID).Scan(&n).Error
	return int(n), err
}

// ─── mappers ───────────────────────────────────────────────────────────────

func measurementToModel(m *measurementDomain.Measurement) models.MeasurementModel {
	return models.MeasurementModel{
		ID:              m.ID,
		GymID:           m.GymID,
		ParticipantID:   m.ParticipantID,
		Version:         m.Version,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
		DeletedAt:       m.DeletedAt,
		Moment:          m.Moment,
		MeasuredAt:      m.MeasuredAt,
		BodyWeightKg:    m.BodyWeightKg,
		BodyFatPct:      m.BodyFatPct,
		LegsWeightKg:    m.LegsWeightKg,
		LegsReps:        m.LegsReps,
		PushWeightKg:    m.PushWeightKg,
		PushReps:        m.PushReps,
		PullWeightKg:    m.PullWeightKg,
		PullReps:        m.PullReps,
		Notes:           m.Notes,
		CreatedByUserID: m.CreatedByUserID,
		SupersededAt:    m.SupersededAt,
		SupersededByID:  m.SupersededByID,
	}
}

func measurementToDomain(m *models.MeasurementModel) *measurementDomain.Measurement {
	return &measurementDomain.Measurement{
		ID:              m.ID,
		GymID:           m.GymID,
		ParticipantID:   m.ParticipantID,
		Version:         m.Version,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
		DeletedAt:       m.DeletedAt,
		Moment:          m.Moment,
		MeasuredAt:      m.MeasuredAt,
		BodyWeightKg:    m.BodyWeightKg,
		BodyFatPct:      m.BodyFatPct,
		LegsWeightKg:    m.LegsWeightKg,
		LegsReps:        m.LegsReps,
		PushWeightKg:    m.PushWeightKg,
		PushReps:        m.PushReps,
		PullWeightKg:    m.PullWeightKg,
		PullReps:        m.PullReps,
		Notes:           m.Notes,
		CreatedByUserID: m.CreatedByUserID,
		SupersededAt:    m.SupersededAt,
		SupersededByID:  m.SupersededByID,
	}
}
