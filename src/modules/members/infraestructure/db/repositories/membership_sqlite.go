//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type MembershipSQLiteRepository struct{}

func NewMembershipSQLiteRepository() *MembershipSQLiteRepository {
	return &MembershipSQLiteRepository{}
}

type sqliteMembershipRow struct {
	ID                   string         `db:"id"`
	GymID                string         `db:"gym_id"`
	Version              int            `db:"version"`
	CreatedAt            int64          `db:"created_at"`
	UpdatedAt            int64          `db:"updated_at"`
	DeletedAt            sql.NullInt64  `db:"deleted_at"`
	SyncedAt             sql.NullInt64  `db:"synced_at"`
	MemberID             string         `db:"member_id"`
	MembershipTypeID     string         `db:"membership_type_id"`
	TypeNameSnapshot     string         `db:"type_name_snapshot"`
	PriceSnapshot        int64          `db:"price_snapshot"`
	DurationDaysSnapshot int            `db:"duration_days_snapshot"`
	StartDate            string         `db:"start_date"`
	ExpiryDate           string         `db:"expiry_date"`
	Status               string         `db:"status"`
	ReplacedBy           sql.NullString `db:"replaced_by"`
}

func (r *MembershipSQLiteRepository) Create(tx sharedDomain.Transaction, m *membershipDomain.Membership) (*membershipDomain.Membership, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := membershipToRow(m)
	const stmt = `
		INSERT INTO memberships (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    member_id, membership_type_id,
		    type_name_snapshot, price_snapshot, duration_days_snapshot,
		    start_date, expiry_date, status, replaced_by
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :member_id, :membership_type_id,
		    :type_name_snapshot, :price_snapshot, :duration_days_snapshot,
		    :start_date, :expiry_date, :status, :replaced_by
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueMembership(stx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *MembershipSQLiteRepository) Update(tx sharedDomain.Transaction, m *membershipDomain.Membership) (*membershipDomain.Membership, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	m.UpdatedAt = time.Now().UTC()
	row := membershipToRow(m)
	const stmt = `
		UPDATE memberships SET
		    version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
		    membership_type_id = :membership_type_id,
		    type_name_snapshot = :type_name_snapshot, price_snapshot = :price_snapshot,
		    duration_days_snapshot = :duration_days_snapshot,
		    start_date = :start_date, expiry_date = :expiry_date,
		    status = :status, replaced_by = :replaced_by
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueMembership(stx, m); err != nil {
		return nil, err
	}
	return m, nil
}

func (r *MembershipSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*membershipDomain.Membership, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteMembershipRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM memberships WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMembershipNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return membershipFromRow(&row), nil
}

func (r *MembershipSQLiteRepository) GetCurrentByMember(tx sharedDomain.Transaction, memberID uuid.UUID) (*membershipDomain.Membership, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteMembershipRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM memberships WHERE member_id = ? AND status = 'active' AND deleted_at IS NULL`,
		memberID.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrNoActiveMembership, "")
	}
	if err != nil {
		return nil, err
	}
	return membershipFromRow(&row), nil
}

// ---------------------------------------------------------------------------
// MembershipAdjustmentRepository
// ---------------------------------------------------------------------------

type MembershipAdjustmentSQLiteRepository struct{}

func NewMembershipAdjustmentSQLiteRepository() *MembershipAdjustmentSQLiteRepository {
	return &MembershipAdjustmentSQLiteRepository{}
}

type sqliteAdjustmentRow struct {
	ID             string        `db:"id"`
	GymID          string        `db:"gym_id"`
	Version        int           `db:"version"`
	CreatedAt      int64         `db:"created_at"`
	UpdatedAt      int64         `db:"updated_at"`
	DeletedAt      sql.NullInt64 `db:"deleted_at"`
	SyncedAt       sql.NullInt64 `db:"synced_at"`
	MembershipID   string        `db:"membership_id"`
	AdjustedBy     string        `db:"adjusted_by"`
	Reason         string        `db:"reason"`
	DaysAdded      int           `db:"days_added"`
	PreviousExpiry string        `db:"previous_expiry"`
	NewExpiry      string        `db:"new_expiry"`
}

func (r *MembershipAdjustmentSQLiteRepository) Create(tx sharedDomain.Transaction, a *membershipDomain.MembershipAdjustment) (*membershipDomain.MembershipAdjustment, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := adjustmentToRow(a)
	const stmt = `
		INSERT INTO membership_adjustments (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    membership_id, adjusted_by, reason, days_added, previous_expiry, new_expiry
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :membership_id, :adjusted_by, :reason, :days_added, :previous_expiry, :new_expiry
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueAdjustment(stx, a); err != nil {
		return nil, err
	}
	return a, nil
}

func (r *MembershipAdjustmentSQLiteRepository) ListByMembership(tx sharedDomain.Transaction, membershipID uuid.UUID) ([]*membershipDomain.MembershipAdjustment, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteAdjustmentRow
	err := stx.Select(context.Background(), &rows,
		`SELECT * FROM membership_adjustments WHERE membership_id = ? AND deleted_at IS NULL ORDER BY created_at DESC`,
		membershipID.String())
	if err != nil {
		return nil, err
	}
	out := make([]*membershipDomain.MembershipAdjustment, len(rows))
	for i := range rows {
		out[i] = adjustmentFromRow(&rows[i])
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Mappers
// ---------------------------------------------------------------------------

func membershipToRow(m *membershipDomain.Membership) sqliteMembershipRow {
	row := sqliteMembershipRow{
		ID:                   m.ID.String(),
		GymID:                m.GymID.String(),
		Version:              m.Version,
		CreatedAt:            m.CreatedAt.UnixMilli(),
		UpdatedAt:            m.UpdatedAt.UnixMilli(),
		MemberID:             m.MemberID.String(),
		MembershipTypeID:     m.MembershipTypeID.String(),
		TypeNameSnapshot:     m.TypeNameSnapshot,
		PriceSnapshot:        toCents(m.PriceSnapshot),
		DurationDaysSnapshot: m.DurationDaysSnapshot,
		StartDate:            m.StartDate.UTC().Format(dateLayout),
		ExpiryDate:           m.ExpiryDate.UTC().Format(dateLayout),
		Status:               m.Status,
	}
	if m.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: m.DeletedAt.UnixMilli(), Valid: true}
	}
	if m.ReplacedBy != nil {
		row.ReplacedBy = sql.NullString{String: m.ReplacedBy.String(), Valid: true}
	}
	return row
}

func membershipFromRow(r *sqliteMembershipRow) *membershipDomain.Membership {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	memberID, _ := uuid.Parse(r.MemberID)
	typeID, _ := uuid.Parse(r.MembershipTypeID)
	startDate, _ := time.Parse(dateLayout, r.StartDate)
	expiryDate, _ := time.Parse(dateLayout, r.ExpiryDate)
	ms := &membershipDomain.Membership{
		ID:                   id,
		GymID:                gymID,
		Version:              r.Version,
		MemberID:             memberID,
		MembershipTypeID:     typeID,
		TypeNameSnapshot:     r.TypeNameSnapshot,
		PriceSnapshot:        fromCents(r.PriceSnapshot),
		DurationDaysSnapshot: r.DurationDaysSnapshot,
		StartDate:            startDate,
		ExpiryDate:           expiryDate,
		Status:               r.Status,
		CreatedAt:            time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:            time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		ms.DeletedAt = &t
	}
	if r.ReplacedBy.Valid {
		rb, _ := uuid.Parse(r.ReplacedBy.String)
		ms.ReplacedBy = &rb
	}
	return ms
}

func adjustmentToRow(a *membershipDomain.MembershipAdjustment) sqliteAdjustmentRow {
	row := sqliteAdjustmentRow{
		ID:             a.ID.String(),
		GymID:          a.GymID.String(),
		Version:        a.Version,
		CreatedAt:      a.CreatedAt.UnixMilli(),
		UpdatedAt:      a.UpdatedAt.UnixMilli(),
		MembershipID:   a.MembershipID.String(),
		AdjustedBy:     a.AdjustedBy.String(),
		Reason:         a.Reason,
		DaysAdded:      a.DaysAdded,
		PreviousExpiry: a.PreviousExpiry.UTC().Format(dateLayout),
		NewExpiry:      a.NewExpiry.UTC().Format(dateLayout),
	}
	if a.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: a.DeletedAt.UnixMilli(), Valid: true}
	}
	return row
}

func adjustmentFromRow(r *sqliteAdjustmentRow) *membershipDomain.MembershipAdjustment {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	mID, _ := uuid.Parse(r.MembershipID)
	by, _ := uuid.Parse(r.AdjustedBy)
	prev, _ := time.Parse(dateLayout, r.PreviousExpiry)
	next, _ := time.Parse(dateLayout, r.NewExpiry)
	a := &membershipDomain.MembershipAdjustment{
		ID:             id,
		GymID:          gymID,
		Version:        r.Version,
		MembershipID:   mID,
		AdjustedBy:     by,
		Reason:         r.Reason,
		DaysAdded:      r.DaysAdded,
		PreviousExpiry: prev,
		NewExpiry:      next,
		CreatedAt:      time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:      time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		a.DeletedAt = &t
	}
	return a
}

func enqueueMembership(stx *sharedDomain.SqlxTransaction, m *membershipDomain.Membership) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":                     m.ID.String(),
		"gym_id":                 m.GymID.String(),
		"version":                m.Version,
		"member_id":              m.MemberID.String(),
		"membership_type_id":     m.MembershipTypeID.String(),
		"type_name_snapshot":     m.TypeNameSnapshot,
		"price_snapshot":         m.PriceSnapshot,
		"duration_days_snapshot": m.DurationDaysSnapshot,
		"start_date":             m.StartDate.UTC().Format(dateLayout),
		"expiry_date":            m.ExpiryDate.UTC().Format(dateLayout),
		"status":                 m.Status,
		"updated_at":             m.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "memberships", m.ID.String(), "upsert", payload, m.Version)
}

func enqueueAdjustment(stx *sharedDomain.SqlxTransaction, a *membershipDomain.MembershipAdjustment) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":              a.ID.String(),
		"gym_id":          a.GymID.String(),
		"version":         a.Version,
		"membership_id":   a.MembershipID.String(),
		"adjusted_by":     a.AdjustedBy.String(),
		"reason":          a.Reason,
		"days_added":      a.DaysAdded,
		"previous_expiry": a.PreviousExpiry.UTC().Format(dateLayout),
		"new_expiry":      a.NewExpiry.UTC().Format(dateLayout),
		"created_at":      a.CreatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "membership_adjustments", a.ID.String(), "upsert", payload, a.Version)
}
