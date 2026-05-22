package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	usersRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// fakeUserRepoForGate stubea sólo GetByID; el resto del UserRepository queda
// nil-embedded (los tests no lo tocan).
type fakeUserRepoForGate struct {
	usersRepo.UserRepository
	user *userDomain.User
	err  error
}

func (f *fakeUserRepoForGate) GetByID(_ sharedDomain.Transaction, _ uuid.UUID) (*userDomain.User, error) {
	return f.user, f.err
}

// fakeEmailGateUoW comparte shape con fakeGateUoW del subscription_gate_test
// pero acá lo redefinimos para no acoplar archivos (cada test debería poder
// correr en aislamiento).
type fakeEmailGateUoW struct{}
type fakeEmailGateTx struct{}

func (fakeEmailGateTx) Execute(fn func(sharedDomain.Transaction) error) error {
	return fn(fakeEmailGateTx{})
}
func (fakeEmailGateUoW) Begin(context.Context) (sharedDomain.Transaction, error) {
	return fakeEmailGateTx{}, nil
}
func (fakeEmailGateUoW) Commit(sharedDomain.Transaction) error   { return nil }
func (fakeEmailGateUoW) Rollback(sharedDomain.Transaction) error { return nil }
func (fakeEmailGateUoW) Query(context.Context) (sharedDomain.Transaction, error) {
	return fakeEmailGateTx{}, nil
}
func (fakeEmailGateUoW) Command(_ context.Context, fn func(sharedDomain.Transaction) error) error {
	return fn(fakeEmailGateTx{})
}

// newVerifiedGateRouter monta un router con un endpoint stub que setea
// user_id en el context (simulando AuthMiddleware) seguido del gate.
func newVerifiedGateRouter(t *testing.T, repo usersRepo.UserRepository, uow sharedDomain.UnitOfWork) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Middleware que finge que AuthMiddleware ya corrió: stash de userID.
	r.Use(func(c *gin.Context) {
		c.Set(UserIDKey, uuid.New())
		c.Next()
	})
	r.Use(RequireEmailVerified(repo, uow))
	r.PATCH("/api/v1/gyms/me/setup", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func unverifiedUser() *userDomain.User {
	return &userDomain.User{
		ID:              uuid.New(),
		Email:           "owner@gym.com",
		EmailVerifiedAt: nil,
	}
}

func verifiedUser() *userDomain.User {
	now := time.Now().UTC()
	return &userDomain.User{
		ID:              uuid.New(),
		Email:           "owner@gym.com",
		EmailVerifiedAt: &now,
	}
}

// TestEmailGate_Blocks403WhenUnverified — el caso central: dueño con correo
// no verificado intenta PATCH al setup → 403 + body estructurado.
func TestEmailGate_Blocks403WhenUnverified(t *testing.T) {
	repo := &fakeUserRepoForGate{user: unverifiedUser()}
	r := newVerifiedGateRouter(t, repo, fakeEmailGateUoW{})

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/gyms/me/setup", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body=%s", w.Code, w.Body.String())
	}
	var body EmailVerifiedGateResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, w.Body.String())
	}
	if body.Error != "email_not_verified" {
		t.Errorf("error = %q, want email_not_verified", body.Error)
	}
}

// TestEmailGate_AllowsVerified — happy path: dueño con email_verified_at
// pasa el gate y el endpoint responde 200.
func TestEmailGate_AllowsVerified(t *testing.T) {
	repo := &fakeUserRepoForGate{user: verifiedUser()}
	r := newVerifiedGateRouter(t, repo, fakeEmailGateUoW{})

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/gyms/me/setup", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("verified user blocked: status = %d body=%s", w.Code, w.Body.String())
	}
}

// TestEmailGate_NilDepsNoOp — repo/uow nil = el middleware no debe romper
// nada. Mismo contrato que RequirePlusPlan (fail-open en infra para no
// bloquear todo el dashboard por bug del wiring).
func TestEmailGate_NilDepsNoOp(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequireEmailVerified(nil, nil))
	r.PATCH("/anything", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodPatch, "/anything", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("nil-deps gate = %d, want 200", w.Code)
	}
}

// TestEmailGate_FailsOpenOnRepoError — si el repo devuelve error (DB caída,
// user no encontrado), dejamos pasar. Es la misma decisión que toma
// RequirePlusPlan: preferimos un riesgo de "pasó sin verificar por bug
// del lookup" a "wizard entero bloqueado por DB caída".
func TestEmailGate_FailsOpenOnRepoError(t *testing.T) {
	repo := &fakeUserRepoForGate{err: errors.New("db down")}
	r := newVerifiedGateRouter(t, repo, fakeEmailGateUoW{})

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/gyms/me/setup", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("repo error blocked request: status = %d, want pass-through 200", w.Code)
	}
}
