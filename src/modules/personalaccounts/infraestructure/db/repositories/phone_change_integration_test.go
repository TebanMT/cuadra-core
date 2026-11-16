//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	hashing "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/budget"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type phoneChangeFixture struct {
	d             personalApp.PhoneChangeDependencies
	startProof    personalApp.StartDeviceProofDependencies
	completeProof personalApp.CompleteDeviceProofDependencies
	refresh       personalApp.RefreshSessionDependencies
	owner         *gorm.DB
	actor         security.AccessClaims
	tokens        personalApp.SessionTokens
	key           ed25519.PrivateKey
	p             *phoneProviderDouble
	phone         string
}

func newPhoneChangeFixture(t *testing.T, max int) *phoneChangeFixture {
	t.Helper()
	rd, owner, tokens, key := refreshTestFixture(t)
	base := finalizationDependencies(t)
	actor := security.AccessClaims{AccountID: uuid.MustParse(tokens.AccountID), SessionID: uuid.MustParse(tokens.SessionID), DeviceID: uuid.MustParse(tokens.DeviceID)}
	// refreshTestFixture supplies a separate token issuer for refresh tests.
	// HTTP fixtures must issue their initial access token with that same issuer.
	initial, claims, err := rd.Tokens.Issue(actor.AccountID, actor.SessionID, actor.DeviceID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tokens.AccessToken, tokens.AccessExpiresAt = initial, claims.ExpiresAt.UTC().Format("2006-01-02T15:04:05.000Z")
	identity, err := NewPhoneChangeRepository(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := budget.NewRules(map[budget.Scope]budget.Limit{budget.Destination: {Window: time.Hour, MaxSend: max, MaxCheck: max}, budget.Origin: {Window: time.Hour, MaxSend: max, MaxCheck: max}, budget.Service: {Window: time.Hour, MaxSend: max, MaxCheck: max}})
	if err != nil {
		t.Fatal(err)
	}
	hashKey := sha256.Sum256([]byte(uuid.NewString()))
	subjects, err := hashing.NewSubjects(hashKey[:])
	if err != nil {
		t.Fatal(err)
	}
	p := &phoneProviderDouble{}
	created := time.Now().UTC().Truncate(time.Millisecond)
	result := verification.Result{Reference: "VE-phone-change-fixture", Channel: verification.WhatsApp, State: verification.Pending, CreatedAt: created, ExpiresAt: created.Add(9 * time.Minute)}
	p.start = func(context.Context, verification.StartRequest) (verification.Result, error) { return result, nil }
	p.check = func(context.Context, verification.CheckRequest) (verification.Result, error) {
		copy := result
		copy.State = verification.Approved
		return copy, nil
	}
	d := personalApp.PhoneChangeDependencies{Phone: personalApp.StartAuthDependencies{UoW: base.UoW, Attempts: base.Attempts, Receipts: base.Receipts, Budgets: NewOTPBudgetRepository(), Rules: rules, Subjects: subjects, Provider: p, ServiceIdentity: uuid.NewString(), ProviderValidity: 10 * time.Minute, Now: time.Now}, Identity: identity, Sessions: base.Sessions, Proofs: NewDeviceProofRepository(), Events: base.Events, Tokens: rd.Tokens, Envelopes: base.Envelopes, MaxChecks: 5}
	f := &phoneChangeFixture{d: d, refresh: rd, owner: owner, actor: actor, tokens: tokens, key: key, p: p}
	f.startProof = personalApp.StartDeviceProofDependencies{UoW: base.UoW, Sessions: base.Sessions, Proofs: d.Proofs, Receipts: base.Receipts, Events: base.Events, Now: time.Now}
	f.completeProof = personalApp.CompleteDeviceProofDependencies{UoW: base.UoW, Sessions: base.Sessions, Proofs: d.Proofs, Receipts: base.Receipts, Events: base.Events, Verifier: base.Proofs, Envelopes: base.Envelopes, Now: time.Now}
	if err = owner.Raw(`SELECT phone_e164 FROM mancuerna.accounts WHERE id=?`, actor.AccountID).Row().Scan(&f.phone); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *phoneChangeFixture) proof(t *testing.T, hash [32]byte) (string, uuid.UUID) {
	t.Helper()
	in := proofTestInput()
	in.Purpose = security.PhoneChange
	in.TargetRequestHash = hash
	p, err := proofTestStart(t, f.startProof).Execute(context.Background(), f.actor, in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := proofTestComplete(t, f.completeProof).Execute(context.Background(), f.actor, proofTestCompleteInput(t, p, f.actor, f.key))
	if err != nil {
		t.Fatal(err)
	}
	return out.ProofToken, in.ProofID
}
func (f *phoneChangeFixture) startInput(t *testing.T, phone string) personalApp.StartPhoneChangeInput {
	t.Helper()
	in := personalApp.StartPhoneChangeInput{Actor: f.actor, OperationID: uuid.New(), RequestHash: sha256.Sum256([]byte(uuid.NewString())), Binding: attempt.Binding{ID: uuid.New(), ContinuationHash: sha256.Sum256([]byte(uuid.NewString()))}, DestinationE164: phone, Channel: verification.WhatsApp, Origin: uuid.NewString()}
	in.ProofToken, _ = f.proof(t, in.RequestHash)
	return in
}
func (f *phoneChangeFixture) completeInput(t *testing.T, start personalApp.StartPhoneChangeInput) (personalApp.CompletePhoneChangeInput, uuid.UUID) {
	t.Helper()
	in := personalApp.CompletePhoneChangeInput{Actor: f.actor, OperationID: uuid.New(), RequestHash: sha256.Sum256([]byte(uuid.NewString())), Binding: start.Binding, Code: "123456", Origin: start.Origin}
	var proofID uuid.UUID
	in.ProofToken, proofID = f.proof(t, in.RequestHash)
	return in, proofID
}
func phoneChangeStart(t *testing.T, d personalApp.PhoneChangeDependencies) *personalApp.StartPhoneChange {
	t.Helper()
	u, err := personalApp.NewStartPhoneChange(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func phoneChangeComplete(t *testing.T, d personalApp.PhoneChangeDependencies) *personalApp.CompletePhoneChange {
	t.Helper()
	u, err := personalApp.NewCompletePhoneChange(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func (f *phoneChangeFixture) attempt(t *testing.T, b attempt.Binding) *attempt.AuthAttempt {
	t.Helper()
	q, err := f.d.Phone.UoW.Query(shared.WithPersonalAccount(context.Background(), f.actor.AccountID))
	if err != nil {
		t.Fatal(err)
	}
	defer shared.CloseQuery(q)
	a, err := f.d.Phone.Attempts.Get(q, b)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func (f *phoneChangeFixture) counts(t *testing.T, start personalApp.StartPhoneChangeInput, sends, checks int) {
	t.Helper()
	for scope, value := range map[budget.Scope]string{budget.Destination: start.DestinationE164, budget.Origin: start.Origin, budget.Service: f.d.Phone.ServiceIdentity} {
		hash, err := f.d.Phone.Subjects.Hash(scope, value)
		if err != nil {
			t.Fatal(err)
		}
		var got struct{ Send, Check int }
		if err = f.owner.Raw(`SELECT coalesce(sum(send_count),0) AS send,coalesce(sum(check_count),0) AS check FROM mancuerna.otp_budgets WHERE scope=? AND subject_hash=?`, string(scope), hash[:]).Scan(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got.Send != sends || got.Check != checks {
			t.Fatalf("phone change budget %s got %d/%d want %d/%d", scope, got.Send, got.Check, sends, checks)
		}
	}
}
func (f *phoneChangeFixture) otherSession(t *testing.T) security.AccessClaims {
	t.Helper()
	other := f.actor
	other.SessionID = uuid.New()
	now := time.Now().UTC().Truncate(time.Millisecond)
	s, err := session.NewSession(other.AccountID, other.DeviceID, other.SessionID, sha256.Sum256([]byte(uuid.NewString())), uuid.New(), sha256.Sum256([]byte(uuid.NewString())), now)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.d.Phone.UoW.Command(shared.WithPersonalAccount(context.Background(), other.AccountID), func(tx shared.Transaction) error { return f.d.Sessions.CreateSession(tx, other.AccountID, s) }); err != nil {
		t.Fatal(err)
	}
	return other
}

func TestPhoneChangePostgresSessionIdentityLostResponseAndRefresh(t *testing.T) {
	f := newPhoneChangeFixture(t, 20)
	other := f.otherSession(t)
	in := f.startInput(t, accessTestPhone())
	start := phoneChangeStart(t, f.d)
	a, err := start.Execute(context.Background(), in)
	if err != nil || a.State != attempt.Pending {
		t.Fatalf("start: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err = start.Execute(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	// Refresh between start and complete preserves SID/DID and actor binding.
	before, err := refreshTestUseCase(t, f.refresh).Execute(context.Background(), refreshTestInput(t, f.tokens, f.key, uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	complete, proofID := f.completeInput(t, in)
	u := phoneChangeComplete(t, f.d)
	out, err := u.Execute(context.Background(), complete)
	if err != nil {
		t.Fatal(err)
	}
	if out.SessionID != f.tokens.SessionID || out.DeviceID != f.tokens.DeviceID || out.AccountID != f.tokens.AccountID || out.Generation != "3" || out.RefreshToken == before.RefreshToken {
		t.Fatal("phone change changed session identity or did not renew")
	}
	if !refreshTestActive(t, f.refresh, out) || authSessionTestActive(t, f.d.Phone.UoW, NewAuthSessionRepository(), other.AccountID, other.SessionID, other.DeviceID) {
		t.Fatal("session revocation boundary incorrect")
	}
	if proofTestGet(t, f.startProof, f.actor, proofID).ConsumedAt == nil {
		t.Fatal("complete proof not consumed")
	}
	for i := 0; i < 3; i++ {
		got, e := u.Execute(context.Background(), complete)
		if e != nil || !reflect.DeepEqual(got, out) {
			t.Fatal("lost-response replay diverged")
		}
	}
	after, err := refreshTestUseCase(t, f.refresh).Execute(context.Background(), refreshTestInput(t, out, f.key, uuid.New()))
	if err != nil || after.Generation != "4" {
		t.Fatalf("phone change session cannot refresh: %v", err)
	}
	got, err := u.Execute(context.Background(), complete)
	if err != nil || !reflect.DeepEqual(got, out) {
		t.Fatal("later refresh prevented exact historical result")
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.accounts WHERE id=? AND phone_e164=? AND revision=1 AND security_version=2 AND phone_changed_at IS NOT NULL`, f.actor.AccountID, in.DestinationE164) != 1 {
		t.Fatal("identity/profile revision incorrect")
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.session_rotations WHERE operation_id=? AND response_ciphertext IS NULL AND key_version IS NULL`, complete.OperationID) != 1 {
		t.Fatal("phone result leaked into refresh history")
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.security_events WHERE account_id=? AND event_type='phone_changed'`, f.actor.AccountID) != 1 {
		t.Fatal("repeated phone effect")
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.recovery_keys WHERE account_id=? AND consumed_at IS NULL AND revoked_at IS NULL`, f.actor.AccountID) != 1 {
		t.Fatal("phone change changed recovery kit")
	}
	f.counts(t, in, 1, 1)
	if f.p.starts.Load() != 1 || f.p.checks.Load() != 1 || f.p.fetches.Load() != 0 {
		t.Fatal("unexpected provider calls")
	}
	// The old generation has no refresh response but retains logout permission.
	oldInput := refreshTestInput(t, before, f.key, complete.OperationID)
	oldInput.RequestHash = complete.RequestHash
	if _, err = refreshTestUseCase(t, f.refresh).Execute(context.Background(), oldInput); !errors.Is(err, personalErrors.ErrResultExpired) {
		t.Fatalf("refresh read phone response: %v", err)
	}
	logoutRepo, err := NewLogoutRepository(budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	account, err := logoutRepo.Resolve(context.Background(), sha256.Sum256([]byte(before.RevocationTicket)))
	if err != nil || account != f.actor.AccountID {
		t.Fatal("old logout ticket lost")
	}
}

func TestPhoneChangePostgresScopeInitiatorAndSamePhoneFailClosed(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	same := f.startInput(t, f.phone)
	u := phoneChangeStart(t, f.d)
	if _, err := u.Execute(context.Background(), same); !errors.Is(err, personalErrors.ErrInvalidPhoneChange) {
		t.Fatalf("same phone accepted: %v", err)
	}
	f.counts(t, same, 0, 0)
	if f.p.starts.Load() != 0 {
		t.Fatal("same phone sent verification")
	}
	in := f.startInput(t, accessTestPhone())
	bad := in
	bad.ProofToken = same.ProofToken
	if _, err := u.Execute(context.Background(), bad); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatalf("wrong request proof accepted: %v", err)
	}
	f.counts(t, in, 0, 0)
	for _, scope := range []context.Context{shared.WithPersonalAttempt(context.Background(), uuid.New(), [32]byte{1}), shared.WithTenant(context.Background(), uuid.New()), shared.WithPersonalAccount(context.Background(), uuid.New())} {
		if _, err := u.Execute(scope, in); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatalf("mixed scope accepted: %v", err)
		}
	}
	if _, err := u.Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	other := f.otherSession(t)
	bad = in
	bad.Actor = other
	if _, err := u.Execute(context.Background(), bad); err == nil {
		t.Fatal("other session recovered start")
	}
	complete, _ := f.completeInput(t, in)
	complete.Actor = other
	if _, err := phoneChangeComplete(t, f.d).Execute(context.Background(), complete); err == nil {
		t.Fatal("other session completed change")
	}
	foreign := newPhoneChangeFixture(t, 10)
	complete.Actor = foreign.actor
	if _, err := phoneChangeComplete(t, f.d).Execute(context.Background(), complete); !errors.Is(err, personalErrors.ErrAuthAttemptNotFound) {
		t.Fatalf("foreign actor obtained attempt: %v", err)
	}
	f.counts(t, in, 1, 0)
	if f.p.checks.Load() != 0 {
		t.Fatal("invalid actor reached provider")
	}
}

func TestPhoneChangePostgresNegativeCheckAndUncertaintyNeverResend(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	in := f.startInput(t, accessTestPhone())
	if _, err := phoneChangeStart(t, f.d).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	complete, proofID := f.completeInput(t, in)
	original := f.p.check
	f.p.check = func(ctx context.Context, r verification.CheckRequest) (verification.Result, error) {
		out, err := original(ctx, r)
		out.State = verification.Pending
		return out, err
	}
	u := phoneChangeComplete(t, f.d)
	for i := 0; i < 3; i++ {
		if _, err := u.Execute(context.Background(), complete); !errors.Is(err, personalErrors.ErrVerificationRejected) {
			t.Fatalf("negative check approved: %v", err)
		}
	}
	if proofTestGet(t, f.startProof, f.actor, proofID).ConsumedAt != nil {
		t.Fatal("negative OTP consumed proof")
	}
	f.counts(t, in, 1, 1)
	if f.p.checks.Load() != 1 {
		t.Fatal("negative same operation checked again")
	}
	complete, _ = f.completeInput(t, in)
	f.p.check = func(context.Context, verification.CheckRequest) (verification.Result, error) {
		return verification.Result{}, personalErrors.ErrVerificationMissing
	}
	for i := 0; i < 3; i++ {
		if _, err := u.Execute(context.Background(), complete); !errors.Is(err, personalErrors.ErrAuthAttemptUncertain) {
			t.Fatalf("missing verification became approval: %v", err)
		}
	}
	f.counts(t, in, 1, 2)
	if f.p.checks.Load() != 2 || f.p.starts.Load() != 1 || f.p.fetches.Load() != 0 {
		t.Fatal("uncertainty caused provider retry")
	}
	if f.attempt(t, in.Binding).State != attempt.Uncertain || !refreshTestActive(t, f.refresh, f.tokens) {
		t.Fatal("uncertainty mutated authority")
	}
}

func TestPhoneChangePostgresEffectRollbackAndExpiredProofRetry(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	in := f.startInput(t, accessTestPhone())
	if _, err := phoneChangeStart(t, f.d).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	complete, proofID := f.completeInput(t, in)
	failed := f.d
	failed.Events = failingSecurityEvent{}
	if out, err := phoneChangeComplete(t, failed).Execute(context.Background(), complete); err == nil || out.AccessToken != "" {
		t.Fatal("failed audit returned credentials")
	}
	if proofTestGet(t, f.startProof, f.actor, proofID).ConsumedAt != nil || f.attempt(t, in.Binding).State != attempt.Verified {
		t.Fatal("failed effect consumed proof/attempt")
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.accounts WHERE id=? AND phone_e164=? AND security_version=1`, f.actor.AccountID, f.phone) != 1 || accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.session_rotations WHERE operation_id=?`, complete.OperationID) != 0 {
		t.Fatal("failed effect persisted authority")
	}
	// The OTP fact is durable; expiring its distinct device proof cannot extend
	// proof TTL or cause another provider Check. A fresh proof can resume effect.
	if err := f.owner.Exec(`UPDATE mancuerna.device_proofs SET expires_at=clock_timestamp()-interval '1 second',verified_at=clock_timestamp()-interval '2 minutes' WHERE id=?`, proofID).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := phoneChangeComplete(t, f.d).Execute(context.Background(), complete); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatalf("expired proof accepted: %v", err)
	}
	complete.ProofToken, _ = f.proof(t, complete.RequestHash)
	if _, err := phoneChangeComplete(t, f.d).Execute(context.Background(), complete); err != nil {
		t.Fatal(err)
	}
	f.counts(t, in, 1, 1)
	if f.p.checks.Load() != 1 {
		t.Fatal("effect retry checked OTP twice")
	}
}

func TestPhoneChangePostgresCompetingAccountsAndBoundAttemptCancellation(t *testing.T) {
	a, b := newPhoneChangeFixture(t, 10), newPhoneChangeFixture(t, 10)
	phone := accessTestPhone()
	ai, bi := a.startInput(t, phone), b.startInput(t, phone)
	for _, v := range []struct {
		f *phoneChangeFixture
		i personalApp.StartPhoneChangeInput
	}{{a, ai}, {b, bi}} {
		if _, err := phoneChangeStart(t, v.f.d).Execute(context.Background(), v.i); err != nil {
			t.Fatal(err)
		}
	}
	other := a.startInput(t, accessTestPhone())
	if _, err := phoneChangeStart(t, a.d).Execute(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	ac, _ := a.completeInput(t, ai)
	bc, _ := b.completeInput(t, bi)
	inputs := []personalApp.CompletePhoneChangeInput{ac, bc}
	fixtures := []*phoneChangeFixture{a, b}
	errs := make([]error, 2)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range inputs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, errs[i] = phoneChangeComplete(t, fixtures[i].d).Execute(ctx, inputs[i])
		}(i)
	}
	close(gate)
	wg.Wait()
	winners := 0
	for _, err := range errs {
		if err == nil {
			winners++
		} else if !errors.Is(err, personalErrors.ErrRecoveryPhoneUnavailable) {
			t.Fatalf("phone race did not fail closed: %v", err)
		}
	}
	if winners != 1 || accessTestCount(t, a.owner, `SELECT count(*) FROM mancuerna.accounts WHERE phone_e164=?`, phone) != 1 {
		t.Fatal("phone race created two owners")
	}
	if errs[0] == nil {
		if a.attempt(t, other.Binding).State != attempt.Cancelled {
			t.Fatal("prior bound challenge not cancelled")
		}
	} else {
		if a.attempt(t, other.Binding).State != attempt.Pending {
			t.Fatal("loser invalidated unrelated challenge")
		}
	}
}

func TestPhoneChangePostgresConcurrentLastSlotAndOriginalResponseWindow(t *testing.T) {
	f := newPhoneChangeFixture(t, 1)
	in := f.startInput(t, accessTestPhone())
	if _, err := phoneChangeStart(t, f.d).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	complete, _ := f.completeInput(t, in)
	u := phoneChangeComplete(t, f.d)
	entered, release := make(chan struct{}), make(chan struct{})
	original := f.p.check
	f.p.check = func(ctx context.Context, request verification.CheckRequest) (verification.Result, error) {
		close(entered)
		select {
		case <-release:
			return original(ctx, request)
		case <-ctx.Done():
			return verification.Result{}, ctx.Err()
		}
	}
	type outcome struct {
		tokens personalApp.SessionTokens
		err    error
	}
	done := make(chan outcome, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { tokens, err := u.Execute(ctx, complete); done <- outcome{tokens, err} }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("check was not called")
	}
	// Budget, check intent and actor binding committed before provider call;
	// the account mutex is free while the external response is outstanding.
	if err := f.d.Phone.UoW.Command(shared.WithPersonalAccount(ctx, f.actor.AccountID), func(tx shared.Transaction) error {
		a, err := f.d.Phone.Attempts.Get(tx, in.Binding)
		if err != nil {
			return err
		}
		if a.State != attempt.Uncertain || a.ProviderOperation.ID != complete.OperationID {
			t.Error("provider call preceded durable check")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Execute(ctx, complete); !errors.Is(err, personalErrors.ErrAuthAttemptUncertain) {
		t.Fatalf("in-flight retry did not return uncertainty: %v", err)
	}
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	const concurrency = 5
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := u.Execute(context.Background(), complete)
			if err == nil && !reflect.DeepEqual(out, result.tokens) {
				err = errors.New("replay result diverged")
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	f.counts(t, in, 1, 1)
	if f.p.checks.Load() != 1 {
		t.Fatal("last-slot retry rechecked provider")
	}
	expired := f.d
	expired.Phone.Now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	if _, err := phoneChangeComplete(t, expired).Execute(context.Background(), complete); !errors.Is(err, personalErrors.ErrResultExpired) {
		t.Fatalf("result window extended: %v", err)
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.sessions WHERE account_id=?`, f.actor.AccountID) != 1 {
		t.Fatal("retry created session")
	}
	if err := f.d.Phone.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, f.actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Execute(context.Background(), complete); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatalf("revoked actor recovered result: %v", err)
	}
}

func TestPhoneChangePostgresRevocationDuringCheckAndStartRollback(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	in := f.startInput(t, accessTestPhone())
	failed := f.d
	failed.Events = failingSecurityEvent{}
	if _, err := phoneChangeStart(t, failed).Execute(context.Background(), in); err == nil {
		t.Fatal("start audit failure committed")
	}
	f.counts(t, in, 0, 0)
	if f.p.starts.Load() != 0 || accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.auth_challenges WHERE id=?`, in.Binding.ID) != 0 {
		t.Fatal("start rollback sent verification or kept challenge")
	}
	if _, err := phoneChangeStart(t, f.d).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	complete, proofID := f.completeInput(t, in)
	original := f.p.check
	f.p.check = func(ctx context.Context, request verification.CheckRequest) (verification.Result, error) {
		if err := f.d.Phone.UoW.Command(shared.WithPersonalAccount(ctx, f.actor.AccountID), func(tx shared.Transaction) error {
			return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, f.actor.SessionID, time.Now())
		}); err != nil {
			return verification.Result{}, err
		}
		return original(ctx, request)
	}
	if _, err := phoneChangeComplete(t, f.d).Execute(context.Background(), complete); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatalf("revoked in-flight actor changed phone: %v", err)
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.accounts WHERE id=? AND phone_e164=? AND security_version=1`, f.actor.AccountID, f.phone) != 1 || accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.device_proofs WHERE id=? AND consumed_at IS NULL`, proofID) != 1 {
		t.Fatal("revoked actor persisted effect")
	}
	f.counts(t, in, 1, 1)
}
