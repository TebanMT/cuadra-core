//go:build server && integration

package repositories

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func rotateRecoveryTestDependencies(t *testing.T, start personalApp.StartDeviceProofDependencies) personalApp.RotateRecoveryKeyDependencies {
	t.Helper()
	d := finalizationDependencies(t)
	return personalApp.RotateRecoveryKeyDependencies{UoW: start.UoW, Sessions: start.Sessions, Recovery: d.Recovery, Proofs: start.Proofs, Receipts: start.Receipts, Events: start.Events, RecoverySecrets: d.RecoverySecrets, Envelopes: d.Envelopes, Now: time.Now}
}
func rotateRecoveryTestUseCase(t *testing.T, d personalApp.RotateRecoveryKeyDependencies) *personalApp.RotateRecoveryKey {
	t.Helper()
	u, err := personalApp.NewRotateRecoveryKey(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func rotateRecoveryTestInput(t *testing.T, start personalApp.StartDeviceProofDependencies, complete personalApp.CompleteDeviceProofDependencies, actor security.AccessClaims, key ed25519.PrivateKey, keyID uuid.UUID, purpose security.ProofPurpose) (personalApp.RotateRecoveryKeyInput, uuid.UUID) {
	t.Helper()
	in := personalApp.RotateRecoveryKeyInput{OperationID: uuid.New(), KeyID: keyID}
	in.RequestHash = sha256.Sum256([]byte("POST /me/recovery-key " + in.OperationID.String() + " " + in.KeyID.String()))
	proof := proofTestInput()
	proof.Purpose = purpose
	proof.TargetRequestHash = in.RequestHash
	challenge, err := proofTestStart(t, start).Execute(context.Background(), actor, proof)
	if err != nil {
		t.Fatal(err)
	}
	token, err := proofTestComplete(t, complete).Execute(context.Background(), actor, proofTestCompleteInput(t, challenge, actor, key))
	if err != nil {
		t.Fatal(err)
	}
	in.ProofToken = token.ProofToken
	return in, proof.ProofID
}
func rotateRecoveryTestLive(t *testing.T, d personalApp.RotateRecoveryKeyDependencies, actor security.AccessClaims) uuid.UUID {
	t.Helper()
	q, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), actor.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	k, err := d.Recovery.GetLive(q, actor.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	return k.ID
}

func TestRotateRecoveryKeyPostgresConcurrentReplacementAndHonestPreparation(t *testing.T) {
	start, complete, owner, actor, private := proofTestFixture(t)
	d := rotateRecoveryTestDependencies(t, start)
	oldID := rotateRecoveryTestLive(t, d, actor)
	old := recoveryTestGet(t, d.UoW, NewRecoveryKeyRepository(), actor.AccountID, oldID)
	if err := old.ConfirmSaved(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error { return d.Recovery.SaveState(tx, actor.AccountID, old) }); err != nil {
		t.Fatal(err)
	}
	in, proofID := rotateRecoveryTestInput(t, start, complete, actor, private, uuid.New(), security.RecoveryKey)
	u := rotateRecoveryTestUseCase(t, d)
	const count = 6
	results := make([]personalApp.RecoveryKit, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-gate; results[i], errs[i] = u.Execute(context.Background(), actor, in) }(i)
	}
	close(gate)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !reflect.DeepEqual(results[0], results[i]) || results[i].ID != in.KeyID.String() || results[i].Generation != "2" {
			t.Fatalf("recovery rotation diverged: %v", errs[i])
		}
	}
	newKey := recoveryTestGet(t, d.UoW, NewRecoveryKeyRepository(), actor.AccountID, in.KeyID)
	if !newKey.Live() || newKey.Prepared() || newKey.PreparedAt != nil || d.RecoverySecrets.Verify(results[0].Secret, newKey.Verifier) != nil {
		t.Fatal("new kit not live/unprepared or verifier does not match secret")
	}
	old = recoveryTestGet(t, d.UoW, NewRecoveryKeyRepository(), actor.AccountID, oldID)
	if old.Live() || old.RevokedAt == nil || old.PreparedAt == nil {
		t.Fatal("old kit not revoked or its preparation history erased")
	}
	proof := proofTestGet(t, start, actor, proofID)
	if proof.ConsumedAt == nil || proof.OperationID == nil || *proof.OperationID != in.OperationID {
		t.Fatal("rotation did not consume its exact proof")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.recovery_keys WHERE account_id=?", actor.AccountID) != 2 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE account_id=? AND request_id=?", actor.AccountID, in.OperationID) != 1 {
		t.Fatal("retry duplicated rotation effects")
	}
	readDeps := finalizationDependencies(t)
	view, err := meTestUseCase(t, readDeps).Execute(context.Background(), actor)
	if err != nil || view.RecoveryPrepared {
		t.Fatal("rotation falsely claimed saved recovery")
	}
	confirmation := confirmationTestUseCase(t, readDeps)
	if _, err = confirmation.Execute(context.Background(), actor, personalApp.ConfirmRecoveryKeyInput{OperationID: uuid.New(), RequestHash: [32]byte{45}, RecoveryKeyID: in.KeyID, Saved: true}); err != nil {
		t.Fatal(err)
	}
	view, err = meTestUseCase(t, readDeps).Execute(context.Background(), actor)
	if err != nil || !view.RecoveryPrepared {
		t.Fatal("new recovery confirmation did not update view")
	}
}

func TestRotateRecoveryKeyPostgresReplayBoundToActorWindowAndLiveKey(t *testing.T) {
	start, complete, owner, actor, private := proofTestFixture(t)
	d := rotateRecoveryTestDependencies(t, start)
	u := rotateRecoveryTestUseCase(t, d)
	in, _ := rotateRecoveryTestInput(t, start, complete, actor, private, uuid.New(), security.RecoveryKey)
	result, err := u.Execute(context.Background(), actor, in)
	if err != nil {
		t.Fatal(err)
	}
	sameDevice := authSessionTestSession(t, actor.AccountID, actor.DeviceID)
	otherDevice := authSessionTestDevice(t, actor.AccountID)
	otherSession := authSessionTestSession(t, actor.AccountID, otherDevice.ID)
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		if err := d.Sessions.CreateSession(tx, actor.AccountID, sameDevice); err != nil {
			return err
		}
		if err := d.Sessions.CreateDevice(tx, actor.AccountID, otherDevice); err != nil {
			return err
		}
		return d.Sessions.CreateSession(tx, actor.AccountID, otherSession)
	}); err != nil {
		t.Fatal(err)
	}
	for _, other := range []security.AccessClaims{{AccountID: actor.AccountID, SessionID: sameDevice.ID, DeviceID: actor.DeviceID}, {AccountID: actor.AccountID, SessionID: otherSession.ID, DeviceID: otherDevice.ID}} {
		if response, err := u.Execute(context.Background(), other, in); !errors.Is(err, personalErrors.ErrInvalidProof) || response != (personalApp.RecoveryKit{}) {
			t.Fatal("another actor recovered sensitive kit receipt")
		}
	}
	var completed time.Time
	if err = owner.Raw("SELECT completed_at FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID).Row().Scan(&completed); err != nil {
		t.Fatal(err)
	}
	later := d
	later.Now = func() time.Time { return completed.Add(6 * time.Minute) }
	if response, err := rotateRecoveryTestUseCase(t, later).Execute(context.Background(), actor, in); err != nil || response != result {
		t.Fatal("expired proof prevented valid response retry")
	}
	later.Now = func() time.Time { return completed.Add(10 * time.Minute) }
	if _, err = rotateRecoveryTestUseCase(t, later).Execute(context.Background(), actor, in); !errors.Is(err, personalErrors.ErrResultExpired) {
		t.Fatal("kit response survived original window")
	}
	next, _ := rotateRecoveryTestInput(t, start, complete, actor, private, uuid.New(), security.RecoveryKey)
	newResult, err := u.Execute(context.Background(), actor, next)
	if err != nil || newResult.Generation != "3" {
		t.Fatal("subsequent rotation failed")
	}
	if response, err := u.Execute(context.Background(), actor, in); !errors.Is(err, personalErrors.ErrRecoveryKeyState) || response != (personalApp.RecoveryKit{}) {
		t.Fatal("obsolete kit secret returned after replacement")
	}
	if rotateRecoveryTestLive(t, d, actor) != next.KeyID || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id IN (?,?)", in.OperationID, next.OperationID) != 2 {
		t.Fatal("old retry revoked latest key or erased receipts")
	}
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		return d.Sessions.RevokeSession(tx, actor.AccountID, actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = u.Execute(context.Background(), actor, next); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("revoked actor recovered kit")
	}
}

func TestRotateRecoveryKeyPostgresFailureRestoresPriorKitAndProof(t *testing.T) {
	start, complete, owner, actor, private := proofTestFixture(t)
	d := rotateRecoveryTestDependencies(t, start)
	oldID := rotateRecoveryTestLive(t, d, actor)
	in, proofID := rotateRecoveryTestInput(t, start, complete, actor, private, uuid.New(), security.RecoveryKey)
	failing := d
	failing.Events = failingSecurityEvent{}
	if response, err := rotateRecoveryTestUseCase(t, failing).Execute(context.Background(), actor, in); err == nil || response != (personalApp.RecoveryKit{}) {
		t.Fatal("failed rotation returned kit")
	}
	if rotateRecoveryTestLive(t, d, actor) != oldID || proofTestGet(t, start, actor, proofID).ConsumedAt != nil || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.recovery_keys WHERE id=?", in.KeyID) != 0 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID) != 0 {
		t.Fatal("failed rotation invalidated old kit or consumed proof")
	}
	u := rotateRecoveryTestUseCase(t, d)
	if _, err := u.Execute(context.Background(), actor, in); err != nil {
		t.Fatal(err)
	}
	collision, collisionProof := rotateRecoveryTestInput(t, start, complete, actor, private, in.KeyID, security.RecoveryKey)
	if _, err := u.Execute(context.Background(), actor, collision); !errors.Is(err, personalErrors.ErrRecoveryKeyConflict) {
		t.Fatalf("existing kit ID overwritten: %v", err)
	}
	if rotateRecoveryTestLive(t, d, actor) != in.KeyID || proofTestGet(t, start, actor, collisionProof).ConsumedAt != nil {
		t.Fatal("ID collision revoked live kit or consumed proof")
	}
	wrongPurpose, wrongProof := rotateRecoveryTestInput(t, start, complete, actor, private, uuid.New(), security.PhoneChange)
	if _, err := u.Execute(context.Background(), actor, wrongPurpose); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("wrong proof purpose rotated kit")
	}
	if proofTestGet(t, start, actor, wrongProof).ConsumedAt != nil {
		t.Fatal("wrong purpose consumed proof")
	}
	changed, changedProof := rotateRecoveryTestInput(t, start, complete, actor, private, uuid.New(), security.RecoveryKey)
	changed.RequestHash[0]++
	if _, err := u.Execute(context.Background(), actor, changed); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("wrong protected request hash rotated kit")
	}
	if proofTestGet(t, start, actor, changedProof).ConsumedAt != nil {
		t.Fatal("wrong target consumed proof")
	}
}

func TestRotateRecoveryKeyPostgresHistoricalGenerationAndIsolation(t *testing.T) {
	start, complete, owner, actor, private := proofTestFixture(t)
	d := rotateRecoveryTestDependencies(t, start)
	oldID := rotateRecoveryTestLive(t, d, actor)
	old := recoveryTestGet(t, d.UoW, NewRecoveryKeyRepository(), actor.AccountID, oldID)
	if err := old.Consume(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error { return d.Recovery.SaveState(tx, actor.AccountID, old) }); err != nil {
		t.Fatal(err)
	}
	in, _ := rotateRecoveryTestInput(t, start, complete, actor, private, uuid.New(), security.RecoveryKey)
	u := rotateRecoveryTestUseCase(t, d)
	if result, err := u.Execute(context.Background(), actor, in); err != nil || result.Generation != "2" {
		t.Fatalf("missing live key recycled historical generation: %v", err)
	}
	_, _, _, foreign, _ := proofTestFixture(t)
	if _, err := u.Execute(context.Background(), foreign, in); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("foreign account recovered or performed rotation")
	}
	if _, err := u.Execute(shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), actor, in); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("rotation inherited attempt")
	}
	if err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		_, err := d.Recovery.NextGeneration(tx, foreign.AccountID)
		return err
	}); !errors.Is(err, personalErrors.ErrAuthSessionScope) {
		t.Fatal("generation lookup crossed owner scope")
	}
	history := recoveryTestKey(t, actor.AccountID, math.MaxInt64)
	if err := history.Revoke(time.Now()); err != nil {
		t.Fatal(err)
	}
	model := recoveryToModel(history)
	if err := owner.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	overflow, proofID := rotateRecoveryTestInput(t, start, complete, actor, private, uuid.New(), security.RecoveryKey)
	if _, err := u.Execute(context.Background(), actor, overflow); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
		t.Fatalf("generation overflow not rejected: %v", err)
	}
	if proofTestGet(t, start, actor, proofID).ConsumedAt != nil || rotateRecoveryTestLive(t, d, actor) != in.KeyID {
		t.Fatal("overflow consumed proof or invalidated active key")
	}
}
