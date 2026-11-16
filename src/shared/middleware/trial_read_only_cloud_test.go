package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// recordingGateUoW anota con qué tenant se consultó el gym: en el cloud el
// lookup corre bajo RLS y sin tenant no ve ninguna fila.
type recordingGateUoW struct {
	fakeGateUoW
	queries int
	tenants []uuid.UUID
}

func (u *recordingGateUoW) Query(ctx context.Context) (sharedDomain.Transaction, error) {
	u.queries++
	u.tenants = append(u.tenants, sharedDomain.TenantID(ctx))
	return fakeGateTx{}, nil
}

var cloudGateRoutes = []struct{ method, path string }{
	{http.MethodGet, "/api/v1/members"},
	{http.MethodPost, "/api/v1/members"},
	{http.MethodPatch, "/api/v1/gyms/me"},
	{http.MethodPost, "/api/v1/payments/membership"},
	{http.MethodPost, "/api/v1/checkins/manual"},
	{http.MethodPost, "/api/v1/checkins/override"},
	{http.MethodPost, "/api/v1/subscriptions/checkout-session"},
	{http.MethodPost, "/api/v1/sync/push"},
	{http.MethodPost, "/api/v1/webhooks/stripe"},
	{http.MethodPost, "/api/v1/auth/logout"},
	{http.MethodPost, "/api/v1/uploads/presign-get"},
	{http.MethodPost, "/api/v1/uploads/presign"},
	{http.MethodPatch, "/api/v1/users/:id/active"},
	{http.MethodPost, "/api/v1/users/:id/rotate-pin"},
	{http.MethodPatch, "/api/v1/users/:id"},
	{http.MethodPost, "/api/mancuerna/v1/commands"},
}

func newCloudGateRouter(t *testing.T, gym *gymDomain.Gym, uow sharedDomain.UnitOfWork) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("test-secret")
	r := gin.New()
	r.Use(EnforceTrialReadOnly(tokens, &fakeGymRepoForGate{gym: gym}, uow))
	for _, rt := range cloudGateRoutes {
		r.Handle(rt.method, rt.path, func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	}
	token, err := tokens.GenerateAccessToken(uuid.New(), gym.ID, "owner")
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return r, token
}

func serveGate(r *gin.Engine, token, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func trialReadOnlyGym() *gymDomain.Gym {
	gym := cancelledGym(time.Hour)
	gym.SubscriptionPlan = gymDomain.PlanTrial
	return gym
}

// Con la prueba terminada el dashboard consulta, registra entradas y puede
// pagar o quitar accesos; cobrar, dar de alta y editar reciben 402.
func TestCloudTrialReadOnly_BlocksDashboardWrites(t *testing.T) {
	r, token := newCloudGateRouter(t, trialReadOnlyGym(), fakeGateUoW{})
	id := uuid.NewString()
	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/v1/members", http.StatusOK},
		{http.MethodPost, "/api/v1/checkins/manual", http.StatusOK},
		{http.MethodPost, "/api/v1/subscriptions/checkout-session", http.StatusOK},
		{http.MethodPost, "/api/v1/sync/push", http.StatusOK},
		{http.MethodPost, "/api/v1/webhooks/stripe", http.StatusOK},
		{http.MethodPost, "/api/v1/auth/logout", http.StatusOK},
		{http.MethodPost, "/api/v1/uploads/presign-get", http.StatusOK},
		{http.MethodPatch, "/api/v1/users/" + id + "/active", http.StatusOK},
		{http.MethodPost, "/api/v1/users/" + id + "/rotate-pin", http.StatusOK},
		{http.MethodPost, "/api/mancuerna/v1/commands", http.StatusOK},
		{http.MethodPost, "/api/v1/members", http.StatusPaymentRequired},
		{http.MethodPatch, "/api/v1/gyms/me", http.StatusPaymentRequired},
		{http.MethodPost, "/api/v1/payments/membership", http.StatusPaymentRequired},
		{http.MethodPost, "/api/v1/checkins/override", http.StatusPaymentRequired},
		{http.MethodPost, "/api/v1/uploads/presign", http.StatusPaymentRequired},
		{http.MethodPatch, "/api/v1/users/" + id, http.StatusPaymentRequired},
	}
	for _, tc := range cases {
		w := serveGate(r, token, tc.method, tc.path)
		if w.Code != tc.want {
			t.Errorf("%s %s = %d, want %d (body=%s)", tc.method, tc.path, w.Code, tc.want, w.Body.String())
			continue
		}
		if tc.want == http.StatusPaymentRequired {
			var body SubscriptionInactiveResponse
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if body.Error != "subscription_read_only" || body.Message == "" {
				t.Errorf("%s %s body = %+v, want subscription_read_only with a message", tc.method, tc.path, body)
			}
		}
	}
}

// El cloud solo aplica la solo lectura de la prueba: en gracia, con plan
// activo o con un plan de pago cancelado (que paga desde el dashboard) no
// frena nada.
func TestCloudTrialReadOnly_LeavesOtherStatesAlone(t *testing.T) {
	grace := cancelledGym(-48 * time.Hour)
	grace.SubscriptionPlan = gymDomain.PlanTrial
	for name, gym := range map[string]*gymDomain.Gym{
		"gracia de la prueba":    grace,
		"plan activo":            activeGym(),
		"plan de pago cancelado": cancelledGym(time.Hour),
	} {
		r, token := newCloudGateRouter(t, gym, fakeGateUoW{})
		if w := serveGate(r, token, http.MethodPost, "/api/v1/members"); w.Code != http.StatusOK {
			t.Errorf("%s: POST /api/v1/members = %d, want 200", name, w.Code)
		}
	}
}

// Las lecturas no consultan la base; las escrituras consultan el gym con el
// tenant del token (RLS en el cloud).
func TestCloudTrialReadOnly_LooksUpGymOnlyForWritesWithTenant(t *testing.T) {
	gym := trialReadOnlyGym()
	uow := &recordingGateUoW{}
	r, token := newCloudGateRouter(t, gym, uow)
	serveGate(r, token, http.MethodGet, "/api/v1/members")
	if uow.queries != 0 {
		t.Fatalf("GET hit the database %d times, want 0", uow.queries)
	}
	if w := serveGate(r, token, http.MethodPost, "/api/v1/members"); w.Code != http.StatusPaymentRequired {
		t.Fatalf("POST /api/v1/members = %d, want 402", w.Code)
	}
	if len(uow.tenants) != 1 || uow.tenants[0] != gym.ID {
		t.Fatalf("lookup tenants = %v, want [%s]", uow.tenants, gym.ID)
	}
}

// Sin JWT de usuario (webhooks, credenciales de sidecar) el gate no decide:
// el controller responde con su propia auth.
func TestCloudTrialReadOnly_PassesThroughWithoutUserJWT(t *testing.T) {
	r, _ := newCloudGateRouter(t, trialReadOnlyGym(), fakeGateUoW{})
	if w := serveGate(r, "not-a-jwt", http.MethodPost, "/api/v1/members"); w.Code != http.StatusOK {
		t.Fatalf("POST without a valid JWT = %d, want 200 (the controller decides)", w.Code)
	}
}
