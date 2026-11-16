//go:build server && integration

package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	usersRepoPg "github.com/cuadra/cuadra-core/src/modules/users/infraestructure/db/repositories"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/testutil"
)

type testRecorder struct{}

func (testRecorder) Record(context.Context, shared.Transaction, audit.Entry) error { return nil }

type refreshFixture struct {
	db     *gorm.DB
	uow    shared.UnitOfWork
	tokens *auth.JWTService
	bl     *usersRepoPg.RefreshTokenBlacklistPostgresRepository
	uc     *usersApp.RefreshSession
	user   *userDomain.User
}

func newRefreshFixture(t *testing.T) refreshFixture {
	t.Helper()
	db := testutil.OpenPostgres(t)
	gymID := uuid.New()
	if err := db.Exec(`INSERT INTO gyms(id,gym_id,version,created_at,updated_at,name,country,timezone) VALUES(?,?,1,NOW(),NOW(),'Refresh test','MX','America/Mexico_City')`, gymID, gymID).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM refresh_token_blacklist WHERE user_id IN (SELECT id FROM users WHERE gym_id=?)`, gymID)
		for _, table := range []string{"sync_entities", "users", "cash_drawers", "gyms"} {
			db.Exec("DELETE FROM "+table+" WHERE gym_id=?", gymID)
		}
	})
	now := time.Now().UTC()
	user := &userDomain.User{ID: uuid.New(), GymID: gymID, Version: 1, CreatedAt: now, UpdatedAt: now,
		Email: "refresh-" + gymID.String()[:8] + "@example.test", PasswordHash: "x", FullName: "Dueña Prueba",
		Role: userDomain.RoleOwner, Active: true}
	uow := shared.NewPostgresUnitOfWork(db)
	users := usersRepoPg.NewUserPostgresRepository()
	if err := uow.Command(context.Background(), func(tx shared.Transaction) error {
		_, err := users.Create(tx, user)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewJWTService("refresh-test-secret")
	bl := usersRepoPg.NewRefreshTokenBlacklistPostgresRepository()
	return refreshFixture{db: db, uow: uow, tokens: tokens, bl: bl, user: user,
		uc: usersApp.NewRefreshSession(users, bl, uow, tokens)}
}

func (f refreshFixture) refreshToken(t *testing.T) string {
	t.Helper()
	tok, err := f.tokens.GenerateRefreshToken(f.user.ID, f.user.GymID, f.user.Role)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// expireGrace moves the rotated token's revocation into the past, as if the
// 30-second window had already gone by.
func (f refreshFixture) expireGrace(t *testing.T, token string) {
	t.Helper()
	hash, _ := f.tokens.HashRefreshToken(token)
	res := f.db.Exec(`UPDATE refresh_token_blacklist SET revoked_at = NOW() - INTERVAL '1 second' WHERE token_hash = ?`, hash)
	if res.Error != nil || res.RowsAffected != 1 {
		t.Fatalf("rotated token not in blacklist: %v rows=%d", res.Error, res.RowsAffected)
	}
}

func requireInvalid(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, userErrors.ErrInvalidRefreshToken) {
		t.Fatalf("want ErrInvalidRefreshToken, got %v", err)
	}
}

func TestRefreshSessionRotatesWithShortGrace(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	old := f.refreshToken(t)

	first, err := f.uc.Execute(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	if first.RefreshToken == old || first.AccessToken == "" {
		t.Fatal("refresh did not mint a new pair")
	}
	claims, err := f.tokens.ValidateAccessToken(first.AccessToken)
	if err != nil || claims.UserID != f.user.ID || claims.GymID != f.user.GymID || claims.Role != userDomain.RoleOwner {
		t.Fatalf("access token claims %+v err=%v", claims, err)
	}

	// A second tab refreshing with the same token inside the window still works.
	second, err := f.uc.Execute(ctx, old)
	if err != nil {
		t.Fatalf("second tab inside grace: %v", err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("two refreshes returned the same refresh token")
	}

	f.expireGrace(t, old)
	_, err = f.uc.Execute(ctx, old)
	requireInvalid(t, err)

	// The new token keeps the session alive.
	if _, err := f.uc.Execute(ctx, first.RefreshToken); err != nil {
		t.Fatalf("rotated token rejected: %v", err)
	}
}

func TestRefreshSessionRejectsLoggedOutTokenEvenInsideGrace(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	old := f.refreshToken(t)
	if _, err := f.uc.Execute(ctx, old); err != nil {
		t.Fatal(err)
	}
	logout := usersApp.NewLogout(f.bl, f.uow, f.tokens, testRecorder{})
	if err := logout.Execute(ctx, usersApp.LogoutInput{RefreshToken: old}); err != nil {
		t.Fatal(err)
	}
	_, err := f.uc.Execute(ctx, old)
	requireInvalid(t, err)

	fresh := f.refreshToken(t)
	if err := logout.Execute(ctx, usersApp.LogoutInput{RefreshToken: fresh}); err != nil {
		t.Fatal(err)
	}
	_, err = f.uc.Execute(ctx, fresh)
	requireInvalid(t, err)
}

func TestRefreshSessionHonorsRevokeAll(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()
	tok := f.refreshToken(t)
	if err := f.uow.Command(ctx, func(tx shared.Transaction) error {
		return f.bl.RevokeAllForUser(tx, f.user.ID, time.Now().UTC().Add(auth.RefreshTokenDuration))
	}); err != nil {
		t.Fatal(err)
	}
	setRevokeAll := func(offset string) {
		t.Helper()
		if err := f.db.Exec(`UPDATE refresh_token_blacklist SET revoked_at = NOW() + ?::interval WHERE user_id = ? AND token_hash = sha256(?::bytea)`,
			offset, f.user.ID, "revoke-all:"+f.user.ID.String()).Error; err != nil {
			t.Fatal(err)
		}
	}

	// Password changed after the token was issued: the session is over.
	setRevokeAll("5 seconds")
	_, err := f.uc.Execute(ctx, tok)
	requireInvalid(t, err)

	// A token issued after the reset (the new login) keeps working.
	setRevokeAll("-5 seconds")
	if _, err := f.uc.Execute(ctx, tok); err != nil {
		t.Fatalf("token issued after revoke-all rejected: %v", err)
	}
}

func TestRefreshSessionRejectsInactiveDeletedAndWrongTokens(t *testing.T) {
	f := newRefreshFixture(t)
	ctx := context.Background()

	access, _ := f.tokens.GenerateAccessToken(f.user.ID, f.user.GymID, f.user.Role)
	_, err := f.uc.Execute(ctx, access)
	requireInvalid(t, err)
	_, err = f.uc.Execute(ctx, "")
	requireInvalid(t, err)
	other := auth.NewJWTService("another-secret")
	forged, _ := other.GenerateRefreshToken(f.user.ID, f.user.GymID, f.user.Role)
	_, err = f.uc.Execute(ctx, forged)
	requireInvalid(t, err)
	wrongGym, _ := f.tokens.GenerateRefreshToken(f.user.ID, uuid.New(), f.user.Role)
	_, err = f.uc.Execute(ctx, wrongGym)
	requireInvalid(t, err)

	if err := f.db.Exec(`UPDATE users SET active=false WHERE id=?`, f.user.ID).Error; err != nil {
		t.Fatal(err)
	}
	_, err = f.uc.Execute(ctx, f.refreshToken(t))
	if !errors.Is(err, userErrors.ErrAccountInactive) {
		t.Fatalf("want ErrAccountInactive, got %v", err)
	}

	if err := f.db.Exec(`UPDATE users SET active=true, deleted_at=NOW() WHERE id=?`, f.user.ID).Error; err != nil {
		t.Fatal(err)
	}
	_, err = f.uc.Execute(ctx, f.refreshToken(t))
	requireInvalid(t, err)
}

func TestRefreshSessionMintsCurrentRole(t *testing.T) {
	f := newRefreshFixture(t)
	tok := f.refreshToken(t)
	if err := f.db.Exec(`UPDATE users SET role=? WHERE id=?`, userDomain.RoleOperator, f.user.ID).Error; err != nil {
		t.Fatal(err)
	}
	out, err := f.uc.Execute(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := f.tokens.ValidateAccessToken(out.AccessToken)
	if out.Role != userDomain.RoleOperator || claims.Role != userDomain.RoleOperator {
		t.Fatalf("refresh kept the old role: out=%s claims=%s", out.Role, claims.Role)
	}
}

func TestRefreshSessionPurgesExpiredRows(t *testing.T) {
	f := newRefreshFixture(t)
	if err := f.db.Exec(`INSERT INTO refresh_token_blacklist(token_hash,user_id,expires_at,revoked_at) VALUES(sha256(?::bytea),?,NOW()-INTERVAL '1 day',NOW()-INTERVAL '31 days')`, "stale-"+f.user.ID.String(), f.user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.uc.Execute(context.Background(), f.refreshToken(t)); err != nil {
		t.Fatal(err)
	}
	var rows struct{ Expired, Live int }
	if err := f.db.Raw(`SELECT count(*) FILTER (WHERE expires_at <= NOW()) AS expired, count(*) FILTER (WHERE expires_at > NOW()) AS live FROM refresh_token_blacklist WHERE user_id=?`, f.user.ID).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows.Expired != 0 || rows.Live != 1 {
		t.Fatalf("want only the rotated token left, got %+v", rows)
	}
}
