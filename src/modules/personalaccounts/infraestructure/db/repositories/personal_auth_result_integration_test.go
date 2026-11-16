//go:build server && integration

package repositories

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	personalApp "github.com/cuadra/cuadra-core/src/modules/personalaccounts/app"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func personalResultDependencies(t *testing.T, f *phoneChangeFixture) personalApp.GetPersonalAuthResultDependencies {
	t.Helper()
	base := finalizationDependencies(t)
	return personalApp.GetPersonalAuthResultDependencies{UoW: f.d.Phone.UoW, Sessions: f.d.Sessions, Results: NewPersonalAuthResultRepository(), Recovery: base.Recovery, RecoverySecrets: base.RecoverySecrets, Envelopes: f.d.Envelopes, Now: time.Now}
}
func personalResultReader(t *testing.T, d personalApp.GetPersonalAuthResultDependencies) *personalApp.GetPersonalAuthResult {
	t.Helper()
	u, err := personalApp.NewGetPersonalAuthResult(d)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func personalResultPhone(t *testing.T, f *phoneChangeFixture) (personalApp.StartPhoneChangeInput, personalApp.CompletePhoneChangeInput, personalApp.SessionTokens) {
	t.Helper()
	in := f.startInput(t, accessTestPhone())
	if _, err := phoneChangeStart(t, f.d).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	complete, _ := f.completeInput(t, in)
	out, err := phoneChangeComplete(t, f.d).Execute(context.Background(), complete)
	if err != nil {
		t.Fatal(err)
	}
	return in, complete, out
}

func TestPersonalAuthResultPostgresPhoneCommittedReadOnlyAndActorScope(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	start, complete, want := personalResultPhone(t, f)
	d := personalResultDependencies(t, f)
	read := personalResultReader(t, d)
	before := f.attempt(t, start.Binding)
	events := accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.security_events WHERE account_id=?`, f.actor.AccountID)
	for i := 0; i < 3; i++ {
		got, err := read.Execute(context.Background(), f.actor, complete.OperationID)
		if err != nil || got.Session == nil || got.RecoveryKit != nil || !reflect.DeepEqual(*got.Session, want) {
			t.Fatalf("result did not match original: %v", err)
		}
		if strings.Contains(fmt.Sprintf("%#v", got), want.RefreshToken) {
			t.Fatal("result formatter leaked token")
		}
	}
	if !reflect.DeepEqual(before, f.attempt(t, start.Binding)) || events != accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.security_events WHERE account_id=?`, f.actor.AccountID) || f.p.checks.Load() != 1 {
		t.Fatal("GET replay changed effect or queried provider")
	}
	other := f.otherSession(t)
	if _, err := read.Execute(context.Background(), other, complete.OperationID); err == nil {
		t.Fatal("same device different session read credentials")
	}
	foreign := newPhoneChangeFixture(t, 10)
	if _, err := read.Execute(context.Background(), foreign.actor, complete.OperationID); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
		t.Fatalf("foreign result not hidden: %v", err)
	}
	if _, err := read.Execute(shared.WithPersonalAttempt(context.Background(), start.Binding.ID, start.Binding.ContinuationHash), f.actor, complete.OperationID); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatalf("attempt capability read session result: %v", err)
	}
	if _, err := read.Execute(shared.WithTenant(context.Background(), uuid.New()), f.actor, complete.OperationID); !errors.Is(err, shared.ErrPersonalScope) {
		t.Fatal("gym scope read personal result")
	}
	if _, err := read.Execute(context.Background(), f.actor, start.OperationID); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
		t.Fatal("allowlist exposed start receipt")
	}
	if _, err := read.Execute(context.Background(), f.actor, uuid.New()); !errors.Is(err, personalErrors.ErrReceiptNotFound) {
		t.Fatal("unknown result not hidden")
	}
	latest, err := refreshTestUseCase(t, f.refresh).Execute(context.Background(), refreshTestInput(t, want, f.key, uuid.New()))
	if err != nil || latest.Generation != "3" {
		t.Fatalf("refresh after change failed: %v", err)
	}
	got, err := read.Execute(context.Background(), f.actor, complete.OperationID)
	if err != nil || got.Session == nil || !reflect.DeepEqual(*got.Session, want) {
		t.Fatal("historical GET result changed after refresh")
	}
}

func TestPersonalAuthResultPostgresUncommittedPhoneHasNoResult(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	in := f.startInput(t, accessTestPhone())
	if _, err := phoneChangeStart(t, f.d).Execute(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	complete, _ := f.completeInput(t, in)
	failed := f.d
	failed.Events = failingSecurityEvent{}
	if _, err := phoneChangeComplete(t, failed).Execute(context.Background(), complete); err == nil {
		t.Fatal("fixture effect unexpectedly committed")
	}
	before := f.attempt(t, in.Binding)
	if before.State != attempt.Verified {
		t.Fatal("fixture did not leave OTP fact")
	}
	read := personalResultReader(t, personalResultDependencies(t, f))
	if got, err := read.Execute(context.Background(), f.actor, complete.OperationID); !errors.Is(err, personalErrors.ErrReceiptNotFound) || got.Session != nil {
		t.Fatalf("reserved check became session result: %v", err)
	}
	if !reflect.DeepEqual(before, f.attempt(t, in.Binding)) || f.p.checks.Load() != 1 || accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.accounts WHERE id=? AND phone_e164=? AND security_version=1`, f.actor.AccountID, f.phone) != 1 {
		t.Fatal("GET finalized pending phone effect")
	}
}

func TestPersonalAuthResultPostgresRecoveryKitLiveActorAndReplacement(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	rotation := rotateRecoveryTestDependencies(t, f.startProof)
	in, _ := rotateRecoveryTestInput(t, f.startProof, f.completeProof, f.actor, f.key, uuid.New(), security.RecoveryKey)
	want, err := rotateRecoveryTestUseCase(t, rotation).Execute(context.Background(), f.actor, in)
	if err != nil {
		t.Fatal(err)
	}
	d := personalResultDependencies(t, f)
	read := personalResultReader(t, d)
	got, err := read.Execute(context.Background(), f.actor, in.OperationID)
	if err != nil || got.RecoveryKit == nil || got.Session != nil || !reflect.DeepEqual(*got.RecoveryKit, want) {
		t.Fatalf("kit result mismatch: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%#v", got), want.Secret) {
		t.Fatal("kit formatter leaked secret")
	}
	other := f.otherSession(t)
	if _, err = read.Execute(context.Background(), other, in.OperationID); !errors.Is(err, personalErrors.ErrInvalidProof) {
		t.Fatalf("other session recovered kit: %v", err)
	}
	replacement, _ := rotateRecoveryTestInput(t, f.startProof, f.completeProof, f.actor, f.key, uuid.New(), security.RecoveryKey)
	if _, err = rotateRecoveryTestUseCase(t, rotation).Execute(context.Background(), f.actor, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err = read.Execute(context.Background(), f.actor, in.OperationID); !errors.Is(err, personalErrors.ErrRecoveryKeyState) {
		t.Fatalf("replaced kit shown as live: %v", err)
	}
	if accessTestCount(t, f.owner, `SELECT count(*) FROM mancuerna.auth_receipts WHERE operation_id=? AND response_ciphertext IS NOT NULL`, in.OperationID) != 1 {
		t.Fatal("reader mutated old receipt")
	}
	if _, err = read.Execute(context.Background(), f.actor, replacement.OperationID); err != nil {
		t.Fatal(err)
	}
}

func TestPersonalAuthResultPostgresExpiryRevocationAndCipherBinding(t *testing.T) {
	f := newPhoneChangeFixture(t, 10)
	_, in, _ := personalResultPhone(t, f)
	d := personalResultDependencies(t, f)
	expired := d
	expired.Now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	if _, err := personalResultReader(t, expired).Execute(context.Background(), f.actor, in.OperationID); !errors.Is(err, personalErrors.ErrResultExpired) {
		t.Fatalf("expired result served: %v", err)
	}
	var cipher []byte
	if err := f.owner.Raw(`SELECT response_ciphertext FROM mancuerna.auth_receipts WHERE operation_id=?`, in.OperationID).Row().Scan(&cipher); err != nil {
		t.Fatal(err)
	}
	broken := append([]byte(nil), cipher...)
	broken[len(broken)-1] ^= 1
	if err := f.owner.Exec(`UPDATE mancuerna.auth_receipts SET response_ciphertext=? WHERE operation_id=?`, broken, in.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	if got, err := personalResultReader(t, d).Execute(context.Background(), f.actor, in.OperationID); err == nil || got.Session != nil {
		t.Fatal("damaged ciphertext returned session")
	}
	if err := f.owner.Exec(`UPDATE mancuerna.auth_receipts SET response_ciphertext=? WHERE operation_id=?`, cipher, in.OperationID).Error; err != nil {
		t.Fatal(err)
	}
	if err := d.UoW.Command(shared.WithPersonalAccount(context.Background(), f.actor.AccountID), func(tx shared.Transaction) error {
		return d.Sessions.RevokeSession(tx, f.actor.AccountID, f.actor.SessionID, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := personalResultReader(t, d).Execute(context.Background(), f.actor, in.OperationID); !errors.Is(err, personalErrors.ErrSessionRevoked) {
		t.Fatalf("revoked session got sensitive result: %v", err)
	}
}
