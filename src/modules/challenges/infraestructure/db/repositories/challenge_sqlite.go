//go:build sidecar

// Package repositories — sidecar (SQLite) mirror of the Postgres repos.
// Same surface, same semantics; only the storage engine + the sync_queue
// enqueue differ. Build tag isolates these from the server build.
package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	challengeDomain "github.com/cuadra/cuadra-core/src/modules/challenges/domain/challenge"
	challengeErrors "github.com/cuadra/cuadra-core/src/modules/challenges/domain/errors"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type ChallengeSQLiteRepository struct{}

func NewChallengeSQLiteRepository() *ChallengeSQLiteRepository {
	return &ChallengeSQLiteRepository{}
}

type sqliteChallengeRow struct {
	ID                    string         `db:"id"`
	GymID                 string         `db:"gym_id"`
	Version               int            `db:"version"`
	CreatedAt             int64          `db:"created_at"`
	UpdatedAt             int64          `db:"updated_at"`
	DeletedAt             sql.NullInt64  `db:"deleted_at"`
	SyncedAt              sql.NullInt64  `db:"synced_at"`
	Name                  string         `db:"name"`
	Description           sql.NullString `db:"description"`
	StartsAt              int64          `db:"starts_at"`
	MeasurementT0Deadline int64          `db:"measurement_t0_deadline"`
	MeasurementT1Start    int64          `db:"measurement_t1_start"`
	EndsAt                int64          `db:"ends_at"`
	Status                string         `db:"status"`
	InscriptionFeeCents   int            `db:"inscription_fee_cents"`
	InscriptionRefundable int            `db:"inscription_refundable"`
	MinWeeklyAttendance   int            `db:"min_weekly_attendance"`
	AttendanceGraceWeeks  int            `db:"attendance_grace_weeks"`
	StrengthCapPct        float64        `db:"strength_cap_pct"`
	TieMarginIR           float64        `db:"tie_margin_ir"`
	BFFloorMalePct        float64        `db:"bf_floor_male_pct"`
	BFFloorFemalePct      float64        `db:"bf_floor_female_pct"`
}

func (r *ChallengeSQLiteRepository) Create(tx sharedDomain.Transaction, c *challengeDomain.Challenge) (*challengeDomain.Challenge, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := challengeToRow(c)
	const stmt = `
		INSERT INTO challenges (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    name, description, starts_at, measurement_t0_deadline, measurement_t1_start, ends_at,
		    status, inscription_fee_cents, inscription_refundable,
		    min_weekly_attendance, attendance_grace_weeks,
		    strength_cap_pct, tie_margin_ir, bf_floor_male_pct, bf_floor_female_pct
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :name, :description, :starts_at, :measurement_t0_deadline, :measurement_t1_start, :ends_at,
		    :status, :inscription_fee_cents, :inscription_refundable,
		    :min_weekly_attendance, :attendance_grace_weeks,
		    :strength_cap_pct, :tie_margin_ir, :bf_floor_male_pct, :bf_floor_female_pct
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueChallenge(stx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *ChallengeSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*challengeDomain.Challenge, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteChallengeRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM challenges WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(challengeErrors.ErrChallengeNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return challengeFromRow(&row), nil
}

func (r *ChallengeSQLiteRepository) Update(tx sharedDomain.Transaction, c *challengeDomain.Challenge) (*challengeDomain.Challenge, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := challengeToRow(c)
	const stmt = `
		UPDATE challenges SET
		    version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
		    name = :name, description = :description,
		    starts_at = :starts_at, measurement_t0_deadline = :measurement_t0_deadline,
		    measurement_t1_start = :measurement_t1_start, ends_at = :ends_at,
		    status = :status,
		    inscription_fee_cents = :inscription_fee_cents,
		    inscription_refundable = :inscription_refundable,
		    min_weekly_attendance = :min_weekly_attendance,
		    attendance_grace_weeks = :attendance_grace_weeks,
		    strength_cap_pct = :strength_cap_pct,
		    tie_margin_ir = :tie_margin_ir,
		    bf_floor_male_pct = :bf_floor_male_pct,
		    bf_floor_female_pct = :bf_floor_female_pct
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueChallenge(stx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *ChallengeSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, statusFilter string) ([]*challengeDomain.Challenge, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	q := `SELECT * FROM challenges WHERE gym_id = ? AND deleted_at IS NULL`
	args := []any{gymID.String()}
	if statusFilter != "" {
		q += ` AND status = ?`
		args = append(args, statusFilter)
	}
	q += ` ORDER BY created_at DESC`
	var rows []sqliteChallengeRow
	if err := stx.Select(context.Background(), &rows, q, args...); err != nil {
		return nil, err
	}
	out := make([]*challengeDomain.Challenge, len(rows))
	for i := range rows {
		out[i] = challengeFromRow(&rows[i])
	}
	return out, nil
}

// ─── mappers ───────────────────────────────────────────────────────────────

func challengeToRow(c *challengeDomain.Challenge) sqliteChallengeRow {
	row := sqliteChallengeRow{
		ID:                    c.ID.String(),
		GymID:                 c.GymID.String(),
		Version:               c.Version,
		CreatedAt:             c.CreatedAt.UnixMilli(),
		UpdatedAt:             c.UpdatedAt.UnixMilli(),
		Name:                  c.Name,
		StartsAt:              c.StartsAt.UnixMilli(),
		MeasurementT0Deadline: c.MeasurementT0Deadline.UnixMilli(),
		MeasurementT1Start:    c.MeasurementT1Start.UnixMilli(),
		EndsAt:                c.EndsAt.UnixMilli(),
		Status:                c.Status,
		InscriptionFeeCents:   c.InscriptionFeeCents,
		InscriptionRefundable: boolToInt(c.InscriptionRefundable),
		MinWeeklyAttendance:   c.MinWeeklyAttendance,
		AttendanceGraceWeeks:  c.AttendanceGraceWeeks,
		StrengthCapPct:        c.StrengthCapPct,
		TieMarginIR:           c.TieMarginIR,
		BFFloorMalePct:        c.BFFloorMalePct,
		BFFloorFemalePct:      c.BFFloorFemalePct,
	}
	if c.Description != "" {
		row.Description = sql.NullString{String: c.Description, Valid: true}
	}
	if c.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: c.DeletedAt.UnixMilli(), Valid: true}
	}
	return row
}

func challengeFromRow(r *sqliteChallengeRow) *challengeDomain.Challenge {
	id, _ := uuid.Parse(r.ID)
	gid, _ := uuid.Parse(r.GymID)
	c := &challengeDomain.Challenge{
		ID:                    id,
		GymID:                 gid,
		Version:               r.Version,
		CreatedAt:             time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:             time.UnixMilli(r.UpdatedAt).UTC(),
		Name:                  r.Name,
		StartsAt:              time.UnixMilli(r.StartsAt).UTC(),
		MeasurementT0Deadline: time.UnixMilli(r.MeasurementT0Deadline).UTC(),
		MeasurementT1Start:    time.UnixMilli(r.MeasurementT1Start).UTC(),
		EndsAt:                time.UnixMilli(r.EndsAt).UTC(),
		Status:                r.Status,
		InscriptionFeeCents:   r.InscriptionFeeCents,
		InscriptionRefundable: r.InscriptionRefundable != 0,
		MinWeeklyAttendance:   r.MinWeeklyAttendance,
		AttendanceGraceWeeks:  r.AttendanceGraceWeeks,
		StrengthCapPct:        r.StrengthCapPct,
		TieMarginIR:           r.TieMarginIR,
		BFFloorMalePct:        r.BFFloorMalePct,
		BFFloorFemalePct:      r.BFFloorFemalePct,
	}
	if r.Description.Valid {
		c.Description = r.Description.String
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		c.DeletedAt = &t
	}
	return c
}

func enqueueChallenge(stx *sharedDomain.SqlxTransaction, c *challengeDomain.Challenge) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":                      c.ID.String(),
		"gym_id":                  c.GymID.String(),
		"version":                 c.Version,
		"created_at":              c.CreatedAt.UnixMilli(),
		"updated_at":              c.UpdatedAt.UnixMilli(),
		"deleted_at":              nullableUnixMs(c.DeletedAt),
		"name":                    c.Name,
		"description":             stringOrNil(c.Description),
		"starts_at":               c.StartsAt.UnixMilli(),
		"measurement_t0_deadline": c.MeasurementT0Deadline.UnixMilli(),
		"measurement_t1_start":    c.MeasurementT1Start.UnixMilli(),
		"ends_at":                 c.EndsAt.UnixMilli(),
		"status":                  c.Status,
		"inscription_fee_cents":   c.InscriptionFeeCents,
		"inscription_refundable":  c.InscriptionRefundable,
		"min_weekly_attendance":   c.MinWeeklyAttendance,
		"attendance_grace_weeks":  c.AttendanceGraceWeeks,
		"strength_cap_pct":        c.StrengthCapPct,
		"tie_margin_ir":           c.TieMarginIR,
		"bf_floor_male_pct":       c.BFFloorMalePct,
		"bf_floor_female_pct":     c.BFFloorFemalePct,
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "challenges", c.ID.String(), "upsert", payload, c.Version)
}

// ─── shared sidecar helpers (used by every challenges sqlite repo) ─────────

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullableUnixMs(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func stringOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableUUIDString(p *uuid.UUID) any {
	if p == nil {
		return nil
	}
	return p.String()
}
