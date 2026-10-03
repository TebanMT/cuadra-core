//go:build sidecar

package sync

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Finalize runs before committing the business transaction. All financial
// rows written by an expense command get a durable completeness manifest.
func (q *SqliteQueue) Finalize(ctx context.Context, tx shared.Transaction) error {
	stx := tx.(*shared.SqlxTransaction)
	hasExpense := false
	members := []string{}
	for key := range stx.SyncWrites {
		kind, _, _ := strings.Cut(key, "/")
		if kind == "expenses" {
			hasExpense = true
		}
		if expenseSyncType(kind) {
			members = append(members, key)
		}
	}
	if !hasExpense {
		return nil
	}
	members = uniqueExpenseKeys(members)
	return joinPendingExpenseMembers(ctx, stx, members)
}

func joinPendingExpenseMembers(ctx context.Context, stx *shared.SqlxTransaction, members []string) error {
	for _, key := range uniqueExpenseKeys(members) {
		kind, id, _ := strings.Cut(key, "/")
		var payload string
		if err := stx.Get(ctx, &payload, `SELECT payload FROM sync_queue WHERE entity_type=? AND entity_id=? AND synced_at IS NULL`, kind, id); err != nil {
			return err
		}
		raw := decodePushPayload([]byte(payload))
		raw[expenseGraphMembersKey] = uniqueExpenseKeys(append(expenseGraphMembers([]byte(payload)), members...))
		encoded, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		if _, err = stx.Exec(ctx, `UPDATE sync_queue SET payload=? WHERE entity_type=? AND entity_id=? AND synced_at IS NULL`, string(encoded), kind, id); err != nil {
			return err
		}
	}
	return nil
}

func expandExpenseGraphBatch(ctx context.Context, stx *shared.SqlxTransaction, initial []PushItem, gymID string) ([]PushItem, error) {
	selected := map[string]bool{}
	keys := map[string]bool{}
	for _, item := range initial {
		selected[item.QueueID] = true
		for _, key := range expenseGraphKeys(item) {
			keys[key] = true
		}
	}
	if len(keys) == 0 {
		return initial, nil
	}
	var queued []struct {
		ID         string `db:"id"`
		Kind       string `db:"entity_type"`
		EntityID   string `db:"entity_id"`
		Operation  string `db:"operation"`
		Payload    string `db:"payload"`
		Version    int    `db:"client_version"`
		EnqueuedAt int64  `db:"enqueued_at"`
	}
	if err := stx.Select(ctx, &queued, `SELECT id,entity_type,entity_id,operation,payload,client_version,enqueued_at FROM sync_queue WHERE synced_at IS NULL AND entity_type IN ('expenses','cash_movements','expense_occurrences') AND (?='' OR json_extract(payload,'$.gym_id')=?) ORDER BY rowid`, gymID, gymID); err != nil {
		return nil, err
	}
	for changed := true; changed; {
		changed = false
		for _, row := range queued {
			if selected[row.ID] {
				continue
			}
			item := PushItem{QueueID: row.ID, EntityType: row.Kind, EntityID: row.EntityID, Operation: row.Operation, ClientVersion: row.Version, Payload: json.RawMessage(row.Payload), EnqueuedAt: time.UnixMilli(row.EnqueuedAt).UTC()}
			for _, key := range expenseGraphKeys(item) {
				if !keys[key] {
					continue
				}
				initial = append(initial, item)
				selected[row.ID] = true
				changed = true
				for _, member := range expenseGraphKeys(item) {
					keys[member] = true
				}
				break
			}
		}
	}
	return initial, nil
}
