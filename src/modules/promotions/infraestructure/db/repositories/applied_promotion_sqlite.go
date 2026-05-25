//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	promoDomain "github.com/cuadra/cuadra-core/src/modules/promotions/domain/promotion"
	promoRepo "github.com/cuadra/cuadra-core/src/modules/promotions/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type AppliedPromotionSQLiteRepository struct{}

func NewAppliedPromotionSQLiteRepository() *AppliedPromotionSQLiteRepository {
	return &AppliedPromotionSQLiteRepository{}
}

type sqliteAppliedRow struct {
	ID                    string         `db:"id"`
	GymID                 string         `db:"gym_id"`
	Version               int            `db:"version"`
	CreatedAt             int64          `db:"created_at"`
	UpdatedAt             int64          `db:"updated_at"`
	DeletedAt             sql.NullInt64  `db:"deleted_at"`
	SyncedAt              sql.NullInt64  `db:"synced_at"`
	PromotionID           string         `db:"promotion_id"`
	PaymentID             string         `db:"payment_id"`
	MemberID              sql.NullString `db:"member_id"`
	AppliedByUserID       string         `db:"applied_by_user_id"`
	PromotionNameSnapshot string         `db:"promotion_name_snapshot"`
	KindSnapshot          string         `db:"kind_snapshot"`
	ValueSnapshot         sql.NullInt64  `db:"value_snapshot"`
	DiscountAmount        int64          `db:"discount_amount"`
	ExtraDaysApplied      int            `db:"extra_days_applied"`
	Notes                 sql.NullString `db:"notes"`
}

func (r *AppliedPromotionSQLiteRepository) Create(tx sharedDomain.Transaction, ap *promoDomain.AppliedPromotion) (*promoDomain.AppliedPromotion, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := appliedToRow(ap)
	const stmt = `
		INSERT INTO applied_promotions (
			id, gym_id, version, created_at, updated_at, deleted_at,
			promotion_id, payment_id, member_id, applied_by_user_id,
			promotion_name_snapshot, kind_snapshot, value_snapshot,
			discount_amount, extra_days_applied, notes
		) VALUES (
			:id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
			:promotion_id, :payment_id, :member_id, :applied_by_user_id,
			:promotion_name_snapshot, :kind_snapshot, :value_snapshot,
			:discount_amount, :extra_days_applied, :notes
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueApplied(stx, ap); err != nil {
		return nil, err
	}
	return ap, nil
}

func (r *AppliedPromotionSQLiteRepository) CountByPromotion(tx sharedDomain.Transaction, promotionID uuid.UUID) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM applied_promotions WHERE promotion_id = ? AND deleted_at IS NULL`,
		promotionID.String())
	return n, err
}

func (r *AppliedPromotionSQLiteRepository) CountByPromotionAndMember(tx sharedDomain.Transaction, promotionID, memberID uuid.UUID) (int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var n int
	err := stx.Get(context.Background(), &n,
		`SELECT COUNT(1) FROM applied_promotions
		 WHERE promotion_id = ? AND member_id = ? AND deleted_at IS NULL`,
		promotionID.String(), memberID.String())
	return n, err
}

type summarySqliteRow struct {
	PromotionID    string `db:"promotion_id"`
	PromotionName  string `db:"promotion_name_snapshot"`
	Kind           string `db:"kind_snapshot"`
	UseCount       int    `db:"use_count"`
	TotalDiscount  int64  `db:"total_discount"` // cents
	MembersReached int    `db:"members_reached"`
}

func (r *AppliedPromotionSQLiteRepository) SummaryByMonth(tx sharedDomain.Transaction, gymID uuid.UUID, monthStart, monthEnd time.Time) ([]promoRepo.AppliedSummary, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	const q = `
		SELECT
			promotion_id,
			MAX(promotion_name_snapshot) AS promotion_name_snapshot,
			MAX(kind_snapshot) AS kind_snapshot,
			COUNT(*) AS use_count,
			COALESCE(SUM(discount_amount), 0) AS total_discount,
			COUNT(DISTINCT member_id) AS members_reached
		FROM applied_promotions
		WHERE gym_id = ? AND deleted_at IS NULL
		  AND created_at >= ? AND created_at < ?
		GROUP BY promotion_id
		ORDER BY total_discount DESC, use_count DESC`
	var rows []summarySqliteRow
	if err := stx.Select(context.Background(), &rows, q,
		gymID.String(), monthStart.UnixMilli(), monthEnd.UnixMilli()); err != nil {
		return nil, err
	}
	out := make([]promoRepo.AppliedSummary, len(rows))
	for i, r := range rows {
		pid, _ := uuid.Parse(r.PromotionID)
		out[i] = promoRepo.AppliedSummary{
			PromotionID:    pid,
			PromotionName:  r.PromotionName,
			Kind:           r.Kind,
			UseCount:       r.UseCount,
			TotalDiscount:  fromCents(r.TotalDiscount),
			MembersReached: r.MembersReached,
		}
	}
	return out, nil
}

func appliedToRow(ap *promoDomain.AppliedPromotion) sqliteAppliedRow {
	row := sqliteAppliedRow{
		ID:                    ap.ID.String(),
		GymID:                 ap.GymID.String(),
		Version:               ap.Version,
		CreatedAt:             ap.CreatedAt.UnixMilli(),
		UpdatedAt:             ap.UpdatedAt.UnixMilli(),
		PromotionID:           ap.PromotionID.String(),
		PaymentID:             ap.PaymentID.String(),
		AppliedByUserID:       ap.AppliedByUserID.String(),
		PromotionNameSnapshot: ap.PromotionNameSnapshot,
		KindSnapshot:          ap.KindSnapshot,
		DiscountAmount:        toCents(ap.DiscountAmount),
		ExtraDaysApplied:      ap.ExtraDaysApplied,
	}
	if ap.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: ap.DeletedAt.UnixMilli(), Valid: true}
	}
	if ap.MemberID != nil {
		row.MemberID = sql.NullString{String: ap.MemberID.String(), Valid: true}
	}
	if ap.ValueSnapshot != nil {
		var v int64
		switch ap.KindSnapshot {
		case promoDomain.KindFixedAmount:
			v = toCents(*ap.ValueSnapshot)
		default:
			v = int64(*ap.ValueSnapshot)
		}
		row.ValueSnapshot = sql.NullInt64{Int64: v, Valid: true}
	}
	if ap.Notes != nil {
		row.Notes = sql.NullString{String: *ap.Notes, Valid: true}
	}
	return row
}

func enqueueApplied(stx *sharedDomain.SqlxTransaction, ap *promoDomain.AppliedPromotion) error {
	if stx.Queue == nil {
		return nil
	}
	var memberID any
	if ap.MemberID != nil {
		memberID = ap.MemberID.String()
	}
	// Wire sidecar → cloud: pesos float, no cents. El cloud Postgres
	// usa NUMERIC(12,2) pesos para value_snapshot + discount_amount;
	// pgx castea desde JSON number sin conversión. Mismo patrón que
	// payments.amount, membership_types.price (ver enqueuePromotion).
	var valueSnap any
	if ap.ValueSnapshot != nil {
		valueSnap = *ap.ValueSnapshot
	}
	payload, err := json.Marshal(map[string]any{
		"id":                      ap.ID.String(),
		"gym_id":                  ap.GymID.String(),
		"version":                 ap.Version,
		"promotion_id":            ap.PromotionID.String(),
		"payment_id":              ap.PaymentID.String(),
		"member_id":               memberID,
		"applied_by_user_id":      ap.AppliedByUserID.String(),
		"promotion_name_snapshot": ap.PromotionNameSnapshot,
		"kind_snapshot":           ap.KindSnapshot,
		"value_snapshot":          valueSnap,
		"discount_amount":         ap.DiscountAmount,
		"extra_days_applied":      ap.ExtraDaysApplied,
		"notes":                   ap.Notes,
		"created_at":              ap.CreatedAt.UnixMilli(),
		"updated_at":              ap.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "applied_promotions", ap.ID.String(), "upsert", payload, ap.Version)
}
