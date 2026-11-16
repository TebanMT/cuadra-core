//go:build server && integration

package repositories

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deviceproof"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

func proofTestFixture(t *testing.T) (personalApp.StartDeviceProofDependencies, personalApp.CompleteDeviceProofDependencies, *gorm.DB, security.AccessClaims, ed25519.PrivateKey) {
	t.Helper()
	_, owner, tokens, key := refreshTestFixture(t)
	d := finalizationDependencies(t)
	start := personalApp.StartDeviceProofDependencies{UoW: d.UoW, Sessions: d.Sessions, Proofs: NewDeviceProofRepository(), Receipts: d.Receipts, Events: d.Events, Now: time.Now}
	complete := personalApp.CompleteDeviceProofDependencies{UoW: d.UoW, Sessions: d.Sessions, Proofs: start.Proofs, Receipts: d.Receipts, Events: d.Events, Verifier: d.Proofs, Envelopes: d.Envelopes, Now: time.Now}
	actor := security.AccessClaims{AccountID: uuid.MustParse(tokens.AccountID), SessionID: uuid.MustParse(tokens.SessionID), DeviceID: uuid.MustParse(tokens.DeviceID)}
	return start, complete, owner, actor, key
}
func proofTestStart(t *testing.T, d personalApp.StartDeviceProofDependencies) *personalApp.StartDeviceProof {
	t.Helper()
	u, err := personalApp.NewStartDeviceProof(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func proofTestComplete(t *testing.T, d personalApp.CompleteDeviceProofDependencies) *personalApp.CompleteDeviceProof {
	t.Helper()
	u, err := personalApp.NewCompleteDeviceProof(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func proofTestInput() personalApp.StartDeviceProofInput {
	return personalApp.StartDeviceProofInput{OperationID: uuid.New(), RequestHash: [32]byte{10}, ProofID: uuid.New(), Purpose: security.RecoveryKey, TargetRequestHash: [32]byte{11}}
}
func proofTestCompleteInput(t *testing.T, p personalApp.DeviceProofResult, actor security.AccessClaims, key ed25519.PrivateKey) personalApp.CompleteDeviceProofInput {
	t.Helper()
	nonce, err := base64.RawURLEncoding.DecodeString(p.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hex.DecodeString(p.RequestHash)
	if err != nil {
		t.Fatal(err)
	}
	expiry, err := time.Parse(time.RFC3339Nano, p.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := crypto.DeviceProofPayload(security.DeviceProof{Purpose: security.ProofPurpose(p.Purpose), AccountID: actor.AccountID, SessionID: actor.SessionID, DeviceID: actor.DeviceID, ProofID: uuid.MustParse(p.ID), Nonce: nonce, RequestHash: [32]byte(hash), ExpiresAt: expiry})
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
	return personalApp.CompleteDeviceProofInput{OperationID: uuid.New(), RequestHash: sha256.Sum256([]byte(signature)), ProofID: uuid.MustParse(p.ID), Signature: signature}
}
func proofTestGet(t *testing.T, d personalApp.StartDeviceProofDependencies, actor security.AccessClaims, id uuid.UUID) *deviceproof.Proof {
	t.Helper()
	var p *deviceproof.Proof
	if err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		var err error
		p, err = d.Proofs.GetForUpdate(tx, actor, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDeviceProofPostgresConcurrentStartCompleteAndNoImplicitConsumption(t *testing.T) {
	d, c, owner, actor, key := proofTestFixture(t)
	start := proofTestStart(t, d)
	in := proofTestInput()
	const count = 5
	results := make([]personalApp.DeviceProofResult, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			results[i], errs[i] = start.Execute(context.Background(), actor, in)
		}(i)
	}
	close(gate)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || !reflect.DeepEqual(results[0], results[i]) {
			t.Fatalf("start replay diverged: %v", errs[i])
		}
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.device_proofs WHERE id=?", in.ProofID) != 1 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE request_id=?", in.OperationID) != 1 {
		t.Fatal("duplicate start effects")
	}
	for _, change := range []func(*personalApp.StartDeviceProofInput){func(in *personalApp.StartDeviceProofInput) { in.Purpose = security.PhoneChange }, func(in *personalApp.StartDeviceProofInput) { in.TargetRequestHash[0]++ }, func(in *personalApp.StartDeviceProofInput) { in.OperationID = uuid.New() }} {
		other := in
		change(&other)
		if _, err := start.Execute(context.Background(), actor, other); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
			t.Fatalf("immutable proof binding overwritten: %v", err)
		}
	}
	for _, change := range []func(*deviceproof.Proof){func(p *deviceproof.Proof) { p.Nonce[0]++ }, func(p *deviceproof.Proof) { p.TargetRequestHash[0]++ }, func(p *deviceproof.Proof) { p.Purpose = security.PhoneChange }, func(p *deviceproof.Proof) { p.ExpiresAt = p.ExpiresAt.Add(time.Millisecond) }} {
		mutated := proofTestGet(t, d, actor, in.ProofID)
		if err := mutated.Verify([32]byte{33}, time.Now()); err != nil {
			t.Fatal(err)
		}
		change(mutated)
		if err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error { return d.Proofs.SaveVerified(tx, actor, mutated) }); !errors.Is(err, personalErrors.ErrInvalidProof) {
			t.Fatalf("repository changed immutable proof binding: %v", err)
		}
	}
	if proofTestGet(t, d, actor, in.ProofID).TokenHash != nil {
		t.Fatal("failed immutable update verified proof")
	}
	request := proofTestCompleteInput(t, results[0], actor, key)
	complete := proofTestComplete(t, c)
	tokens := make([]personalApp.ProofTokenResult, count)
	gate = make(chan struct{})
	for i := range tokens {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			tokens[i], errs[i] = complete.Execute(context.Background(), actor, request)
		}(i)
	}
	close(gate)
	wg.Wait()
	for i := range tokens {
		if errs[i] != nil || !reflect.DeepEqual(tokens[0], tokens[i]) || tokens[i].ExpiresAt != results[0].ExpiresAt {
			t.Fatalf("complete replay diverged or extended expiry: %v", errs[i])
		}
	}
	p := proofTestGet(t, d, actor, in.ProofID)
	hash := sha256.Sum256([]byte(tokens[0].ProofToken))
	if p.TokenHash == nil || *p.TokenHash != hash || p.VerifiedAt == nil || p.OperationID != nil || p.ConsumedAt != nil {
		t.Fatal("complete stored plaintext or consumed proof")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE request_id=?", request.OperationID) != 1 {
		t.Fatal("duplicate completion effects")
	}
	changed := request
	changed.OperationID = uuid.New()
	if _, err := complete.Execute(context.Background(), actor, changed); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("different operation replaced verified token")
	}
	changed = request
	changed.RequestHash[0]++
	if _, err := complete.Execute(context.Background(), actor, changed); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("changed complete hash replayed token")
	}
	if !reflect.DeepEqual(proofTestGet(t, d, actor, in.ProofID), p) {
		t.Fatal("failed completion changed proof")
	}
}

func TestDeviceProofPostgresRollbackAndSingleConsumptionWithEffectReceipt(t *testing.T) {
	d, c, owner, actor, key := proofTestFixture(t)
	in := proofTestInput()
	broken := d
	broken.Events = failingSecurityEvent{}
	if _, err := proofTestStart(t, broken).Execute(context.Background(), actor, in); err == nil {
		t.Fatal("start audit failure ignored")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.device_proofs WHERE id=?", in.ProofID) != 0 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID) != 0 {
		t.Fatal("start failure retained state")
	}
	challenge, err := proofTestStart(t, d).Execute(context.Background(), actor, in)
	if err != nil {
		t.Fatal(err)
	}
	completeIn := proofTestCompleteInput(t, challenge, actor, key)
	badComplete := c
	badComplete.Events = failingSecurityEvent{}
	if _, err = proofTestComplete(t, badComplete).Execute(context.Background(), actor, completeIn); err == nil {
		t.Fatal("complete audit failure ignored")
	}
	p := proofTestGet(t, d, actor, in.ProofID)
	if p.TokenHash != nil || p.VerifiedAt != nil || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", completeIn.OperationID) != 0 {
		t.Fatal("complete failure retained token")
	}
	result, err := proofTestComplete(t, c).Execute(context.Background(), actor, completeIn)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(result.ProofToken))
	effectID := uuid.New()
	failure := errors.New("synthetic sensitive effect failure")
	effect := func(fail bool) error {
		return d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
			active, err := d.Sessions.SessionActive(tx, actor.AccountID, actor.SessionID, actor.DeviceID)
			if err != nil {
				return err
			}
			if !active {
				return personalErrors.ErrSessionRevoked
			}
			owner := receipt.Owner{AccountID: actor.AccountID}
			r, err := d.Receipts.Get(tx, owner, effectID)
			if err == nil {
				return r.Match("synthetic_sensitive_effect", in.TargetRequestHash)
			}
			if !errors.Is(err, personalErrors.ErrReceiptNotFound) {
				return err
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			if err = d.Proofs.Consume(tx, actor, hash, in.Purpose, in.TargetRequestHash, effectID, now); err != nil {
				return err
			}
			if err = d.Events.Append(tx, repository.SecurityEvent{ID: uuid.New(), AccountID: actor.AccountID, ActorDeviceID: actor.DeviceID, OperationID: effectID, Type: "synthetic_sensitive_effect", OccurredAt: now}); err != nil {
				return err
			}
			if fail {
				return failure
			}
			r, err = receipt.New(effectID, owner, "synthetic_sensitive_effect", in.TargetRequestHash, now)
			if err != nil {
				return err
			}
			r.ResultReference = in.ProofID
			return d.Receipts.Create(tx, r)
		})
	}
	if err = effect(true); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if p = proofTestGet(t, d, actor, in.ProofID); p.ConsumedAt != nil || p.OperationID != nil || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE request_id=?", effectID) != 0 {
		t.Fatal("failed effect consumed proof")
	}
	if err = effect(false); err != nil {
		t.Fatal(err)
	}
	if err = effect(false); err != nil {
		t.Fatal(err)
	}
	p = proofTestGet(t, d, actor, in.ProofID)
	if p.ConsumedAt == nil || p.OperationID == nil || *p.OperationID != effectID || *p.OperationID == completeIn.OperationID || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE request_id=?", effectID) != 1 {
		t.Fatal("effect replayed or operation bound to completion")
	}
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		return d.Proofs.Consume(tx, actor, hash, in.Purpose, in.TargetRequestHash, effectID, time.Now())
	}); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("consumed proof reused without receipt")
	}
	replay, err := proofTestComplete(t, c).Execute(context.Background(), actor, completeIn)
	if err != nil || replay != result {
		t.Fatal("identical completion lost its original response")
	}
	if !reflect.DeepEqual(proofTestGet(t, d, actor, in.ProofID), p) {
		t.Fatal("completion replay resurrected consumed proof")
	}
}

func TestDeviceProofPostgresActorIsolationAndCompositeSessionDeviceFK(t *testing.T) {
	d, c, _, actor, key := proofTestFixture(t)
	_, _, _, foreign, _ := proofTestFixture(t)
	in := proofTestInput()
	start := proofTestStart(t, d)
	challenge, err := start.Execute(context.Background(), actor, in)
	if err != nil {
		t.Fatal(err)
	}
	completeIn := proofTestCompleteInput(t, challenge, actor, key)
	result, err := proofTestComplete(t, c).Execute(context.Background(), actor, completeIn)
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
	actors := []security.AccessClaims{foreign, {AccountID: actor.AccountID, SessionID: sameDevice.ID, DeviceID: actor.DeviceID}, {AccountID: actor.AccountID, SessionID: otherSession.ID, DeviceID: otherDevice.ID}}
	hash := sha256.Sum256([]byte(result.ProofToken))
	for _, wrong := range actors {
		if _, err = start.Execute(context.Background(), wrong, in); err == nil {
			t.Fatal("different actor replayed challenge")
		}
		if _, err = proofTestComplete(t, c).Execute(context.Background(), wrong, completeIn); !errors.Is(err, personalErrors.ErrInvalidProof) {
			t.Fatalf("different actor recovered token: %v", err)
		}
		if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), wrong.AccountID), func(tx shared.Transaction) error {
			return d.Proofs.Consume(tx, wrong, hash, in.Purpose, in.TargetRequestHash, uuid.New(), time.Now())
		}); !errors.Is(err, personalErrors.ErrInvalidProof) {
			t.Fatal("different actor consumed proof")
		}
	}
	ambient := shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{9})
	if _, err = start.Execute(ambient, actor, in); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("start inherited attempt")
	}
	if _, err = proofTestComplete(t, c).Execute(ambient, actor, completeIn); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("complete inherited attempt")
	}
	if err = d.UoW.Command(shared.WithPersonalAccount(ambient, actor.AccountID), func(tx shared.Transaction) error {
		return d.Proofs.Consume(tx, actor, hash, in.Purpose, in.TargetRequestHash, uuid.New(), time.Now())
	}); !errors.Is(err, personalErrors.ErrAuthSessionScope) {
		t.Fatal("consume inherited attempt capability")
	}
	p := proofTestGet(t, d, actor, in.ProofID)
	m := deviceProofToModel(p)
	m.ID = uuid.New()
	m.DeviceID = otherDevice.ID
	m.TokenHash = nil
	m.VerifiedAt = nil
	err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error { return tx.(*shared.GormTransaction).Tx.Create(&m).Error })
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23503" {
		t.Fatalf("session/device triple FK did not reject mismatch: %v", err)
	}
}

func TestDeviceProofPostgresOriginalExpiryWrongTargetAndRevocation(t *testing.T) {
	d, c, owner, actor, key := proofTestFixture(t)
	d.Now = func() time.Time { return time.Now().Add(-4 * time.Minute) }
	in := proofTestInput()
	challenge, err := proofTestStart(t, d).Execute(context.Background(), actor, in)
	if err != nil {
		t.Fatal(err)
	}
	completeIn := proofTestCompleteInput(t, challenge, actor, key)
	invalid := completeIn
	invalid.Signature = base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	if _, err = proofTestComplete(t, c).Execute(context.Background(), actor, invalid); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("invalid device signature approved")
	}
	result, err := proofTestComplete(t, c).Execute(context.Background(), actor, completeIn)
	if err != nil || result.ExpiresAt != challenge.ExpiresAt {
		t.Fatalf("completion extended original expiry: %v", err)
	}
	hash := sha256.Sum256([]byte(result.ProofToken))
	for _, bad := range []struct {
		purpose security.ProofPurpose
		target  [32]byte
		token   [32]byte
	}{{security.PhoneChange, in.TargetRequestHash, hash}, {in.Purpose, [32]byte{99}, hash}, {in.Purpose, in.TargetRequestHash, [32]byte{99}}} {
		if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
			return d.Proofs.Consume(tx, actor, bad.token, bad.purpose, bad.target, uuid.New(), time.Now())
		}); !errors.Is(err, personalErrors.ErrInvalidProof) {
			t.Fatal("wrong proof purpose/hash/token consumed")
		}
	}
	expires, err := time.Parse(time.RFC3339Nano, challenge.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	expired := c
	expired.Now = func() time.Time { return expires }
	if _, err = proofTestComplete(t, expired).Execute(context.Background(), actor, completeIn); !errors.Is(err, personalErrors.ErrResultExpired) {
		t.Fatal("complete replay survived original expiry")
	}
	expiredStart := d
	expiredStart.Now = expired.Now
	if _, err = proofTestStart(t, expiredStart).Execute(context.Background(), actor, in); !errors.Is(err, personalErrors.ErrResultExpired) {
		t.Fatal("start replay extended proof")
	}
	if err = owner.Exec("UPDATE mancuerna.device_proofs SET verified_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE id=?", in.ProofID).Error; err != nil {
		t.Fatal(err)
	}
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		return d.Proofs.Consume(tx, actor, hash, in.Purpose, in.TargetRequestHash, uuid.New(), time.Now().Add(-1500*time.Millisecond))
	}); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("past supplied clock evaded database expiry")
	}
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		return d.Sessions.RevokeSession(tx, actor.AccountID, actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = proofTestComplete(t, c).Execute(context.Background(), actor, completeIn); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("revoked actor recovered proof token")
	}
	if _, err = proofTestStart(t, d).Execute(context.Background(), actor, in); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("revoked actor replayed start")
	}
}
