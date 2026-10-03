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

func TestExpenseQueue_CompleteAfterCoalescingAndSmallBatch(t *testing.T) {
	ctx := context.Background()
	gym := uuid.New()
	db, uow := compositeKeyTestDB(t, gym)
	expenseID, cashID, occID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	enqueue := func(tx shared.Transaction, kind, id string, version int, links map[string]any) error {
		raw := map[string]any{"id": id, "gym_id": gym.String(), "version": version, "updated_at": time.Now().UnixMilli()}
		for k, v := range links {
			raw[k] = v
		}
		payload, _ := json.Marshal(raw)
		return tx.(*shared.SqlxTransaction).EnqueueSync(ctx, kind, id, "upsert", payload, version)
	}
	if err := uow.Command(ctx, func(tx shared.Transaction) error {
		if err := enqueue(tx, "expenses", expenseID, 1, map[string]any{"cash_movement_id": cashID, "recurring_occurrence_id": occID}); err != nil {
			return err
		}
		if err := enqueue(tx, "cash_movements", cashID, 1, map[string]any{"expense_id": expenseID}); err != nil {
			return err
		}
		return enqueue(tx, "expense_occurrences", occID, 1, map[string]any{"expense_id": expenseID})
	}); err != nil {
		t.Fatal(err)
	}
	// Reopening removes every explicit link. The original command manifest
	// must still connect the three snapshots after coalescing and restart.
	if err := uow.Command(ctx, func(tx shared.Transaction) error {
		if err := enqueue(tx, "expenses", expenseID, 2, nil); err != nil {
			return err
		}
		return enqueue(tx, "cash_movements", cashID, 2, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return enqueue(tx, "cash_movements", uuid.NewString(), 1, nil) }); err != nil {
		t.Fatal(err)
	}
	restarted := NewAgent(AgentConfig{BaseURL: "http://unused", BatchSize: 1, MaxBatchBytes: 1}, db, uow)
	batch, err := restarted.takeBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 3 {
		t.Fatalf("batch contains %d rows, want the complete 3-row command only", len(batch))
	}
	for _, item := range batch {
		if item.EnqueuedAt.IsZero() {
			t.Fatalf("missing enqueue time for %s", item.EntityType)
		}
		if len(expenseGraphMembers(item.Payload)) != 3 {
			t.Fatalf("missing durable manifest for %s", item.EntityType)
		}
	}
	response := &PushResponse{}
	for _, item := range batch {
		response.Results = append(response.Results, PushItemResult{QueueID: item.QueueID, EntityID: item.EntityID, Status: StatusAccepted})
	}
	// A fresh correction arrives while the older batch is in flight.
	if err := uow.Command(ctx, func(tx shared.Transaction) error {
		return enqueue(tx, "expenses", expenseID, 3, map[string]any{"amount": 200})
	}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.handlePushResponse(ctx, batch, response); err != nil {
		t.Fatal(err)
	}
	var pending int
	if err := db.Get(&pending, `SELECT COUNT(*) FROM sync_queue WHERE synced_at IS NULL`); err != nil || pending != 4 {
		t.Fatalf("late response erased a new edit: pending=%d err=%v", pending, err)
	}
}

func TestExpenseResponse_PendingCanonicalDependencyRetriesTogether(t *testing.T) {
	ctx := context.Background()
	gym := uuid.New()
	db, uow := compositeKeyTestDB(t, gym)
	expenseID, cashID := uuid.NewString(), uuid.NewString()
	enqueue := func(kind, id string) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"id": id, "gym_id": gym.String(), "version": 1})
		if err != nil {
			t.Fatal(err)
		}
		if err = uow.Command(ctx, func(tx shared.Transaction) error {
			return tx.(*shared.SqlxTransaction).EnqueueSync(ctx, kind, id, "upsert", payload, 1)
		}); err != nil {
			t.Fatal(err)
		}
	}
	enqueue("expenses", expenseID)
	agent := NewAgent(AgentConfig{BaseURL: "http://unused", BatchSize: 1}, db, uow)
	batch, err := agent.takeBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The cloud's winning movement also has an unsent local correction. It
	// must join the retry, rather than be overwritten or leave a partial ack.
	enqueue("cash_movements", cashID)
	response := &PushResponse{Results: []PushItemResult{{QueueID: batch[0].QueueID, EntityID: expenseID, Status: StatusConflictServerWins,
		RelatedChanges: []PullChange{{EntityType: "cash_movements", EntityID: cashID}}}}}
	if err = agent.handlePushResponse(ctx, batch, response); err != nil {
		t.Fatal(err)
	}
	retry, err := agent.takeBatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(retry) != 2 {
		t.Fatalf("retry has %d rows, want both pending corrections", len(retry))
	}
	for _, item := range retry {
		if len(expenseGraphMembers(item.Payload)) != 2 {
			t.Fatalf("missing joined retry for %s", item.EntityType)
		}
	}
}

func TestExpenseResponse_CanonicalTombstoneAndMissingCashDependency(t *testing.T) {
	for _, mode := range []string{"tombstone_ms", "tombstone_iso", "cloud_cash"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			gym, user := uuid.New(), uuid.New()
			db, uow := compositeKeyTestDB(t, gym)
			now := time.Now().UTC().Truncate(time.Millisecond)
			if _, err := db.Exec(`INSERT INTO users (id,gym_id,version,created_at,updated_at,email,password_hash,full_name,role,active) VALUES (?,?,1,?,?,?,'unused','Owner','owner',1)`, user, gym, now.UnixMilli(), now.UnixMilli(), user.String()+"@test.local"); err != nil {
				t.Fatal(err)
			}
			expenseID, cashID := uuid.NewString(), uuid.NewString()
			base := func(id string) map[string]any {
				return map[string]any{"id": id, "gym_id": gym.String(), "version": 2, "created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil}
			}
			cash := base(cashID)
			cash["movement_on"], cash["amount"], cash["movement_type"], cash["reason"], cash["operator_id"], cash["classification_status"] = "2026-09-26", 100, "cash_out", "Renta", user.String(), "unclassified"
			expense := base(expenseID)
			expense["expense_date"], expense["amount"], expense["category"], expense["payment_method"], expense["paid_from"], expense["classification"], expense["source"], expense["created_by"] = "2026-09-26", 100, "renta", "transfer", "gym_fund", "fixed", "manual", user.String()
			kind, id, local := "cash_movements", cashID, cash
			if mode == "cloud_cash" {
				kind, id, local = "expenses", expenseID, expense
			}
			payload, _ := json.Marshal(local)
			if err := uow.Command(ctx, func(tx shared.Transaction) error {
				if err := ApplyPullChange(ctx, tx, PullChange{EntityType: kind, EntityID: id, Version: 2, ServerUpdatedAt: now, Payload: payload}); err != nil {
					return err
				}
				return tx.(*shared.SqlxTransaction).EnqueueSync(ctx, kind, id, "upsert", payload, 2)
			}); err != nil {
				t.Fatal(err)
			}
			agent := NewAgent(AgentConfig{BaseURL: "http://unused", BatchSize: 10}, db, uow)
			batch, err := agent.takeBatch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			canonical := decodePushPayload(payload)
			result := PushItemResult{QueueID: batch[0].QueueID, EntityID: id, Status: StatusConflictServerWins, ServerVersion: 2, ServerUpdatedAt: &now}
			if mode == "cloud_cash" {
				canonical["paid_from"], canonical["payment_method"], canonical["cash_movement_id"] = "cash_register", "cash", cashID
				cash["expense_id"], cash["classification_status"] = expenseID, "expense"
				encoded, _ := json.Marshal(cash)
				result.RelatedChanges = []PullChange{{EntityType: "cash_movements", EntityID: cashID, Version: 2, ServerUpdatedAt: now, Payload: encoded}}
			} else if mode == "tombstone_iso" {
				canonical["deleted_at"] = now.Format(time.RFC3339Nano)
			} else {
				canonical["deleted_at"] = now.UnixMilli()
			}
			result.ServerPayload, _ = json.Marshal(canonical)
			if err = agent.handlePushResponse(ctx, batch, &PushResponse{Results: []PushItemResult{result}}); err != nil {
				t.Fatal(err)
			}
			var active int
			if err = db.Get(&active, `SELECT COUNT(*) FROM cash_movements WHERE deleted_at IS NULL`); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "cloud_cash" {
				want = 1
			}
			if active != want {
				t.Fatalf("active cash movements=%d want=%d", active, want)
			}
			var pending int
			if err = db.Get(&pending, `SELECT COUNT(*) FROM sync_queue WHERE synced_at IS NULL`); err != nil || pending != 0 {
				t.Fatalf("pending=%d err=%v", pending, err)
			}
		})
	}
}
