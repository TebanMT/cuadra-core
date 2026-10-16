//go:build sidecar

package sync

import (
	"context"
	"encoding/json"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"strings"
	"time"
)

func finalizeFinancialGraph(ctx context.Context, tx shared.Transaction) error {
	stx := tx.(*shared.SqlxTransaction)
	members := []string{}
	financial := false
	for key := range stx.SyncWrites {
		kind, _, _ := strings.Cut(key, "/")
		if kind == "payments" || kind == "sales" || kind == "refunds" {
			financial = true
		}
		if financialSyncType(kind) {
			members = append(members, key)
		}
	}
	if !financial {
		return nil
	}
	for _, key := range members {
		kind, id, _ := strings.Cut(key, "/")
		var payload string
		if err := stx.Get(ctx, &payload, `SELECT payload FROM sync_queue WHERE entity_type=? AND entity_id=? AND synced_at IS NULL`, kind, id); err != nil {
			return err
		}
		raw := decodePushPayload([]byte(payload))
		raw[financialMembersKey] = uniqueExpenseKeys(append(financialMembers([]byte(payload)), members...))
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

func expandFinancialBatch(ctx context.Context, stx *shared.SqlxTransaction, initial []PushItem, gymID string) ([]PushItem, error) {
	keys := map[string]bool{}
	selected := map[string]bool{}
	for _, item := range initial {
		selected[item.QueueID] = true
		for _, key := range financialKeys(item) {
			keys[key] = true
		}
	}
	if len(keys) == 0 {
		return initial, nil
	}
	var rows []struct {
		ID         string `db:"id"`
		Kind       string `db:"entity_type"`
		EntityID   string `db:"entity_id"`
		Operation  string `db:"operation"`
		Payload    string `db:"payload"`
		Version    int    `db:"client_version"`
		EnqueuedAt int64  `db:"enqueued_at"`
	}
	if err := stx.Select(ctx, &rows, `SELECT id,entity_type,entity_id,operation,payload,client_version,enqueued_at FROM sync_queue WHERE synced_at IS NULL AND (?='' OR json_extract(payload,'$.gym_id')=?) ORDER BY rowid`, gymID, gymID); err != nil {
		return nil, err
	}
	for changed := true; changed; {
		changed = false
		for _, r := range rows {
			if selected[r.ID] {
				continue
			}
			item := PushItem{QueueID: r.ID, EntityType: r.Kind, EntityID: r.EntityID, Operation: r.Operation, Payload: json.RawMessage(r.Payload), ClientVersion: r.Version, EnqueuedAt: time.UnixMilli(r.EnqueuedAt).UTC()}
			for _, key := range financialKeys(item) {
				if keys[key] {
					initial = append(initial, item)
					selected[r.ID] = true
					changed = true
					for _, k := range financialKeys(item) {
						keys[k] = true
					}
					break
				}
			}
		}
	}
	return initial, nil
}
