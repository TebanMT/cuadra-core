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

	promoErrors "github.com/cuadra/cuadra-core/src/modules/promotions/domain/errors"
	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// SQLite usa cents para fixed_amount.value y discount_amount; percent y
// extra_days se guardan como enteros chicos (porcentaje / días) en la
// misma columna value. Fechas calendario van como TEXT 'YYYY-MM-DD'.

const dateLayout = "2006-01-02"

func toCents(v float64) int64   { return int64(math.Round(v * 100)) }
func fromCents(c int64) float64 { return float64(c) / 100 }
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type PromotionSQLiteRepository struct{}

func NewPromotionSQLiteRepository() *PromotionSQLiteRepository {
	return &PromotionSQLiteRepository{}
}

type sqlitePromotionRow struct {
	ID               string         `db:"id"`
	GymID            string         `db:"gym_id"`
	Version          int            `db:"version"`
	CreatedAt        int64          `db:"created_at"`
	UpdatedAt        int64          `db:"updated_at"`
	DeletedAt        sql.NullInt64  `db:"deleted_at"`
	SyncedAt         sql.NullInt64  `db:"synced_at"`
	Name             string         `db:"name"`
	Description      sql.NullString `db:"description"`
	Kind             string         `db:"kind"`
	Value            sql.NullInt64  `db:"value"`
	BuyN             int            `db:"buy_n"`
	CompanionCount   sql.NullInt64  `db:"companion_count"`
	AppliesTo        string         `db:"applies_to"`
	Code             sql.NullString `db:"code"`
	ValidFrom        sql.NullString `db:"valid_from"`
	ValidUntil       sql.NullString `db:"valid_until"`
	MaxUsesTotal     sql.NullInt64  `db:"max_uses_total"`
	MaxUsesPerMember sql.NullInt64  `db:"max_uses_per_member"`
	Active           int            `db:"active"`
}

func (r *PromotionSQLiteRepository) Create(tx sharedDomain.Transaction, p *promoDomain.Promotion) (*promoDomain.Promotion, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := promoToRow(p)
	const stmt = `
		INSERT INTO promotions (
			id, gym_id, version, created_at, updated_at, deleted_at,
			name, description, kind, value, buy_n, companion_count,
			applies_to, code, valid_from, valid_until,
			max_uses_total, max_uses_per_member, active
		) VALUES (
			:id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
			:name, :description, :kind, :value, :buy_n, :companion_count,
			:applies_to, :code, :valid_from, :valid_until,
			:max_uses_total, :max_uses_per_member, :active
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueuePromotion(stx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *PromotionSQLiteRepository) Update(tx sharedDomain.Transaction, p *promoDomain.Promotion) (*promoDomain.Promotion, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	p.UpdatedAt = time.Now().UTC()
	row := promoToRow(p)
	const stmt = `
		UPDATE promotions SET
			version = :version, updated_at = :updated_at, deleted_at = :deleted_at,
			name = :name, description = :description, kind = :kind, value = :value,
			buy_n = :buy_n, companion_count = :companion_count,
			applies_to = :applies_to, code = :code,
			valid_from = :valid_from, valid_until = :valid_until,
			max_uses_total = :max_uses_total, max_uses_per_member = :max_uses_per_member,
			active = :active
		WHERE id = :id`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueuePromotion(stx, p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *PromotionSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*promoDomain.Promotion, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqlitePromotionRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM promotions WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(promoErrors.ErrPromotionNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return promoFromRow(&row), nil
}

func (r *PromotionSQLiteRepository) GetByCode(tx sharedDomain.Transaction, gymID uuid.UUID, codeLower string) (*promoDomain.Promotion, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqlitePromotionRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM promotions WHERE gym_id = ? AND code = ? COLLATE NOCASE AND deleted_at IS NULL`,
		gymID.String(), codeLower)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(promoErrors.ErrPromotionCodeNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return promoFromRow(&row), nil
}

func (r *PromotionSQLiteRepository) List(tx sharedDomain.Transaction, f promoRepo.ListFilter) ([]*promoDomain.Promotion, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	q := `SELECT * FROM promotions WHERE gym_id = ? AND deleted_at IS NULL`
	args := []any{f.GymID.String()}
	if !f.IncludeInactive {
		q += ` AND active = 1`
	}
	if f.AppliesTo != "" {
		q += ` AND applies_to IN (?, ?)`
		args = append(args, f.AppliesTo, promoDomain.AppliesToAny)
	}
	if f.CurrentlyValid != nil {
		d := f.CurrentlyValid.UTC().Format(dateLayout)
		q += ` AND (valid_from IS NULL OR valid_from <= ?)`
		q += ` AND (valid_until IS NULL OR valid_until >= ?)`
		args = append(args, d, d)
	}
	q += ` ORDER BY created_at DESC`
	var rows []sqlitePromotionRow
	if err := stx.Select(context.Background(), &rows, q, args...); err != nil {
		return nil, err
	}
	out := make([]*promoDomain.Promotion, len(rows))
	for i := range rows {
		out[i] = promoFromRow(&rows[i])
	}
	return out, nil
}

func (r *PromotionSQLiteRepository) ExistsByCode(tx sharedDomain.Transaction, gymID uuid.UUID, codeLower string, excludeID *uuid.UUID) (bool, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	q := `SELECT COUNT(1) FROM promotions WHERE gym_id = ? AND code = ? COLLATE NOCASE AND deleted_at IS NULL`
	args := []any{gymID.String(), strings.ToLower(strings.TrimSpace(codeLower))}
	if excludeID != nil {
		q += ` AND id <> ?`
		args = append(args, excludeID.String())
	}
	var n int
	if err := stx.Get(context.Background(), &n, q, args...); err != nil {
		return false, err
	}
	return n > 0, nil
}

func promoToRow(p *promoDomain.Promotion) sqlitePromotionRow {
	row := sqlitePromotionRow{
		ID:        p.ID.String(),
		GymID:     p.GymID.String(),
		Version:   p.Version,
		CreatedAt: p.CreatedAt.UnixMilli(),
		UpdatedAt: p.UpdatedAt.UnixMilli(),
		Name:      p.Name,
		Kind:      p.Kind,
		BuyN:      p.BuyN,
		AppliesTo: p.AppliesTo,
		Active:    boolToInt(p.Active),
	}
	if p.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: p.DeletedAt.UnixMilli(), Valid: true}
	}
	if p.Description != nil {
		row.Description = sql.NullString{String: *p.Description, Valid: true}
	}
	if p.Value != nil {
		var v int64
		switch p.Kind {
		case promoDomain.KindFixedAmount:
			v = toCents(*p.Value)
		default:
			v = int64(*p.Value)
		}
		row.Value = sql.NullInt64{Int64: v, Valid: true}
	}
	if p.CompanionCount != nil {
		row.CompanionCount = sql.NullInt64{Int64: int64(*p.CompanionCount), Valid: true}
	}
	if p.Code != nil {
		row.Code = sql.NullString{String: *p.Code, Valid: true}
	}
	if p.ValidFrom != nil {
		row.ValidFrom = sql.NullString{String: p.ValidFrom.UTC().Format(dateLayout), Valid: true}
	}
	if p.ValidUntil != nil {
		row.ValidUntil = sql.NullString{String: p.ValidUntil.UTC().Format(dateLayout), Valid: true}
	}
	if p.MaxUsesTotal != nil {
		row.MaxUsesTotal = sql.NullInt64{Int64: int64(*p.MaxUsesTotal), Valid: true}
	}
	if p.MaxUsesPerMember != nil {
		row.MaxUsesPerMember = sql.NullInt64{Int64: int64(*p.MaxUsesPerMember), Valid: true}
	}
	return row
}

func promoFromRow(r *sqlitePromotionRow) *promoDomain.Promotion {
	id, _ := uuid.Parse(r.ID)
	gid, _ := uuid.Parse(r.GymID)
	p := &promoDomain.Promotion{
		ID:        id,
		GymID:     gid,
		Version:   r.Version,
		Name:      r.Name,
		Kind:      r.Kind,
		BuyN:      r.BuyN,
		AppliesTo: r.AppliesTo,
		Active:    r.Active != 0,
		CreatedAt: time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt: time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		p.DeletedAt = &t
	}
	if r.Description.Valid {
		v := r.Description.String
		p.Description = &v
	}
	if r.Value.Valid {
		var v float64
		switch r.Kind {
		case promoDomain.KindFixedAmount:
			v = fromCents(r.Value.Int64)
		default:
			v = float64(r.Value.Int64)
		}
		p.Value = &v
	}
	if r.CompanionCount.Valid {
		c := int(r.CompanionCount.Int64)
		p.CompanionCount = &c
	}
	if r.Code.Valid {
		v := r.Code.String
		p.Code = &v
	}
	if r.ValidFrom.Valid {
		if t, err := time.Parse(dateLayout, r.ValidFrom.String); err == nil {
			p.ValidFrom = &t
		}
	}
	if r.ValidUntil.Valid {
		if t, err := time.Parse(dateLayout, r.ValidUntil.String); err == nil {
			p.ValidUntil = &t
		}
	}
	if r.MaxUsesTotal.Valid {
		v := int(r.MaxUsesTotal.Int64)
		p.MaxUsesTotal = &v
	}
	if r.MaxUsesPerMember.Valid {
		v := int(r.MaxUsesPerMember.Int64)
		p.MaxUsesPerMember = &v
	}
	return p
}

func enqueuePromotion(stx *sharedDomain.SqlxTransaction, p *promoDomain.Promotion) error {
	if stx.Queue == nil {
		return nil
	}
	// Wire sidecar → cloud: el cloud Postgres guarda `value` como
	// NUMERIC(12,2) en pesos (no cents) — mismo patrón que membership_types.
	// Enviamos float pesos directo para que el projector genérico lo deje
	// sin transformar. El campo es semánticamente distinto por kind
	// (percent: 0-100; fixed_amount: pesos; extra_days: días) pero la
	// columna es la misma NUMERIC; pgx la castea correctamente desde el
	// JSON number.
	var value any
	if p.Value != nil {
		value = *p.Value
	}
	var validFrom, validUntil any
	if p.ValidFrom != nil {
		validFrom = p.ValidFrom.UTC().Format(dateLayout)
	}
	if p.ValidUntil != nil {
		validUntil = p.ValidUntil.UTC().Format(dateLayout)
	}
	payload, err := json.Marshal(map[string]any{
		"id":                  p.ID.String(),
		"gym_id":              p.GymID.String(),
		"version":             p.Version,
		"name":                p.Name,
		"description":         p.Description,
		"kind":                p.Kind,
		"value":               value,
		"buy_n":               p.BuyN,
		"companion_count":     p.CompanionCount,
		"applies_to":          p.AppliesTo,
		"code":                p.Code,
		"valid_from":          validFrom,
		"valid_until":         validUntil,
		"max_uses_total":      p.MaxUsesTotal,
		"max_uses_per_member": p.MaxUsesPerMember,
		"active":              p.Active,
		"created_at":          p.CreatedAt.UnixMilli(),
		"updated_at":          p.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "promotions", p.ID.String(), "upsert", payload, p.Version)
}
