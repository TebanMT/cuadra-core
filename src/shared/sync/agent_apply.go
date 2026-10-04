//go:build sidecar

package sync

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	sqlite3 "github.com/mattn/go-sqlite3"

	cashCloseDomain "github.com/cuadra/cuadra-core/src/modules/billing/domain/cashclose"
	prodRepos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// blobColumns lists every TEXT(BLOB) column whose JSON wire form is a
// base64 string (Go's default `[]byte` JSON encoding). The sidecar must
// decode before binding, since SQLite distinguishes TEXT from BLOB.
var blobColumns = map[string]bool{
	"template_encrypted":          true,
	"whatsapp_business_token_enc": true,
}

// moneyColumns lista, por tabla, las columnas que el dominio guarda en PESOS
// (float64) pero SQLite guarda en CENTAVOS (INTEGER). El payload de sync viaja
// en PESOS — debe ser así para que el Postgres del cloud (numeric pesos) lo
// reciba sin transformar. Al aterrizar en SQLite hay que convertir
// pesos→centavos (igual que toCents en los repos); sin esto un pull/full-sync
// escribía pesos en una columna de centavos y dividía cada monto entre 100
// (catálogo a $0.50, pagos corruptos). Espejo EXACTO de los toCents() de los
// *_sqlite.go. Ver isMoneyColumn para los casos kind-dependientes (promos).
var moneyColumns = map[string]map[string]bool{
	"payments":                    {"amount": true, "recognized_amount": true, "discount_amount": true, "balance_pending": true},
	"sales":                       {"subtotal": true, "discount": true, "total": true},
	"sale_items":                  {"unit_price_snapshot": true, "unit_cost_snapshot": true, "line_total": true},
	"sale_corrections":            {"monetary_delta": true},
	"refunds":                     {"amount": true, "balance_cancelled": true},
	"refund_items":                {"amount": true},
	"inventory_purchases":         {"unit_cost": true, "total_amount": true},
	"products":                    {"price": true},
	"stock_movements":             {"cost": true},
	"expenses":                    {"amount": true},
	"cash_movements":              {"amount": true},
	"recurring_expense_templates": {"expected_amount": true},
	"expense_occurrences":         {"expected_amount": true},
	"memberships":                 {"price_snapshot": true},
	"membership_types":            {"price": true, "enrollment_fee": true, "maintenance_fee": true},
	"cash_close_events":           {"opening_cash": true, "activity_cash": true, "calculated_cash": true, "counted_cash": true, "cash_left": true, "withdrawn_cash": true},
	"cash_transfers":              {"amount": true},
	"applied_promotions":          {"discount_amount": true},
}

// keepEmptyStringColumns — espejo sidecar de notNullStringColumns del
// projector (cloud): columnas NOT NULL cuyo "" es un valor VÁLIDO del
// dominio y NO debe colapsar a NULL en el apply. El caso que lo estrenó:
// notification_templates.body — "" significa "usa el texto default de la
// librería" (Standard sólo mueve el switch, enqueueTemplate siempre emite
// ""); anularlo rompía el pull/full-sync de cualquier gym con un toggle
// ("NOT NULL constraint failed: notification_templates.body").
var keepEmptyStringColumns = map[string]map[string]bool{
	"users":                  {"email": true},
	"notification_templates": {"body": true},
	// members.phone: "" = socio sin teléfono (NOT NULL en ambos schemas).
	"members": {"phone": true},
}

// isMoneyColumn reporta si (table, col) guarda PESOS en el wire pero CENTAVOS
// en SQLite. promotions.value y applied_promotions.value_snapshot son dinero
// SÓLO cuando el kind es fixed_amount (en percent es 0-100, en extra_days son
// días) — se decide leyendo el discriminador del mismo payload.
func isMoneyColumn(table, col string, pl map[string]any) bool {
	if moneyColumns[table][col] {
		return true
	}
	switch {
	case table == "promotions" && col == "value":
		return pl["kind"] == "fixed_amount"
	case table == "applied_promotions" && col == "value_snapshot":
		return pl["kind_snapshot"] == "fixed_amount"
	}
	return false
}

// ApplyPullChange merges one server-canonical change into the local
// SQLite store. Honours LWW (local.version >= change.version → skip), and
// uses the server's `server_updated_at` as the authoritative `updated_at`
// (ADR-001 §3.1 — server reloj manda).
//
// Returns nil if the change was applied OR skipped (idempotent); only
// real DB errors are surfaced.
func ApplyPullChange(ctx context.Context, tx sharedDomain.Transaction, change PullChange) error {
	return applyPullChange(ctx, tx, change, false)
}

// applyPullChange's forceEqual mode is reserved for /sync/full recovery.
// Incremental pull keeps the historical LWW rule (equal version = skip).
// Full sync may re-apply an equal server version to repair a previously
// materialised stale/corrupt payload, but never over a still-pending local
// queue item for that exact entity.
func applyPullChange(ctx context.Context, tx sharedDomain.Transaction, change PullChange, forceEqual bool) error {
	stx := tx.(*sharedDomain.SqlxTransaction)
	table := FindTable(change.EntityType)
	if table == nil {
		// Unknown entity_type — surface so the agent can record a "skipped"
		// metric. We deliberately don't error: forward-compat with newer
		// servers that introduce types we don't know yet (ADR-001 §3.8).
		return nil
	}

	var pl map[string]any
	if err := json.Unmarshal(change.Payload, &pl); err != nil {
		return fmt.Errorf("payload not a JSON object: %w", err)
	}
	if change.EntityType == "inventory_purchase_receipts" {
		receipt, err := prodRepos.ReceiptFromPayload(change.Payload)
		if err != nil {
			return err
		}
		if receipt.ID.String() != change.EntityID || change.Version != 1 || change.DeletedAt != nil {
			return fmt.Errorf("recepción inválida")
		}
		_, err = prodRepos.NewInventoryPurchaseReceiptSQLiteRepository().Apply(tx, receipt, false)
		return err
	}
	if change.EntityType == "cash_close_events" {
		normalizeCashSessionPull(pl)
		skip, err := resolveCashSessionNaturalCollision(ctx, stx, change, pl)
		if err != nil {
			return err
		}
		if skip {
			return nil
		}
	}

	// Cómo ubicar la fila local: casi todas las tablas usan el id surrogate
	// (= entity_id); las de llave compuesta (owner_alert_configs, sin
	// columna id, PK = gym_id+alert_key) se ubican por su llave natural
	// leída del payload — espejo del path CompositeKey del projector cloud.
	// Se parsea el payload ANTES del LWW check porque el predicado compuesto
	// lo necesita.
	whereClause, whereArgs := localKeyPredicate(table, change.EntityID, pl)

	// LWW idempotency check.
	var localVer sql.NullInt64
	err := stx.Get(ctx, &localVer,
		fmt.Sprintf(`SELECT version FROM %s WHERE %s`, table.Table, whereClause),
		whereArgs...,
	)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if localVer.Valid {
		localVersion := int(localVer.Int64)
		if localVersion > change.Version {
			return nil
		}
		if localVersion == change.Version {
			if !forceEqual {
				return nil
			}
			var pending int
			if err := stx.Get(ctx, &pending, `
				SELECT EXISTS(
					SELECT 1 FROM sync_queue
					 WHERE entity_type = ? AND entity_id = ? AND synced_at IS NULL
				)`, change.EntityType, change.EntityID); err != nil {
				return fmt.Errorf("check pending local change: %w", err)
			}
			if pending != 0 {
				return nil
			}
		}
	}

	// Full-sync recovery for a very specific offline-first collision: this
	// device created a membership type while it had skipped the gym's cloud
	// history, and the canonical cloud type now arrives with the same
	// (gym_id, name) but another UUID. SQLite cannot hold both under the
	// unique name index. If —and only if— the occupant is still pending in
	// sync_queue, preserve both by renaming the local draft and rewriting its
	// queued snapshot atomically before applying the canonical row.
	if err := resolvePendingMembershipTypeNameCollision(ctx, stx, change, pl); err != nil {
		return err
	}

	// Historical movements used zero for an unknown cost. Cost corrections
	// live in inventory_purchases and do not rewrite the physical movement.
	// Accept the legacy representation from older servers without inventing a
	// cost or weakening the positive-cost constraint for other values.
	if change.EntityType == "stock_movements" && pl["cost"] == float64(0) {
		pl["cost"] = nil
	}
	if change.EntityType == "inventory_purchases" && pl["origin"] == nil {
		pl["origin"] = "desktop"
		if pl["stock_movement_id"] == nil || pl["stock_movement_id"] == "00000000-0000-0000-0000-000000000000" {
			pl["origin"] = "cloud"
		}
	}
	if change.EntityType == "products" {
		if stock, ok := pl["stock"].(float64); ok {
			base := stock
			if v, present := pl["stock_base"]; present {
				var valid bool
				base, valid = v.(float64)
				if !valid || base != math.Trunc(base) {
					return fmt.Errorf("saldo de inventario inválido")
				}
			}
			total, err := prodRepos.ReceiptStockSQLite(stx, fmt.Sprint(pl["gym_id"]), change.EntityID)
			if err != nil {
				return err
			}
			pl["stock_base"] = base
			pl["stock"] = base + float64(total)
		}
	}
	// Server timestamp wins for updated_at + synced_at.
	serverUpdatedMs := change.ServerUpdatedAt.UnixMilli()

	// created_at es NOT NULL sin DEFAULT en (casi) todo el esquema SQLite,
	// pero varios enqueues históricos no lo emitían (membership_types,
	// cash_close_events, notification_queue, notification_templates,
	// member_fingerprints, gym_ownership_transfers) — los payloads que ya
	// viven en sync_entities del cloud vienen sin la llave, y el upsert
	// rompía el full-sync de cualquier sidecar NUEVO uniéndose a un gym
	// con historia: "NOT NULL constraint failed: membership_types.created_at".
	// (SQLite valida NOT NULL sobre el arm INSERT ANTES de resolver el
	// conflicto, así que ni siquiera el path de UPDATE se salva solo.)
	//
	// Relleno en orden de honestidad:
	//   - UPDATE (fila local existe): el created_at local VERDADERO — no
	//     inventamos nada; un payload que SÍ trae la llave lo corrige (el
	//     cloud es canónico, p.ej. gymCanonicalAugmentExpr).
	//   - INSERT: updated_at del payload ORIGINAL (estable por versión;
	//     leído ANTES del override de abajo) → server_updated_at del
	//     change. Aproximación asumida: el verdadero created_at de esas
	//     filas ya no viaja en el wire. Los enqueues quedaron corregidos,
	//     así que pushes nuevos traen el valor real.
	// Guard hasCreatedAt: owner_alert_configs no tiene la columna.
	hasCreatedAt := false
	for _, c := range table.Columns {
		if c == "created_at" {
			hasCreatedAt = true
			break
		}
	}
	if v, ok := pl["created_at"]; hasCreatedAt && (!ok || v == nil) {
		filled := false
		if localVer.Valid {
			var localCreated sql.NullInt64
			gerr := stx.Get(ctx, &localCreated,
				fmt.Sprintf(`SELECT created_at FROM %s WHERE %s`, table.Table, whereClause),
				whereArgs...,
			)
			if gerr == nil && localCreated.Valid {
				pl["created_at"] = localCreated.Int64
				filled = true
			}
		}
		if !filled {
			if u, uok := pl["updated_at"]; uok && u != nil {
				pl["created_at"] = u
			} else {
				pl["created_at"] = serverUpdatedMs
			}
		}
	}

	pl["updated_at"] = serverUpdatedMs
	pl["version"] = change.Version
	if change.DeletedAt != nil {
		pl["deleted_at"] = change.DeletedAt.UnixMilli()
	} else {
		// Llave presente con nil a propósito: un pull "vivo" debe LIMPIAR
		// el deleted_at local (resurrección) — con la omisión de llaves
		// ausentes de abajo, dejarla fuera preservaría el soft-delete.
		pl["deleted_at"] = nil
	}

	// Build columns and values aligned with the registry (drops anything in
	// the payload that isn't a real column — forward-compat). Y al revés:
	// una llave AUSENTE del payload (pusher de una era anterior a esa
	// columna — p.ej. membership_types sin enrollment_fee, de antes de
	// cuotas) se OMITE del INSERT y del SET, en vez de escribir NULL
	// explícito: en insert aplica el DEFAULT del esquema (que el NULL
	// explícito derrotaba, rompiendo NOT NULL DEFAULT 0), y en update se
	// preserva el valor local. Llave presente con null JSON sigue
	// escribiendo NULL (semántica de "borrar el campo" intacta).
	cols := make([]string, 0, len(table.Columns)+1)
	args := make([]any, 0, len(table.Columns)+1)
	for _, c := range table.Columns {
		if _, ok := pl[c]; !ok {
			continue
		}
		cols = append(cols, c)
		args = append(args, extractColumnValue(pl, table.Table, c))
	}
	cols = append(cols, "synced_at")
	args = append(args, time.Now().UTC().UnixMilli())

	placeholders := strings.Repeat("?,", len(cols)-1) + "?"

	// Build "DO UPDATE SET" excluding the primary key column(s) — el id
	// surrogate, o las columnas de la llave compuesta (que además son el
	// target del ON CONFLICT).
	setParts := make([]string, 0, len(cols)-1)
	for _, c := range cols {
		if isPrimaryKeyColumn(table, c) {
			continue
		}
		setParts = append(setParts, fmt.Sprintf("%s = excluded.%s", c, c))
	}

	versionOperator := ">"
	if forceEqual {
		versionOperator = ">="
	}
	stmt := fmt.Sprintf(`
		INSERT INTO %s (%s)
		VALUES (%s)
		ON CONFLICT(%s) DO UPDATE SET %s
		WHERE excluded.version %s %s.version`,
		table.Table,
		strings.Join(cols, ","),
		placeholders,
		conflictTarget(table),
		strings.Join(setParts, ","),
		versionOperator,
		table.Table,
	)
	_, err = stx.Exec(ctx, stmt, args...)
	return err
}

func normalizeCashSessionPull(raw map[string]any) {
	if _, ok := raw["drawer_id"]; !ok {
		raw["drawer_id"] = raw["gym_id"]
	}
	if _, ok := raw["drawer_code"]; !ok {
		raw["drawer_code"] = "main"
	}
	if _, ok := raw["operational_date"]; !ok {
		raw["operational_date"] = raw["close_date"]
	}
	if _, ok := raw["sequence"]; !ok {
		raw["sequence"] = 1
	}
	if _, ok := raw["opening_cash"]; !ok {
		raw["opening_cash"] = 0
	}
	if _, ok := raw["opening_cash_known"]; !ok {
		raw["opening_cash_known"] = false
	}
	if _, ok := raw["activity_cash"]; !ok {
		raw["activity_cash"] = raw["calculated_cash"]
	}
	if _, ok := raw["adjusted_after_withdrawal"]; !ok {
		raw["adjusted_after_withdrawal"] = false
	}
	if _, ok := raw["opened_at"]; !ok {
		raw["opened_at"] = legacyPulledCashSessionDayStart(raw)
	}
	if _, ok := raw["opened_by"]; !ok {
		raw["opened_by"] = raw["closed_by"]
	}
	if _, ok := raw["closed_at"]; !ok {
		raw["closed_at"] = raw["created_at"]
	}
	if _, ok := raw["status"]; !ok {
		if raw["counted_cash"] == nil {
			raw["status"] = "closed_unverified"
		} else {
			raw["status"] = "reconciled"
		}
	}
	if raw["counted_cash"] != nil {
		if _, ok := raw["reconciled_at"]; !ok {
			raw["reconciled_at"] = raw["closed_at"]
		}
		if _, ok := raw["reconciled_by"]; !ok {
			raw["reconciled_by"] = raw["closed_by"]
		}
	}
}

func legacyPulledCashSessionDayStart(raw map[string]any) any {
	for _, key := range []string{"operational_date", "close_date"} {
		if value, ok := raw[key].(string); ok && len(value) >= len("2006-01-02") {
			if parsed, err := time.Parse("2006-01-02", value[:len("2006-01-02")]); err == nil {
				return parsed.UTC().UnixMilli()
			}
		}
	}
	if raw["created_at"] != nil {
		return raw["created_at"]
	}
	return raw["updated_at"]
}

// resolveCashSessionNaturalCollision handles the migration edge where a
// pending legacy/random-ID session and the canonical deterministic-ID session
// occupy the same natural slot. A newer pending local edit is re-keyed to the
// canonical ID and remains queued; otherwise the server row wins and the local
// duplicate is tombstoned. No physical snapshot is silently merged.
func resolveCashSessionNaturalCollision(ctx context.Context, stx *sharedDomain.SqlxTransaction, change PullChange, incoming map[string]any) (bool, error) {
	if change.DeletedAt != nil {
		return false, nil
	}
	gymID, _ := incoming["gym_id"].(string)
	drawerID, _ := incoming["drawer_id"].(string)
	operationalDate, _ := incoming["operational_date"].(string)
	sequence := intFromJSON(incoming["sequence"])
	if gymID == "" || drawerID == "" || operationalDate == "" || sequence < 1 {
		return false, nil
	}
	var local struct {
		ID        string `db:"id"`
		Version   int    `db:"version"`
		UpdatedAt int64  `db:"updated_at"`
	}
	err := stx.Get(ctx, &local, `SELECT id,version,updated_at FROM cash_close_events
		WHERE gym_id=? AND drawer_id=? AND operational_date=? AND sequence=?
		  AND id<>? AND deleted_at IS NULL LIMIT 1`, gymID, drawerID, operationalDate, sequence, change.EntityID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cash session natural collision lookup: %w", err)
	}
	var pending int
	if err := stx.Get(ctx, &pending, `SELECT EXISTS(SELECT 1 FROM sync_queue
		WHERE entity_type='cash_close_events' AND entity_id=? AND synced_at IS NULL)`, local.ID); err != nil {
		return false, fmt.Errorf("cash session pending lookup: %w", err)
	}
	incomingUpdated := int64FromJSON(incoming["updated_at"])
	if pending != 0 && local.UpdatedAt > incomingUpdated {
		var canonicalExists int
		if err := stx.Get(ctx, &canonicalExists, `SELECT EXISTS(SELECT 1 FROM cash_close_events WHERE id=?)`, change.EntityID); err != nil {
			return false, err
		}
		if canonicalExists == 0 {
			var transfers []struct {
				ID      string `db:"id"`
				Version int    `db:"version"`
			}
			if err := stx.Select(ctx, &transfers, `SELECT id,version FROM cash_transfers
				WHERE session_id=? AND deleted_at IS NULL`, local.ID); err != nil {
				return false, fmt.Errorf("cash session transfer lookup: %w", err)
			}
			newVersion := local.Version
			if change.Version >= newVersion {
				newVersion = change.Version + 1
			} else {
				newVersion++
			}
			if _, err := stx.Exec(ctx, `UPDATE cash_close_events SET id=?,version=? WHERE id=?`, change.EntityID, newVersion, local.ID); err != nil {
				return false, fmt.Errorf("re-key pending cash session: %w", err)
			}
			canonicalSessionID, parseErr := uuid.Parse(change.EntityID)
			if parseErr != nil {
				return false, fmt.Errorf("parse canonical cash session id: %w", parseErr)
			}
			for _, transfer := range transfers {
				canonicalTransferID := cashCloseDomain.DeterministicTransferID(canonicalSessionID).String()
				transferVersion := transfer.Version + 1
				updatedAt := time.Now().UTC().UnixMilli()
				if _, err := stx.Exec(ctx, `UPDATE cash_transfers
					SET id=?,version=?,updated_at=? WHERE id=?`, canonicalTransferID, transferVersion, updatedAt, transfer.ID); err != nil {
					return false, fmt.Errorf("re-key pending cash transfer: %w", err)
				}
				if _, err := stx.Exec(ctx, `UPDATE sync_queue
					SET entity_id=?,client_version=?,
					    payload=json_set(payload,'$.id',?,'$.session_id',?,'$.version',?,'$.updated_at',?)
					WHERE entity_type='cash_transfers' AND entity_id=? AND synced_at IS NULL`,
					canonicalTransferID, transferVersion, canonicalTransferID, change.EntityID,
					transferVersion, updatedAt, transfer.ID); err != nil {
					return false, fmt.Errorf("re-key queued cash transfer: %w", err)
				}
			}
			if _, err := stx.Exec(ctx, `UPDATE sync_queue
				SET entity_id=?,client_version=?,payload=json_set(payload,'$.id',?,'$.version',?)
				WHERE entity_type='cash_close_events' AND entity_id=? AND synced_at IS NULL`,
				change.EntityID, newVersion, change.EntityID, newVersion, local.ID); err != nil {
				return false, fmt.Errorf("re-key queued cash session: %w", err)
			}
			return true, nil
		}
	}
	if pending != 0 {
		if _, err := stx.Exec(ctx, `DELETE FROM sync_queue WHERE entity_type='cash_close_events' AND entity_id=? AND synced_at IS NULL`, local.ID); err != nil {
			return false, err
		}
	}
	deletedAt := change.ServerUpdatedAt.UnixMilli()
	if deletedAt == 0 {
		deletedAt = time.Now().UTC().UnixMilli()
	}
	if _, err := stx.Exec(ctx, `UPDATE cash_close_events
		SET deleted_at=?,updated_at=?,version=version+1 WHERE id=? AND deleted_at IS NULL`, deletedAt, deletedAt, local.ID); err != nil {
		return false, fmt.Errorf("tombstone duplicate cash session: %w", err)
	}
	return false, nil
}

func intFromJSON(v any) int { return int(int64FromJSON(v)) }
func int64FromJSON(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

// resolvePendingMembershipTypeNameCollision makes a full-sync self-heal a
// same-name/different-ID membership type conflict without merging prices or
// breaking memberships that already reference the local UUID.
//
// Safety boundary: a conflicting row is renamed only when it has an unsynced
// queue item. A cloud-synced/local-canonical row is never rewritten by this
// heuristic; that remains a hard constraint error requiring investigation.
func resolvePendingMembershipTypeNameCollision(
	ctx context.Context,
	stx *sharedDomain.SqlxTransaction,
	change PullChange,
	incoming map[string]any,
) error {
	if change.EntityType != "membership_types" || change.DeletedAt != nil {
		return nil
	}
	gymID, _ := incoming["gym_id"].(string)
	incomingName, _ := incoming["name"].(string)
	incomingName = strings.TrimSpace(incomingName)
	if gymID == "" || incomingName == "" {
		return nil
	}

	var local struct {
		ID      string `db:"id"`
		Version int    `db:"version"`
	}
	err := stx.Get(ctx, &local, `
		SELECT id, version
		  FROM membership_types
		 WHERE gym_id = ? AND name = ? COLLATE NOCASE
		   AND id <> ? AND deleted_at IS NULL
		 LIMIT 1`, gymID, incomingName, change.EntityID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("find local membership type name collision: %w", err)
	}

	var queued struct {
		Payload       string `db:"payload"`
		ClientVersion int    `db:"client_version"`
	}
	err = stx.Get(ctx, &queued, `
		SELECT payload, client_version
		  FROM sync_queue
		 WHERE entity_type = 'membership_types' AND entity_id = ?
		   AND synced_at IS NULL
		 ORDER BY rowid ASC
		 LIMIT 1`, local.ID)
	if err == sql.ErrNoRows {
		// Not a local pending draft: do not mutate it. The caller's INSERT
		// will surface the original UNIQUE violation with row context.
		return nil
	}
	if err != nil {
		return fmt.Errorf("load queued membership type %s: %w", local.ID, err)
	}

	newName, err := availableReceptionName(ctx, stx, gymID, local.ID, incomingName)
	if err != nil {
		return err
	}
	newVersion := local.Version + 1
	if queued.ClientVersion >= newVersion {
		newVersion = queued.ClientVersion + 1
	}
	nowMs := time.Now().UTC().UnixMilli()

	var queuedPayload map[string]any
	if err := json.Unmarshal([]byte(queued.Payload), &queuedPayload); err != nil {
		return fmt.Errorf("decode queued membership type %s: %w", local.ID, err)
	}
	queuedPayload["name"] = newName
	queuedPayload["version"] = newVersion
	queuedPayload["updated_at"] = nowMs
	rewritten, err := json.Marshal(queuedPayload)
	if err != nil {
		return fmt.Errorf("encode queued membership type %s: %w", local.ID, err)
	}

	if _, err := stx.Exec(ctx, `
		UPDATE membership_types
		   SET name = ?, version = ?, updated_at = ?, synced_at = NULL
		 WHERE id = ?`, newName, newVersion, nowMs, local.ID); err != nil {
		return fmt.Errorf("rename pending membership type %s: %w", local.ID, err)
	}
	if _, err := stx.Exec(ctx, `
		UPDATE sync_queue
		   SET payload = ?, client_version = ?, enqueued_at = ?,
		       retry_count = 0, last_error = NULL
		 WHERE entity_type = 'membership_types' AND entity_id = ?
		   AND synced_at IS NULL`,
		string(rewritten), newVersion, nowMs, local.ID); err != nil {
		return fmt.Errorf("rewrite queued membership type %s: %w", local.ID, err)
	}
	return nil
}

func availableReceptionName(
	ctx context.Context,
	stx *sharedDomain.SqlxTransaction,
	gymID, localID, base string,
) (string, error) {
	for attempt := 1; attempt <= 1000; attempt++ {
		suffix := " (recepción)"
		if attempt > 1 {
			suffix = fmt.Sprintf(" (recepción %d)", attempt)
		}
		candidate := fitMembershipTypeName(base, suffix)
		var occupied int
		if err := stx.Get(ctx, &occupied, `
			SELECT EXISTS(
				SELECT 1 FROM membership_types
				 WHERE gym_id = ? AND name = ? COLLATE NOCASE
				   AND id <> ? AND deleted_at IS NULL
			)`, gymID, candidate, localID); err != nil {
			return "", fmt.Errorf("check membership type recovery name: %w", err)
		}
		if occupied == 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no available recovery name for membership type %q", base)
}

func fitMembershipTypeName(base, suffix string) string {
	const maxNameRunes = 100
	baseRunes := []rune(strings.TrimSpace(base))
	suffixRunes := []rune(suffix)
	limit := maxNameRunes - len(suffixRunes)
	if limit < 0 {
		limit = 0
	}
	if len(baseRunes) > limit {
		baseRunes = baseRunes[:limit]
	}
	return strings.TrimSpace(string(baseRunes)) + suffix
}

// ApplyPullPage aplica una página de cambios del cloud en UNA transacción,
// con las FKs DIFERIDAS al COMMIT y errores por-fila con contexto. Es el
// camino único de aterrizaje de páginas para Pull y FullSync.
//
// Por qué diferir las FKs: el cloud ordena cada página por
// (server_updated_at, entity_id) — un orden ciego a la dirección de las FKs
// intra-tipo. El caso real que rompió el full-sync del piloto: Renew marca
// la membresía vieja con replaced_by = <id de la nueva> y crea la nueva en
// la misma tx del cloud, así que ambas llegan con timestamps a
// microsegundos; si la vieja se aplica primero, su INSERT viola la self-FK
// memberships.replaced_by → "FOREIGN KEY constraint failed" y el full-sync
// muere determinista (el cursor re-lee el mismo corte). Con
// defer_foreign_keys la validación corre al COMMIT, cuando toda la página
// ya aterrizó y la cadena está cerrada — cubre por construcción TODA FK
// intra-tipo (replaced_by, payments.parent_payment_id,
// challenge_measurements.superseded_by_id) sin importar el orden interno.
//
// `tail` corre al final, dentro de la misma tx (avance de cursor / estado).
func ApplyPullPage(
	ctx context.Context,
	uow sharedDomain.UnitOfWork,
	changes []PullChange,
	tail func(tx sharedDomain.Transaction) error,
) error {
	return applyPullPage(ctx, uow, changes, tail, false)
}

// applyFullSyncPage differs from incremental ApplyPullPage only in equal
// version handling. It lets the canonical full snapshot repair rows that a
// historical bad wire payload materialised incorrectly, while preserving an
// entity that still has a pending local queue item.
func applyFullSyncPage(
	ctx context.Context,
	uow sharedDomain.UnitOfWork,
	changes []PullChange,
	tail func(tx sharedDomain.Transaction) error,
) error {
	return applyPullPage(ctx, uow, changes, tail, true)
}

func applyPullPage(
	ctx context.Context,
	uow sharedDomain.UnitOfWork,
	changes []PullChange,
	tail func(tx sharedDomain.Transaction) error,
	forceEqual bool,
) error {
	return uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx := tx.(*sharedDomain.SqlxTransaction)
		// defer_foreign_keys es per-transaction en SQLite (se apaga solo en
		// COMMIT/ROLLBACK) y sólo tiene efecto emitido DESPUÉS del BEGIN —
		// fuera de una tx no difiere nada. No toca estado global.
		if _, err := stx.Exec(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
			return fmt.Errorf("defer_foreign_keys: %w", err)
		}
		// A receipt needs its parents to validate/project stock. Preserve the
		// caller's feed order for cursor advancement, but materialize receipts last.
		ordered := make([]PullChange, 0, len(changes))
		for _, ch := range changes {
			if ch.EntityType != "inventory_purchase_receipts" {
				ordered = append(ordered, ch)
			}
		}
		for _, ch := range changes {
			if ch.EntityType == "inventory_purchase_receipts" {
				ordered = append(ordered, ch)
			}
		}
		for _, ch := range ordered {
			if err := applyPullChange(ctx, tx, ch, forceEqual); err != nil {
				// Nunca más un error opaco: el próximo fallo dice QUÉ fila.
				return fmt.Errorf("apply %s/%s (version %d): %w",
					ch.EntityType, ch.EntityID, ch.Version, err)
			}
		}
		if tail == nil {
			return nil
		}
		return tail(tx)
	})
}

// isFKConstraintErr detecta la violación de FK diferida que SQLite reporta
// al COMMIT (mattn hace ROLLBACK tras un COMMIT fallido, así que la
// conexión queda limpia). Chequeo tipado primero; el fallback de string es
// el mensaje canónico de SQLite, estable desde hace décadas.
func isFKConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	var serr sqlite3.Error
	if errors.As(err, &serr) {
		return serr.ExtendedCode == sqlite3.ErrConstraintForeignKey
	}
	return strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// describeFKViolations nombra las filas que rompen una FK al COMMIT de una
// página. SQLite no dice qué fila violó una FK diferida, así que re-aplica
// la página en una tx desechable (mismo defer), corre PRAGMA
// foreign_key_check acotado a las tablas tocadas, y rollbackea siempre.
// Sólo corre en el camino de fallo terminal — el happy path no paga nada.
func describeFKViolations(ctx context.Context, uow sharedDomain.UnitOfWork, changes []PullChange) string {
	var report []string
	errRollback := errors.New("fk-diagnóstico: rollback intencional")
	_ = uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx := tx.(*sharedDomain.SqlxTransaction)
		if _, err := stx.Exec(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
			return errRollback
		}
		tables := make(map[string]bool)
		for _, ch := range changes {
			if t := FindTable(ch.EntityType); t != nil {
				tables[t.Table] = true
			}
			// Best-effort: los errores inmediatos ya salieron con contexto
			// desde ApplyPullPage; acá sólo interesa reproducir el estado.
			_ = ApplyPullChange(ctx, tx, ch)
		}
		for tbl := range tables {
			var viols []struct {
				Table  string        `db:"table"`
				RowID  sql.NullInt64 `db:"rowid"`
				Parent string        `db:"parent"`
				FKID   int64         `db:"fkid"`
			}
			// tbl viene del registry (FindTable), no de input externo.
			if err := stx.Select(ctx, &viols, `PRAGMA foreign_key_check(`+tbl+`)`); err != nil {
				continue
			}
			for _, v := range viols {
				id := "?"
				if v.RowID.Valid {
					var s string
					if err := stx.Get(ctx, &s,
						fmt.Sprintf(`SELECT id FROM %s WHERE rowid = ?`, v.Table),
						v.RowID.Int64); err == nil {
						id = s
					}
				}
				report = append(report, fmt.Sprintf(
					"%s/%s referencia una fila de %s que no existe", v.Table, id, v.Parent))
			}
		}
		return errRollback
	})
	if len(report) == 0 {
		return "foreign_key_check no reprodujo la violación (¿condición transitoria?)"
	}
	sort.Strings(report)
	return strings.Join(report, "; ")
}

// extractColumnValue pulls a single column out of an unmarshalled JSON
// payload, applying type-fixups the sqlite3 driver doesn't do for us
// (base64 → blob, pesos → centavos para columnas de dinero, missing key → nil).
// localKeyPredicate builds the WHERE clause (+ args) that locates a synced
// table's row in local SQLite. Surrogate-id tables use entity_id; tablas de
// llave compuesta (owner_alert_configs — sin columna `id`, PK =
// gym_id+alert_key) se ubican por su llave natural leída del payload,
// espejo de cómo el projector cloud las upsertea (projector.go, path
// CompositeKey). Sin esto, stamp/apply hacían `WHERE id = ?` y reventaban
// con "no such column: id": el row quedaba atascado en sync_queue y
// envenenaba TODO push posterior (el write-back del batch entero hace
// rollback), aunque las otras filas del batch fueran válidas.
func localKeyPredicate(t *EntityTable, entityID string, pl map[string]any) (string, []any) {
	if len(t.CompositeKey) == 0 {
		return "id = ?", []any{entityID}
	}
	clauses := make([]string, 0, len(t.CompositeKey))
	args := make([]any, 0, len(t.CompositeKey))
	for _, c := range t.CompositeKey {
		clauses = append(clauses, c+" = ?")
		args = append(args, extractColumnValue(pl, t.Table, c))
	}
	return strings.Join(clauses, " AND "), args
}

// conflictTarget names the ON CONFLICT column(s) for a table's upsert.
func conflictTarget(t *EntityTable) string {
	if len(t.CompositeKey) > 0 {
		return strings.Join(t.CompositeKey, ",")
	}
	return "id"
}

// isPrimaryKeyColumn reports whether col forms part of the table's primary
// key (excluded from the DO UPDATE SET of the upsert).
func isPrimaryKeyColumn(t *EntityTable, col string) bool {
	if len(t.CompositeKey) == 0 {
		return col == "id"
	}
	for _, c := range t.CompositeKey {
		if c == col {
			return true
		}
	}
	return false
}

func extractColumnValue(pl map[string]any, table, col string) any {
	v, ok := pl[col]
	if !ok || v == nil {
		return nil
	}
	if blobColumns[col] {
		// JSON encoded a []byte as base64 (Go default). Decode.
		s, isStr := v.(string)
		if !isStr {
			return nil
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil
		}
		return b
	}
	// String vacío → NULL: espejo exacto de nullifyEmptyString del projector
	// (cloud). Los enqueues serializan punteros-nil de strings como "" (p.ej.
	// maintenance_frequency en enqueueMT) y los CHECK del esquema exigen NULL
	// — chk de membership_types rechaza (maintenance_fee=0 AND freq='').
	// Excepciones en keepEmptyStringColumns: columnas NOT NULL donde "" es
	// un valor válido del dominio (espejo de notNullStringColumns del
	// projector).
	if s, isStr := v.(string); isStr && s == "" {
		if keepEmptyStringColumns[table][col] {
			return s
		}
		return nil
	}
	// Timestamp como string RFC3339 → epoch-ms: espejo de coerceTimestamp
	// del projector (quinta vez del patrón "el cloud es tolerante, el
	// sidecar no"). Bug concreto: enqueueGym emitía setup_completed_at como
	// *time.Time crudo → "2026-06-28T19:14:04.453Z" quedó en payloads de
	// sync_entities → el apply lo escribía tal cual en la columna INTEGER
	// (SQLite dynamic typing lo acepta sin ruido) → todo Scan posterior del
	// gym en esa máquina moría con "converting string to int64" y los
	// recibos/bienvenidas fallaban en el enqueue. Toda columna *_at del
	// esquema SQLite guarda epoch-ms — la coerción es segura por sufijo.
	if s, isStr := v.(string); isStr && strings.HasSuffix(col, "_at") {
		if t, terr := time.Parse(time.RFC3339Nano, s); terr == nil {
			return t.UnixMilli()
		}
	}
	// JSON booleans → 0/1 for SQLite columns that store INTEGER.
	if b, ok := v.(bool); ok {
		if b {
			return 1
		}
		return 0
	}
	// JSON numbers come as float64.
	if f, ok := v.(float64); ok {
		// Columnas de dinero: el wire trae PESOS, SQLite guarda CENTAVOS.
		// Convertir con el mismo redondeo que toCents (round(x*100)), o el
		// monto queda ÷100. Maneja negativos (refunds) y enteros (50.00).
		if isMoneyColumn(table, col, pl) {
			return int64(math.Round(f * 100))
		}
		// Resto: coerce valores enteros (epoch-ms, version, qty, ids) a int64
		// para que SQLite los guarde en columnas INTEGER limpiamente.
		if f == float64(int64(f)) {
			return int64(f)
		}
		return f
	}
	// Objects / arrays (JSONB columns like payment_methods, kiosk_settings)
	// — re-serialize so SQLite stores TEXT(json).
	switch v.(type) {
	case map[string]any, []any:
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return string(b)
	}
	return v
}
