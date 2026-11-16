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

	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type permUsers struct {
	userRepo.UserRepository // only GetByID is used
	byID                    map[uuid.UUID]*userDomain.User
	lookups                 int
}

func (r *permUsers) GetByID(_ sharedDomain.Transaction, id uuid.UUID) (*userDomain.User, error) {
	r.lookups++
	if u, ok := r.byID[id]; ok {
		return u, nil
	}
	return nil, errors.New("not found")
}

type permUoW struct {
	fakeGateUoW
	tenants []uuid.UUID
}

func (u *permUoW) Query(ctx context.Context) (sharedDomain.Transaction, error) {
	u.tenants = append(u.tenants, sharedDomain.TenantID(ctx))
	return fakeGateTx{}, nil
}

type permFixture struct {
	r      *gin.Engine
	tokens *auth.JWTService
	users  *permUsers
	uow    *permUoW
	gymID  uuid.UUID
	// creditInHandler records what the use case would see for PermCredit.
	creditInHandler error
	sawGrants       bool
}

func newPermFixture(t *testing.T) *permFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := &permFixture{
		tokens: auth.NewJWTService("perm-secret"),
		users:  &permUsers{byID: map[uuid.UUID]*userDomain.User{}},
		uow:    &permUoW{},
		gymID:  uuid.New(),
	}
	r := gin.New()
	r.Use(EnforceOperatorPermissions(f.tokens, f.users, f.uow))
	ok := func(c *gin.Context) {
		_, f.sawGrants = sharedDomain.GrantsFrom(c.Request.Context())
		f.creditInHandler = sharedDomain.RequirePermission(c.Request.Context(), sharedDomain.PermCredit)
		c.Status(http.StatusOK)
	}
	r.GET("/api/v1/products", ok)
	r.POST("/api/v1/products", ok)
	r.PATCH("/api/v1/products/:id", ok)
	r.POST("/api/v1/products/:id/adjust-stock", ok)
	r.POST("/api/v1/product-counts", ok)
	r.POST("/api/v1/checkins/override", ok)
	r.POST("/api/v1/cash-sessions/:id/withdraw", ok)
	r.POST("/api/v1/sales", ok)
	f.r = r
	return f
}

func (f *permFixture) operator(t *testing.T, active bool, perms ...sharedDomain.Permission) string {
	t.Helper()
	u, err := userDomain.NewOperator(uuid.New(), f.gymID, "Ana", "+524421234567", nil, "", uuid.New(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := u.SetPermissions(perms, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	u.Active = active
	f.users.byID[u.ID] = u
	return f.token(t, u.ID, f.gymID, "operator")
}

func (f *permFixture) token(t *testing.T, user, gym uuid.UUID, role string) string {
	t.Helper()
	tok, err := f.tokens.GenerateAccessToken(user, gym, role)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *permFixture) do(method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}

func TestOperatorPermissions_RouteMatrix(t *testing.T) {
	f := newPermFixture(t)
	none := f.operator(t, true)
	products := f.operator(t, true, sharedDomain.PermProducts)

	routes := []struct {
		method, path string
		perm         sharedDomain.Permission
	}{
		{"POST", "/api/v1/products", sharedDomain.PermProducts},
		{"PATCH", "/api/v1/products/" + uuid.NewString(), sharedDomain.PermProducts},
		{"POST", "/api/v1/products/" + uuid.NewString() + "/adjust-stock", sharedDomain.PermInventory},
		{"POST", "/api/v1/product-counts", sharedDomain.PermInventory},
		{"POST", "/api/v1/checkins/override", sharedDomain.PermCheckinOverride},
		{"POST", "/api/v1/cash-sessions/" + uuid.NewString() + "/withdraw", sharedDomain.PermCashOut},
	}
	for _, rt := range routes {
		rec := f.do(rt.method, rt.path, none)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without permission = %d, want 403", rt.method, rt.path, rec.Code)
			continue
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["error"] != "permission_denied" || body["permission"] != string(rt.perm) ||
			body["exception"] != sharedDomain.PermissionDeniedMessage(rt.perm) {
			t.Errorf("%s %s body = %v", rt.method, rt.path, body)
		}
	}
	if rec := f.do("POST", "/api/v1/products", products); rec.Code != http.StatusOK {
		t.Errorf("operator with products = %d", rec.Code)
	}
	if rec := f.do("POST", "/api/v1/products/"+uuid.NewString()+"/adjust-stock", products); rec.Code != http.StatusForbidden {
		t.Errorf("products does not grant inventory: %d", rec.Code)
	}
	for _, tenant := range f.uow.tenants {
		if tenant != f.gymID {
			t.Errorf("user lookup ran under tenant %s, want %s", tenant, f.gymID)
		}
	}
}

func TestOperatorPermissions_UngatedRoutesCarryGrants(t *testing.T) {
	f := newPermFixture(t)
	if rec := f.do("POST", "/api/v1/sales", f.operator(t, true)); rec.Code != http.StatusOK {
		t.Fatalf("sale = %d", rec.Code)
	}
	if !f.sawGrants || !errors.Is(f.creditInHandler, sharedDomain.ErrPermissionDenied) {
		t.Errorf("use case saw grants=%v credit=%v, want the operator's grants", f.sawGrants, f.creditInHandler)
	}
	if rec := f.do("POST", "/api/v1/sales", f.operator(t, true, sharedDomain.PermCredit)); rec.Code != http.StatusOK || f.creditInHandler != nil {
		t.Errorf("operator with credit: code=%d credit=%v", rec.Code, f.creditInHandler)
	}
}

func TestOperatorPermissions_OwnerSkipsTheLookup(t *testing.T) {
	f := newPermFixture(t)
	owner := f.token(t, uuid.New(), f.gymID, "owner")
	for _, path := range []string{"/api/v1/products", "/api/v1/checkins/override", "/api/v1/sales"} {
		if rec := f.do("POST", path, owner); rec.Code != http.StatusOK {
			t.Errorf("owner %s = %d", path, rec.Code)
		}
	}
	if f.users.lookups != 0 {
		t.Errorf("owner requests read the users table %d times", f.users.lookups)
	}
	if f.creditInHandler != nil {
		t.Errorf("owner credit = %v", f.creditInHandler)
	}
}

func TestOperatorPermissions_FailClosed(t *testing.T) {
	f := newPermFixture(t)
	all := sharedDomain.AllPermissions
	inactive := f.operator(t, false, all...)
	unknown := f.token(t, uuid.New(), f.gymID, "operator")
	f.operator(t, true, all...)
	var activeID uuid.UUID
	for id, u := range f.users.byID {
		if u.Active {
			activeID = id
		}
	}
	// A real, active operator presenting a token for another gym.
	wrongGym := f.token(t, activeID, uuid.New(), "operator")

	for name, tok := range map[string]string{"inactive": inactive, "unknown user": unknown, "wrong gym": wrongGym} {
		if rec := f.do("POST", "/api/v1/products", tok); rec.Code != http.StatusForbidden {
			t.Errorf("%s = %d, want 403", name, rec.Code)
		}
	}
}

func TestOperatorPermissions_ReadsCarryGrantsAndAnonymousPassThrough(t *testing.T) {
	f := newPermFixture(t)
	// Reads are never refused here, but they carry the operator's grants so
	// a handler can hide money totals (PermViewTotals).
	if rec := f.do("GET", "/api/v1/products", f.operator(t, true)); rec.Code != http.StatusOK {
		t.Errorf("GET as operator = %d", rec.Code)
	}
	if !f.sawGrants || !errors.Is(f.creditInHandler, sharedDomain.ErrPermissionDenied) {
		t.Errorf("GET saw grants=%v credit=%v, want the operator's grants", f.sawGrants, f.creditInHandler)
	}
	if rec := f.do("GET", "/api/v1/products", f.token(t, uuid.New(), f.gymID, "owner")); rec.Code != http.StatusOK || f.creditInHandler != nil {
		t.Errorf("GET as owner: code=%d credit=%v", rec.Code, f.creditInHandler)
	}
	// No bearer / bad bearer: AuthMiddleware answers 401 on real routes; this
	// middleware stays out of the way and sets no grants.
	for _, tok := range []string{"", "not-a-jwt"} {
		if rec := f.do("POST", "/api/v1/products", tok); rec.Code != http.StatusOK || f.sawGrants {
			t.Errorf("token %q: code=%d grants=%v", tok, rec.Code, f.sawGrants)
		}
	}
}
