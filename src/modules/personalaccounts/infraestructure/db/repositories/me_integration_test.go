//go:build server && integration

package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/me"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/recovery"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func meTestCapabilities() me.Capabilities {
	return me.Capabilities{ProtocolVersion: 1, MinimumProtocolVersion: 1, MinimumAppVersion: "0.0.0", MaxPushOperations: 50, MaxRequestBytes: 2097152, MaxGroupBytes: 1048576, Features: []string{}}
}
func meTestUseCase(t *testing.T, d personalApp.FinalizeAccessDependencies) *personalApp.GetMe {
	t.Helper()
	u, err := personalApp.NewGetMe(personalApp.GetMeDependencies{UoW: d.UoW, Sessions: d.Sessions, Me: NewMeRepository(), Capabilities: meTestCapabilities()})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func meTestFixture(t *testing.T, d personalApp.FinalizeAccessDependencies) (*gorm.DB, security.AccessClaims, *recovery.RecoveryKey) {
	t.Helper()
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	a, device, s := authSessionTestCreate(t, owner, d.UoW, NewAuthSessionRepository())
	key := recoveryTestKey(t, a, 1)
	if err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), a), func(tx shared.Transaction) error { return d.Recovery.Create(tx, a, key) }); err != nil {
		t.Fatal(err)
	}
	return owner, security.AccessClaims{AccountID: a, DeviceID: device.ID, SessionID: s.ID, IssuedAt: s.CreatedAt, ExpiresAt: s.CreatedAt.Add(security.AccessTTL)}, key
}
func confirmationTestUseCase(t *testing.T, d personalApp.FinalizeAccessDependencies) *personalApp.ConfirmRecoveryKey {
	t.Helper()
	u, err := personalApp.NewConfirmRecoveryKey(personalApp.ConfirmRecoveryKeyDependencies{UoW: d.UoW, Sessions: d.Sessions, Recovery: d.Recovery, Receipts: d.Receipts, Events: d.Events, Envelopes: d.Envelopes, Now: d.Now})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func confirmationTestInput(keyID uuid.UUID) personalApp.ConfirmRecoveryKeyInput {
	return personalApp.ConfirmRecoveryKeyInput{OperationID: uuid.New(), RequestHash: [32]byte{42}, RecoveryKeyID: keyID, Saved: true}
}

func TestMePostgresProjectionAndSessionIsolation(t *testing.T) {
	d := finalizationDependencies(t)
	owner, a, _ := meTestFixture(t, d)
	_, b, _ := meTestFixture(t, d)
	if err := owner.Exec("UPDATE mancuerna.accounts SET display_name='Synthetic account A',revision=9007199254740993,security_version=9007199254740995 WHERE id=?", a.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	u := meTestUseCase(t, d)
	got, err := u.Execute(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile.ID != a.AccountID.String() || got.Profile.DisplayName != "Synthetic account A" || got.Profile.Revision != "9007199254740993" || got.SecurityVersion != "9007199254740995" || got.RecoveryPrepared || got.Status != "active" {
		t.Fatal("projection mixed or rounded protected state")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"phone_e164", "refresh_hash", "public_key", "verifier_hash", "+52"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatal("private data leaked through projection")
		}
	}
	if !strings.Contains(string(encoded), `"features":[]`) || !strings.Contains(string(encoded), `"policy_version":null`) || !strings.HasPrefix(got.PhoneMasked, "•••• ") {
		t.Fatal("configured capabilities or mask changed")
	}
	other, err := u.Execute(context.Background(), b)
	if err != nil || other.Profile.ID != b.AccountID.String() || other.Profile.DisplayName == got.Profile.DisplayName {
		t.Fatal("account snapshot isolation failed")
	}
	wrong := a
	wrong.AccountID = b.AccountID
	if _, err = u.Execute(context.Background(), wrong); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatalf("mixed claim owner accepted: %v", err)
	}
	wrong = a
	wrong.DeviceID = b.DeviceID
	if _, err = u.Execute(context.Background(), wrong); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatalf("mixed claim device accepted: %v", err)
	}
	if _, err = u.Execute(shared.WithPersonalAccount(context.Background(), b.AccountID), a); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("existing context account replaced")
	}
	if _, err = u.Execute(shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{9}), a); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("authenticated account inherited attempt capability")
	}
	q, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), a.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	if _, err = NewMeRepository().Get(q, b.AccountID, b.SessionID, b.DeviceID); !errors.Is(err, personalErrors.ErrAuthSessionScope) {
		t.Fatal("repository accepted foreign explicit scope")
	}
}

// Revocation deliberately commits after the use case's initial state check.
// The projection's own predicate must reject it within its statement snapshot.
type meRevokingSessions struct {
	repository.AuthSessionRepository
	revoke func() error
}

func (r meRevokingSessions) SessionActive(tx shared.Transaction, a, s, d uuid.UUID) (bool, error) {
	active, err := r.AuthSessionRepository.SessionActive(tx, a, s, d)
	if err == nil && active {
		err = r.revoke()
	}
	return active, err
}
func TestMePostgresRechecksConcurrentRevocationAndInactiveState(t *testing.T) {
	d := finalizationDependencies(t)
	owner, actor, _ := meTestFixture(t, d)
	uow := d.UoW
	d.Sessions = meRevokingSessions{AuthSessionRepository: d.Sessions, revoke: func() error {
		return uow.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
			return NewAuthSessionRepository().RevokeSession(tx, actor.AccountID, actor.SessionID, time.Now())
		})
	}}
	if _, err := meTestUseCase(t, d).Execute(context.Background(), actor); !errors.Is(err, personalErrors.ErrAuthObjectUnavailable) {
		t.Fatalf("revocation between reads leaked view: %v", err)
	}
	d.Sessions = NewAuthSessionRepository()
	for _, change := range []string{"UPDATE mancuerna.accounts SET deleted_at=clock_timestamp() WHERE id=?", "UPDATE mancuerna.devices SET deleted_at=clock_timestamp() WHERE account_id=?", "UPDATE mancuerna.sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE account_id=?"} {
		_, next, _ := meTestFixture(t, d)
		if err := owner.Exec(change, next.AccountID).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := meTestUseCase(t, d).Execute(context.Background(), next); !errors.Is(err, personalErrors.ErrSessionRevoked) {
			t.Fatalf("inactive actor returned view: %v", err)
		}
	}
}

func TestConfirmRecoveryKeyPostgresAtomicConcurrentAndDurableReplay(t *testing.T) {
	d := finalizationDependencies(t)
	owner, actor, key := meTestFixture(t, d)
	u := confirmationTestUseCase(t, d)
	in := confirmationTestInput(key.ID)
	const count = 6
	results := make([]personalApp.Acknowledgement, count)
	errs := make([]error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = u.Execute(context.Background(), actor, in)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !reflect.DeepEqual(results[0], results[i]) || results[i].Status != "completed" || results[i].OperationID != in.OperationID.String() {
			t.Fatalf("concurrent confirmation diverged: %v", errs[i])
		}
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE account_id=? AND request_id=?", actor.AccountID, in.OperationID) != 1 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID) != 1 {
		t.Fatal("duplicate operation effects")
	}
	saved := recoveryTestGet(t, d.UoW, NewRecoveryKeyRepository(), actor.AccountID, key.ID)
	if !saved.Prepared() {
		t.Fatal("saved state missing")
	}
	view, err := meTestUseCase(t, d).Execute(context.Background(), actor)
	if err != nil || !view.RecoveryPrepared {
		t.Fatal("prepared key not reflected in account")
	}
	q, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), actor.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	r, err := d.Receipts.Get(q, receipt.Owner{AccountID: actor.AccountID}, in.OperationID)
	shared.CloseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := d.Envelopes.OpenReceipt(r.Context(), r.Response)
	if err != nil {
		t.Fatal(err)
	}
	var persisted personalApp.Acknowledgement
	if json.Unmarshal(plain, &persisted) != nil || !reflect.DeepEqual(persisted, results[0]) {
		t.Fatal("ack not persisted atomically")
	}
	if _, err = r.ReadResponse(r.CompletedAt.Add(11 * time.Minute)); !errors.Is(err, personalErrors.ErrResultExpired) {
		t.Fatal("fixture response should expire")
	}
	bad := in
	bad.RequestHash[0]++
	if _, err = u.Execute(context.Background(), actor, bad); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("changed hash reused receipt")
	}
	bad = in
	bad.RecoveryKeyID = uuid.New()
	if _, err = u.Execute(context.Background(), actor, bad); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("different key reused receipt")
	}
	// Replacing the kit must make current readiness false without erasing the
	// completed confirmation receipt or allowing its old effect to run again.
	replacement := recoveryTestKey(t, actor.AccountID, 2)
	err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		if err := saved.Revoke(time.Now()); err != nil {
			return err
		}
		if err := d.Recovery.SaveState(tx, actor.AccountID, saved); err != nil {
			return err
		}
		return d.Recovery.Create(tx, actor.AccountID, replacement)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.Exec("UPDATE mancuerna.auth_receipts SET response_ciphertext=NULL,key_version=NULL,response_expires_at=completed_at WHERE operation_id=?", in.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	replay, err := u.Execute(context.Background(), actor, in)
	if err != nil || !reflect.DeepEqual(replay, results[0]) {
		t.Fatalf("durable ack lost after envelope removal: %v", err)
	}
	view, err = meTestUseCase(t, d).Execute(context.Background(), actor)
	if err != nil || view.RecoveryPrepared {
		t.Fatal("old confirmation prepared new kit")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE account_id=? AND request_id=?", actor.AccountID, in.OperationID) != 1 {
		t.Fatal("replay duplicated audit")
	}
}

func TestConfirmRecoveryKeyPostgresRollbackIsolationAndRevokedActor(t *testing.T) {
	d := finalizationDependencies(t)
	owner, actor, key := meTestFixture(t, d)
	_, other, foreignKey := meTestFixture(t, d)
	u := confirmationTestUseCase(t, d)
	for _, keyID := range []uuid.UUID{foreignKey.ID, uuid.New()} {
		if _, err := u.Execute(context.Background(), actor, confirmationTestInput(keyID)); !errors.Is(err, personalErrors.ErrRecoveryKeyNotFound) {
			t.Fatalf("foreign/missing key difference: %v", err)
		}
	}
	in := confirmationTestInput(key.ID)
	bad := in
	bad.Saved = false
	if _, err := u.Execute(context.Background(), actor, bad); !errors.Is(err, personalErrors.ErrInvalidRecoveryConfirmation) {
		t.Fatal("false saved confirmation accepted")
	}
	if _, err := u.Execute(shared.WithPersonalAccount(context.Background(), other.AccountID), actor, in); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("confirmation changed context account")
	}
	if _, err := u.Execute(shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{9}), actor, in); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("confirmation inherited attempt capability")
	}
	failing := d
	failing.Events = failingSecurityEvent{}
	ack, err := confirmationTestUseCase(t, failing).Execute(context.Background(), actor, in)
	if err == nil || ack != (personalApp.Acknowledgement{}) {
		t.Fatal("audit failure returned successful acknowledgement")
	}
	if recoveryTestGet(t, d.UoW, NewRecoveryKeyRepository(), actor.AccountID, key.ID).Prepared() || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID) != 0 {
		t.Fatal("audit failure retained prepared state or receipt")
	}
	if _, err = u.Execute(context.Background(), actor, in); err != nil {
		t.Fatal(err)
	}
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		return d.Sessions.RevokeSession(tx, actor.AccountID, actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = u.Execute(context.Background(), actor, in); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatalf("revoked actor replayed successful ack: %v", err)
	}
	// A new request may not confirm a consumed/revoked key even though an old
	// completed operation remains safely replayable by an active actor.
	for _, terminal := range []string{"consume", "revoke"} {
		_, active, k := meTestFixture(t, d)
		if terminal == "consume" {
			err = k.Consume(time.Now())
		} else {
			err = k.Revoke(time.Now())
		}
		if err != nil {
			t.Fatal(err)
		}
		if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), active.AccountID), func(tx shared.Transaction) error { return d.Recovery.SaveState(tx, active.AccountID, k) }); err != nil {
			t.Fatal(err)
		}
		if _, err = u.Execute(context.Background(), active, confirmationTestInput(k.ID)); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
			t.Fatalf("%s key confirmed", terminal)
		}
	}
}
