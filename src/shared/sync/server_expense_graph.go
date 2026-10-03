//go:build server

package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"time"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type expenseSnapshot struct {
	row    syncEntityRow
	exists bool
}

// Expense corrections are one financial command. If one changed member loses
// LWW, the canonical component wins together; a newly created losing cash leg
// receives a durable tombstone, so a lost response cannot resurrect it.
func (h *Handler) processExpenseGraph(ctx context.Context, gym, client uuid.UUID, batch []PushItem, indexes []int, schemaVersion int) map[int]PushItemResult {
	result := map[int]PushItemResult{}
	reject := func(status, message string) map[int]PushItemResult {
		for _, i := range indexes {
			result[i] = PushItemResult{QueueID: batch[i].QueueID, EntityID: batch[i].EntityID, Status: status, Error: message}
		}
		return result
	}
	present := map[string]bool{}
	for _, i := range indexes {
		item := batch[i]
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
		if _, err := uuid.Parse(item.EntityID); err != nil {
			return reject(StatusRejectedFinancialConflict, "identificador inválido")
		}
		key := expenseMemberKey(item.EntityType, item.EntityID)
		if present[key] {
			return reject(StatusRejectedFinancialConflict, "registro financiero repetido en el lote")
		}
		present[key] = true
	}
	for _, i := range indexes {
		for _, key := range expenseGraphMembers(batch[i].Payload) {
			if !present[key] {
				return reject(StatusRejectedFinancialConflict, "la corrección del gasto llegó incompleta; se enviará de nuevo con todos sus registros")
			}
		}
	}
	store := h.Store.(*PostgresStore)
	ordered := append([]int(nil), indexes...)
	sort.SliceStable(ordered, func(i, j int) bool {
		rank := func(k string) int {
			if k == "expenses" {
				return 1
			}
			return 0
		}
		a, b := batch[ordered[i]], batch[ordered[j]]
		if rank(a.EntityType) != rank(b.EntityType) {
			return rank(a.EntityType) < rank(b.EntityType)
		}
		return expenseMemberKey(a.EntityType, a.EntityID) < expenseMemberKey(b.EntityType, b.EntityID)
	})
	err := h.UoW.Command(ctx, func(tx shared.Transaction) error {
		g := gormTx(tx).WithContext(ctx)
		// Serializes two first-time offline snapshots as well as existing rows.
		// Cloud commands still use their ordinary row/version locks.
		if err := g.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "expense-sync:"+gym.String()).Error; err != nil {
			return err
		}
		before := map[int]expenseSnapshot{}
		serverWins := false
		now := time.Now().UTC()
		for _, i := range ordered {
			item := batch[i]
			id := uuid.MustParse(item.EntityID)
			// Follow the repository lock order: domain row, then its sync journal.
			var locked []string
			if err := g.Raw("SELECT id::text FROM "+item.EntityType+" WHERE gym_id=? AND id=? FOR UPDATE", gym, id).Scan(&locked).Error; err != nil {
				return err
			}
			row, exists, err := store.lockRow(ctx, g, gym, item.EntityType, id)
			if err != nil {
				return err
			}
			before[i] = expenseSnapshot{row: row, exists: exists}
			updated := extractUpdatedAt(decodePushPayload(item.Payload))
			if store.MaxClockSkew > 0 && updated.After(now.Add(store.MaxClockSkew)) {
				return &pushStatusError{StatusRejectedClockSkew, "el reloj de esta computadora está adelantado"}
			}
			if exists && !sameExpenseSyncSnapshot(item.EntityType, row.Payload, item.Payload) && row.Version >= item.ClientVersion && !updated.After(row.ServerUpdatedAt) {
				serverWins = true
			}
		}
		// Fail closed for split/legacy batches. Never acknowledge an orphan and
		// leave its parent for a later HTTP request.
		for _, i := range ordered {
			item := batch[i]
			for _, key := range expenseGraphKeys(item) {
				if present[key] {
					continue
				}
				kind, id, ok := strings.Cut(key, "/")
				if !ok || !expenseSyncType(kind) {
					return newFinancialConflict("referencia de gasto inválida")
				}
				if _, err := uuid.Parse(id); err != nil {
					return newFinancialConflict("referencia de gasto inválida")
				}
				var exists bool
				if err := g.Raw("SELECT EXISTS(SELECT 1 FROM "+kind+" WHERE gym_id=? AND id=?)", gym, id).Scan(&exists).Error; err != nil {
					return err
				}
				if !exists {
					return newFinancialConflict("la corrección del gasto necesita sus registros relacionados")
				}
			}
		}
		if serverWins {
			if schemaVersion < 4 {
				return &pushStatusError{StatusRejectedSchema, "actualiza Tinta en recepción para resolver esta corrección de gastos"}
			}
			return h.resolveExpenseGraphFromServer(ctx, tx, store, gym, client, batch, ordered, before, result)
		}
		if err := g.SavePoint("expense_graph").Error; err != nil {
			return err
		}
		for _, i := range ordered {
			item := batch[i]
			out := PushItemResult{QueueID: item.QueueID, EntityID: item.EntityID}
			if err := h.applyOneInTx(ctx, tx, gym, client, item, &out); err != nil {
				return err
			}
			if out.Status != StatusAccepted && out.Status != StatusConflictClientWins && out.Status != StatusConflictServerWins {
				return &pushStatusError{out.Status, out.Error}
			}
			result[i] = out
			// LWW may assign a version above the client's snapshot. Keep the
			// domain row and journal aligned, or a later cloud edit would
			// reuse that version and be skipped by the originating desktop.
			if out.Status == StatusConflictClientWins && out.ServerVersion != item.ClientVersion {
				raw := decodePushPayload(item.Payload)
				raw["version"] = out.ServerVersion
				raw["updated_at"] = out.ServerUpdatedAt.UnixMilli()
				payload, err := json.Marshal(raw)
				if err != nil {
					return err
				}
				var deleted *time.Time
				if value := payloadTimestamp(raw["deleted_at"]); !value.IsZero() {
					deleted = &value
				}
				if err = store.updateRow(ctx, g, gym, item.EntityType, uuid.MustParse(item.EntityID), out.ServerVersion, payload, *out.ServerUpdatedAt, deleted); err != nil {
					return err
				}
				if err = project(g, item.EntityType, gym, uuid.MustParse(item.EntityID), payload); err != nil {
					return err
				}
			}
		}
		if err := validateExpenseGraphState(g, gym, batch, indexes); err != nil {
			var conflict *financialConflictError
			if !errors.As(err, &conflict) {
				return err
			}
			// A cloud-only cash leg or a paid occurrence can be absent from the local
			// snapshot. Preserve the complete canonical state instead of mixing it.
			if err2 := g.RollbackTo("expense_graph").Error; err2 != nil {
				return err2
			}
			hasCanonical := false
			for _, snapshot := range before {
				hasCanonical = hasCanonical || snapshot.exists
			}
			if !hasCanonical {
				return err
			}
			if schemaVersion < 4 {
				return &pushStatusError{StatusRejectedSchema, "actualiza Tinta en recepción para resolver esta corrección de gastos"}
			}
			return h.resolveExpenseGraphFromServer(ctx, tx, store, gym, client, batch, ordered, before, result)
		}
		return nil
	})
	if err != nil {
		if status, ok := err.(*pushStatusError); ok {
			return reject(status.status, status.msg)
		}
		out := PushItemResult{}
		classifyPushError(&out, err, batch[indexes[0]])
		return reject(out.Status, out.Error)
	}
	return result
}

func sameExpenseSyncSnapshot(kind string, a, b json.RawMessage) bool {
	left, right := decodePushPayload(a), decodePushPayload(b)
	// Only business columns matter for response-loss replay. Version and server
	// timestamps may already have advanced when the original response was lost.
	for _, key := range FindTable(kind).Columns {
		if key == "version" || key == "updated_at" {
			continue
		}
		if !reflect.DeepEqual(left[key], right[key]) {
			return false
		}
	}
	return true
}

func (h *Handler) resolveExpenseGraphFromServer(ctx context.Context, tx shared.Transaction, store *PostgresStore, gym, client uuid.UUID, batch []PushItem, ordered []int, before map[int]expenseSnapshot, result map[int]PushItemResult) error {
	g := gormTx(tx)
	now := time.Now().UTC()
	for _, i := range ordered {
		item := batch[i]
		snapshot := before[i]
		row := snapshot.row
		payload := row.Payload
		version, updated, deleted := row.Version, row.ServerUpdatedAt, row.DeletedAt
		if !snapshot.exists || version <= item.ClientVersion {
			raw := decodePushPayload(payload)
			if !snapshot.exists {
				raw = decodePushPayload(item.Payload)
				raw["deleted_at"] = now.UnixMilli()
				deleted = &now
				// A discarded draft must not claim unique backlinks belonging to the
				// winner, nor depend on another discarded draft arriving first.
				if item.EntityType == "expenses" {
					raw["cash_movement_id"], raw["recurring_occurrence_id"] = nil, nil
				}
				if item.EntityType == "cash_movements" {
					raw["expense_id"] = nil
					raw["classification_status"] = "unclassified"
				}
				if item.EntityType == "expense_occurrences" {
					raw["expense_id"], raw["resolved_by"], raw["resolved_at"], raw["skip_reason"] = nil, nil, nil, nil
					raw["status"] = "pending"
				}
			}
			version = max(version, item.ClientVersion) + 1
			updated = now
			raw["version"], raw["updated_at"] = version, now.UnixMilli()
			delete(raw, expenseGraphMembersKey)
			var err error
			payload, err = json.Marshal(raw)
			if err != nil {
				return err
			}
			id := uuid.MustParse(item.EntityID)
			if snapshot.exists {
				err = store.updateRow(ctx, g, gym, item.EntityType, id, version, payload, updated, deleted)
			} else {
				err = store.insertRow(ctx, g, gym, item.EntityType, id, version, payload, updated, deleted)
			}
			if err != nil {
				return err
			}
			if err = project(g, item.EntityType, gym, id, payload); err != nil {
				return err
			}
		}
		result[i] = PushItemResult{QueueID: item.QueueID, EntityID: item.EntityID, Status: StatusConflictServerWins, ServerVersion: version, ServerUpdatedAt: &updated, ServerPayload: payload}
		if err := h.Conflicts.Log(ctx, tx, ConflictLogEntry{GymID: gym, EntityType: item.EntityType, EntityID: uuid.MustParse(item.EntityID), ClientID: client, ClientVersion: item.ClientVersion, ServerVersion: row.Version, ClientPayload: item.Payload, ServerPayload: payload, Resolution: "server_wins"}); err != nil {
			return err
		}
		h.Metrics.IncConflict("server_wins")
	}
	related, err := expenseCanonicalDependencies(g, gym, result, batch, ordered)
	if err != nil {
		return err
	}
	first := result[ordered[0]]
	first.RelatedChanges = related
	result[ordered[0]] = first
	return nil
}

func validateExpenseGraphState(g *gorm.DB, gym uuid.UUID, batch []PushItem, indexes []int) error {
	ids := map[string][]string{"expenses": {}, "cash_movements": {}, "expense_occurrences": {}}
	for _, i := range indexes {
		for _, key := range expenseGraphKeys(batch[i]) {
			kind, id, ok := strings.Cut(key, "/")
			if ok && expenseSyncType(kind) {
				ids[kind] = append(ids[kind], id)
			}
		}
	}
	var invalid bool
	// Check both directions: the expense owns its cash/occurrence links, and a
	// classified outflow or paid occurrence must point back to that same expense.
	err := g.Raw(`SELECT EXISTS(SELECT 1 FROM expenses e
 LEFT JOIN cash_movements c ON c.gym_id=e.gym_id AND c.id=e.cash_movement_id
 LEFT JOIN expense_occurrences o ON o.gym_id=e.gym_id AND o.id=e.recurring_occurrence_id
 WHERE e.gym_id=? AND e.deleted_at IS NULL
 AND (e.id IN ? OR e.cash_movement_id IN ? OR e.recurring_occurrence_id IN ?)
 AND ((e.cash_movement_id IS NOT NULL AND (c.id IS NULL OR c.deleted_at IS NOT NULL OR c.expense_id IS DISTINCT FROM e.id OR c.classification_status<>'expense' OR c.movement_type<>'cash_out' OR e.paid_from<>'cash_register' OR c.amount<>e.amount OR c.movement_on<>e.expense_date))
 OR (e.recurring_occurrence_id IS NOT NULL AND (o.id IS NULL OR o.deleted_at IS NOT NULL OR o.status<>'paid' OR o.expense_id IS DISTINCT FROM e.id))))`, gym, ids["expenses"], ids["cash_movements"], ids["expense_occurrences"]).Scan(&invalid).Error
	if err != nil {
		return err
	}
	if invalid {
		return newFinancialConflict("el gasto no coincide con su salida de caja o pago programado")
	}
	err = g.Raw(`SELECT EXISTS(SELECT 1 FROM cash_movements c LEFT JOIN expenses e ON e.gym_id=c.gym_id AND e.id=c.expense_id
 WHERE c.gym_id=? AND c.deleted_at IS NULL AND c.classification_status='expense' AND (c.id IN ? OR c.expense_id IN ?)
 AND (e.id IS NULL OR e.deleted_at IS NOT NULL OR e.cash_movement_id IS DISTINCT FROM c.id OR e.paid_from<>'cash_register'))`, gym, ids["cash_movements"], ids["expenses"]).Scan(&invalid).Error
	if err != nil {
		return err
	}
	if invalid {
		return newFinancialConflict("la salida de caja no coincide con su gasto")
	}
	err = g.Raw(`SELECT EXISTS(SELECT 1 FROM expense_occurrences o LEFT JOIN expenses e ON e.gym_id=o.gym_id AND e.id=o.expense_id
 WHERE o.gym_id=? AND o.deleted_at IS NULL AND o.status='paid' AND (o.id IN ? OR o.expense_id IN ?)
 AND (e.id IS NULL OR e.deleted_at IS NOT NULL OR e.recurring_occurrence_id IS DISTINCT FROM o.id))`, gym, ids["expense_occurrences"], ids["expenses"]).Scan(&invalid).Error
	if err != nil {
		return err
	}
	if invalid {
		return newFinancialConflict("el pago programado no coincide con su gasto")
	}
	return nil
}

// The winning graph can reference a cloud-created movement/occurrence that the
// losing desktop has never pulled. Return that closure with the response so
// applying the correction does not depend on a later pull or page boundary.
func expenseCanonicalDependencies(g *gorm.DB, gym uuid.UUID, result map[int]PushItemResult, batch []PushItem, indexes []int) ([]PullChange, error) {
	seen := map[string]bool{}
	pending := []struct{ kind, id string }{}
	enqueue := func(kind string, raw map[string]any, field string) {
		if id, ok := raw[field].(string); ok && id != "" {
			pending = append(pending, struct{ kind, id string }{kind, id})
		}
	}
	visit := func(kind string, raw map[string]any) {
		switch kind {
		case "expenses":
			enqueue("cash_movements", raw, "cash_movement_id")
			enqueue("expense_occurrences", raw, "recurring_occurrence_id")
			enqueue("users", raw, "created_by")
		case "cash_movements":
			enqueue("expenses", raw, "expense_id")
			enqueue("cash_drawers", raw, "cash_drawer_id")
			enqueue("users", raw, "operator_id")
		case "expense_occurrences":
			enqueue("expenses", raw, "expense_id")
			enqueue("recurring_expense_templates", raw, "template_id")
			enqueue("users", raw, "resolved_by")
		case "recurring_expense_templates", "users":
			enqueue("users", raw, "created_by")
		}
	}
	for _, i := range indexes {
		seen[expenseMemberKey(batch[i].EntityType, batch[i].EntityID)] = true
		visit(batch[i].EntityType, decodePushPayload(result[i].ServerPayload))
	}
	changes := []PullChange{}
	for len(pending) > 0 {
		next := pending[0]
		pending = pending[1:]
		key := expenseMemberKey(next.kind, next.id)
		if seen[key] {
			continue
		}
		seen[key] = true
		table := FindTable(next.kind)
		var rawPayload []byte
		query := g.Raw("SELECT to_jsonb(t) FROM "+table.Table+" t WHERE gym_id=? AND id=? FOR UPDATE", gym, next.id).Row()
		if err := query.Scan(&rawPayload); err != nil {
			if errors.Is(err, sql.ErrNoRows) && next.kind == "cash_drawers" && next.id == gym.String() {
				continue
			}
			return nil, err
		}
		raw := decodePushPayload(rawPayload)
		filtered := map[string]any{}
		for _, column := range table.Columns {
			if value, ok := raw[column]; ok {
				filtered[column] = value
			}
		}
		version, _ := numericInt(raw["version"])
		updated := payloadTimestamp(raw["updated_at"])
		var journal struct {
			Version int
			Updated time.Time `gorm:"column:server_updated_at"`
		}
		if err := g.Raw(`SELECT version,server_updated_at FROM sync_entities WHERE gym_id=? AND entity_type=? AND entity_id=?`, gym, next.kind, next.id).Scan(&journal).Error; err != nil {
			return nil, err
		}
		if journal.Version > version {
			version = journal.Version
		}
		if journal.Updated.After(updated) {
			updated = journal.Updated
		}
		payload, err := json.Marshal(filtered)
		if err != nil {
			return nil, err
		}
		change := PullChange{EntityType: next.kind, EntityID: next.id, Version: version, ServerUpdatedAt: updated, Payload: payload}
		if deleted := payloadTimestamp(raw["deleted_at"]); !deleted.IsZero() {
			change.DeletedAt = &deleted
		}
		changes = append(changes, change)
		visit(next.kind, raw)
	}
	return changes, nil
}
