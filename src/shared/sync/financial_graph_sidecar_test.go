//go:build sidecar

package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func TestFinancialQueueAtomicManifestAndTinyBatch(t *testing.T) {
	ctx := context.Background()
	gym := uuid.New()
	db, uow := compositeKeyTestDB(t, gym)
	queue := NewSqliteQueue()
	ids := []string{uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()}
	kinds := []string{"payments", "sales", "sale_items", "stock_movements"}
	enqueue := func(tx shared.Transaction) error {
		for i, kind := range kinds {
			raw := map[string]any{"id": ids[i], "gym_id": gym, "version": 1, "updated_at": time.Now().UnixMilli()}
			switch kind {
			case "sales":
				raw["payment_id"] = ids[0]
			case "sale_items":
				raw["sale_id"] = ids[1]
			case "stock_movements":
				raw["sale_item_id"] = ids[2]
			}
			payload, _ := json.Marshal(raw)
			if err := queue.Enqueue(ctx, tx, kind, ids[i], OpUpsertStr, payload, 1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := uow.Command(ctx, enqueue); err != nil {
		t.Fatal(err)
	}
	agent := NewAgent(AgentConfig{BaseURL: "http://unused", BatchSize: 1, MaxBatchBytes: 1}, db, uow)
	batch, err := agent.takeBatch(ctx)
	if err != nil || len(batch) != 4 {
		t.Fatalf("split financial command: %d %v", len(batch), err)
	}
	for _, item := range batch {
		if len(financialMembers(item.Payload)) != 4 {
			t.Fatal("manifest missing")
		}
	}
	// Removing metadata simulates an existing queue upgraded from a tagged release.
	if _, err := db.Exec(`UPDATE sync_queue SET payload=json_remove(payload,'$._sync_financial_members')`); err != nil {
		t.Fatal(err)
	}
	batch, err = agent.takeBatch(ctx)
	if err != nil || len(batch) != 4 {
		t.Fatalf("legacy queue split: %d %v", len(batch), err)
	}
	response := &PushResponse{}
	for _, item := range batch {
		response.Results = append(response.Results, PushItemResult{QueueID: item.QueueID, EntityID: item.EntityID, Status: StatusRejectedFinancialConflict, Error: "operación incompleta"})
	}
	for i := 0; i < stuckPushThreshold; i++ {
		if err := agent.handlePushResponse(ctx, batch, response); err != nil {
			t.Fatal(err)
		}
	}
	var pending int
	if err := db.Get(&pending, `SELECT COUNT(*) FROM sync_queue WHERE synced_at IS NULL AND retry_count>=?`, stuckPushThreshold); err != nil || pending != 4 {
		t.Fatalf("financial rejection lost rows: %d %v", pending, err)
	}
}
