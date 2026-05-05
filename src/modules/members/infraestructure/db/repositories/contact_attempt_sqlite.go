//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	caDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/contact_attempt"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type ContactAttemptSQLiteRepository struct{}

func NewContactAttemptSQLiteRepository() *ContactAttemptSQLiteRepository {
	return &ContactAttemptSQLiteRepository{}
}

type sqliteContactAttemptRow struct {
	ID          string         `db:"id"`
	GymID       string         `db:"gym_id"`
	Version     int            `db:"version"`
	CreatedAt   int64          `db:"created_at"`
	UpdatedAt   int64          `db:"updated_at"`
	DeletedAt   sql.NullInt64  `db:"deleted_at"`
	SyncedAt    sql.NullInt64  `db:"synced_at"`
	MemberID    string         `db:"member_id"`
	AttemptAt   int64          `db:"attempt_at"`
	Channel     sql.NullString `db:"channel"`
	Note        sql.NullString `db:"note"`
	ContactedBy string         `db:"contacted_by"`
}

func (r *ContactAttemptSQLiteRepository) Create(tx sharedDomain.Transaction, a *caDomain.ContactAttempt) (*caDomain.ContactAttempt, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := contactAttemptToRow(a)
	const stmt = `
		INSERT INTO contact_attempts (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    member_id, attempt_at, channel, note, contacted_by
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :member_id, :attempt_at, :channel, :note, :contacted_by
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueContactAttempt(stx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (r *ContactAttemptSQLiteRepository) LastByMember(tx sharedDomain.Transaction, memberID uuid.UUID) (*caDomain.ContactAttempt, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteContactAttemptRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM contact_attempts
		 WHERE member_id = ? AND deleted_at IS NULL
		 ORDER BY attempt_at DESC LIMIT 1`,
		memberID.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return contactAttemptFromRow(&row), nil
}

func (r *ContactAttemptSQLiteRepository) ListByMember(tx sharedDomain.Transaction, memberID uuid.UUID, limit int) ([]*caDomain.ContactAttempt, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	var rows []sqliteContactAttemptRow
	if err := stx.Select(context.Background(), &rows,
		`SELECT * FROM contact_attempts
		 WHERE member_id = ? AND deleted_at IS NULL
		 ORDER BY attempt_at DESC LIMIT ?`,
		memberID.String(), limit); err != nil {
		return nil, err
	}
	out := make([]*caDomain.ContactAttempt, len(rows))
	for i := range rows {
		out[i] = contactAttemptFromRow(&rows[i])
	}
	return out, nil
}

func contactAttemptToRow(a *caDomain.ContactAttempt) sqliteContactAttemptRow {
	row := sqliteContactAttemptRow{
		ID:          a.ID.String(),
		GymID:       a.GymID.String(),
		Version:     a.Version,
		CreatedAt:   a.CreatedAt.UnixMilli(),
		UpdatedAt:   a.UpdatedAt.UnixMilli(),
		MemberID:    a.MemberID.String(),
		AttemptAt:   a.AttemptAt.UnixMilli(),
		ContactedBy: a.ContactedBy.String(),
	}
	if a.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: a.DeletedAt.UnixMilli(), Valid: true}
	}
	if a.Channel != nil {
		row.Channel = sql.NullString{String: *a.Channel, Valid: true}
	}
	if a.Note != nil {
		row.Note = sql.NullString{String: *a.Note, Valid: true}
	}
	return row
}

func contactAttemptFromRow(r *sqliteContactAttemptRow) *caDomain.ContactAttempt {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	memberID, _ := uuid.Parse(r.MemberID)
	contactedBy, _ := uuid.Parse(r.ContactedBy)
	a := &caDomain.ContactAttempt{
		ID:          id,
		GymID:       gymID,
		Version:     r.Version,
		MemberID:    memberID,
		AttemptAt:   time.UnixMilli(r.AttemptAt).UTC(),
		ContactedBy: contactedBy,
		CreatedAt:   time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:   time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		a.DeletedAt = &t
	}
	if r.Channel.Valid {
		v := r.Channel.String
		a.Channel = &v
	}
	if r.Note.Valid {
		v := r.Note.String
		a.Note = &v
	}
	return a
}

func enqueueContactAttempt(stx *sharedDomain.SqlxTransaction, a *caDomain.ContactAttempt) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":           a.ID.String(),
		"gym_id":       a.GymID.String(),
		"version":      a.Version,
		"member_id":    a.MemberID.String(),
		"attempt_at":   a.AttemptAt.UnixMilli(),
		"channel":      nullableString(a.Channel),
		"note":         nullableString(a.Note),
		"contacted_by": a.ContactedBy.String(),
		"created_at":   a.CreatedAt.UnixMilli(),
		"updated_at":   a.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "contact_attempts", a.ID.String(), "upsert", payload, a.Version)
}

func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}
