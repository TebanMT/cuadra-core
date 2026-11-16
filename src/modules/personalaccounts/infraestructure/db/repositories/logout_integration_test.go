//go:build server && integration

package repositories

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func logoutTestDependencies(t *testing.T) personalApp.LogoutDependencies {
	t.Helper()
	d := finalizationDependencies(t)
	r, err := NewLogoutRepository(budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	return personalApp.LogoutDependencies{UoW: d.UoW, Logout: r, Sessions: d.Sessions, Receipts: d.Receipts, Events: d.Events, Now: time.Now}
}
func logoutTestUseCase(t *testing.T, d personalApp.LogoutDependencies) *personalApp.Logout {
	t.Helper()
	u, err := personalApp.NewLogout(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func logoutTestInput(ticket string) personalApp.LogoutInput {
	return personalApp.LogoutInput{OperationID: uuid.New(), RequestHash: sha256.Sum256([]byte(ticket)), RevocationTicket: ticket}
}

func TestLogoutPostgresCurrentAndHistoricalTicketsRevokeExactSession(t *testing.T) {
	refreshD, owner, initial, key := refreshTestFixture(t)
	refreshUC := refreshTestUseCase(t, refreshD)
	next, err := refreshUC.Execute(context.Background(), refreshTestInput(t, initial, key, uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	accountID, deviceID := uuid.MustParse(initial.AccountID), uuid.MustParse(initial.DeviceID)
	second := authSessionTestSession(t, accountID, deviceID)
	if err = refreshD.UoW.Command(shared.WithPersonalAccount(context.Background(), accountID), func(tx shared.Transaction) error { return refreshD.Sessions.CreateSession(tx, accountID, second) }); err != nil {
		t.Fatal(err)
	}
	d := logoutTestDependencies(t)
	u := logoutTestUseCase(t, d)
	ack, err := u.Execute(context.Background(), logoutTestInput(initial.RevocationTicket))
	if err != nil || ack.Status != "completed" {
		t.Fatalf("old logout ticket failed: %v", err)
	}
	if refreshTestActive(t, refreshD, next) || !authSessionTestActive(t, refreshD.UoW, NewAuthSessionRepository(), accountID, second.ID, deviceID) {
		t.Fatal("logout revoked wrong session or whole device")
	}
	if _, err = u.Execute(context.Background(), logoutTestInput(next.RevocationTicket)); err != nil {
		t.Fatalf("current ticket cannot acknowledge already revoked session: %v", err)
	}
	if _, err = refreshUC.Execute(context.Background(), refreshTestInput(t, next, key, uuid.New())); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("logout permitted later refresh")
	}
	wrong := refreshTestInput(t, next, key, uuid.New())
	wrong.RefreshToken = next.RevocationTicket
	if _, err = refreshUC.Execute(context.Background(), wrong); !errors.Is(err, personalErrors.ErrInvalidRefresh) {
		t.Fatal("logout ticket granted refresh authority")
	}
	q, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), accountID))
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.Receipts.Get(q, receipt.Owner{AccountID: accountID}, uuid.MustParse(ack.OperationID))
	shared.CloseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Response.Ciphertext) != 0 || r.Response.KeyVersion != "" || r.ResultReference.String() != initial.SessionID {
		t.Fatal("logout receipt stores unnecessary protected material or wrong session")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.sessions WHERE account_id=?", accountID) != 2 {
		t.Fatal("logout deleted session history")
	}
}

func TestLogoutPostgresConcurrentIdempotenceAndRollback(t *testing.T) {
	refreshD, owner, s, _ := refreshTestFixture(t)
	d := logoutTestDependencies(t)
	in := logoutTestInput(s.RevocationTicket)
	failing := d
	failing.Events = failingSecurityEvent{}
	if ack, err := logoutTestUseCase(t, failing).Execute(context.Background(), in); err == nil || ack != (personalApp.Acknowledgement{}) {
		t.Fatal("audit failure returned success")
	}
	if !refreshTestActive(t, refreshD, s) || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID) != 0 {
		t.Fatal("failed logout retained revocation or receipt")
	}
	u := logoutTestUseCase(t, d)
	const count = 6
	results := make([]personalApp.Acknowledgement, count)
	errs := make([]error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; results[i], errs[i] = u.Execute(context.Background(), in) }(i)
	}
	close(start)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !reflect.DeepEqual(results[0], results[i]) {
			t.Fatalf("concurrent logout diverged: %v", errs[i])
		}
	}
	if refreshTestActive(t, refreshD, s) || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE account_id=? AND request_id=?", s.AccountID, in.OperationID) != 1 {
		t.Fatal("logout was repeated or not committed")
	}
	changed := in
	changed.RequestHash[0]++
	if _, err := u.Execute(context.Background(), changed); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("logout operation accepted changed content")
	}
	later := d
	later.Now = func() time.Time { return time.Now().Add(365 * 24 * time.Hour) }
	if ack, err := logoutTestUseCase(t, later).Execute(context.Background(), in); err != nil || ack != results[0] {
		t.Fatal("durable logout acknowledgement expired")
	}
}

func TestLogoutPostgresIsolationInactiveAccountAndNoAmbientScope(t *testing.T) {
	refreshD, owner, a, _ := refreshTestFixture(t)
	_, _, b, _ := refreshTestFixture(t)
	d := logoutTestDependencies(t)
	u := logoutTestUseCase(t, d)
	ha := sha256.Sum256([]byte(a.RevocationTicket))
	hb := sha256.Sum256([]byte(b.RevocationTicket))
	for _, ctx := range []context.Context{shared.WithPersonalAccount(context.Background(), uuid.MustParse(a.AccountID)), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{9}), shared.WithPersonalAccount(context.Background(), uuid.Nil), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err := d.Logout.Resolve(ctx, ha); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("logout identity lookup accepted ambient capability")
		}
		if _, err := u.Execute(ctx, logoutTestInput(a.RevocationTicket)); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("logout inherited ambient scope")
		}
	}
	if err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), uuid.MustParse(a.AccountID)), func(tx shared.Transaction) error {
		_, err := d.Logout.GetSessionForTicket(tx, uuid.MustParse(a.AccountID), hb)
		return err
	}); !errors.Is(err, personalErrors.ErrInvalidLogout) {
		t.Fatal("A scope resolved B logout ticket")
	}
	for _, dsn := range []string{"MANCUERNA_TEST_OWNER_DSN", "MANCUERNA_TEST_DATA_DSN"} {
		if _, err := NewLogoutRepository(budgetTestPool(t, dsn)); !errors.Is(err, personalErrors.ErrInvalidSecurityConfiguration) {
			t.Fatal("logout accepted unsafe pool")
		}
	}
	if _, err := u.Execute(context.Background(), logoutTestInput(a.RefreshToken)); !errors.Is(err, personalErrors.ErrInvalidLogout) {
		t.Fatal("refresh token interpreted as logout ticket")
	}
	first := logoutTestInput(a.RevocationTicket)
	if _, err := u.Execute(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	collision := logoutTestInput(b.RevocationTicket)
	collision.OperationID = first.OperationID
	if _, err := u.Execute(context.Background(), collision); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatalf("foreign operation ID reused: %v", err)
	}
	if !refreshTestActive(t, refreshD, b) {
		t.Fatal("foreign receipt collision retained B revocation")
	}
	if err := owner.Exec("UPDATE mancuerna.accounts SET status='deletion_pending',deletion_requested_at=clock_timestamp(),deleted_at=clock_timestamp() WHERE id=?", a.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := u.Execute(context.Background(), logoutTestInput(a.RevocationTicket)); err != nil {
		t.Fatalf("withdrawn account prevented limited logout: %v", err)
	}
	if !refreshTestActive(t, refreshD, b) {
		t.Fatal("logout affected unrelated account")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.sessions WHERE id=? AND revoked_at IS NOT NULL", a.SessionID) != 1 {
		t.Fatal("inactive account session not revoked")
	}
}
