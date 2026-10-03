//go:build sidecar

package sync_test

import (
	"context"
	syncpkg "github.com/cuadra/cuadra-core/src/shared/sync"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestProductCreditPull_ZeroCollectionAndLaterSettlement(t *testing.T) {
	gym, user, member, root, abono := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	db, uow := freshSidecarDBWithGym(t, gym)
	now := time.Now().UTC()
	ms := now.UnixMilli()
	if _, err := db.Exec(`INSERT INTO users(id,gym_id,version,created_at,updated_at,email,password_hash,full_name,role,active) VALUES(?,?,1,?,?,'credit@test.local','x','Dueño','owner',1)`, user, gym, ms, ms); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO members(id,gym_id,version,created_at,updated_at,folio,full_name,phone,status,created_by) VALUES(?,?,1,?,?,'M-CREDIT','Socio','5550000000','active',?)`, member, gym, ms, ms, user); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"id": root.String(), "gym_id": gym.String(), "member_id": member.String(), "version": 1, "created_at": ms, "updated_at": ms, "folio": "PRD-CREDIT", "amount": 0, "recognized_amount": 0, "balance_pending": 40.25, "payment_method": "cash", "concept": "product", "payment_date": now.Format("2006-01-02"), "operator_id": user.String(), "discount_amount": 0}
	for i := 0; i < 2; i++ {
		if err := syncpkg.ApplyPullPage(context.Background(), uow, []syncpkg.PullChange{change(t, "payments", root, payload, now)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	var amount, balance, count int
	_ = db.Get(&amount, `SELECT amount FROM payments WHERE id=?`, root)
	_ = db.Get(&balance, `SELECT balance_pending FROM payments WHERE id=?`, root)
	_ = db.Get(&count, `SELECT COUNT(*) FROM payments WHERE id=?`, root)
	if amount != 0 || balance != 4025 || count != 1 {
		t.Fatalf("amount=%d balance=%d rows=%d", amount, balance, count)
	}
	paid := map[string]any{"id": abono.String(), "gym_id": gym.String(), "member_id": member.String(), "version": 1, "created_at": ms + 1, "updated_at": ms + 1, "folio": "ABN-CREDIT", "amount": 15.25, "recognized_amount": 15.25, "balance_pending": 0, "parent_payment_id": root.String(), "payment_method": "cash", "concept": "balance_settlement", "payment_date": now.Format("2006-01-02"), "operator_id": user.String(), "discount_amount": 0}
	payload["balance_pending"], payload["version"], payload["updated_at"] = 25, 2, ms+2
	changes := []syncpkg.PullChange{change(t, "payments", abono, paid, now.Add(time.Millisecond)), change(t, "payments", root, payload, now.Add(2*time.Millisecond))}
	changes[1].Version = 2
	for i := 0; i < 2; i++ {
		if err := syncpkg.ApplyPullPage(context.Background(), uow, changes, nil); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Get(&amount, `SELECT SUM(amount) FROM payments`)
	_ = db.Get(&balance, `SELECT balance_pending FROM payments WHERE id=?`, root)
	if amount != 1525 || balance != 2500 {
		t.Fatalf("collected=%d balance=%d", amount, balance)
	}
}
