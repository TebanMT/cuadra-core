//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

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
	EnrollmentFee        int64          `db:"enrollment_fee"`
	MaintenanceFee       int64          `db:"maintenance_fee"`
	MaintenanceFrequency sql.NullString `db:"maintenance_frequency"`
	Active               int            `db:"active"`
}

func toCents(v float64) int64   { return int64(math.Round(v * 100)) }
func fromCents(c int64) float64 { return float64(c) / 100 }

func (r *MembershipTypeSQLiteRepository) Create(tx sharedDomain.Transaction, mt *mtDomain.MembershipType) (*mtDomain.MembershipType, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
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
		Active:         1,
	}
	if mt.MaintenanceFrequency != nil {
		row.MaintenanceFrequency = sql.NullString{String: *mt.MaintenanceFrequency, Valid: true}
	}
	const stmt = `
		INSERT INTO membership_types (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    name, price, duration_days, enrollment_fee, maintenance_fee, maintenance_frequency, active
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :name, :price, :duration_days, :enrollment_fee, :maintenance_fee, :maintenance_frequency, :active
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if stx.Queue != nil {
		payload, _ := json.Marshal(map[string]any{
			"id":            mt.ID.String(),
			"gym_id":        mt.GymID.String(),
			"version":       mt.Version,
			"name":          mt.Name,
			"price":         mt.Price,
			"duration_days": mt.DurationDays,
			"updated_at":    mt.UpdatedAt.UnixMilli(),
		})
		_ = stx.EnqueueSync(context.Background(), "membership_types", mt.ID.String(), "upsert", payload, mt.Version)
	}
	return mt, nil
}

func (r *MembershipTypeSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID) ([]*mtDomain.MembershipType, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteMTRow
	err := stx.Select(context.Background(), &rows,
		`SELECT * FROM membership_types WHERE gym_id = ? AND deleted_at IS NULL ORDER BY created_at ASC`,
		gymID.String())
	if err != nil {
		return nil, err
	}
	out := make([]*mtDomain.MembershipType, len(rows))
	for i := range rows {
		out[i] = sqliteMTToDomain(&rows[i])
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

func sqliteMTToDomain(r *sqliteMTRow) *mtDomain.MembershipType {
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
