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
	"sync"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	hashing "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/budget"
	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type completeRecoveryFixture struct {
	d        personalApp.CompleteRecoveryDependencies
	read     personalApp.GetRecoveryStatusDependencies
	original personalApp.AuthorizedResult
	actor    security.AccessClaims
	owner    *gorm.DB
	phone    string
}

func completeRecoveryTestFixture(t *testing.T, maxChecks int) completeRecoveryFixture {
	t.Helper()
	base := finalizationDependencies(t)
	phone := accessTestPhone()
	_, in, _ := verifiedAccessFixture(t, base, phone, nil)
	u, err := personalApp.NewFinalizeAccess(base)
	if err != nil {
		t.Fatal(err)
	}
	out, err := u.Execute(context.Background(), in)
	if err != nil || out.Authorized == nil || out.Authorized.RecoveryKit == nil {
		t.Fatal("recovery signup fixture failed")
	}
	actor, err := base.Tokens.Verify(out.Authorized.Session.AccessToken, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewRecoveryIdentityRepository(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := budget.NewRecoveryRules(budget.Limit{Window: time.Hour, MaxSend: 1, MaxCheck: maxChecks})
	if err != nil {
		t.Fatal(err)
	}
	hashKey := sha256.Sum256([]byte(uuid.NewString()))
	subjects, err := hashing.NewSubjects(hashKey[:])
	if err != nil {
		t.Fatal(err)
	}
	d := personalApp.CompleteRecoveryDependencies{UoW: base.UoW, Attempts: base.Attempts, Identity: identity, Recovery: base.Recovery, Sessions: base.Sessions, Receipts: base.Receipts, Events: base.Events, Tokens: base.Tokens, RecoverySecrets: base.RecoverySecrets, Envelopes: base.Envelopes, Budgets: NewOTPBudgetRepository(), Rules: rules, Subjects: subjects, Now: time.Now}
	read := personalApp.GetRecoveryStatusDependencies{UoW: d.UoW, Attempts: d.Attempts, Recovery: d.Recovery, Sessions: d.Sessions, Receipts: d.Receipts, RecoverySecrets: d.RecoverySecrets, Envelopes: d.Envelopes, Now: time.Now}
	return completeRecoveryFixture{d: d, read: read, original: *out.Authorized, actor: actor, owner: budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN"), phone: phone}
}
func completeRecoveryTestUseCase(t *testing.T, d personalApp.CompleteRecoveryDependencies) *personalApp.CompleteRecovery {
	t.Helper()
	u, err := personalApp.NewCompleteRecovery(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func completeRecoveryTestReader(t *testing.T, d personalApp.GetRecoveryStatusDependencies) *personalApp.GetRecoveryStatus {
	t.Helper()
	u, err := personalApp.NewGetRecoveryStatus(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func completeRecoveryTestHash(t *testing.T, in *personalApp.CompleteRecoveryInput) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"attempt_id": in.Binding.ID.String(), "recovery_key_id": in.RecoveryKeyID.String(), "recovery_secret": in.RecoverySecret, "confirm_phone_change": in.ConfirmPhoneChange})
	hash, err := crypto.RequestHash("POST", "/auth/recovery/complete", in.OperationID, body)
	if err != nil {
		t.Fatal(err)
	}
	in.RequestHash = hash
}
func completeRecoveryTestInput(t *testing.T, f completeRecoveryFixture, phone string, candidate *attempt.Candidate) personalApp.CompleteRecoveryInput {
	t.Helper()
	start := startInput(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	start.Candidate.PublicKey = [32]byte(pub)
	if candidate != nil {
		start.Candidate = *candidate
	}
	now := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Second)
	a, err := attempt.New(attempt.NewAttempt{Binding: start.Binding, ProposedAccountID: start.ProposedAccountID, Candidate: start.Candidate, Nonce: [32]byte{7}, Purpose: attempt.Recovery, DestinationE164: phone, Channel: verification.SMS, RequestHash: start.RequestHash}, now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	err = f.d.UoW.Command(attemptContext(a), func(tx shared.Transaction) error {
		if err := f.d.Attempts.Create(tx, a); err != nil {
			return err
		}
		send, err := a.ReserveSend(uuid.New(), [32]byte{1}, verification.SMS, now)
		if err != nil {
			return err
		}
		outcome := verification.Result{Reference: "VE-recovery-synthetic", Channel: verification.SMS, State: verification.Pending, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
		if err = a.ApplyOutcome(send, outcome, now); err != nil {
			return err
		}
		check, err := a.ReserveCheck(uuid.New(), [32]byte{2}, now, 5)
		if err != nil {
			return err
		}
		outcome.State = verification.Approved
		if err = a.ApplyOutcome(check, outcome, now); err != nil {
			return err
		}
		if err = f.d.Attempts.Save(tx, a.Binding, a, 1); err != nil {
			return err
		}
		r, err := receipt.New(check.ID, receipt.Owner{AttemptID: a.ID}, "auth_check", check.RequestHash, now)
		if err != nil {
			return err
		}
		return f.d.Receipts.Create(tx, r)
	})
	if err != nil {
		t.Fatal(err)
	}
	in := personalApp.CompleteRecoveryInput{Binding: a.Binding, OperationID: uuid.New(), RecoveryKeyID: uuid.MustParse(f.original.RecoveryKit.ID), RecoverySecret: f.original.RecoveryKit.Secret, ConfirmPhoneChange: phone != f.phone}
	completeRecoveryTestHash(t, &in)
	return in
}
func completeRecoveryTestAttempt(t *testing.T, f completeRecoveryFixture, binding attempt.Binding) *attempt.AuthAttempt {
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
func completeRecoveryTestBudget(t *testing.T, f completeRecoveryFixture, id uuid.UUID) int {
	t.Helper()
	hash, err := f.d.Subjects.Hash(budget.Recovery, id.String())
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.owner.Raw("SELECT coalesce(sum(check_count),0) FROM mancuerna.otp_budgets WHERE scope='recovery' AND subject_hash=?", hash[:]).Row().Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCompleteRecoveryPostgresConcurrentLastSlotAndCandidateOnlyReplay(t *testing.T) {
	f := completeRecoveryTestFixture(t, 1)
	in := completeRecoveryTestInput(t, f, f.phone, nil)
	u := completeRecoveryTestUseCase(t, f.d)
	const count = 6
	results := make([]personalApp.AuthorizedResult, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = u.Execute(context.Background(), in) }(i)
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil || !reflect.DeepEqual(results[i], results[0]) {
			t.Fatal("recovery concurrent retry diverged", errs[i])
		}
	}
	result := results[0]
	a := completeRecoveryTestAttempt(t, f, in.Binding)
	if result.Session.AccountID != f.actor.AccountID.String() || result.Session.DeviceID != a.Candidate.ID.String() || result.Session.Generation != "1" || result.RecoveryKit == nil || result.RecoveryKit.Generation != "2" || a.State != attempt.Consumed || a.ResultReference == nil || *a.ResultReference != in.OperationID {
		t.Fatal("recovery changed account or candidate")
	}
	if completeRecoveryTestBudget(t, f, in.RecoveryKeyID) != 1 {
		t.Fatal("exact retry consumed additional budget")
	}
	if authSessionTestActive(t, f.d.UoW, NewAuthSessionRepository(), f.actor.AccountID, f.actor.SessionID, f.actor.DeviceID) {
		t.Fatal("old session survived recovery")
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.sessions WHERE account_id=? AND revoked_at IS NULL", f.actor.AccountID) != 1 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.devices WHERE account_id=?", f.actor.AccountID) != 2 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.security_events WHERE request_id=?", in.OperationID) != 1 {
		t.Fatal("recovery duplicated effects")
	}
	old := recoveryTestGet(t, f.d.UoW, NewRecoveryKeyRepository(), f.actor.AccountID, in.RecoveryKeyID)
	live := recoveryTestGet(t, f.d.UoW, NewRecoveryKeyRepository(), f.actor.AccountID, uuid.MustParse(result.RecoveryKit.ID))
	if old.ConsumedAt == nil || old.RevokedAt != nil || !live.Live() || live.Prepared() || live.Generation != 2 {
		t.Fatal("recovery key rotation/preparation invalid")
	}
	var row struct {
		Revision, SecurityVersion int64
		PhoneE164                 string
	}
	if err := f.owner.Raw("SELECT revision,security_version,phone_e164 FROM mancuerna.accounts WHERE id=?", f.actor.AccountID).Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Revision != 1 || row.SecurityVersion != 2 || row.PhoneE164 != f.phone {
		t.Fatal("recovery modified profile revision or wrong number")
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=? AND phone_changed_at IS NULL", f.actor.AccountID) != 1 {
		t.Fatal("same-number recovery recorded a phone change")
	}
	reader := completeRecoveryTestReader(t, f.read)
	for i := 0; i < 3; i++ {
		view, err := reader.Execute(context.Background(), in.Binding)
		if err != nil || !reflect.DeepEqual(view.Authorized, &result) {
			t.Fatal("GET did not recover exact committed result", err)
		}
	}
	changed := in
	changed.RequestHash[0] ^= 1
	if _, err := u.Execute(context.Background(), changed); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("changed successful retry accepted")
	}
	changed = in
	changed.OperationID = uuid.New()
	completeRecoveryTestHash(t, &changed)
	if _, err := u.Execute(context.Background(), changed); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
		t.Fatal("consumed kit authorized new operation")
	}
	later := f.read
	later.Now = func() time.Time { return a.ConsumedAt.Add(10 * time.Minute) }
	if expired, err := completeRecoveryTestReader(t, later).Execute(context.Background(), in.Binding); !errors.Is(err, personalErrors.ErrResultExpired) || expired.Authorized != nil {
		t.Fatal("expired recovery replay recreated authority")
	}
	if completeRecoveryTestBudget(t, f, in.RecoveryKeyID) != 1 {
		t.Fatal("read/replay charged another recovery attempt")
	}
}

func TestCompleteRecoveryPostgresRejectedChecksPersistOnceWithoutBinding(t *testing.T) {
	f := completeRecoveryTestFixture(t, 2)
	in := completeRecoveryTestInput(t, f, f.phone, nil)
	in.RecoverySecret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	completeRecoveryTestHash(t, &in)
	u := completeRecoveryTestUseCase(t, f.d)
	var wg sync.WaitGroup
	errs := make([]error, 5)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, errs[i] = u.Execute(context.Background(), in) }(i)
	}
	wg.Wait()
	for _, err := range errs {
		if !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
			t.Fatal("wrong kit exposed a distinct result", err)
		}
	}
	if completeRecoveryTestBudget(t, f, in.RecoveryKeyID) != 1 {
		t.Fatal("rejected replay was not durable")
	}
	a := completeRecoveryTestAttempt(t, f, in.Binding)
	if a.State != attempt.Verified || a.AccountID != nil || a.ResultReference != nil {
		t.Fatal("rejected secret bound account or consumed OTP")
	}
	if !recoveryTestGet(t, f.d.UoW, NewRecoveryKeyRepository(), f.actor.AccountID, in.RecoveryKeyID).Live() || !authSessionTestActive(t, f.d.UoW, NewAuthSessionRepository(), f.actor.AccountID, f.actor.SessionID, f.actor.DeviceID) {
		t.Fatal("wrong kit changed live authority")
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=? AND account_id IS NULL AND attempt_id=? AND response_ciphertext IS NULL", in.OperationID, in.Binding.ID) != 1 {
		t.Fatal("failed receipt exposed account or ciphertext")
	}
	second := in
	second.OperationID = uuid.New()
	completeRecoveryTestHash(t, &second)
	if _, err := u.Execute(context.Background(), second); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
		t.Fatal(err)
	}
	correct := in
	correct.OperationID = uuid.New()
	correct.RecoverySecret = f.original.RecoveryKit.Secret
	completeRecoveryTestHash(t, &correct)
	if _, err := u.Execute(context.Background(), correct); !errors.Is(err, personalErrors.ErrBudgetExceeded) {
		t.Fatal("recovery budget was not enforced", err)
	}
	if _, err := u.Execute(context.Background(), second); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
		t.Fatal("last-slot rejected replay lost durable result", err)
	}
	if completeRecoveryTestBudget(t, f, in.RecoveryKeyID) != 2 {
		t.Fatal("budget exceeded configured check limit")
	}
	unknown := in
	unknown.OperationID = uuid.New()
	unknown.RecoveryKeyID = uuid.New()
	completeRecoveryTestHash(t, &unknown)
	if _, err := u.Execute(context.Background(), unknown); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
		t.Fatal("unknown kit identified account existence", err)
	}
	if completeRecoveryTestBudget(t, f, unknown.RecoveryKeyID) != 1 {
		t.Fatal("unknown key check was unbounded")
	}
}

func TestCompleteRecoveryPostgresPhoneConfirmationOccupiedAndVerifierRotation(t *testing.T) {
	f := completeRecoveryTestFixture(t, 10)
	b := completeRecoveryTestFixture(t, 10)
	occupied := completeRecoveryTestInput(t, f, b.phone, nil)
	u := completeRecoveryTestUseCase(t, f.d)
	if _, err := u.Execute(context.Background(), occupied); !errors.Is(err, personalErrors.ErrRecoveryPhoneUnavailable) {
		t.Fatal("occupied number accepted", err)
	}
	if _, err := u.Execute(context.Background(), occupied); !errors.Is(err, personalErrors.ErrRecoveryPhoneUnavailable) || completeRecoveryTestBudget(t, f, occupied.RecoveryKeyID) != 1 {
		t.Fatal("phone conflict replay changed outcome")
	}
	newPhone := accessTestPhone()
	change := completeRecoveryTestInput(t, f, newPhone, nil)
	change.ConfirmPhoneChange = false
	completeRecoveryTestHash(t, &change)
	if _, err := u.Execute(context.Background(), change); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
		t.Fatal("number changed without confirmation")
	}
	if !recoveryTestGet(t, f.d.UoW, NewRecoveryKeyRepository(), f.actor.AccountID, change.RecoveryKeyID).Live() {
		t.Fatal("unconfirmed change consumed kit")
	}
	changed := f.d
	keyring, err := crypto.NewRecoveryKeys("next", map[string][]byte{"test": bytes.Repeat([]byte{4}, 32), "next": bytes.Repeat([]byte{9}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	changed.RecoverySecrets = keyring
	change.OperationID = uuid.New()
	change.ConfirmPhoneChange = true
	completeRecoveryTestHash(t, &change)
	result, err := completeRecoveryTestUseCase(t, changed).Execute(context.Background(), change)
	if err != nil {
		t.Fatal("previous verifier version could not recover", err)
	}
	live := recoveryTestGet(t, f.d.UoW, NewRecoveryKeyRepository(), f.actor.AccountID, uuid.MustParse(result.RecoveryKit.ID))
	if live.Verifier.KeyVersion != "next" || live.Prepared() {
		t.Fatal("new key did not use active version honestly")
	}
	var phone string
	if err = f.owner.Raw("SELECT phone_e164 FROM mancuerna.accounts WHERE id=?", f.actor.AccountID).Row().Scan(&phone); err != nil {
		t.Fatal(err)
	}
	if phone != newPhone {
		t.Fatal("confirmed new phone not persisted")
	}
	consumedAt := completeRecoveryTestAttempt(t, f, change.Binding).ConsumedAt
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=? AND phone_changed_at=?", f.actor.AccountID, consumedAt) != 1 {
		t.Fatal("new-number recovery omitted phone change timestamp")
	}
	if !authSessionTestActive(t, b.d.UoW, NewAuthSessionRepository(), b.actor.AccountID, b.actor.SessionID, b.actor.DeviceID) {
		t.Fatal("recovery changed another account")
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=? AND phone_e164=?", b.actor.AccountID, b.phone) != 1 {
		t.Fatal("occupied account phone changed")
	}
	read := f.read
	read.RecoverySecrets = keyring
	if replay, err := completeRecoveryTestReader(t, read).Execute(context.Background(), change.Binding); err != nil || !reflect.DeepEqual(replay.Authorized, &result) {
		t.Fatal("versioned recovery replay failed", err)
	}
}

func TestCompleteRecoveryPostgresAuditFailureRollsBackEveryEffect(t *testing.T) {
	f := completeRecoveryTestFixture(t, 10)
	in := completeRecoveryTestInput(t, f, accessTestPhone(), nil)
	failed := f.d
	failed.Events = failingSecurityEvent{}
	if result, err := completeRecoveryTestUseCase(t, failed).Execute(context.Background(), in); err == nil || result.RecoveryKit != nil || result.Session.AccessToken != "" {
		t.Fatal("failed audit leaked or committed authority")
	}
	if completeRecoveryTestBudget(t, f, in.RecoveryKeyID) != 0 {
		t.Fatal("failed authority commit consumed budget")
	}
	a := completeRecoveryTestAttempt(t, f, in.Binding)
	if a.State != attempt.Verified || a.AccountID != nil {
		t.Fatal("failed authority commit bound attempt")
	}
	if !authSessionTestActive(t, f.d.UoW, NewAuthSessionRepository(), f.actor.AccountID, f.actor.SessionID, f.actor.DeviceID) || !recoveryTestGet(t, f.d.UoW, NewRecoveryKeyRepository(), f.actor.AccountID, in.RecoveryKeyID).Live() {
		t.Fatal("rollback lost previous authority")
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.accounts WHERE id=? AND phone_e164=? AND security_version=1 AND revision=1", f.actor.AccountID, f.phone) != 1 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.devices WHERE account_id=?", f.actor.AccountID) != 1 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.recovery_keys WHERE account_id=?", f.actor.AccountID) != 1 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=?", in.OperationID) != 0 {
		t.Fatal("recovery rollback retained partial effect")
	}
	if _, err := completeRecoveryTestUseCase(t, f.d).Execute(context.Background(), in); err != nil {
		t.Fatal("rolled-back recovery could not retry", err)
	}
}

func TestCompleteRecoveryPostgresTwoAttemptsOneKitAndOccupiedDevice(t *testing.T) {
	f := completeRecoveryTestFixture(t, 10)
	requests := []personalApp.CompleteRecoveryInput{completeRecoveryTestInput(t, f, accessTestPhone(), nil), completeRecoveryTestInput(t, f, accessTestPhone(), nil)}
	u := completeRecoveryTestUseCase(t, f.d)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	results := make([]personalApp.AuthorizedResult, 2)
	for i := range requests {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = u.Execute(context.Background(), requests[i]) }(i)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatal("one-use kit authorized two candidates or neither", errs)
	}
	for i, err := range errs {
		a := completeRecoveryTestAttempt(t, f, requests[i].Binding)
		if err == nil {
			if a.State != attempt.Consumed || a.AccountID == nil || results[i].RecoveryKit.Generation != "2" {
				t.Fatal("winning recovery incomplete")
			}
		} else if !errors.Is(err, personalErrors.ErrRecoveryKeyState) || a.State != attempt.Verified || a.AccountID != nil || results[i].RecoveryKit != nil {
			t.Fatal("losing recovery retained authority", err)
		}
	}
	if completeRecoveryTestBudget(t, f, requests[0].RecoveryKeyID) != 2 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.sessions WHERE account_id=? AND revoked_at IS NULL", f.actor.AccountID) != 1 || accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.recovery_keys WHERE account_id=?", f.actor.AccountID) != 2 {
		t.Fatal("one-use recovery duplicated authority or omitted failed check")
	}
	occupied := completeRecoveryTestFixture(t, 10)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	candidate := attempt.Candidate{ID: occupied.actor.DeviceID, PublicKey: [32]byte(pub), Platform: "android", Label: "Occupied candidate"}
	for _, foreign := range []bool{false, true} {
		if foreign {
			candidate.ID = f.actor.DeviceID
		}
		in := completeRecoveryTestInput(t, occupied, accessTestPhone(), &candidate)
		if _, err := completeRecoveryTestUseCase(t, occupied.d).Execute(context.Background(), in); !errors.Is(err, personalErrors.ErrAuthObjectConflict) {
			t.Fatal("occupied candidate UUID was adopted", err)
		}
		if completeRecoveryTestBudget(t, occupied, in.RecoveryKeyID) != 0 || completeRecoveryTestAttempt(t, occupied, in.Binding).AccountID != nil || !recoveryTestGet(t, occupied.d.UoW, NewRecoveryKeyRepository(), occupied.actor.AccountID, in.RecoveryKeyID).Live() || !authSessionTestActive(t, occupied.d.UoW, NewAuthSessionRepository(), occupied.actor.AccountID, occupied.actor.SessionID, occupied.actor.DeviceID) {
			t.Fatal("UUID collision consumed prior authority")
		}
	}
}

func TestCompleteRecoveryPostgresScopeDirectOTPAndPhoneLockGuards(t *testing.T) {
	f := completeRecoveryTestFixture(t, 20)
	in := completeRecoveryTestInput(t, f, f.phone, nil)
	u := completeRecoveryTestUseCase(t, f.d)
	for _, ctx := range []context.Context{shared.WithPersonalAccount(context.Background(), f.actor.AccountID), shared.WithPersonalAttempt(context.Background(), in.Binding.ID, in.Binding.ContinuationHash), shared.WithTenant(context.Background(), uuid.New())} {
		if _, err := u.Execute(ctx, in); !errors.Is(err, shared.ErrPersonalScope) {
			t.Fatal("recovery accepted ambient authority", err)
		}
	}
	wrong := in
	wrong.Binding.ContinuationHash[0] ^= 1
	if _, err := u.Execute(context.Background(), wrong); !errors.Is(err, personalErrors.ErrAuthAttemptNotFound) {
		t.Fatal("wrong continuation accessed kit", err)
	}
	if completeRecoveryTestBudget(t, f, in.RecoveryKeyID) != 0 {
		t.Fatal("rejected scope checked a recovery secret")
	}
	// The mutation port cannot be called with an account/attempt scope alone:
	// it requires both phone locks already held before the account mutex.
	bound := shared.WithPersonalAccount(shared.WithPersonalAttempt(context.Background(), in.Binding.ID, in.Binding.ContinuationHash), f.actor.AccountID)
	err := f.d.UoW.Command(bound, func(tx shared.Transaction) error {
		key, err := f.d.Recovery.Get(tx, f.actor.AccountID, in.RecoveryKeyID)
		if err != nil {
			return err
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		if err = key.Consume(now); err != nil {
			return err
		}
		if err = f.d.Recovery.SaveState(tx, f.actor.AccountID, key); err != nil {
			return err
		}
		return f.d.Identity.ApplyRecovery(tx, in.Binding, f.actor.AccountID, key.ID, false, now)
	})
	if !errors.Is(err, personalErrors.ErrAccessIdentityScope) || !recoveryTestGet(t, f.d.UoW, NewRecoveryKeyRepository(), f.actor.AccountID, in.RecoveryKeyID).Live() {
		t.Fatal("phone mutation bypassed ordered locks", err)
	}
	for _, mutate := range []string{"purpose", "operation", "receipt", "expired"} {
		candidate := completeRecoveryTestInput(t, f, f.phone, nil)
		a := completeRecoveryTestAttempt(t, f, candidate.Binding)
		switch mutate {
		case "purpose":
			err = f.owner.Exec("UPDATE mancuerna.auth_challenges SET purpose='access' WHERE id=?", a.ID).Error
		case "operation":
			err = f.owner.Exec("UPDATE mancuerna.auth_challenges SET provider_operation_kind='send' WHERE id=?", a.ID).Error
		case "receipt":
			err = f.owner.Exec("UPDATE mancuerna.auth_receipts SET purpose='synthetic_unapproved' WHERE operation_id=?", a.ProviderOperation.ID).Error
		case "expired":
			err = f.owner.Exec("UPDATE mancuerna.auth_challenges SET expires_at=clock_timestamp()-interval '1 millisecond' WHERE id=?", a.ID).Error
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err = u.Execute(context.Background(), candidate); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
			t.Fatal("non-direct/unusable OTP reached recovery", mutate, err)
		}
	}
	if completeRecoveryTestBudget(t, f, in.RecoveryKeyID) != 0 {
		t.Fatal("unapproved OTP consumed recovery budget")
	}
	if _, err := u.Execute(context.Background(), in); err != nil {
		t.Fatal("valid request failed after guarded attempts", err)
	}
}

func TestCompleteRecoveryPostgresResultOwnerRevocationAndReplacedKit(t *testing.T) {
	f := completeRecoveryTestFixture(t, 10)
	in := completeRecoveryTestInput(t, f, f.phone, nil)
	result, err := completeRecoveryTestUseCase(t, f.d).Execute(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	reader := completeRecoveryTestReader(t, f.read)
	wrong := in.Binding
	wrong.ContinuationHash[0] ^= 1
	if _, err := reader.Execute(context.Background(), wrong); !errors.Is(err, personalErrors.ErrAuthAttemptNotFound) {
		t.Fatal("wrong continuation recovered credentials")
	}
	newKeyID := uuid.MustParse(result.RecoveryKit.ID)
	if err = f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		key, err := f.d.Recovery.Get(tx, f.actor.AccountID, newKeyID)
		if err != nil {
			return err
		}
		if err = key.Revoke(time.Now()); err != nil {
			return err
		}
		return f.d.Recovery.SaveState(tx, f.actor.AccountID, key)
	}); err != nil {
		t.Fatal(err)
	}
	if replay, err := reader.Execute(context.Background(), in.Binding); !errors.Is(err, personalErrors.ErrRecoveryKeyState) || replay.Authorized != nil {
		t.Fatal("replay presented a replaced kit as current")
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=? AND response_ciphertext IS NOT NULL", in.OperationID) != 1 {
		t.Fatal("failed replay rewrote original receipt")
	}
	if err = f.d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		return f.d.Sessions.RevokeSession(tx, f.actor.AccountID, uuid.MustParse(result.Session.SessionID), time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if replay, err := reader.Execute(context.Background(), in.Binding); !errors.Is(err, personalErrors.ErrSessionRevoked) || replay.Authorized != nil {
		t.Fatal("revoked recovered session replayed credentials")
	}
	if accessTestCount(t, f.owner, "SELECT count(*) FROM mancuerna.sessions WHERE account_id=?", f.actor.AccountID) != 2 {
		t.Fatal("GET recreated a lost session")
	}
}
