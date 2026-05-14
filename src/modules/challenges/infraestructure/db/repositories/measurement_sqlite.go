//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	measurementDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/measurement"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type MeasurementSQLiteRepository struct{}

func NewMeasurementSQLiteRepository() *MeasurementSQLiteRepository {
	return &MeasurementSQLiteRepository{}
}

type sqliteMeasurementRow struct {
	ID              string         `db:"id"`
	GymID           string         `db:"gym_id"`
	ParticipantID   string         `db:"participant_id"`
	Version         int            `db:"version"`
	CreatedAt       int64          `db:"created_at"`
	UpdatedAt       int64          `db:"updated_at"`
	DeletedAt       sql.NullInt64  `db:"deleted_at"`
	SyncedAt        sql.NullInt64  `db:"synced_at"`
	Moment          string         `db:"moment"`
	MeasuredAt      int64          `db:"measured_at"`
	BodyWeightKg    float64        `db:"body_weight_kg"`
	BodyFatPct      float64        `db:"body_fat_pct"`
	LegsWeightKg    float64        `db:"legs_weight_kg"`
	LegsReps        int            `db:"legs_reps"`
	PushWeightKg    float64        `db:"push_weight_kg"`
	PushReps        int            `db:"push_reps"`
	PullWeightKg    float64        `db:"pull_weight_kg"`
	PullReps        int            `db:"pull_reps"`
	Notes           sql.NullString `db:"notes"`
	CreatedByUserID string         `db:"created_by_user_id"`
	SupersededAt    sql.NullInt64  `db:"superseded_at"`
	SupersededByID  sql.NullString `db:"superseded_by_id"`
}

func (r *MeasurementSQLiteRepository) Create(tx sharedDomain.Transaction, m *measurementDomain.Measurement) (*measurementDomain.Measurement, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := measurementToRow(m)
	const stmt = `
		INSERT INTO challenge_measurements (
		    id, gym_id, participant_id, version, created_at, updated_at, deleted_at,
		    moment, measured_at,
		    body_weight_kg, body_fat_pct,
		    legs_weight_kg, legs_reps,
		    push_weight_kg, push_reps,
		    pull_weight_kg, pull_reps,
		    notes, created_by_user_id,
		    superseded_at, superseded_by_id
		) VALUES (
		    :id, :gym_id, :participant_id, :version, :created_at, :updated_at, :deleted_at,
		    :moment, :measured_at,
		    :body_weight_kg, :body_fat_pct,
		    :legs_weight_kg, :legs_reps,
		    :push_weight_kg, :push_reps,
		    :pull_weight_kg, :pull_reps,
		    :notes, :created_by_user_id,
		    :superseded_at, :superseded_by_id
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueMeasurement(stx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *MeasurementSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*measurementDomain.Measurement, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteMeasurementRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM challenge_measurements WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrMeasurementNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return measurementFromRow(&row), nil
}

// Supersede mutates the prior row in place. We re-read it after the update so
// the sync payload reflects the bumped version + superseded link.
func (r *MeasurementSQLiteRepository) Supersede(tx sharedDomain.Transaction, priorID, replacementID uuid.UUID, at time.Time) error {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if _, err := stx.Exec(context.Background(),
		`UPDATE challenge_measurements
		 SET superseded_at = ?, superseded_by_id = ?, updated_at = ?, version = version + 1
		 WHERE id = ? AND deleted_at IS NULL AND superseded_at IS NULL`,
		at.UnixMilli(), replacementID.String(), at.UnixMilli(), priorID.String()); err != nil {
		return err
	}
	// Re-read so the sync payload matches the new on-disk state. The prior
	// row's mutation is part of the same logical write — its successor
	// references it, and clients pulling the delta need both rows updated.
	prior, err := r.GetByID(tx, priorID)
	if err != nil {
		return err
	}
	return enqueueMeasurement(stx, prior)
}

func (r *MeasurementSQLiteRepository) ListByParticipant(tx sharedDomain.Transaction, participantID uuid.UUID) ([]*measurementDomain.Measurement, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteMeasurementRow
	err := stx.Select(context.Background(), &rows,
		`SELECT * FROM challenge_measurements
		 WHERE participant_id = ? AND deleted_at IS NULL
		 ORDER BY created_at DESC`, participantID.String())
	if err != nil {
		return nil, err
	}
	out := make([]*measurementDomain.Measurement, len(rows))
	for i := range rows {
		out[i] = measurementFromRow(&rows[i])
	}
	return out, nil
}

func (r *MeasurementSQLiteRepository) GetActiveByMoment(tx sharedDomain.Transaction, participantID uuid.UUID, moment string) (*measurementDomain.Measurement, bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteMeasurementRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM challenge_measurements
		 WHERE participant_id = ? AND moment = ?
		   AND deleted_at IS NULL AND superseded_at IS NULL
		 LIMIT 1`, participantID.String(), moment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return measurementFromRow(&row), true, nil
}

func (r *MeasurementSQLiteRepository) CountByChallenge(tx sharedDomain.Transaction, challengeID uuid.UUID) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM challenge_measurements m
		 JOIN challenge_participants p ON p.id = m.participant_id
		 WHERE p.challenge_id = ? AND m.deleted_at IS NULL`, challengeID.String())
	return n, err
}

// ─── mappers ───────────────────────────────────────────────────────────────

func measurementToRow(m *measurementDomain.Measurement) sqliteMeasurementRow {
	row := sqliteMeasurementRow{
		ID:              m.ID.String(),
		GymID:           m.GymID.String(),
		ParticipantID:   m.ParticipantID.String(),
		Version:         m.Version,
		CreatedAt:       m.CreatedAt.UnixMilli(),
		UpdatedAt:       m.UpdatedAt.UnixMilli(),
		Moment:          m.Moment,
		MeasuredAt:      m.MeasuredAt.UnixMilli(),
		BodyWeightKg:    m.BodyWeightKg,
		BodyFatPct:      m.BodyFatPct,
		LegsWeightKg:    m.LegsWeightKg,
		LegsReps:        m.LegsReps,
		PushWeightKg:    m.PushWeightKg,
		PushReps:        m.PushReps,
		PullWeightKg:    m.PullWeightKg,
		PullReps:        m.PullReps,
		CreatedByUserID: m.CreatedByUserID.String(),
	}
	if m.Notes != "" {
		row.Notes = sql.NullString{String: m.Notes, Valid: true}
	}
	if m.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: m.DeletedAt.UnixMilli(), Valid: true}
	}
	if m.SupersededAt != nil {
		row.SupersededAt = sql.NullInt64{Int64: m.SupersededAt.UnixMilli(), Valid: true}
	}
	if m.SupersededByID != nil {
		row.SupersededByID = sql.NullString{String: m.SupersededByID.String(), Valid: true}
	}
	return row
}

func measurementFromRow(r *sqliteMeasurementRow) *measurementDomain.Measurement {
	id, _ := uuid.Parse(r.ID)
	gid, _ := uuid.Parse(r.GymID)
	pid, _ := uuid.Parse(r.ParticipantID)
	uid, _ := uuid.Parse(r.CreatedByUserID)
	m := &measurementDomain.Measurement{
		ID:              id,
		GymID:           gid,
		ParticipantID:   pid,
		Version:         r.Version,
		CreatedAt:       time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:       time.UnixMilli(r.UpdatedAt).UTC(),
		Moment:          r.Moment,
		MeasuredAt:      time.UnixMilli(r.MeasuredAt).UTC(),
		BodyWeightKg:    r.BodyWeightKg,
		BodyFatPct:      r.BodyFatPct,
		LegsWeightKg:    r.LegsWeightKg,
		LegsReps:        r.LegsReps,
		PushWeightKg:    r.PushWeightKg,
		PushReps:        r.PushReps,
		PullWeightKg:    r.PullWeightKg,
		PullReps:        r.PullReps,
		CreatedByUserID: uid,
	}
	if r.Notes.Valid {
		m.Notes = r.Notes.String
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		m.DeletedAt = &t
	}
	if r.SupersededAt.Valid {
		t := time.UnixMilli(r.SupersededAt.Int64).UTC()
		m.SupersededAt = &t
	}
	if r.SupersededByID.Valid {
		sid, _ := uuid.Parse(r.SupersededByID.String)
		m.SupersededByID = &sid
	}
	return m
}

func enqueueMeasurement(stx *sharedDomain.SqlxTransaction, m *measurementDomain.Measurement) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":                 m.ID.String(),
		"gym_id":             m.GymID.String(),
		"participant_id":     m.ParticipantID.String(),
		"version":            m.Version,
		"created_at":         m.CreatedAt.UnixMilli(),
		"updated_at":         m.UpdatedAt.UnixMilli(),
		"deleted_at":         nullableUnixMs(m.DeletedAt),
		"moment":             m.Moment,
		"measured_at":        m.MeasuredAt.UnixMilli(),
		"body_weight_kg":     m.BodyWeightKg,
		"body_fat_pct":       m.BodyFatPct,
		"legs_weight_kg":     m.LegsWeightKg,
		"legs_reps":          m.LegsReps,
		"push_weight_kg":     m.PushWeightKg,
		"push_reps":          m.PushReps,
		"pull_weight_kg":     m.PullWeightKg,
		"pull_reps":          m.PullReps,
		"notes":              stringOrNil(m.Notes),
		"created_by_user_id": m.CreatedByUserID.String(),
		"superseded_at":      nullableUnixMs(m.SupersededAt),
		"superseded_by_id":   nullableUUIDString(m.SupersededByID),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "challenge_measurements", m.ID.String(), "upsert", payload, m.Version)
}
