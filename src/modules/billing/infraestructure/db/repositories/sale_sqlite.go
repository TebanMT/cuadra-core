//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	billingErrors "github.com/cuadra/cuadra-core/src/modules/billing/domain/errors"
	saleDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/sale"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type SaleSQLiteRepository struct{}

func NewSaleSQLiteRepository() *SaleSQLiteRepository { return &SaleSQLiteRepository{} }

type sqliteSaleRow struct {
	ID        string         `db:"id"`
	GymID     string         `db:"gym_id"`
	Version   int            `db:"version"`
	CreatedAt int64          `db:"created_at"`
	UpdatedAt int64          `db:"updated_at"`
	DeletedAt sql.NullInt64  `db:"deleted_at"`
	SyncedAt  sql.NullInt64  `db:"synced_at"`
	PaymentID string         `db:"payment_id"`
	MemberID  sql.NullString `db:"member_id"`
	Subtotal  int64          `db:"subtotal"`
	Discount  int64          `db:"discount"`
	Total     int64          `db:"total"`
}

func (r *SaleSQLiteRepository) Create(tx sharedDomain.Transaction, s *saleDomain.Sale) (*saleDomain.Sale, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := saleToRow(s)
	const stmt = `
		INSERT INTO sales (
		    id, gym_id, version, created_at, updated_at, deleted_at,
		    payment_id, member_id, subtotal, discount, total
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :payment_id, :member_id, :subtotal, :discount, :total
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
		    sale_id, product_id, product_name_snapshot, unit_price_snapshot, quantity, line_total
		) VALUES (
		    :id, :gym_id, :version, :created_at, :updated_at, :deleted_at,
		    :sale_id, :product_id, :product_name_snapshot, :unit_price_snapshot, :quantity, :line_total
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

func saleToRow(s *saleDomain.Sale) sqliteSaleRow {
	row := sqliteSaleRow{
		ID:        s.ID.String(),
		GymID:     s.GymID.String(),
		Version:   s.Version,
		CreatedAt: s.CreatedAt.UnixMilli(),
		UpdatedAt: s.UpdatedAt.UnixMilli(),
		PaymentID: s.PaymentID.String(),
		Subtotal:  toCents(s.Subtotal),
		Discount:  toCents(s.Discount),
		Total:     toCents(s.Total),
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
		ID:        id,
		GymID:     gymID,
		Version:   r.Version,
		PaymentID: pid,
		Subtotal:  fromCents(r.Subtotal),
		Discount:  fromCents(r.Discount),
		Total:     fromCents(r.Total),
		CreatedAt: time.UnixMilli(r.CreatedAt).UTC(),
		UpdatedAt: time.UnixMilli(r.UpdatedAt).UTC(),
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
	memberID := ""
	if s.MemberID != nil {
		memberID = s.MemberID.String()
	}
	payload, err := json.Marshal(map[string]any{
		"id":         s.ID.String(),
		"gym_id":     s.GymID.String(),
		"version":    s.Version,
		"payment_id": s.PaymentID.String(),
		"member_id":  memberID,
		"subtotal":   s.Subtotal,
		"discount":   s.Discount,
		"total":      s.Total,
		"updated_at": s.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "sales", s.ID.String(), "upsert", payload, s.Version)
}

func enqueueSaleItem(stx *sharedDomain.SqlxTransaction, it *saleDomain.SaleItem) error {
	if stx.Queue == nil {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"id":                    it.ID.String(),
		"gym_id":                it.GymID.String(),
		"version":               it.Version,
		"sale_id":               it.SaleID.String(),
		"product_id":            it.ProductID.String(),
		"product_name_snapshot": it.ProductNameSnapshot,
		"unit_price_snapshot":   it.UnitPriceSnapshot,
		"quantity":              it.Quantity,
		"line_total":            it.LineTotal,
		"updated_at":            it.UpdatedAt.UnixMilli(),
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "sale_items", it.ID.String(), "upsert", payload, it.Version)
}
