//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/vault"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func vaultTestDependencies(start personalApp.StartDeviceProofDependencies, complete personalApp.CompleteDeviceProofDependencies) (personalApp.CreatePersonalVaultDependencies, personalApp.UnlockPersonalVaultDependencies) {
	create := personalApp.CreatePersonalVaultDependencies{UoW: start.UoW, Sessions: start.Sessions, Vaults: NewVaultRepository(), Receipts: start.Receipts, Events: start.Events, Envelopes: complete.Envelopes, Now: time.Now}
	unlock := personalApp.UnlockPersonalVaultDependencies{UoW: start.UoW, Sessions: start.Sessions, Vaults: create.Vaults, Proofs: start.Proofs, Receipts: start.Receipts, Events: start.Events, Envelopes: complete.Envelopes, Now: time.Now}
	return create, unlock
}
func vaultTestCreate(t *testing.T, d personalApp.CreatePersonalVaultDependencies) *personalApp.CreatePersonalVault {
	t.Helper()
	u, err := personalApp.NewCreatePersonalVault(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func vaultTestUnlock(t *testing.T, d personalApp.UnlockPersonalVaultDependencies) *personalApp.UnlockPersonalVault {
	t.Helper()
	u, err := personalApp.NewUnlockPersonalVault(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func vaultTestCreateInput(id uuid.UUID) personalApp.CreatePersonalVaultInput {
	op := uuid.New()
	return personalApp.CreatePersonalVaultInput{OperationID: op, VaultID: id, RequestHash: sha256.Sum256([]byte("POST /me/vaults " + id.String() + " " + op.String()))}
}
func vaultTestUnlockInput(t *testing.T, start personalApp.StartDeviceProofDependencies, complete personalApp.CompleteDeviceProofDependencies, actor security.AccessClaims, private ed25519.PrivateKey, id uuid.UUID, purpose security.ProofPurpose) (personalApp.UnlockPersonalVaultInput, uuid.UUID) {
	t.Helper()
	in := personalApp.UnlockPersonalVaultInput{OperationID: uuid.New(), VaultID: id}
	in.RequestHash = sha256.Sum256([]byte("POST /me/vaults/" + id.String() + "/unlock " + in.OperationID.String()))
	proof := proofTestInput()
	proof.Purpose, proof.TargetRequestHash = purpose, in.RequestHash
	challenge, err := proofTestStart(t, start).Execute(context.Background(), actor, proof)
	if err != nil {
		t.Fatal(err)
	}
	token, err := proofTestComplete(t, complete).Execute(context.Background(), actor, proofTestCompleteInput(t, challenge, actor, private))
	if err != nil {
		t.Fatal(err)
	}
	in.ProofToken = token.ProofToken
	return in, proof.ProofID
}
func vaultTestGet(t *testing.T, d personalApp.CreatePersonalVaultDependencies, accountID, id uuid.UUID) *vault.Vault {
	t.Helper()
	q, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), accountID))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	v, err := d.Vaults.Get(q, accountID, id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestVaultPostgresConcurrentCreationNeverOverwritesOrStoresPlaintext(t *testing.T) {
	start, complete, owner, actor, _ := proofTestFixture(t)
	d, _ := vaultTestDependencies(start, complete)
	in := vaultTestCreateInput(uuid.New())
	u := vaultTestCreate(t, d)
	const count = 5
	results := make([]personalApp.VaultResult, count)
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
		if errs[i] != nil || results[i] != results[0] || results[i].ID != in.VaultID.String() {
			t.Fatalf("vault creation diverged: %v", errs[i])
		}
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(results[0].DataKey)
	if err != nil || len(key) != 32 || len(results[0].DataKey) != 43 {
		t.Fatal("vault key is not 256 bits")
	}
	v := vaultTestGet(t, d, actor.AccountID, in.VaultID)
	opened, err := d.Envelopes.UnwrapVault(v.Context(), v.Envelope)
	if err != nil || !bytes.Equal(opened, key) || bytes.Contains(v.Envelope.Ciphertext, key) || bytes.Contains(v.Envelope.Ciphertext, []byte(results[0].DataKey)) {
		t.Fatal("permanent envelope is missing, invalid or plaintext")
	}
	var saved struct {
		Ciphertext []byte
		Version    string
		Seconds    int
	}
	if err = owner.Raw("SELECT response_ciphertext AS ciphertext,key_version AS version,extract(epoch from response_expires_at-completed_at)::integer AS seconds FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID).Scan(&saved).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Seconds != 600 || saved.Version != "test" || bytes.Equal(saved.Ciphertext, v.Envelope.Ciphertext) || bytes.Contains(saved.Ciphertext, key) || bytes.Contains(saved.Ciphertext, []byte(results[0].DataKey)) {
		t.Fatal("response envelope or expiration not separated from permanent key")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.vault_keys WHERE vault_id=?", in.VaultID) != 1 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE request_id=?", in.OperationID) != 1 {
		t.Fatal("creation retry duplicated effects")
	}
	if result, err := u.Execute(context.Background(), actor, vaultTestCreateInput(in.VaultID)); !errors.Is(err, personalErrors.ErrVaultConflict) || result != (personalApp.VaultResult{}) {
		t.Fatal("new operation replaced or retrieved existing vault")
	}
	changed := in
	changed.RequestHash[0] ^= 1
	if _, err := u.Execute(context.Background(), actor, changed); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("creation receipt accepted different request")
	}
	otherID, otherDevice, otherSession := authSessionTestCreate(t, owner, d.UoW, NewAuthSessionRepository())
	other := security.AccessClaims{AccountID: otherID, SessionID: otherSession.ID, DeviceID: otherDevice.ID}
	if result, err := u.Execute(context.Background(), other, vaultTestCreateInput(in.VaultID)); !errors.Is(err, personalErrors.ErrVaultConflict) || result != (personalApp.VaultResult{}) {
		t.Fatal("another account overwrote vault UUID")
	}
	q, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), otherID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Vaults.Get(q, otherID, in.VaultID); !errors.Is(err, personalErrors.ErrVaultNotFound) {
		t.Fatal("B read A vault")
	}
	if _, err = d.Vaults.Get(q, actor.AccountID, in.VaultID); !errors.Is(err, personalErrors.ErrAuthSessionScope) {
		t.Fatal("B supplied A as repository scope")
	}
	shared.CloseQuery(q)
	after := vaultTestGet(t, d, actor.AccountID, in.VaultID)
	if !bytes.Equal(v.Envelope.Ciphertext, after.Envelope.Ciphertext) {
		t.Fatal("collision replaced immutable key")
	}
}

func TestVaultPostgresLogoutNewAuthorizationRecoversSameKeyWithPreviousMaster(t *testing.T) {
	_, owner, initial, _ := refreshTestFixture(t)
	f := finalizationDependencies(t)
	actor := security.AccessClaims{AccountID: uuid.MustParse(initial.AccountID), SessionID: uuid.MustParse(initial.SessionID), DeviceID: uuid.MustParse(initial.DeviceID)}
	start := personalApp.StartDeviceProofDependencies{UoW: f.UoW, Sessions: f.Sessions, Proofs: NewDeviceProofRepository(), Receipts: f.Receipts, Events: f.Events, Now: time.Now}
	complete := personalApp.CompleteDeviceProofDependencies{UoW: f.UoW, Sessions: f.Sessions, Proofs: start.Proofs, Receipts: f.Receipts, Events: f.Events, Verifier: f.Proofs, Envelopes: f.Envelopes, Now: time.Now}
	d, unlockD := vaultTestDependencies(start, complete)
	oldKey, newKey := bytes.Repeat([]byte{31}, 32), bytes.Repeat([]byte{32}, 32)
	oldRing, err := crypto.NewEnvelopes("old", map[string][]byte{"old": oldKey})
	if err != nil {
		t.Fatal(err)
	}
	d.Envelopes = oldRing
	in := vaultTestCreateInput(uuid.New())
	created, err := vaultTestCreate(t, d).Execute(context.Background(), actor, in)
	if err != nil {
		t.Fatal(err)
	}
	original := vaultTestGet(t, d, actor.AccountID, in.VaultID)
	if _, err = logoutTestUseCase(t, logoutTestDependencies(t)).Execute(context.Background(), logoutTestInput(initial.RevocationTicket)); err != nil {
		t.Fatal(err)
	}
	if result, err := vaultTestCreate(t, d).Execute(context.Background(), actor, in); !errors.Is(err, personalErrors.ErrSessionRevoked) || result != (personalApp.VaultResult{}) {
		t.Fatal("logged-out actor recovered response")
	}
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// This fixture starts after a separate authorization/recovery has succeeded;
	// it exercises the vault boundary, not the still-independent authorization UX.
	newDevice, err := device.NewAuthorizedDevice(actor.AccountID, attempt.Candidate{ID: uuid.New(), PublicKey: [32]byte(pub), Platform: "android", Label: "Synthetic recovered installation"}, device.Recovery, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	newSession := authSessionTestSession(t, actor.AccountID, newDevice.ID)
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		if err := d.Sessions.CreateDevice(tx, actor.AccountID, newDevice); err != nil {
			return err
		}
		return d.Sessions.CreateSession(tx, actor.AccountID, newSession)
	}); err != nil {
		t.Fatal(err)
	}
	nextActor := security.AccessClaims{AccountID: actor.AccountID, SessionID: newSession.ID, DeviceID: newDevice.ID}
	ring, err := crypto.NewEnvelopes("new", map[string][]byte{"new": newKey, "old": oldKey})
	if err != nil {
		t.Fatal(err)
	}
	unlockD.Envelopes = ring
	unlockIn, proofID := vaultTestUnlockInput(t, start, complete, nextActor, private, in.VaultID, security.VaultUnlock)
	response, err := vaultTestUnlock(t, unlockD).Execute(context.Background(), nextActor, unlockIn)
	if err != nil || response != created || response.KeyVersion != "old" {
		t.Fatal("new authorization did not recover exact original key with previous master")
	}
	after := vaultTestGet(t, d, actor.AccountID, in.VaultID)
	if !bytes.Equal(after.Envelope.Ciphertext, original.Envelope.Ciphertext) || after.Envelope.KeyVersion != "old" || proofTestGet(t, start, nextActor, proofID).ConsumedAt == nil {
		t.Fatal("unlock replaced permanent key or omitted proof effect")
	}
	var receiptVersion string
	if err = owner.Raw("SELECT key_version FROM mancuerna.auth_receipts WHERE operation_id=?", unlockIn.OperationID).Row().Scan(&receiptVersion); err != nil || receiptVersion != "new" {
		t.Fatal("response did not use new active master independently")
	}
	startB, completeB, _, actorB, privateB := proofTestFixture(t)
	_, unlockB := vaultTestDependencies(startB, completeB)
	unlockB.Envelopes = ring
	foreign, foreignProof := vaultTestUnlockInput(t, startB, completeB, actorB, privateB, in.VaultID, security.VaultUnlock)
	if response, err = vaultTestUnlock(t, unlockB).Execute(context.Background(), actorB, foreign); !errors.Is(err, personalErrors.ErrVaultNotFound) || response != (personalApp.VaultResult{}) || proofTestGet(t, startB, actorB, foreignProof).ConsumedAt != nil {
		t.Fatal("account B recovered retained vault or consumed unrelated proof")
	}
	missingOld, err := crypto.NewEnvelopes("new", map[string][]byte{"new": newKey})
	if err != nil {
		t.Fatal(err)
	}
	unlockD.Envelopes = missingOld
	missing, missingProof := vaultTestUnlockInput(t, start, complete, nextActor, private, in.VaultID, security.VaultUnlock)
	if response, err = vaultTestUnlock(t, unlockD).Execute(context.Background(), nextActor, missing); !errors.Is(err, personalErrors.ErrInvalidEnvelope) || response != (personalApp.VaultResult{}) || proofTestGet(t, start, nextActor, missingProof).ConsumedAt != nil {
		t.Fatal("missing previous master guessed key or consumed proof")
	}
}

func TestVaultPostgresProofAndAuditRollbackAreAtomic(t *testing.T) {
	start, complete, owner, actor, private := proofTestFixture(t)
	d, unlockD := vaultTestDependencies(start, complete)
	in := vaultTestCreateInput(uuid.New())
	failingCreate := d
	failingCreate.Events = failingSecurityEvent{}
	if result, err := vaultTestCreate(t, failingCreate).Execute(context.Background(), actor, in); err == nil || result != (personalApp.VaultResult{}) {
		t.Fatal("failed create returned data key")
	}
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.vault_keys WHERE vault_id=?", in.VaultID) != 0 || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID) != 0 {
		t.Fatal("failed create partially committed")
	}
	created, err := vaultTestCreate(t, d).Execute(context.Background(), actor, in)
	if err != nil {
		t.Fatal(err)
	}
	wrong, wrongProof := vaultTestUnlockInput(t, start, complete, actor, private, in.VaultID, security.RecoveryKey)
	if _, err = vaultTestUnlock(t, unlockD).Execute(context.Background(), actor, wrong); !errors.Is(err, personalErrors.ErrInvalidProof) || proofTestGet(t, start, actor, wrongProof).ConsumedAt != nil {
		t.Fatal("wrong purpose authorized unlock")
	}
	unlockIn, proofID := vaultTestUnlockInput(t, start, complete, actor, private, in.VaultID, security.VaultUnlock)
	changed := unlockIn
	changed.RequestHash[0] ^= 1
	if _, err = vaultTestUnlock(t, unlockD).Execute(context.Background(), actor, changed); !errors.Is(err, personalErrors.ErrInvalidProof) || proofTestGet(t, start, actor, proofID).ConsumedAt != nil {
		t.Fatal("wrong target hash consumed proof")
	}
	failingUnlock := unlockD
	failingUnlock.Events = failingSecurityEvent{}
	if result, err := vaultTestUnlock(t, failingUnlock).Execute(context.Background(), actor, unlockIn); err == nil || result != (personalApp.VaultResult{}) {
		t.Fatal("failed audit returned data key")
	}
	if proofTestGet(t, start, actor, proofID).ConsumedAt != nil || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", unlockIn.OperationID) != 0 {
		t.Fatal("failed unlock committed proof or receipt")
	}
	const count = 4
	var wg sync.WaitGroup
	errs := make([]error, count)
	responses := make([]personalApp.VaultResult, count)
	u := vaultTestUnlock(t, unlockD)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); responses[i], errs[i] = u.Execute(context.Background(), actor, unlockIn) }(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || responses[i] != created {
			t.Fatalf("concurrent unlock did not replay original: %v", errs[i])
		}
	}
	proof := proofTestGet(t, start, actor, proofID)
	if proof.OperationID == nil || *proof.OperationID != unlockIn.OperationID || accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.security_events WHERE request_id=?", unlockIn.OperationID) != 1 {
		t.Fatal("unlock duplicated effect or did not bind consuming operation")
	}
	if _, err = u.Execute(context.Background(), actor, changed); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("unlock receipt accepted different hash")
	}
	reuse := unlockIn
	reuse.OperationID = uuid.New()
	if _, err = u.Execute(context.Background(), actor, reuse); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("consumed proof executed new effect")
	}
}

func TestVaultPostgresReplayRequiresExactActorOriginalWindowAndLiveState(t *testing.T) {
	start, complete, owner, actor, private := proofTestFixture(t)
	d, unlockD := vaultTestDependencies(start, complete)
	in := vaultTestCreateInput(uuid.New())
	created, err := vaultTestCreate(t, d).Execute(context.Background(), actor, in)
	if err != nil {
		t.Fatal(err)
	}
	unlockIn, _ := vaultTestUnlockInput(t, start, complete, actor, private, in.VaultID, security.VaultUnlock)
	if _, err = vaultTestUnlock(t, unlockD).Execute(context.Background(), actor, unlockIn); err != nil {
		t.Fatal(err)
	}
	secondSession := authSessionTestSession(t, actor.AccountID, actor.DeviceID)
	secondDevice := authSessionTestDevice(t, actor.AccountID)
	thirdSession := authSessionTestSession(t, actor.AccountID, secondDevice.ID)
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), actor.AccountID), func(tx shared.Transaction) error {
		if err := d.Sessions.CreateSession(tx, actor.AccountID, secondSession); err != nil {
			return err
		}
		if err := d.Sessions.CreateDevice(tx, actor.AccountID, secondDevice); err != nil {
			return err
		}
		return d.Sessions.CreateSession(tx, actor.AccountID, thirdSession)
	}); err != nil {
		t.Fatal(err)
	}
	for _, other := range []security.AccessClaims{{AccountID: actor.AccountID, DeviceID: actor.DeviceID, SessionID: secondSession.ID}, {AccountID: actor.AccountID, DeviceID: secondDevice.ID, SessionID: thirdSession.ID}} {
		if result, err := vaultTestCreate(t, d).Execute(context.Background(), other, in); !errors.Is(err, personalErrors.ErrInvalidProof) || result != (personalApp.VaultResult{}) {
			t.Fatal("another actor replayed create secret")
		}
		if result, err := vaultTestUnlock(t, unlockD).Execute(context.Background(), other, unlockIn); !errors.Is(err, personalErrors.ErrInvalidProof) || result != (personalApp.VaultResult{}) {
			t.Fatal("another actor replayed unlock secret")
		}
	}
	for _, op := range []uuid.UUID{in.OperationID, unlockIn.OperationID} {
		var completed time.Time
		if err = owner.Raw("SELECT completed_at FROM mancuerna.auth_receipts WHERE operation_id=?", op).Row().Scan(&completed); err != nil {
			t.Fatal(err)
		}
		laterCreate, laterUnlock := d, unlockD
		laterCreate.Now = func() time.Time { return completed.Add(6 * time.Minute) }
		laterUnlock.Now = laterCreate.Now
		var response personalApp.VaultResult
		if op == in.OperationID {
			response, err = vaultTestCreate(t, laterCreate).Execute(context.Background(), actor, in)
		} else {
			response, err = vaultTestUnlock(t, laterUnlock).Execute(context.Background(), actor, unlockIn)
		}
		if err != nil || response != created {
			t.Fatal("receipt retry after proof expiry lost original key")
		}
		laterCreate.Now = func() time.Time { return completed.Add(10 * time.Minute) }
		laterUnlock.Now = laterCreate.Now
		if op == in.OperationID {
			_, err = vaultTestCreate(t, laterCreate).Execute(context.Background(), actor, in)
		} else {
			_, err = vaultTestUnlock(t, laterUnlock).Execute(context.Background(), actor, unlockIn)
		}
		if !errors.Is(err, personalErrors.ErrResultExpired) {
			t.Fatal("vault response lived beyond original ten minutes")
		}
	}
	if err = owner.Exec("UPDATE mancuerna.vault_keys SET retired_at=clock_timestamp() WHERE vault_id=?", in.VaultID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = vaultTestCreate(t, d).Execute(context.Background(), actor, in); !errors.Is(err, personalErrors.ErrVaultState) {
		t.Fatal("retired vault create receipt returned key")
	}
	if _, err = vaultTestUnlock(t, unlockD).Execute(context.Background(), actor, unlockIn); !errors.Is(err, personalErrors.ErrVaultState) {
		t.Fatal("retired vault unlock receipt returned key")
	}
	if err = owner.Exec("UPDATE mancuerna.accounts SET deleted_at=clock_timestamp() WHERE id=?", actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err = vaultTestUnlock(t, unlockD).Execute(context.Background(), actor, unlockIn); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("deleted account read cached key")
	}
}

func TestVaultPostgresEnvelopeContextAndMixedScopeFailClosed(t *testing.T) {
	start, complete, owner, actor, private := proofTestFixture(t)
	d, unlockD := vaultTestDependencies(start, complete)
	first, second := vaultTestCreateInput(uuid.New()), vaultTestCreateInput(uuid.New())
	for _, in := range []personalApp.CreatePersonalVaultInput{first, second} {
		if _, err := vaultTestCreate(t, d).Execute(context.Background(), actor, in); err != nil {
			t.Fatal(err)
		}
	}
	v := vaultTestGet(t, d, actor.AccountID, first.VaultID)
	if err := owner.Exec("UPDATE mancuerna.vault_keys SET wrapped_data_key=? WHERE vault_id=?", v.Envelope.Ciphertext, second.VaultID).Error; err != nil {
		t.Fatal(err)
	}
	unlockIn, proofID := vaultTestUnlockInput(t, start, complete, actor, private, second.VaultID, security.VaultUnlock)
	if response, err := vaultTestUnlock(t, unlockD).Execute(context.Background(), actor, unlockIn); !errors.Is(err, personalErrors.ErrInvalidEnvelope) || response != (personalApp.VaultResult{}) || proofTestGet(t, start, actor, proofID).ConsumedAt != nil {
		t.Fatal("swapped vault ciphertext bypassed AAD or consumed proof")
	}
	otherID, otherDevice, otherSession := authSessionTestCreate(t, owner, d.UoW, NewAuthSessionRepository())
	other := security.AccessClaims{AccountID: otherID, SessionID: otherSession.ID, DeviceID: otherDevice.ID}
	foreign := vaultTestCreateInput(uuid.New())
	if _, err := vaultTestCreate(t, d).Execute(context.Background(), other, foreign); err != nil {
		t.Fatal(err)
	}
	if err := owner.Exec("UPDATE mancuerna.vault_keys SET wrapped_data_key=? WHERE vault_id=?", v.Envelope.Ciphertext, foreign.VaultID).Error; err != nil {
		t.Fatal(err)
	}
	foreignV := vaultTestGet(t, d, otherID, foreign.VaultID)
	if _, err := d.Envelopes.UnwrapVault(foreignV.Context(), foreignV.Envelope); !errors.Is(err, personalErrors.ErrInvalidEnvelope) {
		t.Fatal("another account authenticated copied vault envelope")
	}
	for _, ctx := range []context.Context{
		shared.WithPersonalAccount(context.Background(), otherID),
		shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{4}),
		shared.WithTenant(context.Background(), uuid.New()),
	} {
		if _, err := vaultTestCreate(t, d).Execute(ctx, actor, first); err == nil {
			t.Fatal("mixed scope created or replayed vault")
		}
		if _, err := vaultTestUnlock(t, unlockD).Execute(ctx, actor, unlockIn); err == nil {
			t.Fatal("mixed scope unlocked vault")
		}
	}
}
