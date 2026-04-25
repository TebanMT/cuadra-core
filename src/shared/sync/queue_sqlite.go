//go:build sidecar

package sync

import (
	"context"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Enqueue inserts (or updates, if a pending row already exists for the same
// entity) a sync_queue snapshot. Coalescing per ADR-001 §3.2: if there's a
// pending row for the same (entity_type, entity_id), we replace its payload
// instead of stacking entries.
func (q *SqliteQueue) Enqueue(
	ctx context.Context,
	tx sharedDomain.Transaction,
	entityType, entityID, operation string,
	payload []byte,
	clientVersion int,
) error {
	stx := tx.(*sharedDomain.SqlxTransaction)
	nowMs := time.Now().UTC().UnixMilli()

	res, err := stx.Exec(ctx, `
		UPDATE sync_queue
		SET payload = ?, operation = ?, client_version = ?, enqueued_at = ?
		WHERE entity_type = ? AND entity_id = ? AND synced_at IS NULL`,
		string(payload), operation, clientVersion, nowMs, entityType, entityID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}

	id := uuid.New().String()
	_, err = stx.Exec(ctx, `
		INSERT INTO sync_queue (
		    id, entity_type, entity_id, operation, payload, client_version, enqueued_at
		) VALUES (?,?,?,?,?,?,?)`,
		id, entityType, entityID, operation, string(payload), clientVersion, nowMs,
	)
	return err
}
