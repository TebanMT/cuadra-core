//go:build server && integration

package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	expense "github.com/cuadra/cuadra-core/src/modules/expenses/domain/expense"
	expRepos "github.com/cuadra/cuadra-core/src/modules/expenses/infraestructure/db/repositories"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

// A disconnected desktop changes a fund expense to cash. Meanwhile the cloud
// edits the same expense and wins LWW. The losing cash leg must not be accepted
// independently and appear as a drawer outflow linked to a fund expense.
func TestExpenseGraph_ConflictMustNotLeaveCashOutflow(t *testing.T) {
	db := projectorTestDB(t)
	gymID, userID := seedGymAndOwner(t, db)
	uow := shared.NewPostgresUnitOfWork(db)
	now := time.Now().UTC().Truncate(time.Millisecond)
	day, _ := time.Parse("2006-01-02", "2026-09-01")
	expenseID := uuid.New()
	movementID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("expense-cash:"+expenseID.String()))
	e, err := expense.NewPaid(expense.PaidInput{ID: expenseID, GymID: gymID, CreatedBy: userID, PaidOn: day, Amount: 100, Category: "renta", PaymentMethod: "transfer", PaidFrom: "gym_fund", Classification: "fixed", Source: "manual", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	e.Version = 2 // Cloud has edited the previously shared version 1.
	if err = uow.Command(context.Background(), func(tx shared.Transaction) error {
		_, err := expRepos.NewExpensePostgresRepository().Create(tx, e)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err = db.Raw(`SELECT payload FROM sync_entities WHERE gym_id=? AND entity_type='expenses' AND entity_id=?`, gymID, expenseID).Row().Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var localExpense map[string]any
	if err = json.Unmarshal(raw, &localExpense); err != nil {
		t.Fatal(err)
	}
	localTime := now.Add(-time.Minute).UnixMilli()
	localExpense["updated_at"], localExpense["payment_method"], localExpense["paid_from"], localExpense["cash_movement_id"] = localTime, "cash", "cash_register", movementID
	cash := map[string]any{"id": movementID, "gym_id": gymID, "version": 2, "created_at": localTime, "updated_at": localTime, "deleted_at": nil, "movement_on": "2026-09-01", "amount": 100, "movement_type": "cash_out", "reason": "Renta", "operator_id": userID, "expense_id": expenseID, "classification_status": "expense", "cash_drawer_id": gymID}
	makeItem := func(kind string, id uuid.UUID, version int, payload any) PushItem {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return PushItem{QueueID: uuid.NewString(), EntityType: kind, EntityID: id.String(), ClientVersion: version, Operation: OpUpsertStr, Payload: encoded, EnqueuedAt: now.Add(-time.Minute)}
	}
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(userID, gymID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	legacy, code, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: 3, Batch: []PushItem{makeItem("cash_movements", movementID, 2, cash), makeItem("expenses", expenseID, 2, localExpense)}})
	if err != nil || code != 200 {
		t.Fatalf("legacy code=%d err=%v", code, err)
	}
	for _, row := range legacy.Results {
		if row.Status != StatusRejectedSchema {
			t.Fatalf("legacy client cannot apply atomic conflict: %+v", row)
		}
	}
	result, code, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion, Batch: []PushItem{makeItem("cash_movements", movementID, 2, cash), makeItem("expenses", expenseID, 2, localExpense)}})
	if err != nil || code != http.StatusOK {
		t.Fatalf("push code=%d err=%v", code, err)
	}
	var state struct {
		PaidFrom   string
		CashRows   int
		CashAmount float64
	}
	if err = db.Raw(`SELECT e.paid_from, (SELECT COUNT(*) FROM cash_movements c WHERE c.gym_id=e.gym_id AND c.expense_id=e.id AND c.deleted_at IS NULL) AS cash_rows, (SELECT COALESCE(SUM(amount),0) FROM cash_movements c WHERE c.gym_id=e.gym_id AND c.expense_id=e.id AND c.deleted_at IS NULL) AS cash_amount FROM expenses e WHERE e.gym_id=? AND e.id=?`, gymID, expenseID).Scan(&state).Error; err != nil {
		t.Fatal(err)
	}
	t.Logf("cash status=%s, expense status=%s; paid_from=%s, active cash rows=%d, drawer outflow=%.2f", result.Results[0].Status, result.Results[1].Status, state.PaidFrom, state.CashRows, state.CashAmount)
	if state.PaidFrom == "gym_fund" && state.CashRows != 0 {
		t.Fatalf("financial graph split: fund expense retained alongside %.2f of active drawer outflow", state.CashAmount)
	}
	for _, item := range result.Results {
		if item.Status != StatusConflictServerWins || len(item.ServerPayload) == 0 {
			t.Fatalf("incomplete canonical response: %+v", item)
		}
	}
	// Acknowledgement can be lost. Replaying the exact offline correction
	// must keep the losing outflow deleted, with no additional financial rows.
	retry, code, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), ClientNow: now, SchemaVersion: SchemaVersion, Batch: []PushItem{makeItem("cash_movements", movementID, 2, cash), makeItem("expenses", expenseID, 2, localExpense)}})
	if err != nil || code != 200 {
		t.Fatalf("retry code=%d err=%v", code, err)
	}
	for _, item := range retry.Results {
		if item.Status != StatusConflictServerWins {
			t.Fatalf("retry result=%+v", item)
		}
	}
	var active int64
	if err = db.Table("cash_movements").Where("gym_id=? AND deleted_at IS NULL", gymID).Count(&active).Error; err != nil || active != 0 {
		t.Fatalf("retry resurrected cash: active=%d err=%v", active, err)
	}
}

func TestExpenseGraph_SplitBatchAndLostResponse(t *testing.T) {
	db := projectorTestDB(t)
	gym, user := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(user, gym, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	expenseID, cashID := uuid.New(), uuid.New()
	base := func(id uuid.UUID) map[string]any {
		return map[string]any{"id": id, "gym_id": gym, "version": 1, "created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil}
	}
	e := base(expenseID)
	e["expense_date"], e["amount"], e["category"], e["payment_method"], e["paid_from"], e["classification"], e["source"], e["created_by"], e["cash_movement_id"] = "2026-09-26", 100, "renta", "cash", "cash_register", "fixed", "manual", user, cashID
	c := base(cashID)
	c["movement_on"], c["amount"], c["movement_type"], c["reason"], c["operator_id"], c["expense_id"], c["classification_status"] = "2026-09-26", 100, "cash_out", "Renta", user, expenseID, "expense"
	members := []string{expenseMemberKey("expenses", expenseID.String()), expenseMemberKey("cash_movements", cashID.String())}
	e[expenseGraphMembersKey], c[expenseGraphMembersKey] = members, members
	item := func(kind string, id uuid.UUID, raw map[string]any) PushItem {
		payload, _ := json.Marshal(raw)
		return PushItem{QueueID: uuid.NewString(), EntityType: kind, EntityID: id.String(), ClientVersion: 1, Operation: OpUpsertStr, Payload: payload, EnqueuedAt: now}
	}
	batch := []PushItem{item("expenses", expenseID, e), item("cash_movements", cashID, c)}
	// Neither half may commit by itself, in either network order.
	for _, part := range batch {
		response, code, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, Batch: []PushItem{part}})
		if err != nil || code != 200 || response.Results[0].Status != StatusRejectedFinancialConflict {
			t.Fatalf("partial code=%d err=%v result=%+v", code, err, response)
		}
	}
	var count int64
	if err = db.Table("sync_entities").Where("gym_id=? AND entity_type IN ('expenses','cash_movements')", gym).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("partial journal=%d err=%v", count, err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		response, code, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, Batch: batch})
		if err != nil || code != 200 {
			t.Fatalf("code=%d err=%v", code, err)
		}
		for _, r := range response.Results {
			if r.Status != StatusAccepted && r.Status != StatusConflictServerWins {
				t.Fatalf("replay result=%+v", r)
			}
		}
		var state struct {
			Amount float64
			Cash   float64
			Active int
		}
		err = db.Raw(`SELECT (SELECT SUM(amount) FROM expenses WHERE gym_id=? AND deleted_at IS NULL) AS amount,(SELECT SUM(amount) FROM cash_movements WHERE gym_id=? AND deleted_at IS NULL) AS cash,(SELECT COUNT(*) FROM cash_movements WHERE gym_id=? AND deleted_at IS NULL) AS active`, gym, gym, gym).Scan(&state).Error
		if err != nil || state.Amount != 100 || state.Cash != 100 || state.Active != 1 {
			t.Fatalf("replay state=%+v err=%v", state, err)
		}
	}
}

func TestExpenseGraph_TwoOfflinePaymentsOfOneOccurrence(t *testing.T) {
	db := projectorTestDB(t)
	gym, user := seedGymAndOwner(t, db)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(user, gym, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	templateID, occID, expenseID, cashID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	base := func(id uuid.UUID, version int) map[string]any {
		return map[string]any{"id": id, "gym_id": gym, "version": version, "created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "deleted_at": nil}
	}
	item := func(kind string, id uuid.UUID, version int, raw map[string]any) PushItem {
		payload, _ := json.Marshal(raw)
		return PushItem{QueueID: uuid.NewString(), EntityType: kind, EntityID: id.String(), ClientVersion: version, Operation: OpUpsertStr, Payload: payload, EnqueuedAt: now}
	}
	template := base(templateID, 1)
	template["name"], template["category"], template["expected_amount"], template["usual_payment_method"], template["classification"], template["frequency"], template["starts_on"], template["next_due_on"], template["active"], template["created_by"] = "Renta", "renta", 100, "transfer", "fixed", "monthly", "2026-09-26", "2026-10-26", true, user
	occurrence := base(occID, 1)
	occurrence["template_id"], occurrence["due_on"], occurrence["expected_amount"], occurrence["category"], occurrence["payment_method"], occurrence["classification"], occurrence["status"] = templateID, "2026-09-26", 100, "renta", "transfer", "fixed", "pending"
	seed, code, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, Batch: []PushItem{item("recurring_expense_templates", templateID, 1, template), item("expense_occurrences", occID, 1, occurrence)}})
	if err != nil || code != 200 {
		t.Fatalf("seed=%+v code=%d err=%v", seed, code, err)
	}
	makeBatch := func(cash bool) []PushItem {
		e := base(expenseID, 1)
		e["expense_date"], e["amount"], e["category"], e["payment_method"], e["paid_from"], e["classification"], e["source"], e["created_by"], e["recurring_occurrence_id"] = "2026-09-26", 100, "renta", "transfer", "gym_fund", "fixed", "recurring", user, occID
		o := base(occID, 2)
		for key, value := range occurrence {
			o[key] = value
		}
		o["version"], o["status"], o["expense_id"], o["resolved_by"], o["resolved_at"] = 2, "paid", expenseID, user, now.UnixMilli()
		if cash {
			e["payment_method"], e["paid_from"], e["cash_movement_id"] = "cash", "cash_register", cashID
		}
		batch := []PushItem{item("expenses", expenseID, 1, e), item("expense_occurrences", occID, 2, o)}
		if cash {
			c := base(cashID, 2)
			c["movement_on"], c["amount"], c["movement_type"], c["reason"], c["operator_id"], c["expense_id"], c["classification_status"] = "2026-09-26", 100, "cash_out", "Renta", user, expenseID, "expense"
			batch = append(batch, item("cash_movements", cashID, 2, c))
		}
		members := []string{}
		for _, row := range batch {
			members = append(members, expenseMemberKey(row.EntityType, row.EntityID))
		}
		for i := range batch {
			raw := decodePushPayload(batch[i].Payload)
			raw[expenseGraphMembersKey] = members
			batch[i].Payload, _ = json.Marshal(raw)
		}
		return batch
	}
	type outcome struct {
		response PushResponse
		code     int
		err      error
	}
	// Both requests share a previously pending occurrence; one desk paid cash,
	// the other recorded a transfer while offline.
	results := make(chan outcome, 2)
	for _, batch := range [][]PushItem{makeBatch(true), makeBatch(false)} {
		go func(batch []PushItem) {
			response, code, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, Batch: batch})
			results <- outcome{response, code, err}
		}(batch)
	}
	for i := 0; i < 2; i++ {
		out := <-results
		if out.err != nil || out.code != 200 {
			t.Fatalf("outcome=%+v", out)
		}
		for _, result := range out.response.Results {
			if result.Status != StatusAccepted && result.Status != StatusConflictServerWins {
				t.Fatalf("outcome=%+v", result)
			}
		}
	}
	var state struct {
		Expenses        int
		Amount          float64
		Cash            int
		CashAmount      float64
		PaidFrom        string
		PaidOccurrences int
	}
	err = db.Raw(`SELECT (SELECT COUNT(*) FROM expenses WHERE gym_id=? AND deleted_at IS NULL) AS expenses,(SELECT SUM(amount) FROM expenses WHERE gym_id=? AND deleted_at IS NULL) AS amount,(SELECT COUNT(*) FROM cash_movements WHERE gym_id=? AND deleted_at IS NULL) AS cash,(SELECT COALESCE(SUM(amount),0) FROM cash_movements WHERE gym_id=? AND deleted_at IS NULL) AS cash_amount,(SELECT paid_from FROM expenses WHERE gym_id=? AND id=?) AS paid_from,(SELECT COUNT(*) FROM expense_occurrences WHERE gym_id=? AND status='paid') AS paid_occurrences`, gym, gym, gym, gym, gym, expenseID, gym).Scan(&state).Error
	if err != nil || state.Expenses != 1 || state.Amount != 100 || state.PaidOccurrences != 1 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if (state.PaidFrom == "gym_fund" && (state.Cash != 0 || state.CashAmount != 0)) || (state.PaidFrom == "cash_register" && (state.Cash != 1 || state.CashAmount != 100)) {
		t.Fatalf("split financial state=%+v", state)
	}
}

func TestExpenseGraph_ClientWinsKeepsDomainVersionAheadForNextCloudEdit(t *testing.T) {
	db := projectorTestDB(t)
	gym, user := seedGymAndOwner(t, db)
	uow := shared.NewPostgresUnitOfWork(db)
	repo := expRepos.NewExpensePostgresRepository()
	now := time.Now().UTC()
	day, _ := time.Parse("2006-01-02", "2026-09-26")
	e, err := expense.NewPaid(expense.PaidInput{ID: uuid.New(), GymID: gym, CreatedBy: user, PaidOn: day, Amount: 100, Category: "renta", PaymentMethod: "transfer", PaidFrom: "gym_fund", Classification: "fixed", Source: "manual", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	e.Version = 2
	if err = uow.Command(context.Background(), func(tx shared.Transaction) error { _, err := repo.Create(tx, e); return err }); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err = db.Raw(`SELECT payload FROM sync_entities WHERE gym_id=? AND entity_type='expenses' AND entity_id=?`, gym, e.ID).Row().Scan(&payload); err != nil {
		t.Fatal(err)
	}
	raw := decodePushPayload(payload)
	raw["amount"], raw["updated_at"] = 150, now.Add(time.Second).UnixMilli()
	payload, _ = json.Marshal(raw)
	router, tokens := newRealHandler(t, db)
	token, err := tokens.GenerateAccessToken(user, gym, "owner")
	if err != nil {
		t.Fatal(err)
	}
	response, code, err := performSyncPush(router, token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, Batch: []PushItem{{QueueID: uuid.NewString(), EntityType: "expenses", EntityID: e.ID.String(), ClientVersion: 2, Operation: OpUpsertStr, Payload: payload}}})
	if err != nil || code != 200 || response.Results[0].Status != StatusConflictClientWins {
		t.Fatalf("result=%+v code=%d err=%v", response, code, err)
	}
	acceptedVersion := response.Results[0].ServerVersion
	if err = uow.Command(context.Background(), func(tx shared.Transaction) error {
		canonical, err := repo.GetByID(tx, gym, e.ID)
		if err != nil {
			return err
		}
		if canonical.Version != acceptedVersion {
			t.Errorf("domain version=%d, acknowledged=%d", canonical.Version, acceptedVersion)
		}
		if err = canonical.Update(day, 175, "renta", "transfer", nil, time.Now().UTC()); err != nil {
			return err
		}
		_, err = repo.Update(tx, canonical)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var nextVersion int
	if err = db.Raw(`SELECT version FROM sync_entities WHERE gym_id=? AND entity_type='expenses' AND entity_id=?`, gym, e.ID).Row().Scan(&nextVersion); err != nil {
		t.Fatal(err)
	}
	if nextVersion <= acceptedVersion {
		t.Fatalf("cloud edit would be skipped: next=%d acknowledged=%d", nextVersion, acceptedVersion)
	}
}
