//go:build server && integration

package sync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProductCreditSync_ZeroAmountReplayAndLegacyGate(t *testing.T) {
	db := projectorTestDB(t)
	gym, user := seedGymAndOwner(t, db)
	member, payment := uuid.New(), uuid.New()
	if err := db.Exec(`INSERT INTO members(id,gym_id,version,created_at,updated_at,folio,full_name,phone,status,created_by)
 VALUES(?,?,1,NOW(),NOW(),'M-CREDIT','Socio de prueba','5550000000','active',?)`, member, gym, user).Error; err != nil {
		t.Fatal(err)
	}
	router, tokens := newRealHandler(t, db)
	token, _ := tokens.GenerateAccessToken(user, gym, "owner")
	// Deploying the new API alone must not disconnect an unaffected old desk.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sync/pull", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Tinta-Sync-Schema", "5")
	before := httptest.NewRecorder()
	router.ServeHTTP(before, req)
	if before.Code != 200 {
		t.Fatalf("legacy before credit=%d %s", before.Code, before.Body.String())
	}
	now := time.Now().UTC()
	raw := map[string]any{"id": payment, "gym_id": gym, "version": 1, "created_at": now.UnixMilli(), "updated_at": now.UnixMilli(), "folio": "PRD-CREDIT", "member_id": member, "amount": 0, "recognized_amount": 0, "payment_method": "cash", "concept": "product", "balance_pending": 40.25, "payment_date": now.Format("2006-01-02"), "operator_id": user, "discount_amount": 0}
	payload, _ := json.Marshal(raw)
	push := PushRequest{ClientID: uuid.NewString(), SchemaVersion: SchemaVersion, ClientNow: now, Batch: []PushItem{{QueueID: uuid.NewString(), EntityType: "payments", EntityID: payment.String(), ClientVersion: 1, Operation: OpUpsertStr, Payload: payload}}}
	for i := 0; i < 2; i++ {
		result, code, err := performSyncPush(router, token, push)
		if err != nil || code != 200 || len(result.Results) != 1 || result.Results[0].Error != "" {
			t.Fatalf("push %d: code=%d err=%v result=%+v", i, code, err, result)
		}
	}
	var values struct {
		Amount, Recognized, Balance float64
		N                           int
	}
	if err := db.Raw(`SELECT COUNT(*) AS n,MAX(amount) AS amount,MAX(recognized_amount) AS recognized,MAX(balance_pending) AS balance FROM payments WHERE id=?`, payment).Scan(&values).Error; err != nil {
		t.Fatal(err)
	}
	if values.N != 1 || values.Amount != 0 || values.Recognized != 0 || values.Balance != 40.25 {
		t.Fatalf("canonical=%+v", values)
	}
	for _, path := range []string{"/api/v1/sync/pull", "/api/v1/sync/full"} {
		for _, version := range []int{5, SchemaVersion} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("X-Tinta-Sync-Schema", strconv.Itoa(version))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			if version == 5 {
				if w.Code != 426 {
					t.Fatalf("legacy %s=%d %s", path, w.Code, w.Body.String())
				}
				continue
			}
			if w.Code != 200 {
				t.Fatalf("current %s=%d %s", path, w.Code, w.Body.String())
			}
			var response PullResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, change := range response.Changes {
				if change.EntityID == payment.String() {
					var p map[string]any
					_ = json.Unmarshal(change.Payload, &p)
					if p["amount"] != float64(0) || p["balance_pending"] != 40.25 {
						t.Fatalf("wire=%s", change.Payload)
					}
					found = true
				}
			}
			if !found {
				t.Fatalf("missing credit in %s", path)
			}
		}
	}
	push.SchemaVersion = 5
	response := doJSONReq(t, router, "POST", "/api/v1/sync/push", token, push)
	if response.Code != 426 {
		t.Fatalf("old push=%d", response.Code)
	}
}
