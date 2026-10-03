//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	saleDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/sale"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type SaleSQLiteRepository struct{}

func NewSaleSQLiteRepository() *SaleSQLiteRepository { return &SaleSQLiteRepository{} }

type sqliteSaleRow struct {
	ID                string         `db:"id"`
	GymID             string         `db:"gym_id"`
	Version           int            `db:"version"`
	CorrectionVersion int            `db:"correction_version"`
	CreatedAt         int64          `db:"created_at"`
	UpdatedAt         int64          `db:"updated_at"`
	DeletedAt         sql.NullInt64  `db:"deleted_at"`
	SyncedAt          sql.NullInt64  `db:"synced_at"`
	PaymentID         string         `db:"payment_id"`
	MemberID          sql.NullString `db:"member_id"`
	Subtotal          int64          `db:"subtotal"`
	Discount          int64          `db:"discount"`
	Total             int64          `db:"total"`
}

func (r *SaleSQLiteRepository) Create(tx sharedDomain.Transaction, s *saleDomain.Sale) (*saleDomain.Sale, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := saleToRow(s)
	const stmt = `
		INSERT INTO sales (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    payment_id, member_id, subtotal, discount, total, correction_version
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :payment_id, :member_id, :subtotal, :discount, :total, :correction_version
		)`
	if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
		return nil, err
	}
	if err := enqueueSale(stx, s); err != nil {
		return nil, err
	}
	return s, nil
}

func (r *SaleSQLiteRepository) GetByID(tx sharedDomain.Transaction, id uuid.UUID) (*saleDomain.Sale, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteSaleRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM sales WHERE id = ? AND deleted_at IS NULL`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrSaleNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return saleFromRow(&row), nil
}

func (r *SaleSQLiteRepository) GetByPaymentID(tx sharedDomain.Transaction, paymentID uuid.UUID) (*saleDomain.Sale, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var row sqliteSaleRow
	err := stx.Get(context.Background(), &row,
		`SELECT * FROM sales WHERE payment_id = ? AND deleted_at IS NULL`, paymentID.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sharedDomain.NewBusinessError(billingErrors.ErrSaleNotFound, "")
	}
	if err != nil {
		return nil, err
	}
	return saleFromRow(&row), nil
}

type SaleItemSQLiteRepository struct{}

func NewSaleItemSQLiteRepository() *SaleItemSQLiteRepository { return &SaleItemSQLiteRepository{} }

type sqliteSaleItemRow struct {
	ID                  string        `db:"id"`
	GymID               string        `db:"gym_id"`
	Version             int           `db:"version"`
	CreatedAt           int64         `db:"created_at"`
	UpdatedAt           int64         `db:"updated_at"`
	DeletedAt           sql.NullInt64 `db:"deleted_at"`
	SyncedAt            sql.NullInt64 `db:"synced_at"`
	SaleID              string        `db:"sale_id"`
	ProductID           string        `db:"product_id"`
	ProductNameSnapshot string        `db:"product_name_snapshot"`
	UnitPriceSnapshot   int64         `db:"unit_price_snapshot"`
	UnitCostSnapshot    sql.NullInt64 `db:"unit_cost_snapshot"`
	Quantity            int           `db:"quantity"`
	LineTotal           int64         `db:"line_total"`
}

func (r *SaleItemSQLiteRepository) CreateMany(tx sharedDomain.Transaction, items []*saleDomain.SaleItem) error {
	if len(items) == 0 {
		return nil
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	const stmt = `
		INSERT INTO sale_items (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    sale_id, product_id, product_name_snapshot, unit_price_snapshot, unit_cost_snapshot, quantity, line_total
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :sale_id, :product_id, :product_name_snapshot, :unit_price_snapshot, :unit_cost_snapshot, :quantity, :line_total
		)`
	for _, it := range items {
		row := saleItemToRow(it)
		if _, err := stx.NamedExec(context.Background(), stmt, row); err != nil {
			return err
		}
		if err := enqueueSaleItem(stx, it); err != nil {
			return err
		}
	}
	return nil
}

func (r *SaleItemSQLiteRepository) ListBySale(tx sharedDomain.Transaction, saleID uuid.UUID) ([]*saleDomain.SaleItem, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	var rows []sqliteSaleItemRow
	if err := stx.Select(context.Background(), &rows,
		`SELECT * FROM sale_items WHERE sale_id = ? AND deleted_at IS NULL ORDER BY created_at ASC`,
		saleID.String()); err != nil {
		return nil, err
	}
	out := make([]*saleDomain.SaleItem, len(rows))
	for i := range rows {
		out[i] = saleItemFromRow(&rows[i])
	}
	return out, nil
}

// SaleSummariesByPaymentIDs — espejo del PG: "Agua 1L ×2 · Proteína" por
// payment_id. group_concat de SQLite no garantiza orden — aceptable para
// una línea de resumen.
func (r *SaleItemSQLiteRepository) SaleSummariesByPaymentIDs(tx sharedDomain.Transaction, paymentIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(paymentIDs))
	if len(paymentIDs) == 0 {
		return out, nil
	}
	stx := tx.(*sharedDomain.SqlxTransaction)
	placeholders := make([]string, len(paymentIDs))
	args := make([]any, len(paymentIDs))
	for i, id := range paymentIDs {
		placeholders[i] = "?"
		args[i] = id.String()
	}
	type row struct {
		PaymentID string `db:"payment_id"`
		Summary   string `db:"summary"`
	}
	var rows []row
	q := fmt.Sprintf(`
		SELECT s.payment_id AS payment_id,
		       group_concat(
		         si.product_name_snapshot ||
		           CASE WHEN si.quantity > 1 THEN ' ×' || si.quantity ELSE '' END,
		         ' · ') AS summary
		FROM sales s
		JOIN sale_items si ON si.sale_id = s.id AND si.deleted_at IS NULL
		WHERE s.deleted_at IS NULL AND s.payment_id IN (%s)
		GROUP BY s.payment_id`, strings.Join(placeholders, ","))
	if err := stx.Select(context.Background(), &rows, q, args...); err != nil {
		return nil, err
	}
	for _, x := range rows {
		id, err := uuid.Parse(x.PaymentID)
		if err != nil {
			continue
		}
		out[id] = x.Summary
	}
	return out, nil
}

func (r *SaleItemSQLiteRepository) SaleIDsByPaymentIDs(tx sharedDomain.Transaction, paymentIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	out := make(map[uuid.UUID]uuid.UUID, len(paymentIDs))
	if len(paymentIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(paymentIDs))
	args := make([]any, len(paymentIDs))
	for i, id := range paymentIDs {
		placeholders[i], args[i] = "?", id.String()
	}
	type row struct {
		PaymentID string `db:"payment_id"`
		SaleID    string `db:"sale_id"`
	}
	var rows []row
	query := fmt.Sprintf(`SELECT p.id AS payment_id,s.id AS sale_id
		FROM payments p
		LEFT JOIN payments parent ON parent.id=p.parent_payment_id
		JOIN sales s ON s.payment_id=COALESCE(parent.parent_payment_id,p.parent_payment_id,p.id)
		WHERE p.id IN (%s) AND s.deleted_at IS NULL`, strings.Join(placeholders, ","))
	err := tx.(*sharedDomain.SqlxTransaction).Select(context.Background(), &rows, query, args...)
	for _, value := range rows {
		paymentID, paymentErr := uuid.Parse(value.PaymentID)
		saleID, saleErr := uuid.Parse(value.SaleID)
		if paymentErr == nil && saleErr == nil {
			out[paymentID] = saleID
		}
	}
	return out, err
}

func saleToRow(s *saleDomain.Sale) sqliteSaleRow {
	row := sqliteSaleRow{
		ID:                s.ID.String(),
		GymID:             s.GymID.String(),
		Version:           s.Version,
		CorrectionVersion: s.CorrectionVersion,
		CreatedAt:         s.CreatedAt.UnixMilli(),
		UpdatedAt:         s.UpdatedAt.UnixMilli(),
		PaymentID:         s.PaymentID.String(),
		Subtotal:          toCents(s.Subtotal),
		Discount:          toCents(s.Discount),
		Total:             toCents(s.Total),
	}
	if s.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: s.DeletedAt.UnixMilli(), Valid: true}
	}
	if s.MemberID != nil {
		row.MemberID = sql.NullString{String: s.MemberID.String(), Valid: true}
	}
	return row
}

func saleFromRow(r *sqliteSaleRow) *saleDomain.Sale {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	pid, _ := uuid.Parse(r.PaymentID)
	s := &saleDomain.Sale{
		ID:                id,
		GymID:             gymID,
		Version:           r.Version,
		CorrectionVersion: r.CorrectionVersion,
		PaymentID:         pid,
		Subtotal:          fromCents(r.Subtotal),
		Discount:          fromCents(r.Discount),
		Total:             fromCents(r.Total),
		CreatedAt:         time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:         time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		s.DeletedAt = &t
	}
	if r.MemberID.Valid {
		mid, _ := uuid.Parse(r.MemberID.String)
		s.MemberID = &mid
	}
	return s
}

func saleItemToRow(it *saleDomain.SaleItem) sqliteSaleItemRow {
	row := sqliteSaleItemRow{
		ID:                  it.ID.String(),
		GymID:               it.GymID.String(),
		Version:             it.Version,
		CreatedAt:           it.CreatedAt.UnixMilli(),
		UpdatedAt:           it.UpdatedAt.UnixMilli(),
		SaleID:              it.SaleID.String(),
		ProductID:           it.ProductID.String(),
		ProductNameSnapshot: it.ProductNameSnapshot,
		UnitPriceSnapshot:   toCents(it.UnitPriceSnapshot),
		Quantity:            it.Quantity,
		LineTotal:           toCents(it.LineTotal),
	}
	if it.UnitCostSnapshot != nil {
		row.UnitCostSnapshot = sql.NullInt64{Int64: toCents(*it.UnitCostSnapshot), Valid: true}
	}
	if it.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: it.DeletedAt.UnixMilli(), Valid: true}
	}
	return row
}

func saleItemFromRow(r *sqliteSaleItemRow) *saleDomain.SaleItem {
	id, _ := uuid.Parse(r.ID)
	gymID, _ := uuid.Parse(r.GymID)
	saleID, _ := uuid.Parse(r.SaleID)
	prodID, _ := uuid.Parse(r.ProductID)
	it := &saleDomain.SaleItem{
		ID:                  id,
		GymID:               gymID,
		Version:             r.Version,
		SaleID:              saleID,
		ProductID:           prodID,
		ProductNameSnapshot: r.ProductNameSnapshot,
		UnitPriceSnapshot:   fromCents(r.UnitPriceSnapshot),
		Quantity:            r.Quantity,
		LineTotal:           fromCents(r.LineTotal),
		CreatedAt:           time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt:           time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.UnitCostSnapshot.Valid {
		v := fromCents(r.UnitCostSnapshot.Int64)
		it.UnitCostSnapshot = &v
	}
	if r.DeletedAt.Valid {
		t := time.UnixMilli(r.DeletedAt.Int64).UTC()
		it.DeletedAt = &t
	}
	return it
}

func enqueueSale(stx *sharedDomain.SqlxTransaction, s *saleDomain.Sale) error {
	if stx.Queue == nil {
		return nil
	}
	var deletedAt any
	operation := "upsert"
	if s.DeletedAt != nil {
		deletedAt = s.DeletedAt.UnixMilli()
		operation = "delete"
	}
	// All NOT NULL columns must be in the payload — the cloud projector's
	// UPSERT only emits columns present in the map, and a missing required
	// column on first-sight INSERT triggers a 23502 NOT NULL violation.
	payload, err := json.Marshal(map[string]any{
		"id":                 s.ID.String(),
		"gym_id":             s.GymID.String(),
		"version":            s.Version,
		"created_at":         s.CreatedAt.UnixMilli(),
		"updated_at":         s.UpdatedAt.UnixMilli(),
		"deleted_at":         deletedAt,
		"payment_id":         s.PaymentID.String(),
		"member_id":          uuidPtrOrNil(s.MemberID),
		"subtotal":           s.Subtotal,
		"discount":           s.Discount,
		"total":              s.Total,
		"correction_version": s.CorrectionVersion,
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "sales", s.ID.String(), operation, payload, s.Version)
}

func enqueueSaleItem(stx *sharedDomain.SqlxTransaction, it *saleDomain.SaleItem) error {
	if stx.Queue == nil {
		return nil
	}
	var deletedAt any
	operation := "upsert"
	if it.DeletedAt != nil {
		deletedAt = it.DeletedAt.UnixMilli()
		operation = "delete"
	}
	// All NOT NULL columns must be in the payload — the cloud projector's
	// UPSERT only emits columns present in the map, and a missing required
	// column on first-sight INSERT triggers a 23502 NOT NULL violation.
	payload, err := json.Marshal(map[string]any{
		"id":                    it.ID.String(),
		"gym_id":                it.GymID.String(),
		"version":               it.Version,
		"created_at":            it.CreatedAt.UnixMilli(),
		"updated_at":            it.UpdatedAt.UnixMilli(),
		"deleted_at":            deletedAt,
		"sale_id":               it.SaleID.String(),
		"product_id":            it.ProductID.String(),
		"product_name_snapshot": it.ProductNameSnapshot,
		"unit_price_snapshot":   it.UnitPriceSnapshot,
		"unit_cost_snapshot":    it.UnitCostSnapshot,
		"quantity":              it.Quantity,
		"line_total":            it.LineTotal,
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "sale_items", it.ID.String(), operation, payload, it.Version)
}
