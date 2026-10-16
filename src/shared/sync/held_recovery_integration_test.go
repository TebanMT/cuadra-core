//go:build server && integration

package sync

import (
	"context"
	"encoding/json"
	noti "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	repos "github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/repositories"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestRecoveredNotificationCannotBeResurrectedByOldSidecar(t *testing.T) {
	db := projectorTestDB(t)
	gym, user := seedGymAndOwner(t, db)
	uow := shared.NewPostgresUnitOfWork(db)
	repo := repos.NewNotificationPostgresRepository()
	ctx := shared.WithTenant(context.Background(), gym)
	old := time.Now().AddDate(0, 0, -30)
	msg, _ := noti.New(uuid.New(), gym, user, "whatsapp", "receipt_product", "user", "+525512345678", map[string]string{}, old, old, nil)
	msg.MarkHeld("Llegó tarde", time.Now())
	msg.ResolveHeld(msg.Version, "discard", time.Now())
	if err := uow.Command(ctx, func(tx shared.Transaction) error { _, err := repo.Create(tx, msg); return err }); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"id": msg.ID, "gym_id": gym, "version": 999, "channel": "whatsapp", "template_key": "receipt_product", "recipient_type": "user", "recipient_id": user, "recipient_address": "+525512345678", "payload": map[string]string{}, "status": "pending", "created_at": old.UnixMilli(), "updated_at": time.Now().UnixMilli(), "scheduled_for": time.Now().UnixMilli()})
	var result UpsertResult
	err := uow.Command(ctx, func(tx shared.Transaction) error {
		var e error
		result, e = NewPostgresStore().UpsertOne(ctx, tx, gym, PushItem{QueueID: uuid.NewString(), EntityType: "notification_queue", EntityID: msg.ID.String(), Operation: OpUpsertStr, ClientVersion: 999, Payload: payload})
		return e
	})
	if err != nil || result.Status != StatusConflictServerWins {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var status string
	db.Raw(`SELECT status FROM notification_queue WHERE id=?`, msg.ID).Scan(&status)
	if status != "cancelled" {
		t.Fatal("discard overwritten", status)
	}
	var body map[string]any
	if err = json.Unmarshal(result.ServerPayload, &body); err != nil || body["status"] != "cancelled" {
		t.Fatal("wrong canonical state")
	}
}
