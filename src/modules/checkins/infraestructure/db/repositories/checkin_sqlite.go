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
	chkRepo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

const sqliteCheckinDateFmt = "2006-01-02"

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

func (r *CheckinSQLiteRepository) CountTodayByGym(tx sharedDomain.Transaction, gymID uuid.UUID, today time.Time) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	start := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	end := start + int64(24*time.Hour/time.Millisecond)
	var n int
	err := stx.Get(context.Background(), &n, `
		SELECT COUNT(1) FROM checkins
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND result LIKE 'allowed%'
		  AND checkin_at >= ? AND checkin_at < ?`,
		gymID.String(), start, end)
	return n, err
}

func (r *CheckinSQLiteRepository) ListRecentByGym(tx sharedDomain.Transaction, gymID uuid.UUID, limit int) ([]chkRepo.RecentCheckinRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	type row struct {
		ID             string         `db:"id"`
		MemberID       sql.NullString `db:"member_id"`
		MemberName     sql.NullString `db:"member_name"`
		Method         string         `db:"method"`
		Result         string         `db:"result"`
		ManualOverride int            `db:"manual_override"`
		OverrideReason sql.NullString `db:"override_reason"`
		OperatorName   sql.NullString `db:"operator_name"`
		CheckinAt      int64          `db:"checkin_at"`
		ExpiryDate     sql.NullString `db:"expiry_date"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
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
		LIMIT ?`, gymID.String(), limit); err != nil {
		return nil, err
	}
	out := make([]chkRepo.RecentCheckinRow, 0, len(rows))
	for _, x := range rows {
		id, _ := uuid.Parse(x.ID)
		entry := chkRepo.RecentCheckinRow{
			ID:             id,
			Method:         x.Method,
			Result:         x.Result,
			ManualOverride: x.ManualOverride == 1,
			CheckinAt:      time.UnixMilli(x.CheckinAt).UTC(),
		}
		if x.MemberID.Valid {
			mid, _ := uuid.Parse(x.MemberID.String)
			entry.MemberID = &mid
		}
		if x.MemberName.Valid {
			v := x.MemberName.String
			entry.MemberName = &v
		}
		if x.OverrideReason.Valid {
			v := x.OverrideReason.String
			entry.OverrideReason = &v
		}
		if x.OperatorName.Valid {
			v := x.OperatorName.String
			entry.OperatorName = &v
		}
		if x.ExpiryDate.Valid {
			t, err := time.Parse(sqliteCheckinDateFmt, x.ExpiryDate.String)
			if err == nil {
				entry.ExpiryDate = &t
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// ListByGymBetween — drill-down: check-ins en rango [from,to] al día.
// Misma shape de salida que ListRecentByGym.
func (r *CheckinSQLiteRepository) ListByGymBetween(tx sharedDomain.Transaction, gymID uuid.UUID, from, to time.Time, limit int) ([]chkRepo.RecentCheckinRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	fromMs := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC).UnixMilli()
	toMs := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1).UnixMilli()
	type row struct {
		ID             string         `db:"id"`
		MemberID       sql.NullString `db:"member_id"`
		MemberName     sql.NullString `db:"member_name"`
		Method         string         `db:"method"`
		Result         string         `db:"result"`
		ManualOverride int            `db:"manual_override"`
		OverrideReason sql.NullString `db:"override_reason"`
		OperatorName   sql.NullString `db:"operator_name"`
		CheckinAt      int64          `db:"checkin_at"`
		ExpiryDate     sql.NullString `db:"expiry_date"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
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
		LIMIT ?`, gymID.String(), fromMs, toMs, limit); err != nil {
		return nil, err
	}
	out := make([]chkRepo.RecentCheckinRow, 0, len(rows))
	for _, x := range rows {
		id, _ := uuid.Parse(x.ID)
		entry := chkRepo.RecentCheckinRow{
			ID:             id,
			Method:         x.Method,
			Result:         x.Result,
			ManualOverride: x.ManualOverride == 1,
			CheckinAt:      time.UnixMilli(x.CheckinAt).UTC(),
		}
		if x.MemberID.Valid {
			mid, _ := uuid.Parse(x.MemberID.String)
			entry.MemberID = &mid
		}
		if x.MemberName.Valid {
			v := x.MemberName.String
			entry.MemberName = &v
		}
		if x.OverrideReason.Valid {
			v := x.OverrideReason.String
			entry.OverrideReason = &v
		}
		if x.OperatorName.Valid {
			v := x.OperatorName.String
			entry.OperatorName = &v
		}
		if x.ExpiryDate.Valid {
			t, err := time.Parse(sqliteCheckinDateFmt, x.ExpiryDate.String)
			if err == nil {
				entry.ExpiryDate = &t
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

func (r *CheckinSQLiteRepository) ListByMemberDetailed(tx sharedDomain.Transaction, gymID, memberID uuid.UUID, limit int) ([]chkRepo.RecentCheckinRow, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	type row struct {
		ID             string         `db:"id"`
		MemberID       sql.NullString `db:"member_id"`
		Method         string         `db:"method"`
		Result         string         `db:"result"`
		ManualOverride int            `db:"manual_override"`
		OverrideReason sql.NullString `db:"override_reason"`
		OperatorName   sql.NullString `db:"operator_name"`
		CheckinAt      int64          `db:"checkin_at"`
	}
	var rows []row
	if err := stx.Select(context.Background(), &rows, `
		SELECT c.id, c.member_id,
		       c.method, c.result, c.manual_override, c.override_reason,
		       u.full_name AS operator_name,
		       c.checkin_at
		FROM checkins c
		LEFT JOIN users u ON u.id = c.operator_id AND u.deleted_at IS NULL
		WHERE c.gym_id = ? AND c.member_id = ? AND c.deleted_at IS NULL
		ORDER BY c.checkin_at DESC
		LIMIT ?`, gymID.String(), memberID.String(), limit); err != nil {
		return nil, err
	}
	out := make([]chkRepo.RecentCheckinRow, 0, len(rows))
	for _, x := range rows {
		id, _ := uuid.Parse(x.ID)
		entry := chkRepo.RecentCheckinRow{
			ID:             id,
			Method:         x.Method,
			Result:         x.Result,
			ManualOverride: x.ManualOverride == 1,
			CheckinAt:      time.UnixMilli(x.CheckinAt).UTC(),
		}
		if x.MemberID.Valid {
			mid, _ := uuid.Parse(x.MemberID.String)
			entry.MemberID = &mid
		}
		if x.OverrideReason.Valid {
			v := x.OverrideReason.String
			entry.OverrideReason = &v
		}
		if x.OperatorName.Valid {
			v := x.OperatorName.String
			entry.OperatorName = &v
		}
		out = append(out, entry)
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
	// All NOT NULL columns must be in the payload — the cloud projector's
	// UPSERT only emits columns present in the map, and a missing required
	// column on first-sight INSERT triggers a 23502 NOT NULL violation.
	payload, err := json.Marshal(map[string]any{
		"id":              c.ID.String(),
		"gym_id":          c.GymID.String(),
		"version":         c.Version,
		"created_at":      c.CreatedAt.UnixMilli(),
		"updated_at":      c.UpdatedAt.UnixMilli(),
		"member_id":       c.MemberID.String(),
		"checkin_at":      c.CheckinAt.UnixMilli(),
		"method":          c.Method,
		"result":          c.Result,
		"operator_id":     nullableUUIDString(c.OperatorID),
		"manual_override": c.ManualOverride,
		"override_reason": strPtrOrNil(c.OverrideReason),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "checkins", c.ID.String(), "upsert", payload, c.Version)
}

// strPtrOrNil returns the dereferenced string for non-nil pointers and
// untyped nil otherwise. Using nil instead of "" for nullable Postgres
// columns avoids relying on the projector's empty-string nullification.
func strPtrOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableUUIDString(p *uuid.UUID) any {
	if p == nil {
		return nil
	}
	return p.String()
}
