//go:build server

package repositories

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	cashclose "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type CashDrawerPostgresRepository struct{}

func NewCashDrawerPostgresRepository() *CashDrawerPostgresRepository {
	return &CashDrawerPostgresRepository{}
}

func (r *CashDrawerPostgresRepository) Create(tx sharedDomain.Transaction, drawer *cashclose.CashDrawer) (*cashclose.CashDrawer, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	err := g.Exec(`INSERT INTO cash_drawers(id,gym_id,version,created_at,updated_at,deleted_at,
		code,name,active,is_main,idempotency_key) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		drawer.ID, drawer.GymID, drawer.Version, drawer.CreatedAt, drawer.UpdatedAt, drawer.DeletedAt,
		drawer.Code, drawer.Name, drawer.Active, drawer.IsMain, drawer.IdempotencyKey).Error
	if err != nil {
		return nil, cashDrawerConstraintError(err)
	}
	if err := mirrorCashDrawer(g, drawer); err != nil {
		return nil, err
	}
	return drawer, nil
}

func (r *CashDrawerPostgresRepository) Update(tx sharedDomain.Transaction, drawer *cashclose.CashDrawer, expectedVersion int) (*cashclose.CashDrawer, error) {
	g := tx.(*sharedDomain.GormTransaction).Tx
	res := g.Exec(`UPDATE cash_drawers SET version=?,updated_at=?,name=?,active=?
		WHERE gym_id=? AND id=? AND version=? AND deleted_at IS NULL`, drawer.Version,
		drawer.UpdatedAt, drawer.Name, drawer.Active, drawer.GymID, drawer.ID, expectedVersion)
	if res.Error != nil {
		return nil, cashDrawerConstraintError(res.Error)
	}
	if res.RowsAffected != 1 {
		return nil, cashclose.ErrDrawerVersionConflict
	}
	if err := mirrorCashDrawer(g, drawer); err != nil {
		return nil, err
	}
	return drawer, nil
}

func (r *CashDrawerPostgresRepository) GetByID(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID) (*cashclose.CashDrawer, error) {
	var row cashclose.CashDrawer
	err := tx.(*sharedDomain.GormTransaction).Tx.Raw(`SELECT * FROM cash_drawers
		WHERE gym_id=? AND id=? AND deleted_at IS NULL`, gymID, drawerID).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == uuid.Nil {
		return nil, nil
	}
	return &row, nil
}

func (r *CashDrawerPostgresRepository) GetByIdempotencyKey(tx sharedDomain.Transaction, gymID uuid.UUID, key string) (*cashclose.CashDrawer, error) {
	var row cashclose.CashDrawer
	err := tx.(*sharedDomain.GormTransaction).Tx.Raw(`SELECT * FROM cash_drawers
		WHERE gym_id=? AND idempotency_key=? AND deleted_at IS NULL`, gymID, strings.TrimSpace(key)).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	if row.ID == uuid.Nil {
		return nil, nil
	}
	return &row, nil
}

func (r *CashDrawerPostgresRepository) ListByGym(tx sharedDomain.Transaction, gymID uuid.UUID, includeInactive bool) ([]*cashclose.CashDrawer, error) {
	query := `SELECT * FROM cash_drawers WHERE gym_id=? AND deleted_at IS NULL`
	args := []any{gymID}
	if !includeInactive {
		query += ` AND active=TRUE`
	}
	query += ` ORDER BY is_main DESC,lower(name),id`
	var rows []*cashclose.CashDrawer
	if err := tx.(*sharedDomain.GormTransaction).Tx.Raw(query, args...).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *CashDrawerPostgresRepository) HasOpenActivity(tx sharedDomain.Transaction, gymID, drawerID uuid.UUID) (bool, error) {
	var busy bool
	err := tx.(*sharedDomain.GormTransaction).Tx.Raw(`SELECT EXISTS(
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
		LIMIT 1)`, gymID, drawerID, gymID, drawerID, drawerID, gymID, drawerID, drawerID).Scan(&busy).Error
	return busy, err
}

func mirrorCashDrawer(g *gorm.DB, drawer *cashclose.CashDrawer) error {
	payload, err := json.Marshal(map[string]any{
		"id": drawer.ID, "gym_id": drawer.GymID, "version": drawer.Version,
		"created_at": drawer.CreatedAt.UnixMilli(), "updated_at": drawer.UpdatedAt.UnixMilli(),
		"deleted_at": drawer.DeletedAt, "code": drawer.Code, "name": drawer.Name,
		"active": drawer.Active, "is_main": drawer.IsMain, "idempotency_key": drawer.IdempotencyKey,
	})
	if err != nil {
		return err
	}
	return g.Exec(`INSERT INTO sync_entities(gym_id,entity_type,entity_id,version,payload,server_updated_at,deleted_at)
		VALUES(?,'cash_drawers',?,?,?::jsonb,NOW(),?)
		ON CONFLICT(gym_id,entity_type,entity_id) DO UPDATE SET version=EXCLUDED.version,
		payload=EXCLUDED.payload,server_updated_at=NOW(),deleted_at=EXCLUDED.deleted_at`,
		drawer.GymID, drawer.ID, drawer.Version, string(payload), drawer.DeletedAt).Error
}

func cashDrawerConstraintError(err error) error {
	if err == nil {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "uq_cash_drawers_name") {
		return cashclose.ErrDrawerNameConflict
	}
	if strings.Contains(message, "uq_cash_drawers_idempotency") {
		return cashclose.ErrDrawerIdempotencyConflict
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return cashclose.ErrDrawerNameConflict
	}
	return err
}
