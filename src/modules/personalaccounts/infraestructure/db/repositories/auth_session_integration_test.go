//go:build server && integration

package repositories

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	deviceDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	sessionDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

func authSessionTestDevice(t *testing.T, accountID uuid.UUID) *deviceDomain.AuthorizedDevice {
	t.Helper()
	candidate := attempt.Candidate{ID: uuid.New(), PublicKey: sha256.Sum256([]byte(uuid.NewString())), Platform: "ios", Label: "Synthetic session device"}
	d, err := deviceDomain.NewAuthorizedDevice(accountID, candidate, deviceDomain.Signup, time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func authSessionTestSession(t *testing.T, accountID, deviceID uuid.UUID) *sessionDomain.Session {
	t.Helper()
	s, err := sessionDomain.NewSession(accountID, deviceID, uuid.New(), sha256.Sum256([]byte(uuid.NewString())), uuid.New(), sha256.Sum256([]byte(uuid.NewString())), time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func authSessionTestCreate(t *testing.T, owner *gorm.DB, uow shared.UnitOfWork, repo *AuthSessionPostgresRepository) (uuid.UUID, *deviceDomain.AuthorizedDevice, *sessionDomain.Session) {
	t.Helper()
	accountID := accessTestExisting(t, owner, accessTestPhone(), "active", true)
	d := authSessionTestDevice(t, accountID)
	s := authSessionTestSession(t, accountID, d.ID)
	if err := uow.Command(shared.WithPersonalAccount(context.Background(), accountID), func(tx shared.Transaction) error {
		if err := repo.CreateDevice(tx, accountID, d); err != nil {
			return err
		}
		return repo.CreateSession(tx, accountID, s)
	}); err != nil {
		t.Fatal(err)
	}
	return accountID, d, s
}
func authSessionTestActive(t *testing.T, uow shared.UnitOfWork, repo *AuthSessionPostgresRepository, accountID, sessionID, deviceID uuid.UUID) bool {
	t.Helper()
	tx, err := uow.Query(shared.WithPersonalAccount(context.Background(), accountID))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(tx)
	active, err := repo.SessionActive(tx, accountID, sessionID, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	return active
}

func TestAuthSessionPostgresBoundSignupAndCallerRollback(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uow := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	repo := NewAuthSessionRepository()
	identity := accessTestRepository(t)
	f := accessTestFixture(t, owner, "", uuid.Nil)
	var candidate struct {
		ID       uuid.UUID
		Key      []byte
		Platform string
		Label    string
	}
	if err := owner.Raw("SELECT candidate_device_id AS id,candidate_public_key AS key,candidate_platform AS platform,candidate_label AS label FROM mancuerna.auth_challenges WHERE id=?", f.attempt).Scan(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	failure := errors.New("synthetic later credential receipt failure")
	var device *deviceDomain.AuthorizedDevice
	var session *sessionDomain.Session
	create := func(tx shared.Transaction) error {
		bound, err := identity.ResolveVerifiedAccess(tx, f.attempt)
		if err != nil {
			return err
		}
		if !bound.Created {
			t.Fatal("unexpected existing identity")
		}
		device, err = deviceDomain.NewAuthorizedDevice(bound.AccountID, attempt.Candidate{ID: candidate.ID, PublicKey: [32]byte(candidate.Key), Platform: candidate.Platform, Label: candidate.Label}, deviceDomain.Signup, time.Now().UTC().Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		if err = repo.CreateDevice(tx, bound.AccountID, device); err != nil {
			return err
		}
		session = authSessionTestSession(t, bound.AccountID, device.ID)
		return repo.CreateSession(tx, bound.AccountID, session)
	}
	err := uow.Command(accessTestContext(f), func(tx shared.Transaction) error {
		if err := create(tx); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	for _, table := range []string{"devices", "sessions", "sync_streams"} {
		if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", f.proposed) != 0 {
			t.Fatal("caller rollback retained signup effects")
		}
	}
	if err = uow.Command(accessTestContext(f), create); err != nil {
		t.Fatal(err)
	}
	if !authSessionTestActive(t, uow, repo, f.proposed, session.ID, device.ID) {
		t.Fatal("committed signup session inactive")
	}
}

func TestAuthSessionPostgresIsolationAndNoOverwrite(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uow := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	repo := NewAuthSessionRepository()
	a, da, sa := authSessionTestCreate(t, owner, uow, repo)
	b, db, sb := authSessionTestCreate(t, owner, uow, repo)
	ctx := shared.WithPersonalAccount(context.Background(), a)
	wrong := authSessionTestSession(t, a, db.ID)
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.CreateSession(tx, a, wrong) }); !errors.Is(err, personalErrors.ErrAuthObjectUnavailable) {
		t.Fatalf("foreign device accepted: %v", err)
	}
	// Even if an application omitted its parent check, the composite foreign
	// key must reject an A session referring to a B device under the A scope.
	fkErr := uow.Command(ctx, func(tx shared.Transaction) error {
		row := sessionToModel(wrong)
		return tx.(*shared.GormTransaction).Tx.Create(&row).Error
	})
	var pgError *pgconn.PgError
	if !errors.As(fkErr, &pgError) || pgError.Code != "23503" {
		t.Fatalf("composite owner/device FK did not reject mismatch: %v", fkErr)
	}
	missing := authSessionTestSession(t, a, uuid.New())
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.CreateSession(tx, a, missing) }); !errors.Is(err, personalErrors.ErrAuthObjectUnavailable) {
		t.Fatal("foreign and missing parent errors differ")
	}
	changed := *da
	changed.PublicKey = [32]byte{9}
	changed.Label = "replacement"
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.CreateDevice(tx, a, &changed) }); !errors.Is(err, personalErrors.ErrAuthObjectConflict) {
		t.Fatalf("device upsert accepted: %v", err)
	}
	reused := *sa
	reused.RefreshHash = [32]byte{8}
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.CreateSession(tx, a, &reused) }); !errors.Is(err, personalErrors.ErrAuthObjectConflict) {
		t.Fatalf("session upsert accepted: %v", err)
	}
	tx, err := uow.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(tx)
	got, err := repo.GetDevice(tx, a, da.ID)
	if err != nil || got.PublicKey != da.PublicKey || got.Label != da.Label {
		t.Fatal("existing candidate was overwritten")
	}
	gotSession, err := repo.GetSession(tx, a, sa.ID)
	if err != nil || gotSession.RefreshHash != sa.RefreshHash || gotSession.Generation != 1 {
		t.Fatal("existing session was overwritten")
	}
	if _, err = repo.GetDevice(tx, a, db.ID); !errors.Is(err, personalErrors.ErrAuthObjectNotFound) {
		t.Fatal("foreign device visible")
	}
	if _, err = repo.GetSession(tx, a, sb.ID); !errors.Is(err, personalErrors.ErrAuthObjectNotFound) {
		t.Fatal("foreign session visible")
	}
	if _, err = repo.SessionActive(tx, b, sb.ID, db.ID); !errors.Is(err, personalErrors.ErrAuthSessionScope) {
		t.Fatal("claims with another account accepted")
	}
	if active, err := repo.SessionActive(tx, a, sa.ID, db.ID); err != nil || active {
		t.Fatal("session with another device accepted")
	}
}

func TestAuthSessionPostgresRevocationIsAtomicAndIdempotent(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uow := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	repo := NewAuthSessionRepository()
	a, d, s := authSessionTestCreate(t, owner, uow, repo)
	second := authSessionTestSession(t, a, d.ID)
	ctx := shared.WithPersonalAccount(context.Background(), a)
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.CreateSession(tx, a, second) }); err != nil {
		t.Fatal(err)
	}
	firstTime := time.Now().UTC().Truncate(time.Microsecond)
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.RevokeSession(tx, a, s.ID, firstTime) }); err != nil {
		t.Fatal(err)
	}
	if authSessionTestActive(t, uow, repo, a, s.ID, d.ID) || !authSessionTestActive(t, uow, repo, a, second.ID, d.ID) {
		t.Fatal("single revocation affected wrong sessions")
	}
	failure := errors.New("synthetic later revocation failure")
	if err := uow.Command(ctx, func(tx shared.Transaction) error {
		if err := repo.RevokeDevice(tx, a, d.ID, firstTime.Add(time.Second), "user"); err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if !authSessionTestActive(t, uow, repo, a, second.ID, d.ID) {
		t.Fatal("rollback retained device or session consequence")
	}
	deviceTime := firstTime.Add(2 * time.Second)
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.RevokeDevice(tx, a, d.ID, deviceTime, "user") }); err != nil {
		t.Fatal(err)
	}
	if err := uow.Command(ctx, func(tx shared.Transaction) error {
		return repo.RevokeDevice(tx, a, d.ID, deviceTime.Add(time.Second), "recovery")
	}); err != nil {
		t.Fatal(err)
	}
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.RevokeSession(tx, a, s.ID, deviceTime.Add(time.Second)) }); err != nil {
		t.Fatal(err)
	}
	if authSessionTestActive(t, uow, repo, a, second.ID, d.ID) {
		t.Fatal("device revocation left active session")
	}
	tx, err := uow.Query(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(tx)
	gotDevice, err := repo.GetDevice(tx, a, d.ID)
	if err != nil || !gotDevice.RevokedAt.Equal(deviceTime) || gotDevice.RevocationReason != "user" {
		t.Fatal("device revocation overwritten")
	}
	gotSession, err := repo.GetSession(tx, a, s.ID)
	if err != nil || !gotSession.RevokedAt.Equal(firstTime) {
		t.Fatal("session revocation overwritten")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.devices WHERE id=?", d.ID) != 1 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.sessions WHERE account_id=?", a) != 2 {
		t.Fatal("revocation deleted rows")
	}
	newSession := authSessionTestSession(t, a, d.ID)
	if err := uow.Command(ctx, func(tx shared.Transaction) error { return repo.CreateSession(tx, a, newSession) }); !errors.Is(err, personalErrors.ErrAuthObjectUnavailable) {
		t.Fatal("new session on revoked device accepted")
	}
}

func TestAuthSessionPostgresInactiveStatesAndCurrentClock(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	uow := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	repo := NewAuthSessionRepository()
	for _, state := range []string{"expired", "device_deleted", "account_deleted", "account_inactive"} {
		t.Run(state, func(t *testing.T) {
			a, d, s := authSessionTestCreate(t, owner, uow, repo)
			query := ""
			var id uuid.UUID
			switch state {
			case "expired":
				query, id = "UPDATE mancuerna.sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=?", s.ID
			case "device_deleted":
				query, id = "UPDATE mancuerna.devices SET deleted_at=clock_timestamp() WHERE id=?", d.ID
			case "account_deleted":
				query, id = "UPDATE mancuerna.accounts SET deleted_at=clock_timestamp() WHERE id=?", a
			case "account_inactive":
				query, id = "UPDATE mancuerna.accounts SET status='deletion_pending',deletion_requested_at=clock_timestamp() WHERE id=?", a
			}
			if err := owner.Exec(query, id).Error; err != nil {
				t.Fatal(err)
			}
			if authSessionTestActive(t, uow, repo, a, s.ID, d.ID) {
				t.Fatal("inactive state accepted")
			}
		})
	}
	a, d, s := authSessionTestCreate(t, owner, uow, repo)
	if err := owner.Exec("UPDATE mancuerna.sessions SET expires_at=clock_timestamp()+interval '200 milliseconds' WHERE id=?", s.ID).Error; err != nil {
		t.Fatal(err)
	}
	tx, err := uow.Query(shared.WithPersonalAccount(context.Background(), a))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(tx)
	if err = tx.(*shared.GormTransaction).Tx.Exec("SELECT pg_sleep(0.25)").Error; err != nil {
		t.Fatal(err)
	}
	if active, err := repo.SessionActive(tx, a, s.ID, d.ID); err != nil || active {
		t.Fatalf("transaction start time kept an expired session alive: %v", err)
	}
}
