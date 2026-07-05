//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// SQLite stores money in cents (ADR-002 §2 type table). Convert at the edge.

type MembershipTypeSQLiteRepository struct{}

func NewMembershipTypeSQLiteRepository() *MembershipTypeSQLiteRepository {
	return &MembershipTypeSQLiteRepository{}
}

type sqliteMTRow struct {
	ID                   string         `db:"id"`
	GymID                string         `db:"gym_id"`
	Version              int            `db:"version"`
	CreatedAt            int64          `db:"created_at"`
	UpdatedAt            int64          `db:"updated_at"`
	DeletedAt            sql.NullInt64  `db:"deleted_at"`
	SyncedAt             sql.NullInt64  `db:"synced_at"`
	Name                 string         `db:"name"`
	Price                int64          `db:"price"`
	DurationDays         int            `db:"duration_days"`
	DurationMonths       sql.NullInt64  `db:"duration_months"`
	EnrollmentFee        int64          `db:"enrollment_fee"`
	MaintenanceFee       int64          `db:"maintenance_fee"`
	MaintenanceFrequency sql.NullString `db:"maintenance_frequency"`
	Active               int            `db:"active"`
}

func toCents(v float64) int64   { return int64(math.Round(v * 100)) }
func fromCents(c int64) float64 { return float64(c) / 100 }

func (r *MembershipTypeSQLiteRepository) Create(tx sharedDomain.Transaction, mt *mtDomain.MembershipType) (*mtDomain.MembershipType, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := mtToRow(mt)
	const stmt = `
		INSERT INTO membership_types (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    name, price, duration_days, duration_months,
		    enrollment_fee, maintenance_fee, maintenance_frequency, active
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :name, :price, :duration_days, :duration_months,
		    :enrollment_fee, :maintenance_fee, :maintenance_frequency, :active
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueMT(stx, mt); err != nil {
		return nil, err
	}
	return mt, nil
}

func (r *MembershipTypeSQLiteRepository) Update(tx sharedDomain.Transaction, mt *mtDomain.MembershipType) (*mtDomain.MembershipType, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	mt.UpdatedAt = time.Now().UTC()
	row := mtToRow(mt)
	const stmt = `
		UPDATE membership_types SET
		    version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
		    name = :name, price = :price, duration_days = :duration_days,
		    duration_months = :duration_months,
		    enrollment_fee = :enrollment_fee, maintenance_fee = :maintenance_fee,
		    maintenance_frequency = :maintenance_frequency, active = :active
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueMT(stx, mt); err != nil {
		return nil, err
	}
	return mt, nil
}

func (r *MembershipTypeSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*mtDomain.MembershipType, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteMTRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM membership_types WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return mtFromRow(&row), nil
}

func (r *MembershipTypeSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, includeInactive bool) ([]*mtDomain.MembershipType, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	q := `SELECT * FROM membership_types WHERE gym_id = ? AND deleted_at IS NULL`
	args := []any{gymID.String()}
	if !includeInactive {
		q += ` AND active = 1`
	}
	q += ` ORDER BY created_at ASC`
	var rows []sqliteMTRow
	if err := stx.Select(context.Background(), &rows, q, args...); err != nil {
		return nil, err
	}
	out := make([]*mtDomain.MembershipType, len(rows))
	for i := range rows {
		out[i] = mtFromRow(&rows[i])
	}
	return out, nil
}

func (r *MembershipTypeSQLiteRepository) ExistsByGymAndName(tx sharedDomain.Transaction, gymID uuid.UUID, name string) (bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM membership_types WHERE gym_id = ? AND name = ? COLLATE NOCASE AND deleted_at IS NULL`,
		gymID.String(), strings.ToLower(name))
	return n > 0, err
}

func (r *MembershipTypeSQLiteRepository) CountActiveMembershipsByType(tx sharedDomain.Transaction, typeID uuid.UUID) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM memberships WHERE membership_type_id = ? AND status = 'active' AND deleted_at IS NULL`,
		typeID.String())
	return n, err
}

func (r *MembershipTypeSQLiteRepository) CountByGym(tx sharedDomain.Transaction, gymID uuid.UUID) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM membership_types WHERE gym_id = ? AND deleted_at IS NULL`,
		gymID.String())
	return n, err
}

func mtToRow(mt *mtDomain.MembershipType) sqliteMTRow {
	row := sqliteMTRow{
		ID:             mt.ID.String(),
		GymID:          mt.GymID.String(),
		Version:        mt.Version,
		CreatedAt:      mt.CreatedAt.UnixMilli(),
		UpdatedAt:      mt.UpdatedAt.UnixMilli(),
		Name:           mt.Name,
		Price:          toCents(mt.Price),
		DurationDays:   mt.DurationDays,
		EnrollmentFee:  toCents(mt.EnrollmentFee),
		MaintenanceFee: toCents(mt.MaintenanceFee),
		Active:         boolToInt(mt.Active),
	}
	if mt.DurationMonths != nil {
		row.DurationMonths = sql.NullInt64{Int64: int64(*mt.DurationMonths), Valid: true}
	}
	if mt.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: mt.DeletedAt.UnixMilli(), Valid: true}
	}
	if mt.MaintenanceFrequency != nil {
		row.MaintenanceFrequency = sql.NullString{String: *mt.MaintenanceFrequency, Valid: true}
	}
	return row
}

func mtFromRow(r *sqliteMTRow) *mtDomain.MembershipType {
	id, _ := uuid.Parse(r.ID)
	gid, _ := uuid.Parse(r.GymID)
	mt := &mtDomain.MembershipType{
		ID:             id,
		GymID:          gid,
		Version:        r.Version,
		Name:           r.Name,
		Price:          fromCents(r.Price),
		DurationDays:   r.DurationDays,
		EnrollmentFee:  fromCents(r.EnrollmentFee),
		MaintenanceFee: fromCents(r.MaintenanceFee),
		Active:         r.Active != 0,
		CreatedAt:      time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:      time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DurationMonths.Valid {
		v := int(r.DurationMonths.Int64)
		mt.DurationMonths = &v
	}
	if r.MaintenanceFrequency.Valid {
		f := r.MaintenanceFrequency.String
		mt.MaintenanceFrequency = &f
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		mt.DeletedAt = &t
	}
	return mt
}

func enqueueMT(stx *sharedDomain.SqlxTransaction, mt *mtDomain.MembershipType) error {
	if stx.Queue == nil {
		return nil
	}
	freq := ""
	if mt.MaintenanceFrequency != nil {
		freq = *mt.MaintenanceFrequency
	}
	payload, err := json.Marshal(map[string]any{
		"id":                    mt.ID.String(),
		"gym_id":                mt.GymID.String(),
		"version":               mt.Version,
		"name":                  mt.Name,
		"price":                 mt.Price,
		"duration_days":         mt.DurationDays,
		"duration_months":       mt.DurationMonths,
		"enrollment_fee":        mt.EnrollmentFee,
		"maintenance_fee":       mt.MaintenanceFee,
		"maintenance_frequency": freq,
		"active":                mt.Active,
		"created_at":            mt.CreatedAt.UnixMilli(),
		"updated_at":            mt.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "membership_types", mt.ID.String(), "upsert", payload, mt.Version)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
