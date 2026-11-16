package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type plusGateGymRepo struct {
	gymRepo.GymRepository
	gym *gymDomain.Gym
	err error
}

func (r plusGateGymRepo) GetByID(sharedDomain.Transaction, uuid.UUID) (*gymDomain.Gym, error) {
	return r.gym, r.err
}

type plusGateTx struct{}

func (plusGateTx) Execute(fn func(sharedDomain.Transaction) error) error { return fn(plusGateTx{}) }

type plusGateUoW struct{ err error }

func (u plusGateUoW) Begin(context.Context) (sharedDomain.Transaction, error) {
	return plusGateTx{}, u.err
}
func (plusGateUoW) Commit(sharedDomain.Transaction) error   { return nil }
func (plusGateUoW) Rollback(sharedDomain.Transaction) error { return nil }
func (u plusGateUoW) Query(context.Context) (sharedDomain.Transaction, error) {
	return plusGateTx{}, u.err
}
func (u plusGateUoW) Command(_ context.Context, fn func(sharedDomain.Transaction) error) error {
	return fn(plusGateTx{})
}

func plusGateRequest(t *testing.T, gate gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(GymIDKey, uuid.New()); c.Next() })
	r.Use(gate)
	r.GET("/plus", func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/plus", nil))
	return w
}

func TestRequirePlusPlan_FailsClosedWhenLookupUnavailable(t *testing.T) {
	cases := []struct {
		name string
		gate gin.HandlerFunc
	}{
		{"nil dependencies", RequirePlusPlan(nil, nil)},
		{"uow failure", RequirePlusPlan(plusGateGymRepo{}, plusGateUoW{err: errors.New("db down")})},
		{"repo failure", RequirePlusPlan(plusGateGymRepo{err: errors.New("db down")}, plusGateUoW{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := plusGateRequest(t, tc.gate)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
			}
			if got := w.Body.String(); got == "" || !contains(got, "plan_check_unavailable") {
				t.Errorf("body = %s, want plan_check_unavailable", got)
			}
		})
	}
}

// Standard trae todo lo que existe (análisis incluido); sólo las features que
// siguen en Plus (número propio, envíos sin tope) responden 402.
func TestRequireFeature_StandardPassesExceptPlusOnly(t *testing.T) {
	t.Setenv("TINTA_MODE", "test")
	standard := plusGateGymRepo{gym: &gymDomain.Gym{SubscriptionPlan: gymDomain.PlanStandardMonthly}}
	if w := plusGateRequest(t, RequirePlusPlan(standard, plusGateUoW{})); w.Code != http.StatusOK {
		t.Errorf("standard analytics status = %d, want 200", w.Code)
	}
	if w := plusGateRequest(t, RequireFeature(standard, plusGateUoW{}, gymDomain.FeatureBroadcasts)); w.Code != http.StatusPaymentRequired {
		t.Errorf("standard broadcasts status = %d, want 402", w.Code)
	}
	plus := plusGateGymRepo{gym: &gymDomain.Gym{SubscriptionPlan: gymDomain.PlanPlusMonthly}}
	if w := plusGateRequest(t, RequireFeature(plus, plusGateUoW{}, gymDomain.FeatureBroadcasts)); w.Code != http.StatusOK {
		t.Errorf("plus broadcasts status = %d, want 200", w.Code)
	}
}

// PATCH /gyms/me: un gym Standard sin grant puede guardar branding, datos
// fiscales y webhook; una feature que sigue en Plus aborta con 402.
func TestEnforceFeaturesInline_StandardProfileFields(t *testing.T) {
	t.Setenv("TINTA_MODE", "test")
	repo := plusGateGymRepo{gym: &gymDomain.Gym{SubscriptionPlan: gymDomain.PlanStandardMonthly}}
	inline := func(features ...gymDomain.Feature) gin.HandlerFunc {
		return func(c *gin.Context) {
			if EnforceFeaturesInline(c, repo, plusGateUoW{}, features...) {
				c.Next()
			}
		}
	}
	if w := plusGateRequest(t, inline(gymDomain.FeatureBranding, gymDomain.FeatureFiscalDetails, gymDomain.FeatureAccessHardware)); w.Code != http.StatusOK {
		t.Errorf("standard profile fields status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if w := plusGateRequest(t, inline(gymDomain.FeatureBranding, gymDomain.FeatureOwnWhatsApp)); w.Code != http.StatusPaymentRequired {
		t.Errorf("standard + own_whatsapp status = %d, want 402", w.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestLaunchAccessGatesByFeature(t *testing.T) {
	t.Setenv("TINTA_MODE", "test")
	gym := &gymDomain.Gym{SubscriptionPlan: gymDomain.PlanStandardMonthly, AllFeaturesAccess: true}
	repo := plusGateGymRepo{gym: gym}
	if w := plusGateRequest(t, RequireFeature(repo, plusGateUoW{}, gymDomain.FeatureRecurringExpenses)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := plusGateRequest(t, RequireFeature(repo, plusGateUoW{}, "unknown")); w.Code != 402 {
		t.Fatal("unknown feature allowed")
	}
	if w := plusGateRequest(t, func(c *gin.Context) {
		if EnforceFeaturesInline(c, repo, plusGateUoW{}, gymDomain.FeatureOwnWhatsApp) {
			c.Next()
		}
	}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
