//go:build sidecar

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	cashclose "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CashDrawerSQLiteRepository struct{}

func NewCashDrawerSQLiteRepository() *CashDrawerSQLiteRepository {
	return &CashDrawerSQLiteRepository{}
}

type sqliteCashDrawerRow struct {
	ID             string         `db:"id"`
	GymID          string         `db:"gym_id"`
	Version        int            `db:"version"`
	CreatedAt      int64          `db:"created_at"`
	UpdatedAt      int64          `db:"updated_at"`
	DeletedAt      sql.NullInt64  `db:"deleted_at"`
	SyncedAt       sql.NullInt64  `db:"synced_at"`
	Code           string         `db:"code"`
	Name           string         `db:"name"`
	Active         int            `db:"active"`
	IsMain         int            `db:"is_main"`
	IdempotencyKey sql.NullString `db:"idempotency_key"`
}

func (r *CashDrawerSQLiteRepository) Create(tx sharedDomain.Transaction, drawer *cashclose.CashDrawer) (*cashclose.CashDrawer, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := cashDrawerToSQLiteRow(drawer)
	_, err := stx.NamedExec(context.Background(), `INSERT INTO cash_drawers(
		id,gym_id,version,created_at,updated_at,deleted_at,code,name,active,is_main,idempotency_key)
		VALUES(:id,:gym_id,:version,:created_at,:updated_at,:deleted_at,:code,:name,:active,:is_main,:idempotency_key)`, row)
	if err != nil {
		return nil, sqliteCashDrawerConstraintError(err)
	}
	if err := enqueueCashDrawer(stx, drawer); err != nil {
		return nil, err
	}
	return drawer, nil
}

func (r *CashDrawerSQLiteRepository) Update(tx sharedDomain.Transaction, drawer *cashclose.CashDrawer, expectedVersion int) (*cashclose.CashDrawer, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	row := cashDrawerToSQLiteRow(drawer)
	res, err := stx.NamedExec(context.Background(), `UPDATE cash_drawers SET
		version=:version,updated_at=:updated_at,name=:name,active=:active
		WHERE gym_id=:gym_id AND id=:id AND version=:expected_version AND deleted_at IS NULL`, map[string]any{
		"version": row.Version, "updated_at": row.UpdatedAt, "name": row.Name, "active": row.Active,
		"gym_id": row.GymID, "id": row.ID, "expected_version": expectedVersion,
	})
	if err != nil {
		return nil, sqliteCashDrawerConstraintError(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, cashclose.ErrDrawerVersionConflict
	}
	if err := enqueueCashDrawer(stx, drawer); err != nil {
		return nil, err
	}
	return drawer, nil
}

func (r *CashDrawerSQLiteRepository) GetByID(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID) (*cashclose.CashDrawer, error) {
	var row sqliteCashDrawerRow
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &row,
		`SELECT * FROM cash_drawers WHERE gym_id=? AND id=? AND deleted_at IS NULL`, gymID.String(), drawerID.String())
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cashDrawerFromSQLiteRow(row), nil
}

func (r *CashDrawerSQLiteRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*cashclose.CashDrawer, error) {
	var row sqliteCashDrawerRow
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &row,
		`SELECT * FROM cash_drawers WHERE gym_id=? AND idempotency_key=? AND deleted_at IS NULL`,
		gymID.String(), strings.TrimSpace(key))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cashDrawerFromSQLiteRow(row), nil
}

func (r *CashDrawerSQLiteRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, includeInactive bool) ([]*cashclose.CashDrawer, error) {
	query := `SELECT * FROM cash_drawers WHERE gym_id=? AND deleted_at IS NULL`
	if !includeInactive {
		query += ` AND active=1`
	}
	query += ` ORDER BY is_main DESC,lower(name),id`
	var rows []sqliteCashDrawerRow
	if err := tx.(*sharedDomain.SqlxTransaction).Select(context.Background(), &rows, query, gymID.String()); err != nil {
		return nil, err
	}
	out := make([]*cashclose.CashDrawer, 0, len(rows))
	for _, row := range rows {
		out = append(out, cashDrawerFromSQLiteRow(row))
	}
	return out, nil
}

func (r *CashDrawerSQLiteRepository) HasOpenActivity(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID) (bool, error) {
	var busy int
	err := tx.(*sharedDomain.SqlxTransaction).Get(context.Background(), &busy, `SELECT EXISTS(
		SELECT 1 FROM cash_close_events s
		 WHERE s.gym_id=? AND s.drawer_id=? AND s.deleted_at IS NULL AND s.status<>'withdrawn'
		UNION ALL
		SELECT 1 FROM payments p
		 WHERE p.gym_id=? AND COALESCE(p.cash_drawer_id,p.gym_id)=? AND p.payment_method='cash' AND p.cash_destination<>'gym_fund'
		   AND p.deleted_at IS NULL AND NOT EXISTS(
		     SELECT 1 FROM cash_close_events s WHERE s.gym_id=p.gym_id AND s.drawer_id=?
		       AND s.operational_date=p.payment_date AND s.status='withdrawn' AND s.deleted_at IS NULL
		       AND s.opened_at<=p.created_at AND s.withdrawn_at>=p.created_at)
		UNION ALL
		SELECT 1 FROM cash_movements m
		 WHERE m.gym_id=? AND COALESCE(m.cash_drawer_id,m.gym_id)=? AND m.deleted_at IS NULL AND NOT EXISTS(
		     SELECT 1 FROM cash_close_events s WHERE s.gym_id=m.gym_id AND s.drawer_id=?
		       AND s.operational_date=m.movement_on AND s.status='withdrawn' AND s.deleted_at IS NULL
		       AND s.opened_at<=m.created_at AND s.withdrawn_at>=m.created_at)
		LIMIT 1)`, gymID.String(), drawerID.String(), gymID.String(), drawerID.String(), drawerID.String(),
		gymID.String(), drawerID.String(), drawerID.String())
	return busy != 0, err
}

func cashDrawerToSQLiteRow(drawer *cashclose.CashDrawer) sqliteCashDrawerRow {
	row := sqliteCashDrawerRow{ID: drawer.ID.String(), GymID: drawer.GymID.String(), Version: drawer.Version,
		CreatedAt: drawer.CreatedAt.UnixMilli(), UpdatedAt: drawer.UpdatedAt.UnixMilli(),
		Code: drawer.Code, Name: drawer.Name, Active: cashDrawerBoolInt(drawer.Active), IsMain: cashDrawerBoolInt(drawer.IsMain)}
	if drawer.DeletedAt != nil {
		row.DeletedAt = sql.NullInt64{Int64: drawer.DeletedAt.UnixMilli(), Valid: true}
	}
	if drawer.IdempotencyKey != nil {
		row.IdempotencyKey = sql.NullString{String: *drawer.IdempotencyKey, Valid: true}
	}
	return row
}

func cashDrawerBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func cashDrawerFromSQLiteRow(row sqliteCashDrawerRow) *cashclose.CashDrawer {
	id, _ := uuid.Parse(row.ID)
	gymID, _ := uuid.Parse(row.GymID)
	drawer := &cashclose.CashDrawer{ID: id, GymID: gymID, Version: row.Version,
		Code: row.Code, Name: row.Name, Active: row.Active != 0, IsMain: row.IsMain != 0,
		CreatedAt: time.UnixMilli(row.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(row.UpdatedAt).UTC()}
	if row.DeletedAt.Valid {
		deleted := time.UnixMilli(row.DeletedAt.Int64).UTC()
		drawer.DeletedAt = &deleted
	}
	if row.IdempotencyKey.Valid {
		key := row.IdempotencyKey.String
		drawer.IdempotencyKey = &key
	}
	return drawer
}

func enqueueCashDrawer(stx *sharedDomain.SqlxTransaction, drawer *cashclose.CashDrawer) error {
	payload, err := json.Marshal(map[string]any{
		"id": drawer.ID.String(), "gym_id": drawer.GymID.String(), "version": drawer.Version,
		"created_at": drawer.CreatedAt.UnixMilli(), "updated_at": drawer.UpdatedAt.UnixMilli(),
		"deleted_at": drawer.DeletedAt, "code": drawer.Code, "name": drawer.Name,
		"active": drawer.Active, "is_main": drawer.IsMain, "idempotency_key": drawer.IdempotencyKey,
	})
	if err != nil {
		return err
	}
	return stx.EnqueueSync(context.Background(), "cash_drawers", drawer.ID.String(), "upsert", payload, drawer.Version)
}

func sqliteCashDrawerConstraintError(err error) error {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "uq_cash_drawers_name") || strings.Contains(message, "cash_drawers.gym_id, cash_drawers.name") {
		return cashclose.ErrDrawerNameConflict
	}
	if strings.Contains(message, "uq_cash_drawers_idempotency") || strings.Contains(message, "cash_drawers.gym_id, cash_drawers.idempotency_key") {
		return cashclose.ErrDrawerIdempotencyConflict
	}
	return err
}
