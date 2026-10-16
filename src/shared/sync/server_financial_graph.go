//go:build server

package sync

import (
	"context"
	"errors"
	"sort"
	"strings"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// processFinancialGraph is the transaction boundary of an ordinary offline
// command. No journal row, money, service period or inventory event is published
// if any member fails. Existing refund evidence validators remain authoritative.
func (h *Handler) processFinancialGraph(ctx context.Context, gym, client uuid.UUID, batch []PushItem, indexes []int) map[int]PushItemResult {
	unit := refundPushUnit{indexes: indexes}
	reject := func(status, message string) map[int]PushItemResult {
		return rejectRefundUnit(batch, unit, status, message)
	}
	present := map[string]bool{}
	for _, i := range indexes {
		item := batch[i]
		if !financialSyncType(item.EntityType) {
			return reject(StatusRejectedFinancialConflict, "la operación incluye un tipo de registro no permitido")
		}
		if invalid := validatePushItem(item); invalid != nil {
			return reject(invalid.Status, invalid.Error)
		}
		raw := decodePushPayload(item.Payload)
		if raw["gym_id"] != gym.String() {
			return reject(StatusRejectedUnauthorized, "gimnasio inválido")
		}
		if raw["id"] != item.EntityID || item.ClientVersion < 1 {
			return reject(StatusRejectedFinancialConflict, "registro financiero inválido")
		}
		if id, err := uuid.Parse(item.EntityID); err != nil || id == uuid.Nil {
			return reject(StatusRejectedFinancialConflict, "identificador inválido")
		}
		key := expenseMemberKey(item.EntityType, item.EntityID)
		if present[key] {
			return reject(StatusRejectedFinancialConflict, "registro financiero repetido en el lote")
		}
		present[key] = true
	}
	for _, i := range indexes {
		for _, key := range financialMembers(batch[i].Payload) {
			if !present[key] {
				return reject(StatusRejectedFinancialConflict, "operación incompleta: faltan registros relacionados; actualiza Tinta y vuelve a sincronizar")
			}
		}
	}
	ordered := append([]int(nil), indexes...)
	rank := func(item PushItem) int {
		switch item.EntityType {
		case "members":
			return -3
		case "memberships":
			return -2
		case "membership_adjustments":
			return -1
		case "payment_corrections":
			return 7
		case "stock_movements":
			return 9
		case "applied_promotions":
			return 10
		}
		if item.EntityType == "payments" && decodePushPayload(item.Payload)["parent_payment_id"] != nil && decodePushPayload(item.Payload)["concept"] != "refund" {
			return 1
		}
		return 2 * refundPushRank(item)
	}
	sort.SliceStable(ordered, func(i, j int) bool { return rank(batch[ordered[i]]) < rank(batch[ordered[j]]) })
	results := map[int]PushItemResult{}
	err := h.UoW.Command(ctx, func(tx shared.Transaction) error {
		g := gormTx(tx).WithContext(ctx)
		// Same lock order for overlapping offline graphs, including first inserts.
		if err := g.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "financial-sync:"+gym.String()).Error; err != nil {
			return err
		}
		subset := make([]PushItem, len(indexes))
		for n, i := range indexes {
			subset[n] = batch[i]
		}
		refundUnits := []refundPushUnit{}
		for _, u := range groupRefundPushItems(subset) {
			if len(u.graphIDs) > 0 {
				refundUnits = append(refundUnits, u)
			}
		}
		if len(refundUnits) > 0 {
			if err := g.Exec(`SELECT set_config(?, '1', true)`, atomicRefundSetting).Error; err != nil {
				return err
			}
			for _, u := range refundUnits {
				ids := []uuid.UUID{}
				for _, s := range u.graphIDs {
					id, err := uuid.Parse(s)
					if err != nil {
						return newFinancialConflict("identificador de devolución inválido")
					}
					ids = append(ids, id)
				}
				if err := validateRefundGraphInput(g, gym, ids, subset, u); err != nil {
					return err
				}
			}
		}
		for _, i := range ordered {
			item := batch[i]
			out := PushItemResult{QueueID: item.QueueID, EntityID: item.EntityID}
			if err := h.applyOneInTx(ctx, tx, gym, client, item, &out); err != nil {
				return err
			}
			switch out.Status {
			case StatusAccepted, StatusConflictClientWins, StatusConflictServerWins:
			default:
				return &pushStatusError{out.Status, out.Error}
			}
			results[i] = out
		}
		// Explicit and inferred references must close in this same transaction.
		for _, i := range indexes {
			for _, key := range financialKeys(batch[i]) {
				kind, id, ok := strings.Cut(key, "/")
				if !ok || !financialSyncType(kind) {
					return newFinancialConflict("referencia financiera inválida")
				}
				if _, err := uuid.Parse(id); err != nil {
					return newFinancialConflict("referencia financiera inválida")
				}
				var exists bool
				if err := g.Raw("SELECT EXISTS(SELECT 1 FROM "+kind+" WHERE gym_id=? AND id=?)", gym, id).Scan(&exists).Error; err != nil {
					return err
				}
				if !exists {
					return newFinancialConflict("operación incompleta: faltan registros relacionados; actualiza Tinta y vuelve a sincronizar")
				}
			}
		}
		if err := validateOrdinaryFinancialState(g, gym, subset); err != nil {
			return err
		}
		for _, u := range refundUnits {
			ids := []uuid.UUID{}
			for _, s := range u.graphIDs {
				ids = append(ids, uuid.MustParse(s))
			}
			if err := validateAtomicRefundGraphs(g, gym, ids, subset, u); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		return results
	}
	var status *pushStatusError
	if errors.As(err, &status) {
		return reject(status.status, status.msg)
	}
	out := PushItemResult{}
	classifyPushError(&out, err, batch[indexes[0]])
	return reject(out.Status, out.Error)
}

func validateOrdinaryFinancialState(g *gorm.DB, gym uuid.UUID, batch []PushItem) error {
	payments, sales := []string{}, []string{}
	for _, item := range batch {
		if item.EntityType == "payments" {
			payments = append(payments, item.EntityID)
		}
		if item.EntityType == "sales" {
			sales = append(sales, item.EntityID)
		}
		if item.EntityType == "sale_items" {
			if id, ok := decodePushPayload(item.Payload)["sale_id"].(string); ok {
				sales = append(sales, id)
			}
		}
	}
	var invalid bool
	// A product collection always has its sale, even when a legacy client has
	// omitted a manifest or its HTTP batch ended just after the payment.
	if err := g.Raw(`SELECT EXISTS(SELECT 1 FROM payments p WHERE p.gym_id=? AND p.id IN ? AND p.deleted_at IS NULL AND p.concept='product' AND NOT EXISTS(SELECT 1 FROM sales s WHERE s.gym_id=p.gym_id AND s.payment_id=p.id))`, gym, payments).Scan(&invalid).Error; err != nil {
		return err
	}
	if invalid {
		return newFinancialConflict("venta incompleta: falta su detalle; actualiza Tinta y vuelve a sincronizar")
	}
	if err := g.Raw(`SELECT EXISTS(SELECT 1 FROM sales s WHERE s.gym_id=? AND (s.id IN ? OR s.payment_id IN ?) AND s.deleted_at IS NULL AND
 (NOT EXISTS(SELECT 1 FROM sale_items i WHERE i.gym_id=s.gym_id AND i.sale_id=s.id AND i.deleted_at IS NULL)
 OR ROUND(s.subtotal*100)<>(SELECT COALESCE(SUM(ROUND(i.line_total*100)),0) FROM sale_items i WHERE i.gym_id=s.gym_id AND i.sale_id=s.id AND i.deleted_at IS NULL)
 OR ROUND(s.total*100)<>ROUND((s.subtotal-s.discount)*100)
 OR (s.correction_version=0 AND EXISTS(SELECT 1 FROM sale_items i WHERE i.gym_id=s.gym_id AND i.sale_id=s.id AND i.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM stock_movements m WHERE m.gym_id=i.gym_id AND m.sale_item_id=i.id AND m.movement_type='sale' AND m.delta=-i.quantity AND m.product_id=i.product_id AND m.deleted_at IS NULL)))))`, gym, sales, payments).Scan(&invalid).Error; err != nil {
		return err
	}
	if invalid {
		return newFinancialConflict("venta incompleta: el detalle, importe o movimiento de inventario no coincide; vuelve a sincronizar la operación completa")
	}
	return nil
}
