//go:build sidecar

package sync_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
)

func TestApplyPullPage_ExpensesPlusRoundTripsMoneyLinksAndTombstone(t *testing.T) {
	gymID, userID := uuid.New(), uuid.New()
	db, uow := freshSidecarDBWithGym(t, gymID)
	now := time.Now().UTC()
	ms := now.UnixMilli()
	if _, err := db.Exec(`INSERT INTO users(id,gym_id,version,created_at,updated_at,email,full_name,role,active,must_change_password) VALUES(?,?,?,?,?,?,?,?,1,0)`, userID, gymID, 1, ms, ms, "owner-sync@gym.test", "Owner Sync", "owner"); err != nil {
		t.Fatal(err)
	}
	templateID, occurrenceID, movementID, expenseID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	payload := func(v map[string]any) json.RawMessage {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	base := func(id uuid.UUID) map[string]any {
		return map[string]any{"id": id, "gym_id": gymID, "version": 1, "created_at": ms, "updated_at": ms}
	}
	tpl := base(templateID)
	tpl["name"], tpl["category"], tpl["expected_amount"] = "Publicidad", "marketing", 1250.75
	tpl["usual_payment_method"], tpl["classification"], tpl["frequency"] = "cash", "variable", "monthly"
	tpl["starts_on"], tpl["next_due_on"], tpl["active"], tpl["created_by"] = "2026-08-13", "2026-09-13", true, userID
	occ := base(occurrenceID)
	occ["version"] = 2
	occ["template_id"], occ["due_on"], occ["expected_amount"] = templateID, "2026-08-13", 1250.75
	occ["category"], occ["payment_method"], occ["classification"], occ["status"] = "marketing", "cash", "variable", "paid"
	occ["expense_id"], occ["resolved_by"], occ["resolved_at"] = expenseID, userID, ms
	mov := base(movementID)
	mov["movement_on"], mov["amount"], mov["movement_type"] = "2026-08-13", 1250.75, "cash_out"
	mov["reason"], mov["operator_id"], mov["expense_id"], mov["classification_status"] = "Publicidad", userID, expenseID, "expense"
	exp := base(expenseID)
	exp["expense_date"], exp["amount"], exp["category"] = "2026-08-13", 1250.75, "marketing"
	exp["payment_method"], exp["created_by"], exp["classification"], exp["source"] = "cash", userID, "variable", "recurring"
	exp["recurring_occurrence_id"], exp["cash_movement_id"] = occurrenceID, movementID

	changes := []syncpkg.PullChange{
		{EntityType: "recurring_expense_templates", EntityID: templateID.String(), Version: 1, Payload: payload(tpl), ServerUpdatedAt: now},
		{EntityType: "expense_occurrences", EntityID: occurrenceID.String(), Version: 2, Payload: payload(occ), ServerUpdatedAt: now},
		{EntityType: "cash_movements", EntityID: movementID.String(), Version: 1, Payload: payload(mov), ServerUpdatedAt: now},
		{EntityType: "expenses", EntityID: expenseID.String(), Version: 1, Payload: payload(exp), ServerUpdatedAt: now},
	}
	if err := syncpkg.ApplyPullPage(context.Background(), uow, changes, nil); err != nil {
		t.Fatal(err)
	}
	var amount, expected int64
	var classification, occurrenceClassification, cashLink string
	if err := db.QueryRow(`SELECT amount,classification,cash_movement_id FROM expenses WHERE id=?`, expenseID).Scan(&amount, &classification, &cashLink); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT expected_amount,classification FROM expense_occurrences WHERE id=?`, occurrenceID).Scan(&expected, &occurrenceClassification); err != nil {
		t.Fatal(err)
	}
	if amount != 125075 || expected != 125075 || classification != "variable" || occurrenceClassification != "variable" || cashLink != movementID.String() {
		t.Fatalf("amount=%d expected=%d class=%s occurrence_class=%s cash_link=%s", amount, expected, classification, occurrenceClassification, cashLink)
	}

	// A stale full-sync page must not revive an already-paid occurrence.
	staleOccurrence := base(occurrenceID)
	staleOccurrence["template_id"], staleOccurrence["due_on"], staleOccurrence["expected_amount"] = templateID, "2026-08-13", 1250.75
	staleOccurrence["category"], staleOccurrence["payment_method"], staleOccurrence["classification"], staleOccurrence["status"] = "marketing", "cash", "variable", "pending"
	if err := syncpkg.ApplyPullPage(context.Background(), uow, []syncpkg.PullChange{{EntityType: "expense_occurrences", EntityID: occurrenceID.String(), Version: 1, Payload: payload(staleOccurrence), ServerUpdatedAt: now.Add(time.Hour)}}, nil); err != nil {
		t.Fatal(err)
	}
	var occurrenceStatus string
	if err := db.Get(&occurrenceStatus, `SELECT status FROM expense_occurrences WHERE id=?`, occurrenceID); err != nil || occurrenceStatus != "paid" {
		t.Fatalf("stale full-sync revived occurrence: status=%q err=%v", occurrenceStatus, err)
	}

	deletedAt := now.Add(time.Minute)
	exp["version"], exp["updated_at"], exp["deleted_at"] = 2, deletedAt.UnixMilli(), deletedAt.UnixMilli()
	if err := syncpkg.ApplyPullPage(context.Background(), uow, []syncpkg.PullChange{{EntityType: "expenses", EntityID: expenseID.String(), Version: 2, Payload: payload(exp), ServerUpdatedAt: deletedAt, DeletedAt: &deletedAt}}, nil); err != nil {
		t.Fatal(err)
	}
	var deletedMS *int64
	if err := db.Get(&deletedMS, `SELECT deleted_at FROM expenses WHERE id=?`, expenseID); err != nil || deletedMS == nil {
		t.Fatalf("tombstone deleted_at=%v err=%v", deletedMS, err)
	}

	// The same LWW rule keeps an older live row from reviving a tombstone.
	staleExpense := base(expenseID)
	staleExpense["expense_date"], staleExpense["amount"], staleExpense["category"] = "2026-08-13", 1250.75, "marketing"
	staleExpense["payment_method"], staleExpense["created_by"], staleExpense["classification"], staleExpense["source"] = "cash", userID, "variable", "recurring"
	staleExpense["recurring_occurrence_id"], staleExpense["cash_movement_id"] = occurrenceID, movementID
	if err := syncpkg.ApplyPullPage(context.Background(), uow, []syncpkg.PullChange{{EntityType: "expenses", EntityID: expenseID.String(), Version: 1, Payload: payload(staleExpense), ServerUpdatedAt: deletedAt.Add(time.Hour)}}, nil); err != nil {
		t.Fatal(err)
	}
	deletedMS = nil
	if err := db.Get(&deletedMS, `SELECT deleted_at FROM expenses WHERE id=?`, expenseID); err != nil || deletedMS == nil {
		t.Fatalf("stale full-sync revived deleted expense: deleted_at=%v err=%v", deletedMS, err)
	}
}
