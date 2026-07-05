//go:build sidecar

package sync

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

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
	"payments":           {"amount": true, "discount_amount": true, "balance_pending": true},
	"sales":              {"subtotal": true, "discount": true, "total": true},
	"sale_items":         {"unit_price_snapshot": true, "line_total": true},
	"products":           {"price": true},
	"stock_movements":    {"cost": true},
	"expenses":           {"amount": true},
	"memberships":        {"price_snapshot": true},
	"membership_types":   {"price": true, "enrollment_fee": true, "maintenance_fee": true},
	"cash_close_events":  {"calculated_cash": true, "counted_cash": true},
	"applied_promotions": {"discount_amount": true},
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
	stx := tx.(*sharedDomain.SqlxTransaction)
	table := FindTable(change.EntityType)
	if table == nil {
		// Unknown entity_type — surface so the agent can record a "skipped"
		// metric. We deliberately don't error: forward-compat with newer
		// servers that introduce types we don't know yet (ADR-001 §3.8).
		return nil
	}

	// LWW idempotency check.
	var localVer sql.NullInt64
	err := stx.Get(ctx, &localVer,
		fmt.Sprintf(`SELECT version FROM %s WHERE id = ?`, table.Table),
		change.EntityID,
	)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if localVer.Valid && int(localVer.Int64) >= change.Version {
		return nil
	}

	var pl map[string]any
	if err := json.Unmarshal(change.Payload, &pl); err != nil {
		return fmt.Errorf("payload not a JSON object: %w", err)
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
				fmt.Sprintf(`SELECT created_at FROM %s WHERE id = ?`, table.Table),
				change.EntityID,
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

	// Build "DO UPDATE SET" excluding the primary key.
	setParts := make([]string, 0, len(cols)-1)
	for _, c := range cols {
		if c == "id" {
			continue
		}
		setParts = append(setParts, fmt.Sprintf("%s = excluded.%s", c, c))
	}

	stmt := fmt.Sprintf(`
		INSERT INTO %s (%s)
		VALUES (%s)
		ON CONFLICT(id) DO UPDATE SET %s
		WHERE excluded.version > %s.version`,
		table.Table,
		strings.Join(cols, ","),
		placeholders,
		strings.Join(setParts, ","),
		table.Table,
	)
	_, err = stx.Exec(ctx, stmt, args...)
	return err
}

// extractColumnValue pulls a single column out of an unmarshalled JSON
// payload, applying type-fixups the sqlite3 driver doesn't do for us
// (base64 → blob, pesos → centavos para columnas de dinero, missing key → nil).
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
	// — chk de membership_types rechaza (maintenance_fee=0 AND freq=''). El
	// mismo razonamiento del projector aplica acá: ningún enqueue emite ""
	// para una columna string NOT NULL.
	if s, isStr := v.(string); isStr && s == "" {
		return nil
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
