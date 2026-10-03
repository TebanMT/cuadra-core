//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	saleDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/sale"
	correctionDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/salecorrection"
	"github.com/cuadra/cuadra-core/src/modules/billing/infraestructure/db/models"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type SaleCorrectionPostgresRepository struct{}

func NewSaleCorrectionPostgresRepository() *SaleCorrectionPostgresRepository {
	return &SaleCorrectionPostgresRepository{}
}

type correctionPGRow struct {
	ID, GymID, SaleID, CreatedBy  uuid.UUID
	Version, ExpectedSaleVersion  int
	CreatedAt, UpdatedAt          time.Time
	DeletedAt                     *time.Time
	Reason, CorrectionType        string
	MoneyResolution               string
	IncreaseResolution            string
	MonetaryDelta                 float64
	BeforeSnapshot, AfterSnapshot []byte
	RefundID                      *uuid.UUID
	IdempotencyKey                string
	IdempotencyFingerprint        string
	IdempotencyResult             []byte
}

func (correctionPGRow) TableName() string { return "sale_corrections" }

func (r *SaleCorrectionPostgresRepository) ApplySaleCorrection(tx sharedDomain.Transaction, sale *saleDomain.Sale, items []*saleDomain.SaleItem, expected int) error {
	g := tx.(*sharedDomain.GormTransaction).Tx
	res := g.Model(&models.SaleModel{}).Where("gym_id=? AND id=? AND correction_version=? AND deleted_at IS NULL", sale.GymID, sale.ID, expected).
		Updates(map[string]any{"version": sale.Version, "updated_at": sale.UpdatedAt, "subtotal": sale.Subtotal,
			"discount": sale.Discount, "total": sale.Total, "correction_version": sale.CorrectionVersion,
			"deleted_at": sale.DeletedAt})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return sharedDomain.NewBusinessError(billingErrors.ErrSaleVersionConflict, "")
	}
	for _, item := range items {
		row := saleItemToModel(item)
		if err := g.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns([]string{
			"version", "updated_at", "deleted_at", "product_id", "product_name_snapshot", "unit_price_snapshot", "unit_cost_snapshot", "quantity", "line_total",
		})}).Create(&row).Error; err != nil {
			return err
		}
	}
	if err := mirrorSaleCorrectionState(g, sale, items); err != nil {
		return err
	}
	return nil
}

func (r *SaleCorrectionPostgresRepository) CreateCorrection(tx sharedDomain.Transaction, c *correctionDomain.Correction) (*correctionDomain.Correction, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	if err := g.Exec(`INSERT INTO sale_corrections(id,gym_id,version,created_at,updated_at,deleted_at,sale_id,expected_sale_version,
		reason,correction_type,money_resolution,increase_resolution,monetary_delta,before_snapshot,after_snapshot,refund_id,idempotency_key,idempotency_fingerprint,idempotency_result,created_by)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?::jsonb,?::jsonb,?,?,?,?::jsonb,?)`, c.ID, c.GymID, c.Version, c.CreatedAt, c.UpdatedAt, c.DeletedAt, c.SaleID,
		c.ExpectedSaleVersion, c.Reason, c.CorrectionType, c.MoneyResolution, nullableCorrectionText(c.IncreaseResolution), c.MonetaryDelta, string(c.BeforeSnapshot), string(c.AfterSnapshot), c.RefundID,
		c.IdempotencyKey, c.IdempotencyFingerprint, nullableCorrectionJSON(c.IdempotencyResult), c.CreatedBy).Error; err != nil {
		return nil, err
	}
	if err := mirrorCorrection(g, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *SaleCorrectionPostgresRepository) FinalizeCorrectionIdempotency(tx sharedDomain.Transaction, c *correctionDomain.Correction) error {
	g := tx.(*sharedDomain.GormTransaction).Tx
	if !json.Valid(c.IdempotencyResult) {
		return errors.New("sale correction idempotency result is invalid")
	}
	res := g.Model(&correctionPGRow{}).
		Where("gym_id=? AND id=? AND deleted_at IS NULL", c.GymID, c.ID).
		Update("idempotency_result", string(c.IdempotencyResult))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return billingErrors.ErrSaleCorrectionNotFound
	}
	return mirrorCorrection(g, c)
}

func (r *SaleCorrectionPostgresRepository) GetCorrectionByID(tx sharedDomain.Transaction, gymID, id uuid.UUID, lock bool) (*correctionDomain.Correction, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	if lock {
		g = g.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return getCorrectionPG(g, "gym_id=? AND id=?", gymID, id)
}
func (r *SaleCorrectionPostgresRepository) GetCorrectionByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*correctionDomain.Correction, error) {
	c, err := getCorrectionPG(tx.(*sharedDomain.GormTransaction).Tx, "gym_id=? AND idempotency_key=?", gymID, key)
	if errors.Is(err, billingErrors.ErrSaleCorrectionNotFound) {
		return nil, nil
	}
	return c, err
}
func getCorrectionPG(g *gorm.DB, where string, args ...any) (*correctionDomain.Correction, error) {
	var row correctionPGRow
	err := g.Where(where+" AND deleted_at IS NULL", args...).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, billingErrors.ErrSaleCorrectionNotFound
	}
	if err != nil {
		return nil, err
	}
	return correctionFromPG(row), nil
}
func (r *SaleCorrectionPostgresRepository) ListCorrectionsBySale(tx sharedDomain.Transaction, gymID, saleID uuid.UUID) ([]*correctionDomain.Correction, error) {
	var rows []correctionPGRow
	err := tx.(*sharedDomain.GormTransaction).Tx.Where("gym_id=? AND sale_id=? AND deleted_at IS NULL", gymID, saleID).Order("created_at,id").Find(&rows).Error
	out := make([]*correctionDomain.Correction, len(rows))
	for i := range rows {
		out[i] = correctionFromPG(rows[i])
	}
	return out, err
}
func (r *SaleCorrectionPostgresRepository) PendingAmount(tx sharedDomain.Transaction, gymID, id uuid.UUID, lock bool) (float64, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	if lock {
		var row correctionPGRow
		err := g.Clauses(clause.Locking{Strength: "UPDATE"}).Where("gym_id=? AND id=? AND deleted_at IS NULL", gymID, id).First(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, billingErrors.ErrPendingRefundNotFound
		}
		if err != nil {
			return 0, err
		}
	}
	var due *float64
	err := g.Raw(`SELECT GREATEST(c.monetary_delta-COALESCE(SUM(r.amount) FILTER(WHERE r.kind='overcollection_settlement' AND r.deleted_at IS NULL),0),0)
		FROM sale_corrections c LEFT JOIN refunds r ON r.correction_id=c.id WHERE c.gym_id=? AND c.id=? AND c.money_resolution='refund_pending' AND c.deleted_at IS NULL GROUP BY c.id`, gymID, id).Scan(&due).Error
	if err != nil {
		return 0, err
	}
	if due == nil {
		return 0, billingErrors.ErrPendingRefundNotFound
	}
	return *due, nil
}
func correctionFromPG(x correctionPGRow) *correctionDomain.Correction {
	return &correctionDomain.Correction{ID: x.ID, GymID: x.GymID, Version: x.Version, SaleID: x.SaleID, ExpectedSaleVersion: x.ExpectedSaleVersion, Reason: x.Reason, CorrectionType: x.CorrectionType, MoneyResolution: x.MoneyResolution, IncreaseResolution: x.IncreaseResolution, MonetaryDelta: x.MonetaryDelta, BeforeSnapshot: json.RawMessage(x.BeforeSnapshot), AfterSnapshot: json.RawMessage(x.AfterSnapshot), RefundID: x.RefundID, IdempotencyKey: x.IdempotencyKey, IdempotencyFingerprint: x.IdempotencyFingerprint, IdempotencyResult: json.RawMessage(x.IdempotencyResult), CreatedBy: x.CreatedBy, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt, DeletedAt: x.DeletedAt}
}

func mirrorSaleCorrectionState(g *gorm.DB, s *saleDomain.Sale, items []*saleDomain.SaleItem) error {
	sp, _ := json.Marshal(map[string]any{"id": s.ID, "gym_id": s.GymID, "version": s.Version, "created_at": s.CreatedAt.UnixMilli(), "updated_at": s.UpdatedAt.UnixMilli(), "payment_id": s.PaymentID, "member_id": s.MemberID, "subtotal": s.Subtotal, "discount": s.Discount, "total": s.Total, "correction_version": s.CorrectionVersion})
	if err := upsertFinancialMirror(g, s.GymID, "sales", s.ID, s.Version, sp, s.DeletedAt); err != nil {
		return err
	}
	for _, it := range items {
		p, _ := json.Marshal(map[string]any{"id": it.ID, "gym_id": it.GymID, "version": it.Version, "created_at": it.CreatedAt.UnixMilli(), "updated_at": it.UpdatedAt.UnixMilli(), "deleted_at": it.DeletedAt, "sale_id": it.SaleID, "product_id": it.ProductID, "product_name_snapshot": it.ProductNameSnapshot, "unit_price_snapshot": it.UnitPriceSnapshot, "unit_cost_snapshot": it.UnitCostSnapshot, "quantity": it.Quantity, "line_total": it.LineTotal})
		if err := upsertFinancialMirror(g, it.GymID, "sale_items", it.ID, it.Version, p, it.DeletedAt); err != nil {
			return err
		}
	}
	return nil
}
func mirrorCorrection(g *gorm.DB, c *correctionDomain.Correction) error {
	p, _ := json.Marshal(map[string]any{"id": c.ID, "gym_id": c.GymID, "version": c.Version, "created_at": c.CreatedAt.UnixMilli(), "updated_at": c.UpdatedAt.UnixMilli(), "sale_id": c.SaleID, "expected_sale_version": c.ExpectedSaleVersion, "reason": c.Reason, "correction_type": c.CorrectionType, "money_resolution": c.MoneyResolution, "increase_resolution": c.IncreaseResolution, "monetary_delta": c.MonetaryDelta, "before_snapshot": c.BeforeSnapshot, "after_snapshot": c.AfterSnapshot, "idempotency_key": c.IdempotencyKey, "idempotency_fingerprint": c.IdempotencyFingerprint, "idempotency_result": c.IdempotencyResult, "created_by": c.CreatedBy})
	return upsertFinancialMirror(g, c.GymID, "sale_corrections", c.ID, c.Version, p, c.DeletedAt)
}
func upsertFinancialMirror(g *gorm.DB, gymID uuid.UUID, typ string, id uuid.UUID, version int, payload []byte, deleted *time.Time) error {
	return g.Exec(`INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at,deleted_at) VALUES(?,?,?,?,?::jsonb,NOW(),?) ON CONFLICT(gym_id,entity_type,entity_id) DO UPDATE SET version=EXCLUDED.version,payload=EXCLUDED.payload,server_updated_at=NOW(),deleted_at=EXCLUDED.deleted_at`, gymID, typ, id, version, string(payload), deleted).Error
}

func nullableCorrectionJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func nullableCorrectionText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
