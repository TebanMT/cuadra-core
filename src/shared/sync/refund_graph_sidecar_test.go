//go:build sidecar

package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

func TestTakeBatch_ExpandsConnectedRefundGraphsAndQuarantinesTogether(t *testing.T) {
	ctx := context.Background()
	gymID := uuid.New()
	db, uow := compositeKeyTestDB(t, gymID)
	queue := NewSqliteQueue()
	graphA, graphB := uuid.NewString(), uuid.NewString()
	rootID := uuid.NewString()

	type queued struct {
		entityType, entityID string
		graphs               []string
	}
	rows := []queued{
		// The coalesced root snapshot joins two offline refunds. BatchSize=1
		// first sees only this row; expansion must follow both graph ids.
		{"payments", rootID, []string{graphA, graphB}},
		{"payments", uuid.NewString(), []string{graphA}},
		{"refunds", graphA, []string{graphA}},
		{"refund_items", uuid.NewString(), []string{graphA}},
		{"payments", uuid.NewString(), []string{graphB}},
		{"refunds", graphB, []string{graphB}},
		{"refund_items", uuid.NewString(), []string{graphB}},
	}
	if err := uow.Command(ctx, func(tx sharedDomain.Transaction) error {
		for _, row := range rows {
			payload, err := json.Marshal(map[string]any{
				"id": row.entityID, "gym_id": gymID.String(), "version": 1,
				"created_at": time.Now().UTC().UnixMilli(), "updated_at": time.Now().UTC().UnixMilli(),
				refundGraphIDsKey: row.graphs,
			})
			if err != nil {
				return err
			}
			if err := queue.Enqueue(ctx, tx, row.entityType, row.entityID, OpUpsertStr, payload, 1); err != nil {
				return err
			}
		}
		unrelatedID := uuid.NewString()
		unrelated, err := json.Marshal(map[string]any{
			"id": unrelatedID, "gym_id": gymID.String(), "version": 1,
			"created_at": time.Now().UTC().UnixMilli(), "updated_at": time.Now().UTC().UnixMilli(),
		})
		if err != nil {
			return err
		}
		return queue.Enqueue(ctx, tx, "payments", unrelatedID, OpUpsertStr, unrelated, 1)
	}); err != nil {
		t.Fatal(err)
	}

	agent := NewAgent(AgentConfig{BaseURL: "http://unused", BatchSize: 1, MaxBatchBytes: 1}, db, uow)
	batch, err := agent.takeBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != len(rows) {
		t.Fatalf("expanded batch=%d, want connected component %d", len(batch), len(rows))
	}
	if batch[0].EntityID != rootID {
		t.Fatalf("expanded order starts with %s, want root %s", batch[0].EntityID, rootID)
	}
	seen := make(map[string]bool, len(batch))
	for _, item := range batch {
		seen[item.EntityID] = true
	}
	for _, row := range rows {
		if !seen[row.entityID] {
			t.Errorf("expanded batch omitted %s/%s", row.entityType, row.entityID)
		}
	}

	response := &PushResponse{Results: make([]PushItemResult, 0, len(batch))}
	for _, item := range batch {
		response.Results = append(response.Results, PushItemResult{
			QueueID: item.QueueID, EntityID: item.EntityID,
			Status: StatusRejectedFinancialConflict,
			Error:  "otra recepción ya consumió la misma unidad devuelta",
		})
	}
	for attempt := 0; attempt < stuckPushThreshold; attempt++ {
		if err := agent.handlePushResponse(ctx, batch, response); err != nil {
			t.Fatal(err)
		}
	}
	var stuckGraphRows, unrelatedRetries int
	if err := db.Get(&stuckGraphRows, `SELECT COUNT(*) FROM sync_queue
		WHERE synced_at IS NULL AND retry_count>=? AND json_type(payload,'$._sync_graph_ids')='array'`,
		stuckPushThreshold); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&unrelatedRetries, `SELECT COALESCE(MAX(retry_count),0) FROM sync_queue
		WHERE json_type(payload,'$._sync_graph_ids') IS NULL`); err != nil {
		t.Fatal(err)
	}
	if stuckGraphRows != len(rows) || unrelatedRetries != 0 {
		t.Fatalf("stuck graph rows=%d unrelated retries=%d", stuckGraphRows, unrelatedRetries)
	}
	again, err := agent.takeBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(rows) {
		t.Fatalf("retry batch=%d, want complete graph %d", len(again), len(rows))
	}
	items := agent.loadStuckItems(ctx, &sharedDomain.SqlxTransaction{DB: db})
	if len(items) == 0 || items[0].Kind != StuckKindFinancialConflict {
		t.Fatalf("stuck detail=%+v, want visible financial conflict", items)
	}
}
