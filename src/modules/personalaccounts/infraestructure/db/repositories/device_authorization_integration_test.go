//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceauthorization"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	cursorCrypto "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/security"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type authorizationFixture struct {
	d             personalApp.FinalizeAccessDependencies
	start         personalApp.StartDeviceAuthorizationDependencies
	decide        personalApp.DecideDeviceAuthorizationDependencies
	status        personalApp.GetDeviceAuthorizationStatusDependencies
	proofStart    personalApp.StartDeviceProofDependencies
	proofComplete personalApp.CompleteDeviceProofDependencies
	owner         *gorm.DB
	actor         security.AccessClaims
	key           ed25519.PrivateKey
	phone         string
}

func authorizationTestFixture(t *testing.T) authorizationFixture {
	t.Helper()
	d := finalizationDependencies(t)
	a, in, key := verifiedAccessFixture(t, d, accessTestPhone(), nil)
	u, err := personalApp.NewFinalizeAccess(d)
	if err != nil {
		t.Fatal(err)
	}
	result, err := u.Execute(context.Background(), in)
	if err != nil || result.Authorized == nil {
		t.Fatal("signup fixture failed")
	}
	actor, err := d.Tokens.Verify(result.Authorized.Session.AccessToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	authorizations := NewDeviceAuthorizationRepository()
	proofs := NewDeviceProofRepository()
	f := authorizationFixture{d: d, owner: budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN"), actor: actor, key: key, phone: a.DestinationE164}
	f.start = personalApp.StartDeviceAuthorizationDependencies{UoW: d.UoW, Attempts: d.Attempts, Authorizations: authorizations, Receipts: d.Receipts, Events: d.Events, Now: time.Now}
	f.decide = personalApp.DecideDeviceAuthorizationDependencies{UoW: d.UoW, Attempts: d.Attempts, Authorizations: authorizations, Sessions: d.Sessions, Proofs: proofs, Receipts: d.Receipts, Events: d.Events, Tokens: d.Tokens, Envelopes: d.Envelopes, Now: time.Now}
	f.status = personalApp.GetDeviceAuthorizationStatusDependencies{UoW: d.UoW, Attempts: d.Attempts, Authorizations: authorizations, Sessions: d.Sessions, Receipts: d.Receipts, Envelopes: d.Envelopes, Now: time.Now}
	f.proofStart = personalApp.StartDeviceProofDependencies{UoW: d.UoW, Sessions: d.Sessions, Proofs: proofs, Receipts: d.Receipts, Events: d.Events, Now: time.Now}
	f.proofComplete = personalApp.CompleteDeviceProofDependencies{UoW: d.UoW, Sessions: d.Sessions, Proofs: proofs, Receipts: d.Receipts, Events: d.Events, Verifier: d.Proofs, Envelopes: d.Envelopes, Now: time.Now}
	return f
}
func authorizationTestStart(t *testing.T, d personalApp.StartDeviceAuthorizationDependencies) *personalApp.StartDeviceAuthorization {
	t.Helper()
	u, err := personalApp.NewStartDeviceAuthorization(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func authorizationTestDecide(t *testing.T, d personalApp.DecideDeviceAuthorizationDependencies) *personalApp.DecideDeviceAuthorization {
	t.Helper()
	u, err := personalApp.NewDecideDeviceAuthorization(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func authorizationTestStatus(t *testing.T, d personalApp.GetDeviceAuthorizationStatusDependencies) *personalApp.GetDeviceAuthorizationStatus {
	t.Helper()
	u, err := personalApp.NewGetDeviceAuthorizationStatus(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func authorizationTestList(t *testing.T, f authorizationFixture) *personalApp.ListDeviceAuthorizations {
	t.Helper()
	signer, err := cursorCrypto.NewCursorSigner("authorization", map[string][]byte{"authorization": bytes.Repeat([]byte{44}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	u, err := personalApp.NewListDeviceAuthorizations(personalApp.ListDeviceAuthorizationsDependencies{UoW: f.d.UoW, Sessions: f.d.Sessions, Authorizations: f.start.Authorizations, Cursors: signer, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func authorizationTestCandidate(t *testing.T, f authorizationFixture, candidate *attempt.Candidate) (personalApp.StartDeviceAuthorizationInput, personalApp.FinalizeAccessInput) {
	t.Helper()
	_, check, _ := verifiedAccessFixture(t, f.d, f.phone, candidate)
	u, err := personalApp.NewFinalizeAccess(f.d)
	if err != nil {
		t.Fatal(err)
	}
	result, err := u.Execute(context.Background(), check)
	if err != nil || !result.AuthorizationRequired || result.Authorized != nil {
		t.Fatal("existing account did not require separate authorization")
	}
	in := personalApp.StartDeviceAuthorizationInput{Binding: check.Binding, AuthorizationID: uuid.New(), OperationID: uuid.New()}
	in.RequestHash = sha256.Sum256([]byte(in.OperationID.String() + in.AuthorizationID.String() + in.Binding.ID.String()))
	return in, check
}
func authorizationTestDecision(t *testing.T, f authorizationFixture, actor security.AccessClaims, key ed25519.PrivateKey, id uuid.UUID, decision deviceauthorization.Decision) (personalApp.DecideDeviceAuthorizationInput, uuid.UUID) {
	t.Helper()
	in := personalApp.DecideDeviceAuthorizationInput{AuthorizationID: id, OperationID: uuid.New(), Decision: decision}
	body, _ := json.Marshal(map[string]string{"decision": string(decision)})
	var err error
	in.RequestHash, err = crypto.RequestHash("POST", "/me/device-authorizations/"+id.String()+"/decision", in.OperationID, body)
	if err != nil {
		t.Fatal(err)
	}
	proof := proofTestInput()
	proof.Purpose = security.AuthorizeDevice
	proof.TargetRequestHash = in.RequestHash
	challenge, err := proofTestStart(t, f.proofStart).Execute(context.Background(), actor, proof)
	if err != nil {
		t.Fatal(err)
	}
	token, err := proofTestComplete(t, f.proofComplete).Execute(context.Background(), actor, proofTestCompleteInput(t, challenge, actor, key))
	if err != nil {
		t.Fatal(err)
	}
	in.ProofToken = token.ProofToken
	return in, proof.ProofID
}
func authorizationTestAttempt(t *testing.T, f authorizationFixture, binding attempt.Binding) *attempt.AuthAttempt {
	t.Helper()
	q, err := f.d.UoW.Query(shared.WithPersonalAttempt(context.Background(), binding.ID, binding.ContinuationHash))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	a, err := f.d.Attempts.Get(q, binding)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func authorizationTestAdditionalActor(t *testing.T, f authorizationFixture) (security.AccessClaims, ed25519.PrivateKey) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	d, err := device.NewAuthorizedDevice(f.actor.AccountID, attempt.Candidate{ID: uuid.New(), PublicKey: [32]byte(pub), Platform: "ios", Label: "Synthetic second approver"}, device.Device, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	s := authSessionTestSession(t, f.actor.AccountID, d.ID)
	err = f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		if err := f.d.Sessions.CreateDevice(tx, f.actor.AccountID, d); err != nil {
			return err
		}
		return f.d.Sessions.CreateSession(tx, f.actor.AccountID, s)
	})
	if err != nil {
		t.Fatal(err)
	}
	return security.AccessClaims{AccountID: f.actor.AccountID, SessionID: s.ID, DeviceID: d.ID}, key
}

func TestDeviceAuthorizationPostgresConcurrentStartAndBoundCandidate(t *testing.T) {
	f := authorizationTestFixture(t)
	in, _ := authorizationTestCandidate(t, f, nil)
	u := authorizationTestStart(t, f.start)
	const count = 5
	var wg sync.WaitGroup
	results := make([]personalApp.DeviceAuthorizationResult, count)
	errs := make([]error, count)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = u.Execute(context.Background(), in) }(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || results[i] != results[0] || results[i].Status != "pending" {
			t.Fatalf("start replay diverged: %v", errs[i])
		}
	}
	a := authorizationTestAttempt(t, f, in.Binding)
	if results[0].Candidate.ID != a.Candidate.ID.String() || len(results[0].ComparisonCode) != 6 || results[0].ExpiresAt != a.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z") {
		t.Fatal("request changed exact candidate or expiry")
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.device_authorizations WHERE challenge_id=?", in.Binding.ID) != 1 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.security_events WHERE request_id=? AND actor_device_id IS NULL", in.OperationID) != 1 {
		t.Fatal("start did not audit exactly one unauthorised request")
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.devices WHERE account_id=?", f.actor.AccountID) != 1 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.sessions WHERE account_id=?", f.actor.AccountID) != 1 {
		t.Fatal("request granted candidate access")
	}
	status, err := authorizationTestStatus(t, f.status).Execute(context.Background(), in.Binding)
	if err != nil || status.StateOverride != personalApp.AuthorizationPending || status.Authorized != nil {
		t.Fatal("pending request status granted access")
	}
	changed := in
	changed.RequestHash[0] ^= 1
	if _, err = u.Execute(context.Background(), changed); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("changed start receipt accepted")
	}
	changed = in
	changed.AuthorizationID = uuid.New()
	changed.OperationID = uuid.New()
	if _, err = u.Execute(context.Background(), changed); !errors.Is(err, personalErrors.ErrDeviceAuthorizationState) {
		t.Fatal("same attempt created another request")
	}
	changed = in
	changed.Binding.ContinuationHash[0] ^= 1
	if _, err = u.Execute(context.Background(), changed); !errors.Is(err, personalErrors.ErrAuthAttemptNotFound) {
		t.Fatal("wrong continuation retrieved request")
	}
	if _, err = u.Execute(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), in); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("start accepted authenticated/mixed scope")
	}
}

func TestDeviceAuthorizationPostgresApprovalSeparateReceiptsAndReadOnlyResult(t *testing.T) {
	f := authorizationTestFixture(t)
	in, check := authorizationTestCandidate(t, f, nil)
	if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	decision, proofID := authorizationTestDecision(t, f, f.actor, f.key, in.AuthorizationID, deviceauthorization.Approve)
	u := authorizationTestDecide(t, f.decide)
	const count = 5
	var wg sync.WaitGroup
	acks := make([]personalApp.Acknowledgement, count)
	errs := make([]error, count)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); acks[i], errs[i] = u.Execute(context.Background(), f.actor, decision) }(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || acks[i] != acks[0] {
			t.Fatalf("decision replay diverged: %v", errs[i])
		}
	}
	a := authorizationTestAttempt(t, f, in.Binding)
	if a.State != attempt.Consumed || a.ResultReference == nil || *a.ResultReference == decision.OperationID || *a.ResultReference == check.OperationID {
		t.Fatal("approval reused OTP or approver result identity")
	}
	statusUC := authorizationTestStatus(t, f.status)
	status, err := statusUC.Execute(context.Background(), in.Binding)
	if err != nil || status.Authorized == nil || status.Authorized.RecoveryKit != nil || status.Authorized.Session.DeviceID != a.Candidate.ID.String() {
		t.Fatal("candidate did not receive its committed session", err)
	}
	claims, err := f.d.Tokens.Verify(status.Authorized.Session.AccessToken, time.Now())
	if err != nil || claims.AccountID != f.actor.AccountID || claims.DeviceID != a.Candidate.ID {
		t.Fatal("candidate credential does not identify exact device")
	}
	for i := 0; i < 3; i++ {
		replay, err := statusUC.Execute(context.Background(), in.Binding)
		if err != nil || !reflect.DeepEqual(replay.Authorized, status.Authorized) {
			t.Fatal("GET did not replay exact result")
		}
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.devices WHERE account_id=?", f.actor.AccountID) != 2 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.sessions WHERE account_id=?", f.actor.AccountID) != 2 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.recovery_keys WHERE account_id=?", f.actor.AccountID) != 1 {
		t.Fatal("approval/GET duplicated device, session or recovery kit")
	}
	if !authSessionTestActive(t, f.d.UoW, NewAuthSessionRepository(), f.actor.AccountID, f.actor.SessionID, f.actor.DeviceID) {
		t.Fatal("approval revoked original device")
	}
	proof := proofTestGet(t, f.proofStart, f.actor, proofID)
	if proof.OperationID == nil || *proof.OperationID != decision.OperationID {
		t.Fatal("approval did not consume exact proof")
	}
	q, err := f.d.UoW.Query(shared.WithPersonalAccount(context.Background(), f.actor.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	ackReceipt, err := f.d.Receipts.Get(q, receipt.Owner{AccountID: f.actor.AccountID}, decision.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	resultReceipt, err := f.d.Receipts.Get(q, receipt.Owner{AccountID: f.actor.AccountID, AttemptID: a.ID}, *a.ResultReference)
	shared.CloseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(ackReceipt.Response.Ciphertext) != 0 || len(ackReceipt.ResultSecretHash) != 32 || ackReceipt.ResultReference != in.AuthorizationID || resultReceipt.Purpose != "device_authorization_result" || len(resultReceipt.Response.Ciphertext) == 0 || resultReceipt.ResponseExpiresAt.Sub(resultReceipt.CompletedAt) != 10*time.Minute {
		t.Fatal("candidate result not separated from durable actor Ack")
	}
	finalizer, err := personalApp.NewFinalizeAccess(f.d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = finalizer.Execute(context.Background(), check); err == nil {
		t.Fatal("OTP finalizer adopted a different authorization effect")
	}
	second := authSessionTestSession(t, f.actor.AccountID, f.actor.DeviceID)
	if err = f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error { return f.d.Sessions.CreateSession(tx, f.actor.AccountID, second) }); err != nil {
		t.Fatal(err)
	}
	otherActor := f.actor
	otherActor.SessionID = second.ID
	if _, err = u.Execute(context.Background(), otherActor, decision); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("other session recovered actor Ack")
	}
	later := f.status
	later.Now = func() time.Time { return resultReceipt.CompletedAt.Add(10 * time.Minute) }
	if response, err := authorizationTestStatus(t, later).Execute(context.Background(), in.Binding); !errors.Is(err, personalErrors.ErrResultExpired) || response.Authorized != nil {
		t.Fatal("expired result recreated session")
	}
	if err = f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, claims.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = statusUC.Execute(context.Background(), in.Binding); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("revoked candidate recovered credentials")
	}
}

func TestDeviceAuthorizationPostgresDenyApproveRaceAndExpiry(t *testing.T) {
	f := authorizationTestFixture(t)
	in, _ := authorizationTestCandidate(t, f, nil)
	if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	approve, approveProof := authorizationTestDecision(t, f, f.actor, f.key, in.AuthorizationID, deviceauthorization.Approve)
	deny, denyProof := authorizationTestDecision(t, f, f.actor, f.key, in.AuthorizationID, deviceauthorization.Deny)
	u := authorizationTestDecide(t, f.decide)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, d := range []personalApp.DecideDeviceAuthorizationInput{approve, deny} {
		wg.Add(1)
		go func(i int, d personalApp.DecideDeviceAuthorizationInput) {
			defer wg.Done()
			_, errs[i] = u.Execute(context.Background(), f.actor, d)
		}(i, d)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatal("race did not choose exactly one terminal decision")
	}
	for _, err := range errs {
		if err != nil && !errors.Is(err, personalErrors.ErrDeviceAuthorizationState) {
			t.Fatal("unexpected race failure", err)
		}
	}
	a := authorizationTestAttempt(t, f, in.Binding)
	if (errs[0] == nil && a.State != attempt.Consumed) || (errs[1] == nil && a.State != attempt.Cancelled) {
		t.Fatal("terminal decision did not match attempt")
	}
	p1, p2 := proofTestGet(t, f.proofStart, f.actor, approveProof), proofTestGet(t, f.proofStart, f.actor, denyProof)
	if (p1.ConsumedAt == nil) == (p2.ConsumedAt == nil) {
		t.Fatal("losing race consumed proof")
	}
	deniedIn, _ := authorizationTestCandidate(t, f, nil)
	if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), deniedIn); err != nil {
		t.Fatal(err)
	}
	denied, _ := authorizationTestDecision(t, f, f.actor, f.key, deniedIn.AuthorizationID, deviceauthorization.Deny)
	ack, err := u.Execute(context.Background(), f.actor, denied)
	if err != nil {
		t.Fatal(err)
	}
	view, err := authorizationTestStatus(t, f.status).Execute(context.Background(), deniedIn.Binding)
	if err != nil || view.StateOverride != personalApp.AuthorizationDenied || view.Authorized != nil {
		t.Fatal("denied attempt granted authority")
	}
	later := f.decide
	later.Now = func() time.Time { return time.Now().Add(24 * time.Hour) }
	if replay, err := authorizationTestDecide(t, later).Execute(context.Background(), f.actor, denied); err != nil || replay != ack {
		t.Fatal("durable actor acknowledgement depended on response ciphertext")
	}
	expiredIn, _ := authorizationTestCandidate(t, f, nil)
	if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), expiredIn); err != nil {
		t.Fatal(err)
	}
	expired, proofID := authorizationTestDecision(t, f, f.actor, f.key, expiredIn.AuthorizationID, deviceauthorization.Approve)
	a = authorizationTestAttempt(t, f, expiredIn.Binding)
	exact := f.decide
	exact.Now = func() time.Time { return a.ExpiresAt }
	if _, err = authorizationTestDecide(t, exact).Execute(context.Background(), f.actor, expired); !errors.Is(err, personalErrors.ErrDeviceAuthorizationExpired) || proofTestGet(t, f.proofStart, f.actor, proofID).ConsumedAt != nil {
		t.Fatal("authorization survived exact original expiry")
	}
}

func TestDeviceAuthorizationPostgresRollbackAndCandidateUUIDNeverOverwrite(t *testing.T) {
	f := authorizationTestFixture(t)
	in, _ := authorizationTestCandidate(t, f, nil)
	failedStart := f.start
	failedStart.Events = failingSecurityEvent{}
	if _, err := authorizationTestStart(t, failedStart).Execute(context.Background(), in); err == nil {
		t.Fatal("audit failure committed start")
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.device_authorizations WHERE id=?", in.AuthorizationID) != 0 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID) != 0 {
		t.Fatal("start rollback retained partial effects")
	}
	if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	decision, proofID := authorizationTestDecision(t, f, f.actor, f.key, in.AuthorizationID, deviceauthorization.Approve)
	failed := f.decide
	failed.Events = failingSecurityEvent{}
	if ack, err := authorizationTestDecide(t, failed).Execute(context.Background(), f.actor, decision); err == nil || ack != (personalApp.Acknowledgement{}) {
		t.Fatal("audit failure returned decision")
	}
	a := authorizationTestAttempt(t, f, in.Binding)
	if a.State != attempt.Verified || a.ResultReference != nil || proofTestGet(t, f.proofStart, f.actor, proofID).ConsumedAt != nil {
		t.Fatal("rollback consumed attempt or proof")
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.devices WHERE account_id=?", f.actor.AccountID) != 1 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.sessions WHERE account_id=?", f.actor.AccountID) != 1 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE attempt_id=? AND purpose='device_authorization_result'", in.Binding.ID) != 0 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", decision.OperationID) != 0 {
		t.Fatal("approval rollback retained credentials or receipts")
	}
	if _, err := authorizationTestDecide(t, f.decide).Execute(context.Background(), f.actor, decision); err != nil {
		t.Fatal("rolled-back proof could not retry", err)
	}
	other, key := authorizationTestAdditionalActor(t, f)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	candidate := attempt.Candidate{ID: f.actor.DeviceID, PublicKey: [32]byte(pub), Platform: "android", Label: "Occupied UUID"}
	occupied, _ := authorizationTestCandidate(t, f, &candidate)
	if _, err = authorizationTestStart(t, f.start).Execute(context.Background(), occupied); err != nil {
		t.Fatal(err)
	}
	collision, collisionProof := authorizationTestDecision(t, f, other, key, occupied.AuthorizationID, deviceauthorization.Approve)
	if _, err = authorizationTestDecide(t, f.decide).Execute(context.Background(), other, collision); !errors.Is(err, personalErrors.ErrAuthObjectConflict) {
		t.Fatal("occupied UUID was adopted or overwritten", err)
	}
	if proofTestGet(t, f.proofStart, other, collisionProof).ConsumedAt != nil || authorizationTestAttempt(t, f, occupied.Binding).State != attempt.Verified {
		t.Fatal("UUID collision consumed authority")
	}
	var existing []byte
	if err = f.owner.Raw("SELECT public_key FROM mancuerna.devices WHERE id=?", f.actor.DeviceID).Row().Scan(&existing); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(existing, f.key.Public().(ed25519.PublicKey)) {
		t.Fatal("existing device key changed")
	}
	self, selfProof := authorizationTestDecision(t, f, f.actor, f.key, occupied.AuthorizationID, deviceauthorization.Approve)
	if _, err = authorizationTestDecide(t, f.decide).Execute(context.Background(), f.actor, self); !errors.Is(err, personalErrors.ErrInvalidDeviceAuthorization) || proofTestGet(t, f.proofStart, f.actor, selfProof).ConsumedAt != nil {
		t.Fatal("candidate approved itself")
	}
}

func TestDeviceAuthorizationPostgresPhoneAndActorBindingsFailClosed(t *testing.T) {
	f := authorizationTestFixture(t)
	in, _ := authorizationTestCandidate(t, f, nil)
	if err := f.owner.Exec("UPDATE mancuerna.accounts SET phone_e164=? WHERE id=?", accessTestPhone(), f.actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), in); !errors.Is(err, personalErrors.ErrDeviceAuthorizationState) {
		t.Fatal("old phone started authorization", err)
	}
	if err := f.owner.Exec("UPDATE mancuerna.accounts SET phone_e164=? WHERE id=?", f.phone, f.actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	decision, proofID := authorizationTestDecision(t, f, f.actor, f.key, in.AuthorizationID, deviceauthorization.Approve)
	if err := f.owner.Exec("UPDATE mancuerna.accounts SET phone_e164=? WHERE id=?", accessTestPhone(), f.actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := authorizationTestDecide(t, f.decide).Execute(context.Background(), f.actor, decision); !errors.Is(err, personalErrors.ErrDeviceAuthorizationState) {
		t.Fatal("old phone authorized candidate", err)
	}
	if proofTestGet(t, f.proofStart, f.actor, proofID).ConsumedAt != nil || authorizationTestAttempt(t, f, in.Binding).State != attempt.Verified {
		t.Fatal("phone mismatch consumed authority")
	}
	if err := f.owner.Exec("UPDATE mancuerna.accounts SET phone_e164=? WHERE id=?", f.phone, f.actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	other := authorizationTestFixture(t)
	if _, err := authorizationTestDecide(t, f.decide).Execute(context.Background(), other.actor, decision); !errors.Is(err, personalErrors.ErrDeviceAuthorizationNotFound) {
		t.Fatal("account B could inspect authorization A", err)
	}
	if _, err := authorizationTestDecide(t, f.decide).Execute(shared.WithPersonalAttempt(context.Background(), in.Binding.ID, in.Binding.ContinuationHash), f.actor, decision); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("decision accepted mixed attempt scope")
	}
	if _, err := authorizationTestDecide(t, f.decide).Execute(context.Background(), f.actor, decision); err != nil {
		t.Fatal(err)
	}
	view, err := authorizationTestStatus(t, f.status).Execute(context.Background(), in.Binding)
	if err != nil || view.Authorized == nil {
		t.Fatal(err)
	}
	if err = f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, f.actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = authorizationTestDecide(t, f.decide).Execute(context.Background(), f.actor, decision); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("revoked approver replayed acknowledgement")
	}
	if replay, err := authorizationTestStatus(t, f.status).Execute(context.Background(), in.Binding); err != nil || !reflect.DeepEqual(replay.Authorized, view.Authorized) {
		t.Fatal("historical approval was lost after approver logout", err)
	}
	a := authorizationTestAttempt(t, f, in.Binding)
	if err = f.owner.Exec("UPDATE mancuerna.devices SET public_key=? WHERE id=?", bytes.Repeat([]byte{0x77}, 32), a.Candidate.ID).Error; err != nil {
		t.Fatal(err)
	}
	if invalid, err := authorizationTestStatus(t, f.status).Execute(context.Background(), in.Binding); !errors.Is(err, personalErrors.ErrInvalidReceipt) || invalid.Authorized != nil {
		t.Fatal("snapshot released credentials for changed candidate key")
	}
}

func TestDeviceAuthorizationPostgresListCursorOwnerEpochAndTerminalRequests(t *testing.T) {
	f := authorizationTestFixture(t)
	var requests []personalApp.StartDeviceAuthorizationInput
	base := time.Now().UTC().Add(-time.Second).Truncate(time.Second)
	stamps := []time.Time{base.Add(123456 * time.Microsecond), base.Add(123455 * time.Microsecond), base.Add(123455 * time.Microsecond), base.Add(123001 * time.Microsecond)}
	for _, stamp := range stamps {
		in, _ := authorizationTestCandidate(t, f, nil)
		requests = append(requests, in)
		if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		if err := f.owner.Exec("UPDATE mancuerna.device_authorizations SET created_at=?,expires_at=? WHERE id=?", stamp, base.Add(7*time.Minute), in.AuthorizationID).Error; err != nil {
			t.Fatal(err)
		}
	}
	expected := append([]personalApp.StartDeviceAuthorizationInput(nil), requests...)
	sort.Slice(expected, func(i, j int) bool {
		var a, b time.Time
		for n, r := range requests {
			if r.AuthorizationID == expected[i].AuthorizationID {
				a = stamps[n]
			}
			if r.AuthorizationID == expected[j].AuthorizationID {
				b = stamps[n]
			}
		}
		if a.Equal(b) {
			return expected[i].AuthorizationID.String() > expected[j].AuthorizationID.String()
		}
		return a.After(b)
	})
	u := authorizationTestList(t, f)
	var cursor, firstCursor string
	var ids []string
	for {
		page, err := u.Execute(context.Background(), f.actor, personalApp.ListDeviceAuthorizationsInput{Cursor: cursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 {
			t.Fatal("keyset skipped a request")
		}
		ids = append(ids, page.Items[0].ID)
		wire, _ := json.Marshal(page)
		if strings.Contains(string(wire), f.phone) || strings.Contains(string(wire), "continuation") {
			t.Fatal("list exposed prelogin credentials")
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
		if firstCursor == "" {
			firstCursor = cursor
		}
		if len(ids) > len(requests) {
			t.Fatal("keyset repeated requests")
		}
	}
	for i, r := range expected {
		if ids[i] != r.AuthorizationID.String() {
			t.Fatal("keyset lost timestamp precision or stable UUID order")
		}
	}
	other := authorizationTestFixture(t)
	if _, err := u.Execute(context.Background(), other.actor, personalApp.ListDeviceAuthorizationsInput{Cursor: firstCursor}); !errors.Is(err, protocol.ErrCursorInvalid) {
		t.Fatal("cursor crossed account")
	}
	if page, err := u.Execute(context.Background(), other.actor, personalApp.ListDeviceAuthorizationsInput{}); err != nil || len(page.Items) != 0 {
		t.Fatal("account B saw authorization A")
	}
	if _, err := u.Execute(context.Background(), f.actor, personalApp.ListDeviceAuthorizationsInput{Cursor: firstCursor + "x"}); !errors.Is(err, protocol.ErrCursorInvalid) {
		t.Fatal("tampered cursor accepted")
	}
	if err := f.owner.Exec("UPDATE mancuerna.sync_streams SET epoch=? WHERE account_id=?", uuid.New(), f.actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := u.Execute(context.Background(), f.actor, personalApp.ListDeviceAuthorizationsInput{Cursor: firstCursor}); !errors.Is(err, protocol.ErrCursorEpochChanged) {
		t.Fatal("cursor survived epoch change")
	}
	if err := f.owner.Exec("UPDATE mancuerna.device_authorizations SET status='cancelled',decided_at=clock_timestamp() WHERE id=?", requests[0].AuthorizationID).Error; err != nil {
		t.Fatal(err)
	}
	if err := f.owner.Exec("UPDATE mancuerna.device_authorizations SET expires_at=clock_timestamp()-interval '1 millisecond' WHERE id=?", requests[1].AuthorizationID).Error; err != nil {
		t.Fatal(err)
	}
	if page, err := u.Execute(context.Background(), f.actor, personalApp.ListDeviceAuthorizationsInput{}); err != nil || len(page.Items) != 2 {
		t.Fatal("list included terminal or expired request", err)
	}
	if _, err := u.Execute(shared.WithPersonalAttempt(context.Background(), requests[2].Binding.ID, requests[2].Binding.ContinuationHash), f.actor, personalApp.ListDeviceAuthorizationsInput{}); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("list accepted mixed scope")
	}
	if err := f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, f.actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Execute(context.Background(), f.actor, personalApp.ListDeviceAuthorizationsInput{}); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("revoked session listed requests")
	}
}

func TestDeviceAuthorizationPostgresCandidateAuditRequiresExactScopeAndReceipt(t *testing.T) {
	f := authorizationTestFixture(t)
	in, _ := authorizationTestCandidate(t, f, nil)
	if _, err := authorizationTestStart(t, f.start).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	q, err := f.d.UoW.Query(shared.WithPersonalAccount(context.Background(), f.actor.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.d.Receipts.Get(q, receipt.Owner{AccountID: f.actor.AccountID, AttemptID: in.Binding.ID}, in.OperationID)
	shared.CloseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	bound := shared.WithPersonalAccount(shared.WithPersonalAttempt(context.Background(), in.Binding.ID, in.Binding.ContinuationHash), f.actor.AccountID)
	event := repository.SecurityEvent{ID: uuid.New(), AccountID: f.actor.AccountID, OperationID: in.OperationID, Type: "device_authorization_requested", OccurredAt: r.CompletedAt}
	checkRejected := func(ctx context.Context, e repository.SecurityEvent) {
		t.Helper()
		err := f.d.UoW.Command(ctx, func(tx shared.Transaction) error { return f.d.Events.Append(tx, e) })
		var classified shared.CustomError
		if !errors.As(err, &classified) || classified.ErrorCode != shared.CodeValidation {
			t.Fatal("candidate audit accepted wrong authority", err)
		}
	}
	checkRejected(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), event)
	wrong := event
	wrong.OperationID = uuid.New()
	checkRejected(bound, wrong)
	wrong = event
	wrong.OccurredAt = wrong.OccurredAt.Add(time.Millisecond)
	checkRejected(bound, wrong)
	for _, kind := range []string{"signup", "device_authorization_approved", "device_authorization_denied", "vault_created"} {
		wrong = event
		wrong.Type = kind
		checkRejected(bound, wrong)
	}
	badHash := in.Binding.ContinuationHash
	badHash[0] ^= 1
	checkRejected(shared.WithPersonalAccount(shared.WithPersonalAttempt(context.Background(), in.Binding.ID, badHash), f.actor.AccountID), event)
	if err := f.owner.Exec("UPDATE mancuerna.accounts SET phone_e164=? WHERE id=?", accessTestPhone(), f.actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	checkRejected(bound, event)
	if err := f.owner.Exec("UPDATE mancuerna.accounts SET phone_e164=? WHERE id=?", f.phone, f.actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	decision, _ := authorizationTestDecision(t, f, f.actor, f.key, in.AuthorizationID, deviceauthorization.Approve)
	if _, err := authorizationTestDecide(t, f.decide).Execute(context.Background(), f.actor, decision); err != nil {
		t.Fatal(err)
	}
	checkRejected(bound, event)
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.security_events WHERE request_id=?", in.OperationID) != 1 {
		t.Fatal("failed audit guard wrote extra event")
	}
}
