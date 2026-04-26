//go:build sidecar

package sync

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	pl["updated_at"] = serverUpdatedMs
	pl["version"] = change.Version
	if change.DeletedAt != nil {
		pl["deleted_at"] = change.DeletedAt.UnixMilli()
	}

	// Build columns and values aligned with the registry (drops anything in
	// the payload that isn't a real column — forward-compat).
	cols := append([]string{}, table.Columns...)
	cols = append(cols, "synced_at")
	args := make([]any, 0, len(cols))
	for _, c := range table.Columns {
		args = append(args, extractColumnValue(pl, c))
	}
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
// (base64 → blob, missing key → nil).
func extractColumnValue(pl map[string]any, col string) any {
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
	// JSON booleans → 0/1 for SQLite columns that store INTEGER.
	if b, ok := v.(bool); ok {
		if b {
			return 1
		}
		return 0
	}
	// JSON numbers come as float64 — coerce integer-looking values to int64
	// so SQLite stores them in INTEGER affinity columns cleanly.
	if f, ok := v.(float64); ok {
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
