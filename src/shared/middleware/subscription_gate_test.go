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
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// fakeGymRepoForGate satisface gymRepo.GymRepository devolviendo el gym que
// el test configure. Embedded composition cubre los métodos no usados.
type fakeGymRepoForGate struct {
	gymRepo.GymRepository
	gym *gymDomain.Gym
}

func (f *fakeGymRepoForGate) GetByID(_ sharedDomain.Transaction, _ uuid.UUID) (*gymDomain.Gym, error) {
	return f.gym, nil
}

type fakeGateUoW struct{}
type fakeGateTx struct{}

func (fakeGateTx) Execute(fn func(sharedDomain.Transaction) error) error {
	return fn(fakeGateTx{})
}
func (fakeGateUoW) Begin(context.Context) (sharedDomain.Transaction, error) {
	return fakeGateTx{}, nil
}
func (fakeGateUoW) Commit(sharedDomain.Transaction) error   { return nil }
func (fakeGateUoW) Rollback(sharedDomain.Transaction) error { return nil }
func (fakeGateUoW) Query(context.Context) (sharedDomain.Transaction, error) {
	return fakeGateTx{}, nil
}
func (fakeGateUoW) Command(ctx context.Context, fn func(sharedDomain.Transaction) error) error {
	return fn(fakeGateTx{})
}

// newGateRouter monta un engine con el middleware más un endpoint dummy y
// rutas bypass-able para validar el comportamiento end-to-end.
func newGateRouter(t *testing.T, gym *gymDomain.Gym) (*gin.Engine, auth.TokenService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tokens := auth.NewJWTService("test-secret")
	repo := &fakeGymRepoForGate{gym: gym}
	r := gin.New()
	r.Use(EnforceActiveSubscription(tokens, repo, fakeGateUoW{}))
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.GET("/api/v1/auth/me", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.GET("/api/v1/gyms/me", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.GET("/api/v1/subscriptions/me", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.GET("/api/v1/sync/status", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.POST("/api/v1/members", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	r.PATCH("/api/v1/gyms/me", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	return r, tokens
}

func makeAccessToken(t *testing.T, tokens auth.TokenService) string {
	t.Helper()
	tok, err := tokens.GenerateAccessToken(uuid.New(), uuid.New(), "owner")
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return tok
}

func cancelledGym(graceVencidaHace time.Duration) *gymDomain.Gym {
	now := time.Now().UTC()
	endsAt := now.Add(-graceVencidaHace)
	return &gymDomain.Gym{
		ID:                 uuid.New(),
		SubscriptionPlan:   gymDomain.PlanStandardMonthly,
		SubscriptionStatus: gymDomain.StatusCancelled,
		SubscriptionEndsAt: &endsAt,
	}
}

func activeGym() *gymDomain.Gym {
	now := time.Now().UTC()
	endsAt := now.Add(72 * time.Hour)
	return &gymDomain.Gym{
		ID:                 uuid.New(),
		SubscriptionPlan:   gymDomain.PlanStandardMonthly,
		SubscriptionStatus: gymDomain.StatusActive,
		SubscriptionEndsAt: &endsAt,
	}
}

// TestGate_BlocksBusinessRouteWhenCancelled — endpoint de mutación recibe 402
// con shape estructurada.
func TestGate_BlocksBusinessRouteWhenCancelled(t *testing.T) {
	r, tokens := newGateRouter(t, cancelledGym(72*time.Hour))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/members", nil)
	req.Header.Set("Authorization", "Bearer "+makeAccessToken(t, tokens))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402, body=%s", w.Code, w.Body.String())
	}
	var body SubscriptionInactiveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if body.Error != "subscription_inactive" {
		t.Errorf("error = %q, want subscription_inactive", body.Error)
	}
	if body.SubscriptionStatus != gymDomain.StatusCancelled {
		t.Errorf("subscription_status = %q, want cancelled", body.SubscriptionStatus)
	}
	if body.Message == "" {
		t.Errorf("expected non-empty message")
	}
}

// TestGate_AllowsBypassPathsWhenCancelled — incluso con gym bloqueado, los
// paths whitelisteados deben pasar para que el FE pueda mostrar la pantalla
// de bloqueo y permitir auth/escape.
func TestGate_AllowsBypassPathsWhenCancelled(t *testing.T) {
	r, tokens := newGateRouter(t, cancelledGym(72*time.Hour))
	token := makeAccessToken(t, tokens)

	bypass := []struct {
		method, path string
	}{
		{http.MethodGet, "/health"},
		{http.MethodGet, "/api/v1/auth/me"},
		{http.MethodGet, "/api/v1/gyms/me"},
		{http.MethodGet, "/api/v1/subscriptions/me"},
		{http.MethodGet, "/api/v1/sync/status"},
	}
	for _, b := range bypass {
		req := httptest.NewRequest(b.method, b.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s %s = %d, want 200 (bypass) body=%s", b.method, b.path, w.Code, w.Body.String())
		}
	}
}

// TestGate_AllowsAllRoutesWhenActive — gym activo: ningún 402, todo pasa.
func TestGate_AllowsAllRoutesWhenActive(t *testing.T) {
	r, tokens := newGateRouter(t, activeGym())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/members", nil)
	req.Header.Set("Authorization", "Bearer "+makeAccessToken(t, tokens))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("active gym mutation = %d, want 200", w.Code)
	}
}

// TestGate_PassesThroughWhenJWTMissing — sin JWT no podemos decidir gym_id,
// dejamos pasar y el AuthMiddleware del controller responderá 401. El gate
// no es para autenticación, sólo para bloqueo por estado de suscripción.
func TestGate_PassesThroughWhenJWTMissing(t *testing.T) {
	r, _ := newGateRouter(t, cancelledGym(72*time.Hour))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/members", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	// El stub /api/v1/members responde 200 — sin auth middleware real esto
	// pasa. La intención es: el gate NO bloquea por falta de JWT.
	if w.Code == http.StatusPaymentRequired {
		t.Errorf("expected gate to pass through missing JWT, got 402")
	}
}

// TestGate_AllowsGracePeriod — cancelled + grace vigente debe pasar (es la
// ventana corta donde Stripe canceló pero el dueño todavía puede operar
// hasta fin de período pagado).
func TestGate_AllowsGracePeriod(t *testing.T) {
	now := time.Now().UTC()
	endsAt := now.Add(48 * time.Hour)
	g := &gymDomain.Gym{
		ID:                 uuid.New(),
		SubscriptionPlan:   gymDomain.PlanStandardMonthly,
		SubscriptionStatus: gymDomain.StatusCancelled,
		SubscriptionEndsAt: &endsAt,
	}
	r, tokens := newGateRouter(t, g)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/members", nil)
	req.Header.Set("Authorization", "Bearer "+makeAccessToken(t, tokens))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("grace period mutation = %d, want 200", w.Code)
	}
}

// TestGate_NilDeps_NoOps — si tokens/repo/uow son nil el middleware no
// debería romper nada. Usado en tests aislados que no quieren montar
// dominio completo.
func TestGate_NilDeps_NoOps(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(EnforceActiveSubscription(nil, nil, nil))
	r.POST("/anything", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	req := httptest.NewRequest(http.MethodPost, "/anything", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("nil-deps gate = %d, want 200", w.Code)
	}
}
