//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func finalizationDependencies(t *testing.T) personalApp.FinalizeAccessDependencies {
	t.Helper()
	uow := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := crypto.NewAccessTokens("test", priv, map[string]ed25519.PublicKey{"test": pub})
	if err != nil {
		t.Fatal(err)
	}
	envelopes, err := crypto.NewEnvelopes("test", map[string][]byte{"test": bytes.Repeat([]byte{3}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := crypto.NewRecoveryKeys("test", map[string][]byte{"test": bytes.Repeat([]byte{4}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewIdentityAccessRepository(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return personalApp.FinalizeAccessDependencies{UoW: uow, Attempts: NewAuthAttemptPostgresRepository(), Identity: identity, Sessions: NewAuthSessionRepository(), Recovery: NewRecoveryKeyRepository(), Receipts: NewAuthReceiptPostgresRepository(), Events: NewSecurityEventRepository(), Tokens: tokens, Proofs: crypto.DeviceProofs{}, RecoverySecrets: keys, Envelopes: envelopes, Now: time.Now}
}

// The fixture starts from an already durably verified OTP. No provider or
// SMS is called; this suite specifically proves the following atomic effect.
func verifiedAccessFixture(t *testing.T, d personalApp.FinalizeAccessDependencies, phone string, candidate *attempt.Candidate, rejected ...personalApp.FinalizeAccessInput) (*attempt.AuthAttempt, personalApp.FinalizeAccessInput, ed25519.PrivateKey) {
	t.Helper()
	in := startInput(t)
	in.DestinationE164 = phone
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	in.Candidate.PublicKey = [32]byte(pub)
	if candidate != nil {
		in.Candidate = *candidate
	}
	now := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Second)
	a, err := attempt.New(attempt.NewAttempt{Binding: in.Binding, ProposedAccountID: in.ProposedAccountID, Candidate: in.Candidate, Nonce: [32]byte{7}, Purpose: attempt.Access, DestinationE164: phone, Channel: verification.SMS, RequestHash: in.RequestHash}, now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	checkID := uuid.New()
	checkHash := [32]byte{8}
	err = d.UoW.Command(attemptContext(a), func(tx shared.Transaction) error {
		if err := d.Attempts.Create(tx, a); err != nil {
			return err
		}
		send, err := a.ReserveSend(uuid.New(), [32]byte{9}, verification.SMS, now)
		if err != nil {
			return err
		}
		outcome := verification.Result{Reference: "VE-synthetic", Channel: verification.SMS, State: verification.Pending, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
		if err = a.ApplyOutcome(send, outcome, now); err != nil {
			return err
		}
		for _, prior := range rejected {
			failedCheck, err := a.ReserveCheck(prior.OperationID, prior.RequestHash, now, 5)
			if err != nil {
				return err
			}
			if err = a.ApplyOutcome(failedCheck, outcome, now); err != nil {
				return err
			}
			failedReceipt, err := receipt.New(prior.OperationID, receipt.Owner{AttemptID: a.ID}, "auth_check", prior.RequestHash, now)
			if err != nil {
				return err
			}
			envelope, err := d.Envelopes.SealReceipt(failedReceipt.Context(), []byte(`{"status":"pending"}`))
			if err != nil {
				return err
			}
			if err = failedReceipt.SetResponse(envelope, now.Add(10*time.Minute), uuid.Nil); err != nil {
				return err
			}
			if err = d.Receipts.Create(tx, failedReceipt); err != nil {
				return err
			}
		}
		check, err := a.ReserveCheck(checkID, checkHash, now, 5)
		if err != nil {
			return err
		}
		outcome.State = verification.Approved
		if err = a.ApplyOutcome(check, outcome, now); err != nil {
			return err
		}
		if err = d.Attempts.Save(tx, a.Binding, a, 1); err != nil {
			return err
		}
		r, err := receipt.New(checkID, receipt.Owner{AttemptID: a.ID}, "auth_check", checkHash, now)
		if err != nil {
			return err
		}
		return d.Receipts.Create(tx, r)
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, personalApp.FinalizeAccessInput{Binding: a.Binding, OperationID: checkID, RequestHash: checkHash}, private
}

func TestFinalizeAccessPostgresSignupAndLostResponseAreAtomic(t *testing.T) {
	d := finalizationDependencies(t)
	a, in, _ := verifiedAccessFixture(t, d, "+525555555601", nil)
	useCase, err := personalApp.NewFinalizeAccess(d)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan personalApp.AccessResult, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := useCase.Execute(context.Background(), in)
			results <- r
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *personalApp.AuthorizedResult
	for result := range results {
		if result.Authorized == nil || result.Authorized.RecoveryKit == nil {
			t.Fatal("signup did not issue session and kit")
		}
		if first == nil {
			first = result.Authorized
		} else if !reflect.DeepEqual(first, result.Authorized) {
			t.Fatal("lost response changed credentials")
		}
	}
	accountID := uuid.MustParse(first.Session.AccountID)
	if accountID != a.ProposedAccountID {
		t.Fatal("new identity changed")
	}
	q, err := d.UoW.Query(shared.WithPersonalAccount(context.Background(), accountID))
	if err != nil {
		t.Fatal(err)
	}
	key, err := d.Recovery.GetLive(q, accountID)
	shared.CloseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	if key.Prepared() {
		t.Fatal("issuing kit falsely confirmed it saved")
	}
	if err = d.RecoverySecrets.Verify(first.RecoveryKit.Secret, key.Verifier); err != nil {
		t.Fatal("kit verifier mismatch")
	}
	claims, err := d.Tokens.Verify(first.Session.AccessToken, time.Now())
	if err != nil || claims.AccountID != accountID {
		t.Fatal("access token not independently verifiable")
	}
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	for _, table := range []string{"devices", "sessions", "recovery_keys", "security_events", "sync_streams"} {
		var count int64
		if err = owner.Raw("SELECT count(*) FROM mancuerna."+table+" WHERE account_id=?", accountID).Scan(&count).Error; err != nil || count != 1 {
			t.Fatalf("atomic %s count=%d error=%v", table, count, err)
		}
	}
	other, otherInput, _ := verifiedAccessFixture(t, d, a.DestinationE164, nil)
	required, err := useCase.Execute(context.Background(), otherInput)
	if err != nil || !required.AuthorizationRequired || required.Authorized != nil || required.Attempt.AccountID == nil || *required.Attempt.AccountID != accountID {
		t.Fatal("OTP alone authorized existing account")
	}
	if other.ProposedAccountID == accountID {
		t.Fatal("fixture failed to use a distinct proposed identity")
	}
	if err = d.UoW.Command(shared.WithPersonalAccount(context.Background(), accountID), func(tx shared.Transaction) error {
		return d.Sessions.RevokeSession(tx, accountID, claims.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = useCase.Execute(context.Background(), in); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatal("lost-response replay bypassed revocation")
	}
}

type failingSecurityEvent struct{}

func (failingSecurityEvent) Append(shared.Transaction, repository.SecurityEvent) error {
	return errors.New("synthetic post-session failure")
}

func TestFinalizeAccessPostgresFailureRollsBackAllAuthority(t *testing.T) {
	d := finalizationDependencies(t)
	a, in, _ := verifiedAccessFixture(t, d, "+525555555602", nil)
	failing := d
	failing.Events = failingSecurityEvent{}
	u, err := personalApp.NewFinalizeAccess(failing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = u.Execute(context.Background(), in); err == nil {
		t.Fatal("injected failure ignored")
	}
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	var count int64
	if err = owner.Raw("SELECT count(*) FROM mancuerna.accounts WHERE id=?", a.ProposedAccountID).Scan(&count).Error; err != nil || count != 0 {
		t.Fatal("account survived failure")
	}
	loaded := readAttempt(t, d.UoW, a)
	if loaded.State != attempt.Verified || loaded.AccountID != nil {
		t.Fatal("failure consumed or bound attempt")
	}
	working, err := personalApp.NewFinalizeAccess(d)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := working.Execute(context.Background(), in); err != nil || r.Authorized == nil {
		t.Fatal("clean retry could not finish signup", err)
	}
}

func TestFinalizeAccessPostgresResumeRequiresRegisteredDeviceSignature(t *testing.T) {
	d := finalizationDependencies(t)
	a, in, private := verifiedAccessFixture(t, d, "+525555555603", nil)
	u, err := personalApp.NewFinalizeAccess(d)
	if err != nil {
		t.Fatal(err)
	}
	first, err := u.Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	resume, resumeInput, _ := verifiedAccessFixture(t, d, a.DestinationE164, &a.Candidate)
	proof := security.ResumeProof{AttemptID: resume.ID, DeviceID: resume.Candidate.ID, Nonce: resume.Nonce[:], ExpiresAt: resume.ExpiresAt}
	payload, err := crypto.ResumeProofPayload(proof)
	if err != nil {
		t.Fatal(err)
	}
	resumeInput.DeviceSignature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))
	result, err := u.Execute(context.Background(), resumeInput)
	if err != nil || result.Authorized == nil {
		t.Fatal("registered device could not resume", err)
	}
	if result.Authorized.Session.AccountID != first.Authorized.Session.AccountID || result.Authorized.Session.DeviceID != first.Authorized.Session.DeviceID || result.Authorized.RecoveryKit != nil {
		t.Fatal("resume changed identity or rotated kit")
	}
	bad, badInput, _ := verifiedAccessFixture(t, d, a.DestinationE164, &a.Candidate)
	badInput.DeviceSignature = resumeInput.DeviceSignature
	if _, err = u.Execute(context.Background(), badInput); err == nil {
		t.Fatal("signature from another attempt replayed")
	}
	if current := readAttempt(t, d.UoW, bad); current.State != attempt.Verified {
		t.Fatal("invalid signature consumed OTP")
	}
}

func TestFinalizeAccessPostgresRejectsPriorCheckReceiptAndHash(t *testing.T) {
	d := finalizationDependencies(t)
	prior := personalApp.FinalizeAccessInput{OperationID: uuid.New(), RequestHash: [32]byte{19}}
	a, approved, _ := verifiedAccessFixture(t, d, accessTestPhone(), nil, prior)
	prior.Binding = a.Binding
	u, err := personalApp.NewFinalizeAccess(d)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := u.Execute(context.Background(), prior); !errors.Is(err, personalErrors.ErrAuthAttemptOperation) || result.Authorized != nil {
		t.Fatalf("earlier failed check finalized a later approval: %v", err)
	}
	changed := approved
	changed.RequestHash[0] ^= 1
	if result, err := u.Execute(context.Background(), changed); !errors.Is(err, personalErrors.ErrAuthAttemptOperation) || result.Authorized != nil {
		t.Fatalf("approved operation accepted different request hash: %v", err)
	}
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	if accessTestCount(t, owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=?", a.ProposedAccountID) != 0 {
		t.Fatal("mismatched check created authority")
	}
	current := readAttempt(t, d.UoW, a)
	if current.State != attempt.Verified || current.ConsumedAt != nil || current.AccountID != nil || current.ProviderOperation.ID != approved.OperationID {
		t.Fatal("mismatched check changed verified attempt")
	}
	first, err := u.Execute(context.Background(), approved)
	if err != nil || first.Authorized == nil {
		t.Fatalf("correct approved check could not finalize: %v", err)
	}
	if result, err := u.Execute(context.Background(), prior); err == nil || result.Authorized != nil {
		t.Fatal("earlier receipt replayed successful authorization")
	}
	repeated, err := u.Execute(context.Background(), approved)
	if err != nil || !reflect.DeepEqual(first.Authorized, repeated.Authorized) {
		t.Fatal("correct lost-response replay changed")
	}
	q, err := d.UoW.Query(attemptContext(a))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	r, err := d.Receipts.Get(q, receipt.Owner{AttemptID: a.ID}, prior.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := r.ReadResponse(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	plain, err := d.Envelopes.OpenReceipt(r.Context(), envelope)
	if err != nil || string(plain) != `{"status":"pending"}` {
		t.Fatal("prior rejected check receipt was replaced")
	}
}
