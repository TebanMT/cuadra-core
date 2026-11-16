//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	cursorCrypto "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/security"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func devicesTestDependencies(start personalApp.StartDeviceProofDependencies) personalApp.RevokeDevicesDependencies {
	return personalApp.RevokeDevicesDependencies{UoW: start.UoW, Sessions: start.Sessions, Devices: NewDevicesRepository(), Proofs: start.Proofs, Receipts: start.Receipts, Events: start.Events, Now: time.Now}
}
func devicesTestList(t *testing.T, start personalApp.StartDeviceProofDependencies, now func() time.Time) *personalApp.ListDevices {
	t.Helper()
	cursors, err := cursorCrypto.NewCursorSigner("test_devices", map[string][]byte{"test_devices": bytes.Repeat([]byte{0x51}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	u, err := personalApp.NewListDevices(personalApp.ListDevicesDependencies{UoW: start.UoW, Sessions: start.Sessions, Devices: NewDevicesRepository(), Cursors: cursors, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func devicesTestRevoke(t *testing.T, d personalApp.RevokeDevicesDependencies) *personalApp.RevokeDevices {
	t.Helper()
	u, err := personalApp.NewRevokeDevices(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func devicesTestAdd(t *testing.T, start personalApp.StartDeviceProofDependencies, actor security.AccessClaims) security.AccessClaims {
	t.Helper()
	d := authSessionTestDevice(t, actor.AccountID)
	s := authSessionTestSession(t, actor.AccountID, d.ID)
	if err := start.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		if err := start.Sessions.CreateDevice(tx, actor.AccountID, d); err != nil {
			return err
		}
		return start.Sessions.CreateSession(tx, actor.AccountID, s)
	}); err != nil {
		t.Fatal(err)
	}
	return security.AccessClaims{AccountID: actor.AccountID, DeviceID: d.ID, SessionID: s.ID}
}
func devicesTestInput(t *testing.T, start personalApp.StartDeviceProofDependencies, complete personalApp.CompleteDeviceProofDependencies, actor security.AccessClaims, key ed25519.PrivateKey, target device.RevocationTarget, operation uuid.UUID, purpose security.ProofPurpose) (personalApp.RevokeDevicesInput, uuid.UUID) {
	t.Helper()
	if operation == uuid.Nil {
		operation = uuid.New()
	}
	in := personalApp.RevokeDevicesInput{OperationID: operation, Target: target}
	path, body := "/me/devices/"+target.DeviceID.String()+"/revoke", []byte("{}")
	if target.DeviceID == uuid.Nil {
		path = "/me/devices/revoke"
		body, _ = json.Marshal(struct {
			Scope string `json:"scope"`
		}{target.Scope})
	}
	var err error
	in.RequestHash, err = crypto.RequestHash("POST", path, operation, body)
	if err != nil {
		t.Fatal(err)
	}
	p := proofTestInput()
	p.Purpose, p.TargetRequestHash = purpose, in.RequestHash
	challenge, err := proofTestStart(t, start).Execute(context.Background(), actor, p)
	if err != nil {
		t.Fatal(err)
	}
	token, err := proofTestComplete(t, complete).Execute(context.Background(), actor, proofTestCompleteInput(t, challenge, actor, key))
	if err != nil {
		t.Fatal(err)
	}
	in.ProofToken = token.ProofToken
	return in, p.ProofID
}

func TestDevicesPostgresKeysetPrecisionOwnerEpochAndRevokedActor(t *testing.T) {
	start, _, owner, actor, _ := proofTestFixture(t)
	_, other, _ := meTestFixture(t, finalizationDependencies(t))
	actors := []security.AccessClaims{actor, devicesTestAdd(t, start, actor), devicesTestAdd(t, start, actor), devicesTestAdd(t, start, actor)}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	stamps := []time.Time{base.Add(123456 * time.Microsecond), base.Add(123455 * time.Microsecond), base.Add(123455 * time.Microsecond), base.Add(123001 * time.Microsecond)}
	for i, a := range actors {
		if err := owner.Exec("UPDATE mancuerna.devices SET authorized_at=?,created_at=?,updated_at=? WHERE account_id=? AND id=?", stamps[i], base, stamps[i], actor.AccountID, a.DeviceID).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := owner.Exec("UPDATE mancuerna.devices SET revoked_at=clock_timestamp(),revocation_reason='fixture' WHERE account_id=? AND id=?", actor.AccountID, actors[3].DeviceID).Error; err != nil {
		t.Fatal(err)
	}
	deleted := devicesTestAdd(t, start, actor)
	if err := owner.Exec("UPDATE mancuerna.devices SET deleted_at=clock_timestamp() WHERE account_id=? AND id=?", actor.AccountID, deleted.DeviceID).Error; err != nil {
		t.Fatal(err)
	}
	u := devicesTestList(t, start, time.Now)
	first, err := u.Execute(context.Background(), actor, personalApp.ListDevicesInput{Limit: 1})
	if err != nil || len(first.Items) != 1 || first.NextCursor == nil || first.Items[0].ID != actor.DeviceID.String() || !first.Items[0].Current {
		t.Fatal("first page/current device mismatch", err)
	}
	ids := []string{first.Items[0].ID}
	page := first
	for page.NextCursor != nil {
		page, err = u.Execute(context.Background(), actor, personalApp.ListDevicesInput{Limit: 1, Cursor: *page.NextCursor})
		if err != nil || len(page.Items) != 1 {
			t.Fatal("pagination lost sub-millisecond rows", err)
		}
		ids = append(ids, page.Items[0].ID)
		if len(ids) > len(actors) {
			t.Fatal("keyset repeated a page")
		}
	}
	expected := []string{actors[0].DeviceID.String(), actors[1].DeviceID.String(), actors[2].DeviceID.String(), actors[3].DeviceID.String()}
	sort.Slice(expected[1:3], func(i, j int) bool { return expected[1+i] > expected[1+j] })
	if !reflect.DeepEqual(ids, expected) || page.Items[0].RevokedAt == nil {
		t.Fatal("keyset order, deleted exclusion, or revoked history changed")
	}
	raw, _ := json.Marshal(first)
	for _, forbidden := range []string{"public_key", "refresh", "account_id", "provider", "revocation_ticket"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("private fields leaked from list")
		}
	}
	for _, invalid := range []struct {
		actor security.AccessClaims
		in    personalApp.ListDevicesInput
	}{{other, personalApp.ListDevicesInput{Cursor: *first.NextCursor}}, {actor, personalApp.ListDevicesInput{Cursor: *first.NextCursor + "x"}}, {actor, personalApp.ListDevicesInput{Limit: 101}}} {
		if got, err := u.Execute(context.Background(), invalid.actor, invalid.in); err == nil || len(got.Items) != 0 {
			t.Fatal("invalid/foreign cursor or limit returned data")
		}
	}
	if _, err := devicesTestList(t, start, func() time.Time { return time.Now().Add(25 * time.Hour) }).Execute(context.Background(), actor, personalApp.ListDevicesInput{Cursor: *first.NextCursor}); !errors.Is(err, protocol.ErrCursorExpired) {
		t.Fatal("expired cursor accepted", err)
	}
	if err := owner.Exec("UPDATE mancuerna.sync_streams SET epoch=? WHERE account_id=?", uuid.New(), actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := u.Execute(context.Background(), actor, personalApp.ListDevicesInput{Cursor: *first.NextCursor}); !errors.Is(err, protocol.ErrCursorEpochChanged) {
		t.Fatal("old epoch accepted", err)
	}
	if err := start.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		return start.Sessions.RevokeSession(tx, actor.AccountID, actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := u.Execute(context.Background(), actor, personalApp.ListDevicesInput{Cursor: "invalid"}); !errors.Is(err, personalErrors.ErrSessionRevoked) || len(got.Items) != 0 {
		t.Fatal("revoked actor reached cursor or data", err)
	}
}

func TestDevicesPostgresProofBindingConcurrentReplayAndTargetIsolation(t *testing.T) {
	start, complete, owner, actor, key := proofTestFixture(t)
	otherDevice := devicesTestAdd(t, start, actor)
	_, foreign, _ := meTestFixture(t, finalizationDependencies(t))
	d := devicesTestDependencies(start)
	u := devicesTestRevoke(t, d)
	bad, proofID := devicesTestInput(t, start, complete, actor, key, device.RevocationTarget{DeviceID: foreign.DeviceID}, uuid.Nil, security.RevokeDevices)
	if _, err := u.Execute(context.Background(), actor, bad); !errors.Is(err, personalErrors.ErrAuthObjectNotFound) || proofTestGet(t, start, actor, proofID).ConsumedAt != nil {
		t.Fatal("foreign target disclosed or consumed proof", err)
	}
	wrongPurpose, wrongProof := devicesTestInput(t, start, complete, actor, key, device.RevocationTarget{DeviceID: otherDevice.DeviceID}, uuid.Nil, security.RecoveryKey)
	if _, err := u.Execute(context.Background(), actor, wrongPurpose); !errors.Is(err, personalErrors.ErrInvalidProof) || proofTestGet(t, start, actor, wrongProof).ConsumedAt != nil {
		t.Fatal("proof crossed purpose", err)
	}
	in, proofID := devicesTestInput(t, start, complete, actor, key, device.RevocationTarget{DeviceID: otherDevice.DeviceID}, uuid.Nil, security.RevokeDevices)
	changed := in
	changed.RequestHash[0]++
	if _, err := u.Execute(context.Background(), actor, changed); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("proof crossed request hash", err)
	}
	const count = 5
	acks := make([]personalApp.Acknowledgement, count)
	errs := make([]error, count)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range acks {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-gate; acks[i], errs[i] = u.Execute(context.Background(), actor, in) }(i)
	}
	close(gate)
	wg.Wait()
	for i := range acks {
		if errs[i] != nil || acks[i].Status != "completed" || acks[i] != acks[0] {
			t.Fatal("concurrent acknowledgement differed", errs[i])
		}
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE request_id=?", in.OperationID) != 1 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID) != 1 || proofTestGet(t, start, actor, proofID).ConsumedAt == nil {
		t.Fatal("revocation duplicated or failed to consume proof")
	}
	if authSessionTestActive(t, start.UoW, NewAuthSessionRepository(), actor.AccountID, otherDevice.SessionID, otherDevice.DeviceID) || !authSessionTestActive(t, start.UoW, NewAuthSessionRepository(), actor.AccountID, actor.SessionID, actor.DeviceID) {
		t.Fatal("revocation hit the wrong device")
	}
	if _, err := u.Execute(context.Background(), actor, changed); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("changed replay accepted", err)
	}
	newActor := devicesTestAdd(t, start, actor)
	if _, err := u.Execute(context.Background(), newActor, in); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("different live session retrieved original receipt", err)
	}
}

func devicesTestAuthorization(t *testing.T, owner *gorm.DB, accountID, approver uuid.UUID, authorizationState, challengeState string) accessFixture {
	t.Helper()
	f := accessTestFixture(t, owner, "", uuid.Nil)
	var approverValue any
	if approver != uuid.Nil {
		approverValue = approver
	}
	if err := owner.Exec(`UPDATE mancuerna.auth_challenges SET account_id=?,status=?,revision=4,
 provider_operation_id=?,provider_operation_hash=?,provider_operation_kind='check',provider_operation_started_at=created_at+interval '500 milliseconds',
 verified_at=CASE WHEN ? IN ('pending','uncertain') THEN NULL ELSE verified_at END,
 consumed_at=CASE WHEN ?='consumed' THEN clock_timestamp() ELSE NULL END,
 result_reference=CASE WHEN ?='consumed' THEN ?::uuid ELSE NULL END WHERE id=?`, accountID, challengeState, uuid.New(), f.hash[:], challengeState, challengeState, challengeState, uuid.New(), f.attempt).Error; err != nil {
		t.Fatal(err)
	}
	if err := owner.Exec(`INSERT INTO mancuerna.device_authorizations(id,account_id,challenge_id,approved_by_device_id,comparison_code,status,expires_at,decided_at)
 VALUES(?,?,?,?,'123456',?,clock_timestamp()+interval '3 minutes',CASE WHEN ?='approved' THEN clock_timestamp() ELSE NULL END)`, uuid.New(), accountID, f.attempt, approverValue, authorizationState, authorizationState).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDevicesPostgresOthersCancelsOnlyDependentUnconsumedApprovals(t *testing.T) {
	start, complete, owner, actor, key := proofTestFixture(t)
	target := devicesTestAdd(t, start, actor)
	secondSession := authSessionTestSession(t, actor.AccountID, target.DeviceID)
	if err := start.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		return start.Sessions.CreateSession(tx, actor.AccountID, secondSession)
	}); err != nil {
		t.Fatal(err)
	}
	dependent := devicesTestAuthorization(t, owner, actor.AccountID, target.DeviceID, "approved", "verified")
	independent := devicesTestAuthorization(t, owner, actor.AccountID, actor.DeviceID, "approved", "verified")
	pending := devicesTestAuthorization(t, owner, actor.AccountID, uuid.Nil, "pending", "uncertain")
	consumed := devicesTestAuthorization(t, owner, actor.AccountID, target.DeviceID, "approved", "consumed")
	expired := devicesTestAuthorization(t, owner, actor.AccountID, target.DeviceID, "approved", "expired")
	in, _ := devicesTestInput(t, start, complete, actor, key, device.RevocationTarget{Scope: device.RevokeOthers}, uuid.Nil, security.RevokeDevices)
	if _, err := devicesTestRevoke(t, devicesTestDependencies(start)).Execute(context.Background(), actor, in); err != nil {
		t.Fatal(err)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.sessions WHERE account_id=? AND device_id=? AND revoked_at IS NULL", actor.AccountID, target.DeviceID) != 0 || !authSessionTestActive(t, start.UoW, NewAuthSessionRepository(), actor.AccountID, actor.SessionID, actor.DeviceID) {
		t.Fatal("others did not retire every target session or preserved wrong actor")
	}
	for _, row := range []struct {
		f               accessFixture
		auth, challenge string
		revision        int
	}{
		{dependent, "cancelled", "cancelled", 5}, {independent, "approved", "verified", 4}, {pending, "pending", "uncertain", 4}, {consumed, "approved", "consumed", 4}, {expired, "approved", "expired", 4},
	} {
		if accessTestCount(t, owner, `SELECT count(*) FROM mancuerna.device_authorizations a JOIN mancuerna.auth_challenges c ON c.id=a.challenge_id WHERE c.id=? AND a.status=? AND c.status=? AND c.revision=? AND c.provider_operation_id IS NOT NULL AND c.provider_operation_hash=?`, row.f.attempt, row.auth, row.challenge, row.revision, row.f.hash[:]) != 1 {
			t.Fatal("cancellation affected independent/terminal authorization or erased provider evidence")
		}
	}
}

type devicesFailEvent struct {
	repository.SecurityEventRepository
}

func (d devicesFailEvent) Append(tx shared.Transaction, event repository.SecurityEvent) error {
	if err := d.SecurityEventRepository.Append(tx, event); err != nil {
		return err
	}
	return errors.New("synthetic failure after revocation receipt and audit")
}

func TestDevicesPostgresSelfAllRollbackAndLateOutcomeCannotRevive(t *testing.T) {
	start, complete, owner, actor, key := proofTestFixture(t)
	other := devicesTestAdd(t, start, actor)
	pending := devicesTestAuthorization(t, owner, actor.AccountID, uuid.Nil, "pending", "uncertain")
	binding := attempt.Binding{ID: pending.attempt, ContinuationHash: pending.hash}
	q, err := start.UoW.Query(accessTestContext(pending))
	if err != nil {
		t.Fatal(err)
	}
	before, err := NewAuthAttemptPostgresRepository().Get(q, binding)
	shared.CloseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	in, proofID := devicesTestInput(t, start, complete, actor, key, device.RevocationTarget{Scope: device.RevokeAll}, uuid.Nil, security.RevokeDevices)
	d := devicesTestDependencies(start)
	failing := d
	failing.Events = devicesFailEvent{d.Events}
	if _, err = devicesTestRevoke(t, failing).Execute(context.Background(), actor, in); err == nil {
		t.Fatal("injected commit failure ignored")
	}
	if proofTestGet(t, start, actor, proofID).ConsumedAt != nil || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.devices WHERE account_id=? AND revoked_at IS NOT NULL", actor.AccountID) != 0 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID) != 0 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE request_id=?", in.OperationID) != 0 || accessTestCount(t, owner, "SELECT revision FROM mancuerna.auth_challenges WHERE id=?", pending.attempt) != 4 {
		t.Fatal("rollback lost proof, receipt, audit, challenge or device atomicity")
	}
	u := devicesTestRevoke(t, d)
	if ack, err := u.Execute(context.Background(), actor, in); err != nil || ack.Status != "completed" {
		t.Fatal("all revocation did not commit", err)
	}
	for _, a := range []security.AccessClaims{actor, other} {
		if authSessionTestActive(t, start.UoW, NewAuthSessionRepository(), a.AccountID, a.SessionID, a.DeviceID) {
			t.Fatal("all retained a device session")
		}
	}
	if ack, err := u.Execute(context.Background(), actor, in); !errors.Is(err, personalErrors.ErrSessionRevoked) || ack != (personalApp.Acknowledgement{}) {
		t.Fatal("self-revoked actor bypassed live authority via receipt", err)
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_challenges WHERE id=? AND status='cancelled' AND revision=5 AND provider_operation_hash=?", pending.attempt, pending.hash[:]) != 1 {
		t.Fatal("orphaned pending authorization not cancelled")
	}
	op := before.ProviderOperation
	token := attempt.OperationToken{ID: op.ID, RequestHash: op.RequestHash, Kind: op.Kind, Revision: before.Revision}
	if err := before.ApplyOutcome(token, verification.Result{Reference: before.ProviderReference, Channel: before.Channel, State: verification.Pending, CreatedAt: before.CreatedAt, ExpiresAt: before.ExpiresAt}, time.Now()); err != nil {
		t.Fatal("fixture cannot simulate delayed provider response", err)
	}
	if err := start.UoW.Command(accessTestContext(pending), func(tx shared.Transaction) error {
		return NewAuthAttemptPostgresRepository().Save(tx, binding, before, token.Revision)
	}); !errors.Is(err, personalErrors.ErrAuthAttemptConflict) {
		t.Fatal("late provider response revived cancelled attempt", err)
	}
}

func TestDevicesPostgresSingleSelfAndGlobalReceiptCollisionFailClosed(t *testing.T) {
	start, complete, owner, actor, key := proofTestFixture(t)
	other := devicesTestAdd(t, start, actor)
	in, _ := devicesTestInput(t, start, complete, actor, key, device.RevocationTarget{DeviceID: actor.DeviceID}, uuid.Nil, security.RevokeDevices)
	if _, err := devicesTestRevoke(t, devicesTestDependencies(start)).Execute(context.Background(), actor, in); err != nil {
		t.Fatal(err)
	}
	page, err := devicesTestList(t, start, time.Now).Execute(context.Background(), other, personalApp.ListDevicesInput{})
	if err != nil || len(page.Items) != 2 {
		t.Fatal("single self-revocation affected other device", err)
	}
	startB, completeB, _, b, keyB := proofTestFixture(t)
	targetB := devicesTestAdd(t, startB, b)
	collision, proofID := devicesTestInput(t, startB, completeB, b, keyB, device.RevocationTarget{DeviceID: targetB.DeviceID}, in.OperationID, security.RevokeDevices)
	if _, err := devicesTestRevoke(t, devicesTestDependencies(startB)).Execute(context.Background(), b, collision); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("global receipt collision revealed foreign result or committed", err)
	}
	if proofTestGet(t, startB, b, proofID).ConsumedAt != nil || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.devices WHERE account_id=? AND revoked_at IS NOT NULL", b.AccountID) != 0 {
		t.Fatal("global collision escaped rollback")
	}
	dataUoW, err := shared.NewPersonalPostgresUnitOfWork(budgetTestPool(t, "MANCUERNA_TEST_DATA_DSN"), shared.PersonalDataRole)
	if err != nil {
		t.Fatal(err)
	}
	if err := dataUoW.Command(shared.WithPersonalAccount(context.Background(), b.AccountID), func(tx shared.Transaction) error { _, err := NewDevicesRepository().StreamEpoch(tx, b); return err }); !errors.Is(err, personalErrors.ErrAuthSessionScope) {
		t.Fatal("data role used security management repository", err)
	}
	if _, err := devicesTestList(t, startB, time.Now).Execute(shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{9}), b, personalApp.ListDevicesInput{}); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("mixed capability reached security list", err)
	}
}
