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
	participantDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/participant"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type ParticipantSQLiteRepository struct{}

func NewParticipantSQLiteRepository() *ParticipantSQLiteRepository {
	return &ParticipantSQLiteRepository{}
}

type sqliteParticipantRow struct {
	ID                     string         `db:"id"`
	GymID                  string         `db:"gym_id"`
	ChallengeID            string         `db:"challenge_id"`
	MemberID               string         `db:"member_id"`
	CategoryID             string         `db:"category_id"`
	Version                int            `db:"version"`
	CreatedAt              int64          `db:"created_at"`
	UpdatedAt              int64          `db:"updated_at"`
	DeletedAt              sql.NullInt64  `db:"deleted_at"`
	SyncedAt               sql.NullInt64  `db:"synced_at"`
	ExerciseLegs           sql.NullString `db:"exercise_legs"`
	ExercisePush           sql.NullString `db:"exercise_push"`
	ExercisePull           sql.NullString `db:"exercise_pull"`
	InscriptionFeePaid     int            `db:"inscription_fee_paid"`
	InscriptionPaidAt      sql.NullInt64  `db:"inscription_paid_at"`
	InscriptionRefundedAt  sql.NullInt64  `db:"inscription_refunded_at"`
	Status                 string         `db:"status"`
	DisqualificationReason sql.NullString `db:"disqualification_reason"`
	DisqualifiedAt         sql.NullInt64  `db:"disqualified_at"`
}

func (r *ParticipantSQLiteRepository) Create(tx sharedDomain.Transaction, p *participantDomain.Participant) (*participantDomain.Participant, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := participantToRow(p)
	const stmt = `
		INSERT INTO challenge_participants (
		    id, gym_id, challenge_id, member_id, category_id,
		    version, created_at, updated_at, deleted_at,
		    exercise_legs, exercise_push, exercise_pull,
		    inscription_fee_paid, inscription_paid_at, inscription_refunded_at,
		    status, disqualification_reason, disqualified_at
		) VALUES (
		    :id, :gym_id, :challenge_id, :member_id, :category_id,
		    :version, :created_at, :updated_at, :deleted_at,
		    :exercise_legs, :exercise_push, :exercise_pull,
		    :inscription_fee_paid, :inscription_paid_at, :inscription_refunded_at,
		    :status, :disqualification_reason, :disqualified_at
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueParticipant(stx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *ParticipantSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*participantDomain.Participant, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteParticipantRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM challenge_participants WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrParticipantNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return participantFromRow(&row), nil
}

func (r *ParticipantSQLiteRepository) Update(tx sharedDomain.Transaction, p *participantDomain.Participant) (*participantDomain.Participant, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := participantToRow(p)
	const stmt = `
		UPDATE challenge_participants SET
		    version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
		    category_id = :category_id,
		    exercise_legs = :exercise_legs, exercise_push = :exercise_push, exercise_pull = :exercise_pull,
		    inscription_fee_paid = :inscription_fee_paid,
		    inscription_paid_at = :inscription_paid_at,
		    inscription_refunded_at = :inscription_refunded_at,
		    status = :status,
		    disqualification_reason = :disqualification_reason,
		    disqualified_at = :disqualified_at
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueParticipant(stx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *ParticipantSQLiteRepository) SoftDelete(tx sharedDomain.Transaction, id uuid.UUID) error {
	stx := tx.(*sharedDomain.SqlxTransaction)
	p, err := r.GetByID(tx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	p.DeletedAt = &now
	p.UpdatedAt = now
	p.Version++
	if _, err := stx.Exec(context.Background(),
		`UPDATE challenge_participants
		 SET deleted_at = ?, updated_at = ?, version = ?
		 WHERE id = ?`, now.UnixMilli(), now.UnixMilli(), p.Version, id.String()); err != nil {
		return err
	}
	return enqueueParticipant(stx, p)
}

func (r *ParticipantSQLiteRepository) ListByChallenge(tx sharedDomain.Transaction, challengeID uuid.UUID, statusFilter string, categoryFilter *uuid.UUID) ([]*participantDomain.Participant, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	q := `SELECT * FROM challenge_participants WHERE challenge_id = ? AND deleted_at IS NULL`
	args := []any{challengeID.String()}
	if statusFilter != "" {
		q += ` AND status = ?`
		args = append(args, statusFilter)
	}
	if categoryFilter != nil {
		q += ` AND category_id = ?`
		args = append(args, categoryFilter.String())
	}
	q += ` ORDER BY created_at ASC`
	var rows []sqliteParticipantRow
	if err := stx.Select(context.Background(), &rows, q, args...); err != nil {
		return nil, err
	}
	out := make([]*participantDomain.Participant, len(rows))
	for i := range rows {
		out[i] = participantFromRow(&rows[i])
	}
	return out, nil
}

func (r *ParticipantSQLiteRepository) ExistsByMember(tx sharedDomain.Transaction, challengeID, memberID uuid.UUID) (bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM challenge_participants
		 WHERE challenge_id = ? AND member_id = ? AND deleted_at IS NULL`,
		challengeID.String(), memberID.String())
	return n > 0, err
}

func (r *ParticipantSQLiteRepository) HasAnyMeasurement(tx sharedDomain.Transaction, participantID uuid.UUID) (bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM challenge_measurements
		 WHERE participant_id = ? AND deleted_at IS NULL`, participantID.String())
	return n > 0, err
}

// ─── mappers ───────────────────────────────────────────────────────────────

func participantToRow(p *participantDomain.Participant) sqliteParticipantRow {
	row := sqliteParticipantRow{
		ID:                 p.ID.String(),
		GymID:              p.GymID.String(),
		ChallengeID:        p.ChallengeID.String(),
		MemberID:           p.MemberID.String(),
		CategoryID:         p.CategoryID.String(),
		Version:            p.Version,
		CreatedAt:          p.CreatedAt.UnixMilli(),
		UpdatedAt:          p.UpdatedAt.UnixMilli(),
		InscriptionFeePaid: boolToInt(p.InscriptionFeePaid),
		Status:             p.Status,
	}
	if p.ExerciseLegs != "" {
		row.ExerciseLegs = sql.NullString{String: p.ExerciseLegs, Valid: true}
	}
	if p.ExercisePush != "" {
		row.ExercisePush = sql.NullString{String: p.ExercisePush, Valid: true}
	}
	if p.ExercisePull != "" {
		row.ExercisePull = sql.NullString{String: p.ExercisePull, Valid: true}
	}
	if p.DisqualificationReason != "" {
		row.DisqualificationReason = sql.NullString{String: p.DisqualificationReason, Valid: true}
	}
	if p.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: p.DeletedAt.UnixMilli(), Valid: true}
	}
	if p.InscriptionPaidAt != nil {
		row.InscriptionPaidAt = sql.NullInt64{Int64: p.InscriptionPaidAt.UnixMilli(), Valid: true}
	}
	if p.InscriptionRefundedAt != nil {
		row.InscriptionRefundedAt = sql.NullInt64{Int64: p.InscriptionRefundedAt.UnixMilli(), Valid: true}
	}
	if p.DisqualifiedAt != nil {
		row.DisqualifiedAt = sql.NullInt64{Int64: p.DisqualifiedAt.UnixMilli(), Valid: true}
	}
	return row
}

func participantFromRow(r *sqliteParticipantRow) *participantDomain.Participant {
	id, _ := uuid.Parse(r.ID)
	gid, _ := uuid.Parse(r.GymID)
	chid, _ := uuid.Parse(r.ChallengeID)
	mid, _ := uuid.Parse(r.MemberID)
	cid, _ := uuid.Parse(r.CategoryID)
	p := &participantDomain.Participant{
		ID:                 id,
		GymID:              gid,
		ChallengeID:        chid,
		MemberID:           mid,
		CategoryID:         cid,
		Version:            r.Version,
		CreatedAt:          time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:          time.UnixMilli(r.UpdatedAt).UTC(),
		InscriptionFeePaid: r.InscriptionFeePaid != 0,
		Status:             r.Status,
	}
	if r.ExerciseLegs.Valid {
		p.ExerciseLegs = r.ExerciseLegs.String
	}
	if r.ExercisePush.Valid {
		p.ExercisePush = r.ExercisePush.String
	}
	if r.ExercisePull.Valid {
		p.ExercisePull = r.ExercisePull.String
	}
	if r.DisqualificationReason.Valid {
		p.DisqualificationReason = r.DisqualificationReason.String
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		p.DeletedAt = &t
	}
	if r.InscriptionPaidAt.Valid {
		t := time.UnixMilli(r.InscriptionPaidAt.Int64).UTC()
		p.InscriptionPaidAt = &t
	}
	if r.InscriptionRefundedAt.Valid {
		t := time.UnixMilli(r.InscriptionRefundedAt.Int64).UTC()
		p.InscriptionRefundedAt = &t
	}
	if r.DisqualifiedAt.Valid {
		t := time.UnixMilli(r.DisqualifiedAt.Int64).UTC()
		p.DisqualifiedAt = &t
	}
	return p
}

func enqueueParticipant(stx *sharedDomain.SqlxTransaction, p *participantDomain.Participant) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":                      p.ID.String(),
		"gym_id":                  p.GymID.String(),
		"challenge_id":            p.ChallengeID.String(),
		"member_id":               p.MemberID.String(),
		"category_id":             p.CategoryID.String(),
		"version":                 p.Version,
		"created_at":              p.CreatedAt.UnixMilli(),
		"updated_at":              p.UpdatedAt.UnixMilli(),
		"deleted_at":              nullableUnixMs(p.DeletedAt),
		"exercise_legs":           stringOrNil(p.ExerciseLegs),
		"exercise_push":           stringOrNil(p.ExercisePush),
		"exercise_pull":           stringOrNil(p.ExercisePull),
		"inscription_fee_paid":    p.InscriptionFeePaid,
		"inscription_paid_at":     nullableUnixMs(p.InscriptionPaidAt),
		"inscription_refunded_at": nullableUnixMs(p.InscriptionRefundedAt),
		"status":                  p.Status,
		"disqualification_reason": stringOrNil(p.DisqualificationReason),
		"disqualified_at":         nullableUnixMs(p.DisqualifiedAt),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "challenge_participants", p.ID.String(), "upsert", payload, p.Version)
}
