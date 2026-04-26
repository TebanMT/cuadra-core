//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	checkinDomain "github.com/cuadra/cuadra-core/src/modules/checkins/domain/checkin"
	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CheckinSQLiteRepository struct{}

func NewCheckinSQLiteRepository() *CheckinSQLiteRepository {
	return &CheckinSQLiteRepository{}
}

type sqliteCheckinRow struct {
	ID             string         `db:"id"`
	GymID          string         `db:"gym_id"`
	Version        int            `db:"version"`
	CreatedAt      int64          `db:"created_at"`
	UpdatedAt      int64          `db:"updated_at"`
	DeletedAt      sql.NullInt64  `db:"deleted_at"`
	SyncedAt       sql.NullInt64  `db:"synced_at"`
	MemberID       string         `db:"member_id"`
	CheckinAt      int64          `db:"checkin_at"`
	Method         string         `db:"method"`
	Result         string         `db:"result"`
	OperatorID     sql.NullString `db:"operator_id"`
	ManualOverride int            `db:"manual_override"`
	OverrideReason sql.NullString `db:"override_reason"`
}

func (r *CheckinSQLiteRepository) Create(tx sharedDomain.Transaction, c *checkinDomain.Checkin) (*checkinDomain.Checkin, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := checkinToRow(c)
	const stmt = `
		INSERT INTO checkins (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    member_id, checkin_at, method, result,
		    operator_id, manual_override, override_reason
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :member_id, :checkin_at, :method, :result,
		    :operator_id, :manual_override, :override_reason
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueCheckin(stx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *CheckinSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*checkinDomain.Checkin, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteCheckinRow
	err := stx.Get(context.Background(), &row,
		`SELECT id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
		        member_id, checkin_at, method, result, operator_id, manual_override, override_reason
		 FROM checkins
		 WHERE id = ? AND deleted_at IS NULL`,
		id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(chkErrors.ErrCheckinNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return checkinFromRow(&row), nil
}

func (r *CheckinSQLiteRepository) ListByMember(tx sharedDomain.Transaction, memberID uuid.UUID, since time.Time, limit int) ([]*checkinDomain.Checkin, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	args := []any{memberID.String()}
	q := `SELECT id, gym_id, version, created_at, updated_at, deleted_at, synced_at,
	             member_id, checkin_at, method, result, operator_id, manual_override, override_reason
	      FROM checkins
	      WHERE member_id = ? AND deleted_at IS NULL`
	if !since.IsZero() {
		q += ` AND checkin_at >= ?`
		args = append(args, since.UnixMilli())
	}
	q += ` ORDER BY checkin_at DESC LIMIT ?`
	args = append(args, limit)
	var rows []sqliteCheckinRow
	if err := stx.Select(context.Background(), &rows, q, args...); err != nil {
		return nil, err
	}
	out := make([]*checkinDomain.Checkin, 0, len(rows))
	for i := range rows {
		out = append(out, checkinFromRow(&rows[i]))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func checkinToRow(c *checkinDomain.Checkin) sqliteCheckinRow {
	row := sqliteCheckinRow{
		ID:        c.ID.String(),
		GymID:     c.GymID.String(),
		Version:   c.Version,
		CreatedAt: c.CreatedAt.UnixMilli(),
		UpdatedAt: c.UpdatedAt.UnixMilli(),
		MemberID:  c.MemberID.String(),
		CheckinAt: c.CheckinAt.UnixMilli(),
		Method:    c.Method,
		Result:    c.Result,
	}
	if c.ManualOverride {
		row.ManualOverride = 1
	}
	if c.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: c.DeletedAt.UnixMilli(), Valid: true}
	}
	if c.OperatorID != nil {
		row.OperatorID = sql.NullString{String: c.OperatorID.String(), Valid: true}
	}
	if c.OverrideReason != nil {
		row.OverrideReason = sql.NullString{String: *c.OverrideReason, Valid: true}
	}
	return row
}

func checkinFromRow(r *sqliteCheckinRow) *checkinDomain.Checkin {
	id, _ := uuid.Parse(r.ID)
	gid, _ := uuid.Parse(r.GymID)
	mid, _ := uuid.Parse(r.MemberID)
	c := &checkinDomain.Checkin{
		ID:             id,
		GymID:          gid,
		Version:        r.Version,
		MemberID:       mid,
		CheckinAt:      time.UnixMilli(r.CheckinAt).UTC(),
		Method:         r.Method,
		Result:         r.Result,
		ManualOverride: r.ManualOverride != 0,
		CreatedAt:      time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:      time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.OperatorID.Valid {
		op, _ := uuid.Parse(r.OperatorID.String)
		c.OperatorID = &op
	}
	if r.OverrideReason.Valid {
		v := r.OverrideReason.String
		c.OverrideReason = &v
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		c.DeletedAt = &t
	}
	return c
}

func enqueueCheckin(stx *sharedDomain.SqlxTransaction, c *checkinDomain.Checkin) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":              c.ID.String(),
		"gym_id":          c.GymID.String(),
		"version":         c.Version,
		"member_id":       c.MemberID.String(),
		"checkin_at":      c.CheckinAt.UnixMilli(),
		"method":          c.Method,
		"result":          c.Result,
		"operator_id":     nullableUUIDString(c.OperatorID),
		"manual_override": c.ManualOverride,
		"override_reason": c.OverrideReason,
		"updated_at":      c.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "checkins", c.ID.String(), "upsert", payload, c.Version)
}

func nullableUUIDString(p *uuid.UUID) any {
	if p == nil {
		return nil
	}
	return p.String()
}
