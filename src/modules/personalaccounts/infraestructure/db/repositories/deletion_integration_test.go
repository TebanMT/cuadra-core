//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/deletion"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	syncApp "github.com/cuadra/cuadra-core/src/shared/sync/personal/app"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	streamRepos "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/db/repositories"
	cursorCrypto "github.com/cuadra/cuadra-core/src/shared/sync/personal/infraestructure/security"
	"github.com/google/uuid"
)

func deletionTestDependencies(f *phoneChangeFixture) personalApp.RequestAccountDeletionDependencies {
	return personalApp.RequestAccountDeletionDependencies{UoW: f.d.Phone.UoW, Sessions: f.d.Sessions, Deletion: NewDeletionRepository(), Proofs: f.d.Proofs, Receipts: f.d.Phone.Receipts, Events: f.d.Events, Policy: &deletion.Policy{Version: "synthetic-test-only", PurgeDelay: 2 * time.Hour}, Now: time.Now}
}
func deletionTestUseCase(t *testing.T, d personalApp.RequestAccountDeletionDependencies) *personalApp.RequestAccountDeletion {
	t.Helper()
	u, err := personalApp.NewRequestAccountDeletion(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func deletionTestReader(t *testing.T, f *phoneChangeFixture) *personalApp.GetDeletionResult {
	t.Helper()
	resolver, err := NewDeletionResultResolver(budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := personalApp.NewGetDeletionResult(personalApp.GetDeletionResultDependencies{UoW: f.d.Phone.UoW, Resolver: resolver, Deletion: NewDeletionRepository()})
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func deletionTestInput(t *testing.T, f *phoneChangeFixture) (personalApp.RequestAccountDeletionInput, uuid.UUID) {
	t.Helper()
	seed := sha256.Sum256([]byte(uuid.NewString()))
	in := personalApp.RequestAccountDeletionInput{OperationID: uuid.New(), RequestHash: sha256.Sum256([]byte(uuid.NewString())), Confirmation: deletion.Confirmation, PolicyVersion: "synthetic-test-only", ResultSecret: base64.RawURLEncoding.EncodeToString(seed[:])}
	p := proofTestInput()
	p.Purpose = security.DeleteAccount
	p.TargetRequestHash = in.RequestHash
	challenge, err := proofTestStart(t, f.startProof).Execute(context.Background(), f.actor, p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := proofTestComplete(t, f.completeProof).Execute(context.Background(), f.actor, proofTestCompleteInput(t, challenge, f.actor, f.key))
	if err != nil {
		t.Fatal(err)
	}
	in.ProofToken = out.ProofToken
	return in, p.ProofID
}
func deletionTestReadInput(in personalApp.RequestAccountDeletionInput) personalApp.GetDeletionResultInput {
	return personalApp.GetDeletionResultInput{OperationID: in.OperationID, ResultSecret: in.ResultSecret}
}
func deletionTestUnchanged(t *testing.T, f *phoneChangeFixture, in personalApp.RequestAccountDeletionInput, proofID uuid.UUID) {
	t.Helper()
	if !refreshTestActive(t, f.refresh, f.tokens) || accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.accounts WHERE id=? AND status='active' AND security_version=1 AND deletion_requested_at IS NULL AND deletion_policy_version IS NULL AND purge_after IS NULL`, f.actor.AccountID) != 1 {
		t.Fatal("failed deletion changed account authority")
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.device_proofs WHERE id=? AND consumed_at IS NULL AND operation_id IS NULL`, proofID) != 1 || accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?`, in.OperationID) != 0 || accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.security_events WHERE request_id=?`, in.OperationID) != 0 {
		t.Fatal("failed deletion left proof or receipt effects")
	}
}

func TestDeletionPostgresAtomicAcceptanceLostResponseAndIsolation(t *testing.T) {
	f, b := newPhoneChangeFixture(t, 20), newPhoneChangeFixture(t, 20)
	other := f.otherSession(t)
	phone := f.startInput(t, accessTestPhone())
	if _, err := phoneChangeStart(t, f.d).Execute(context.Background(), phone); err != nil {
		t.Fatal(err)
	}
	create, unlock := vaultTestDependencies(f.startProof, f.completeProof)
	vaultInput := vaultTestCreateInput(uuid.New())
	if _, err := vaultTestCreate(t, create).Execute(context.Background(), f.actor, vaultInput); err != nil {
		t.Fatal(err)
	}
	vaultBefore := vaultTestGet(t, create, f.actor.AccountID, vaultInput.VaultID)
	unlockInput, _ := vaultTestUnlockInput(t, f.startProof, f.completeProof, f.actor, f.key, vaultInput.VaultID, security.VaultUnlock)
	beforeKeys := accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.recovery_keys WHERE account_id=? AND revoked_at IS NULL AND consumed_at IS NULL`, f.actor.AccountID)
	unbound := accessTestFixture(t, f.owner, f.phone, uuid.Nil)
	in, proofID := deletionTestInput(t, f)
	if _, err := deletionTestUseCase(t, deletionTestDependencies(f)).Execute(context.Background(), b.actor, in); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("another account used deletion proof", err)
	}
	deletionTestUnchanged(t, f, in, proofID)
	reader := deletionTestReader(t, f)
	if _, err := reader.Execute(context.Background(), deletionTestReadInput(in)); !errors.Is(err, personalErrors.ErrDeletionResultNotFound) {
		t.Fatal("uncommitted request had result", err)
	}
	ack, err := deletionTestUseCase(t, deletionTestDependencies(f)).Execute(context.Background(), f.actor, in)
	if err != nil || ack.Status != "deletion_pending" || ack.OperationID != in.OperationID.String() {
		t.Fatal("deletion failed", err)
	}
	if refreshTestActive(t, f.refresh, f.tokens) || authSessionTestActive(t, f.d.Phone.UoW, NewAuthSessionRepository(), other.AccountID, other.SessionID, other.DeviceID) || !refreshTestActive(t, b.refresh, b.tokens) {
		t.Fatal("revocation crossed account or missed session")
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.accounts WHERE id=? AND phone_e164=? AND status='deletion_pending' AND security_version=2 AND revision=1 AND deleted_at IS NULL AND deletion_policy_version=? AND purge_after=deletion_requested_at+interval '2 hours'`, f.actor.AccountID, f.phone, in.PolicyVersion) != 1 {
		t.Fatal("deletion modified identity/profile or retention incorrectly")
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.device_proofs WHERE id=? AND consumed_at IS NOT NULL AND operation_id=?`, proofID, in.OperationID) != 1 {
		t.Fatal("proof consumption missing")
	}
	if f.attempt(t, phone.Binding).State != attempt.Cancelled {
		t.Fatal("own outstanding attempt not cancelled")
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.recovery_keys WHERE account_id=? AND revoked_at IS NULL AND consumed_at IS NULL`, f.actor.AccountID) != beforeKeys {
		t.Fatal("request destroyed retained verifier")
	}
	var wrapped []byte
	if err = f.owner.Raw(`SELECT wrapped_data_key FROM mancuerna.vault_keys WHERE vault_id=? AND retired_at IS NULL`, vaultInput.VaultID).Row().Scan(&wrapped); err != nil || !bytes.Equal(wrapped, vaultBefore.Envelope.Ciphertext) {
		t.Fatal("request destroyed vault envelope")
	}
	for i := 0; i < 3; i++ {
		got, err := reader.Execute(context.Background(), deletionTestReadInput(in))
		if err != nil || !reflect.DeepEqual(got, ack) {
			t.Fatal("lost response failed", err)
		}
	}
	if _, err = deletionTestUseCase(t, deletionTestDependencies(f)).Execute(context.Background(), f.actor, in); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("POST bypassed revoked session", err)
	}
	if _, err = refreshTestUseCase(t, f.refresh).Execute(context.Background(), refreshTestInput(t, f.tokens, f.key, uuid.New())); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("deleted account refreshed", err)
	}
	if _, err = meTestUseCase(t, finalizationDependencies(t)).Execute(context.Background(), f.actor); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("old signed actor still read profile", err)
	}
	if _, err = accessTestResolve(f.d.Phone.UoW, accessTestRepository(t), unbound); !errors.Is(err, personalErrors.ErrAccessIdentityUnavailable) {
		t.Fatal("old unbound verified attempt reauthorized deletion", err)
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.accounts WHERE phone_e164=?`, f.phone) != 1 {
		t.Fatal("old attempt recreated the phone identity")
	}
	if _, err = vaultTestUnlock(t, unlock).Execute(context.Background(), f.actor, unlockInput); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("deleted account unlocked vault", err)
	}
	if _, err = phoneChangeResend(t, phoneChangeAttemptDependencies(f)).Execute(context.Background(), phoneChangeResendInput(phone)); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("deleted account resent phone attempt", err)
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=? AND purpose='delete_account' AND response_ciphertext IS NULL AND key_version IS NULL AND response_expires_at=completed_at`, in.OperationID) != 1 || accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.security_events WHERE request_id=? AND event_type='account_deletion_requested'`, in.OperationID) != 1 {
		t.Fatal("minimal receipt/event missing or duplicated")
	}
	// Minimal acceptance survives the sensitive-response TTL; it never claims
	// that a retention worker or a purge has completed.
	if err = f.owner.Exec(`UPDATE mancuerna.accounts SET deletion_requested_at=deletion_requested_at-interval '1 day',purge_after=purge_after-interval '1 day' WHERE id=?`, f.actor.AccountID).Error; err != nil {
		t.Fatal(err)
	}
	if err = f.owner.Exec(`UPDATE mancuerna.auth_receipts SET completed_at=completed_at-interval '1 day',response_expires_at=response_expires_at-interval '1 day' WHERE operation_id=?`, in.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	if got, err := reader.Execute(context.Background(), deletionTestReadInput(in)); err != nil || got.Status != "deletion_pending" {
		t.Fatal("minimal receipt incorrectly expired", err)
	}
}

func TestDeletionPostgresPolicyProofAndCallerFailureRollback(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	in, proofID := deletionTestInput(t, f)
	d := deletionTestDependencies(f)
	d.Policy = nil
	if _, err := deletionTestUseCase(t, d).Execute(context.Background(), f.actor, in); !errors.Is(err, personalErrors.ErrDeletionPolicyNotConfigured) {
		t.Fatal(err)
	}
	deletionTestUnchanged(t, f, in, proofID)
	d = deletionTestDependencies(f)
	for _, change := range []func(*personalApp.RequestAccountDeletionInput){func(x *personalApp.RequestAccountDeletionInput) { x.PolicyVersion = "not-reviewed" }, func(x *personalApp.RequestAccountDeletionInput) { x.Confirmation = "yes" }, func(x *personalApp.RequestAccountDeletionInput) { x.RequestHash[0]++ }, func(x *personalApp.RequestAccountDeletionInput) {
		x.ProofToken = "malformed-but-contract-sized-credential"
	}} {
		bad := in
		change(&bad)
		if _, err := deletionTestUseCase(t, d).Execute(context.Background(), f.actor, bad); err == nil {
			t.Fatal("invalid request accepted")
		}
		deletionTestUnchanged(t, f, in, proofID)
	}
	failed := d
	failed.Events = failingSecurityEvent{}
	if _, err := deletionTestUseCase(t, failed).Execute(context.Background(), f.actor, in); err == nil {
		t.Fatal("event failure committed")
	}
	deletionTestUnchanged(t, f, in, proofID)
	if _, err := deletionTestReader(t, f).Execute(context.Background(), deletionTestReadInput(in)); !errors.Is(err, personalErrors.ErrDeletionResultNotFound) {
		t.Fatal("rollback result visible")
	}
	// Exact proof expiry is enforced by PostgreSQL and the aggregate, even
	// while the caller's signed access token is otherwise current.
	if err := f.owner.Exec(`UPDATE mancuerna.device_proofs SET verified_at=verified_at-interval '10 minutes',expires_at=expires_at-interval '10 minutes' WHERE id=?`, proofID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := deletionTestUseCase(t, d).Execute(context.Background(), f.actor, in); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("expired proof accepted", err)
	}
	deletionTestUnchanged(t, f, in, proofID)
}

func TestDeletionPostgresResultSecretOperationAndScopeBinding(t *testing.T) {
	a, b := newPhoneChangeFixture(t, 10), newPhoneChangeFixture(t, 10)
	inA, _ := deletionTestInput(t, a)
	inB, _ := deletionTestInput(t, b)
	// Reusing the same client secret across operations is not recommended, but
	// cannot make resolve_identity choose the other account's authority.
	inB.ResultSecret = inA.ResultSecret
	ackA, err := deletionTestUseCase(t, deletionTestDependencies(a)).Execute(context.Background(), a.actor, inA)
	if err != nil {
		t.Fatal(err)
	}
	ackB, err := deletionTestUseCase(t, deletionTestDependencies(b)).Execute(context.Background(), b.actor, inB)
	if err != nil {
		t.Fatal(err)
	}
	reader := deletionTestReader(t, a)
	for _, pair := range []struct {
		in   personalApp.RequestAccountDeletionInput
		want personalApp.Acknowledgement
	}{{inA, ackA}, {inB, ackB}} {
		got, err := reader.Execute(context.Background(), deletionTestReadInput(pair.in))
		if err != nil || got != pair.want {
			t.Fatal("reused secret crossed accounts", err)
		}
	}
	wrong := deletionTestReadInput(inA)
	wrong.ResultSecret = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{99}, 32))
	if _, err = reader.Execute(context.Background(), wrong); !errors.Is(err, personalErrors.ErrDeletionResultNotFound) {
		t.Fatal("wrong secret read result", err)
	}
	wrong = deletionTestReadInput(inA)
	wrong.OperationID = uuid.New()
	if _, err = reader.Execute(context.Background(), wrong); !errors.Is(err, personalErrors.ErrDeletionResultNotFound) {
		t.Fatal("UUID substitution read result", err)
	}
	for _, ctx := range []context.Context{shared.WithPersonalAccount(context.Background(), a.actor.AccountID), shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err = reader.Execute(ctx, deletionTestReadInput(inA)); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("mixed credential scope accepted", err)
		}
	}
	hashA, _ := deletion.ResultHash(inA.OperationID, inA.ResultSecret)
	q, err := a.d.Phone.UoW.Query(shared.WithPersonalAccount(context.Background(), b.actor.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewDeletionRepository().GetResult(q, b.actor.AccountID, inA.OperationID, hashA)
	shared.CloseQuery(q)
	if !errors.Is(err, personalErrors.ErrDeletionResultNotFound) {
		t.Fatal("B directly read A receipt", err)
	}
	for _, env := range []string{"MANCUERNA_TEST_DATA_DSN", "MANCUERNA_TEST_OWNER_DSN"} {
		if _, err := NewDeletionResultResolver(budgetTestPool(t, env)); err == nil {
			t.Fatal("privileged/data pool accepted for resolver")
		}
	}
	data, err := shared.NewPersonalPostgresUnitOfWork(budgetTestPool(t, "MANCUERNA_TEST_DATA_DSN"), shared.PersonalDataRole)
	if err != nil {
		t.Fatal(err)
	}
	q, err = data.Query(shared.WithPersonalAccount(context.Background(), a.actor.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewDeletionRepository().GetResult(q, a.actor.AccountID, inA.OperationID, hashA)
	shared.CloseQuery(q)
	if !errors.Is(err, personalErrors.ErrAuthSessionScope) {
		t.Fatal("data pool read deletion capability", err)
	}
	// Simulate corrupt duplicate stored hashes. The initial lookup may return
	// either owner; the exact operation/owner recheck must never return B's Ack.
	if err = a.owner.Exec(`UPDATE mancuerna.auth_receipts SET result_secret_hash=? WHERE operation_id=?`, hashA[:], inB.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	got, err := reader.Execute(context.Background(), deletionTestReadInput(inA))
	if err == nil && got != ackA {
		t.Fatal("duplicate hash returned another account")
	}
	if err != nil && !errors.Is(err, personalErrors.ErrDeletionResultNotFound) {
		t.Fatal(err)
	}
}

func TestDeletionPostgresConcurrentRequestAndDataBoundary(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	in, _ := deletionTestInput(t, f)
	u := deletionTestUseCase(t, deletionTestDependencies(f))
	data := profileCommandDependencies(t)
	cursors, err := cursorCrypto.NewCursorSigner("synthetic", map[string][]byte{"synthetic": bytes.Repeat([]byte{71}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	pull, err := syncApp.NewPull(syncApp.PullDependencies{UoW: data.UoW, Commands: data.Commands, Stream: streamRepos.NewStream(), Cursors: cursors, Canonicalizer: crypto.JSONCanonicalizer{}, Now: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	var epoch uuid.UUID
	if err = f.owner.Raw(`SELECT epoch FROM mancuerna.sync_streams WHERE account_id=?`, f.actor.AccountID).Row().Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	cursor := issuePull(t, cursors, protocol.PullBinding(f.actor.AccountID, epoch), "0", time.Now())
	if _, err = pull.Execute(context.Background(), pullActor(f.actor), syncApp.PullInput{Cursor: cursor}); err != nil {
		t.Fatal("initial pull", err)
	}
	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-gate; _, errs[i] = u.Execute(context.Background(), f.actor, in) }(i)
	}
	close(gate)
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, personalErrors.ErrSessionRevoked) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatal("concurrent deletion applied more than once")
	}
	if _, err = pull.Execute(context.Background(), pullActor(f.actor), syncApp.PullInput{Cursor: cursor}); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("deleted account pulled old cursor", err)
	}
	if _, err = profileCommandUseCase(t, data).Execute(context.Background(), f.actor, profileCommand(f.actor, "1", "Pending before deletion")); !errors.Is(err, protocol.ErrCommandActor) {
		t.Fatal("pending profile revived account", err)
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.security_events WHERE request_id=? AND event_type='account_deletion_requested'`, in.OperationID) != 1 || accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.sync_changes WHERE account_id=?`, f.actor.AccountID) != 0 {
		t.Fatal("deletion duplicated effect or wrote journal")
	}
	if _, err = deletionTestReader(t, f).Execute(context.Background(), deletionTestReadInput(in)); err != nil {
		t.Fatal(err)
	}
}

type deletionFailAfterEffect struct{ repository.DeletionRepository }

func (r deletionFailAfterEffect) Request(tx shared.Transaction, actor security.AccessClaims, in deletion.Request) error {
	if err := r.DeletionRepository.Request(tx, actor, in); err != nil {
		return err
	}
	return errors.New("synthetic caller failure")
}
func TestDeletionPostgresEffectFailureAndPureScope(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	in, proofID := deletionTestInput(t, f)
	d := deletionTestDependencies(f)
	d.Deletion = deletionFailAfterEffect{NewDeletionRepository()}
	if _, err := deletionTestUseCase(t, d).Execute(context.Background(), f.actor, in); err == nil {
		t.Fatal("caller failure committed")
	}
	deletionTestUnchanged(t, f, in, proofID)
	d = deletionTestDependencies(f)
	for _, ctx := range []context.Context{shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithTenant(context.Background(), uuid.New()), shared.WithPersonalAccount(context.Background(), uuid.New())} {
		if _, err := deletionTestUseCase(t, d).Execute(ctx, f.actor, in); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("mixed scope reached deletion", err)
		}
		deletionTestUnchanged(t, f, in, proofID)
	}
	request := deletion.Request{OperationID: in.OperationID, RequestHash: in.RequestHash, ProofHash: sha256.Sum256([]byte(in.ProofToken)), Policy: *d.Policy, RequestedAt: time.Now().UTC().Truncate(time.Millisecond)}
	if err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error { return d.Deletion.Request(tx, f.actor, request) }); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatal("repository bypassed unconsumed proof", err)
	}
	deletionTestUnchanged(t, f, in, proofID)
	if _, err := deletionTestUseCase(t, d).Execute(context.Background(), f.actor, in); err != nil {
		t.Fatal("rollback could not retry", err)
	}
}
