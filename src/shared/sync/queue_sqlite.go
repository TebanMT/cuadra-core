//go:build sidecar

package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	if stx.SyncWrites == nil {
		stx.SyncWrites = make(map[string]bool)
	}
	stx.SyncWrites[expenseMemberKey(entityType, entityID)] = true
	nowMs := time.Now().UTC().UnixMilli()

	// A coalesced mutable row (most notably the root Payment) can connect
	// several immutable refund graphs. Replacing its JSON snapshot must retain
	// those protocol-only ids or a later refund could silently split the first
	// graph across HTTP batches.
	var existing struct {
		ID      string `db:"id"`
		Payload string `db:"payload"`
	}
	err := stx.Get(ctx, &existing, `SELECT id,payload FROM sync_queue
		WHERE entity_type=? AND entity_id=? AND synced_at IS NULL LIMIT 1`, entityType, entityID)
	if err == nil {
		payload = mergeRefundGraphMetadata(json.RawMessage(existing.Payload), payload)
		payload = mergeExpenseGraphMetadata(json.RawMessage(existing.Payload), payload)
		_, err = stx.Exec(ctx, `UPDATE sync_queue
			SET payload=?,operation=?,client_version=?,enqueued_at=? WHERE id=? AND synced_at IS NULL`,
			string(payload), operation, clientVersion, nowMs, existing.ID)
		return err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
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

func mergeRefundGraphMetadata(existing, incoming json.RawMessage) json.RawMessage {
	existingRaw, incomingRaw := decodePushPayload(existing), decodePushPayload(incoming)
	if incomingRaw == nil {
		return incoming
	}
	seen := make(map[string]bool)
	var graphIDs []string
	for _, payload := range []json.RawMessage{existing, incoming} {
		for _, graphID := range refundGraphIDs(payload) {
			if !seen[graphID] {
				seen[graphID] = true
				graphIDs = append(graphIDs, graphID)
			}
		}
	}
	if len(graphIDs) > 0 {
		incomingRaw[refundGraphIDsKey] = graphIDs
	}
	if _, present := incomingRaw[refundGraphExpectedItemsKey]; !present {
		if expected, exists := existingRaw[refundGraphExpectedItemsKey]; exists {
			incomingRaw[refundGraphExpectedItemsKey] = expected
		}
	}
	encoded, err := json.Marshal(incomingRaw)
	if err != nil {
		return incoming
	}
	return encoded
}
