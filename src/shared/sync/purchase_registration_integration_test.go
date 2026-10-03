//go:build server && integration

package sync

import (
	"context"
	gymRepos "github.com/cuadra/cuadra-core/src/modules/gyms/infraestructure/db/repositories"
	prodApp "github.com/cuadra/cuadra-core/src/modules/products/app"
	repos "github.com/cuadra/cuadra-core/src/modules/products/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPurchaseRegistrationRequiresCompatibleReadersAndWriters(t *testing.T) {
	for _, kind := range []string{"negative_stock", "desktop_purchase"} {
		t.Run(kind, func(t *testing.T) {
			f := newRemotePG(t)
			if kind == "negative_stock" {
				if err := f.db.Exec("UPDATE products SET stock=-1,stock_base=-1 WHERE id=?", f.product).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				uc := prodApp.NewRegisterInventoryPurchase(repos.NewInventoryPurchasePostgresRepository(), repos.NewInventoryPurchaseReceiptPostgresRepository(), repos.NewProductPostgresRepository(), gymRepos.NewGymPostgresRepository(), nil, f.uow, audit.NewPostgresRecorder(), "desktop")
				_, err := uc.Execute(context.Background(), prodApp.RegisterInventoryPurchaseInput{ID: uuid.New(), GymID: f.gym, ActorUserID: f.user, ActorRole: "owner", Items: []prodApp.PurchaseLineInput{{ProductID: f.product, Quantity: 2, UnitCost: 10}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			router, tokens := newRealHandler(t, f.db)
			token, _ := tokens.GenerateAccessToken(f.user, f.gym, "owner")
			for _, path := range []string{"/api/v1/sync/pull", "/api/v1/sync/full"} {
				for _, version := range []string{"4", "5"} {
					req := httptest.NewRequest(http.MethodGet, path, nil)
					req.Header.Set("Authorization", "Bearer "+token)
					req.Header.Set("X-Tinta-Sync-Schema", version)
					w := httptest.NewRecorder()
					router.ServeHTTP(w, req)
					want := 200
					if version == "4" {
						want = 426
					}
					if w.Code != want {
						t.Fatalf("%s schema %s: %d %s", path, version, w.Code, w.Body.String())
					}
				}
			}
			response := doJSONReq(t, router, "POST", "/api/v1/sync/push", token, PushRequest{ClientID: uuid.NewString(), SchemaVersion: 4, Batch: []PushItem{}})
			if response.Code != 426 {
				t.Fatalf("old push=%d %s", response.Code, response.Body.String())
			}
		})
	}
}
