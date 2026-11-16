//go:build server && integration

package repositories

import (
	"bytes"
	"context"
	"errors"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func attemptContext(a *attempt.AuthAttempt) context.Context {
	return sharedDomain.WithPersonalAttempt(context.Background(), a.ID, a.ContinuationHash)
}
func attemptUOW(t *testing.T) sharedDomain.UnitOfWork {
	t.Helper()
	uow, err := sharedDomain.NewPersonalPostgresUnitOfWork(profileTestPool(t, "MANCUERNA_TEST_AUTH_DSN"), sharedDomain.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	return uow
}
func persistAttempt(t *testing.T, uow sharedDomain.UnitOfWork, a *attempt.AuthAttempt) {
	t.Helper()
	if err := uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return NewAuthAttemptPostgresRepository().Create(tx, a) }); err != nil {
		t.Fatal(err)
	}
}
func readAttempt(t *testing.T, uow sharedDomain.UnitOfWork, a *attempt.AuthAttempt) *attempt.AuthAttempt {
	t.Helper()
	tx, err := uow.Query(attemptContext(a))
	if err != nil {
		t.Fatal(err)
	}
	defer sharedDomain.CloseQuery(tx)
	current, err := NewAuthAttemptPostgresRepository().Get(tx, a.Binding)
	if err != nil {
		t.Fatal(err)
	}
	return current
}
func TestAttemptPostgresBindingIsolationAndImmutableFields(t *testing.T) {
	uow := attemptUOW(t)
	repository := NewAuthAttemptPostgresRepository()
	a, b := attemptFixture(t), attemptFixture(t)
	persistAttempt(t, uow, a)
	persistAttempt(t, uow, b)
	for name, binding := range map[string]attempt.Binding{"other": b.Binding, "missing": {ID: uuid.New(), ContinuationHash: a.ContinuationHash}, "wrong credential": {ID: a.ID, ContinuationHash: [32]byte{9}}} {
		t.Run(name, func(t *testing.T) {
			tx, err := uow.Query(attemptContext(a))
			if err != nil {
				t.Fatal(err)
			}
			defer sharedDomain.CloseQuery(tx)
			if _, err = repository.Get(tx, binding); !errors.Is(err, personalErrors.ErrAuthAttemptNotFound) {
				t.Fatal("scope leaked attempt", err)
			}
		})
	}
	if err := uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Create(tx, a) }); !errors.Is(err, personalErrors.ErrAuthAttemptConflict) {
		t.Fatal("duplicate replaced challenge", err)
	}
	for name, change := range map[string]func(*attempt.AuthAttempt){
		"candidate": func(a *attempt.AuthAttempt) { a.Candidate.ID = uuid.New() }, "key": func(a *attempt.AuthAttempt) { a.Candidate.PublicKey = [32]byte{9} }, "nonce": func(a *attempt.AuthAttempt) { a.Nonce = [32]byte{9} }, "phone": func(a *attempt.AuthAttempt) { a.DestinationE164 = "+525599999999" }, "purpose": func(a *attempt.AuthAttempt) { a.Purpose = attempt.Recovery }, "proposed": func(a *attempt.AuthAttempt) { a.ProposedAccountID = uuid.New() }, "request": func(a *attempt.AuthAttempt) { a.RequestHash = [32]byte{9} }, "created": func(a *attempt.AuthAttempt) {
			a.CreatedAt = a.CreatedAt.Add(-time.Second)
			a.ExpiresAt = a.ExpiresAt.Add(-time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			proposal := *a
			change(&proposal)
			proposal.Revision++
			err := uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Save(tx, a.Binding, &proposal, 1) })
			if !errors.Is(err, personalErrors.ErrInvalidAuthAttempt) {
				t.Fatal("immutable binding changed", err)
			}
		})
	}
	if current := readAttempt(t, uow, a); !reflect.DeepEqual(a, current) {
		t.Fatal("binding changed after rejected writes")
	}
	if current := readAttempt(t, uow, b); !reflect.DeepEqual(b, current) {
		t.Fatal("foreign attempt changed")
	}
}

func TestAttemptPostgresDurableUncertaintyCASAndRollback(t *testing.T) {
	uow := attemptUOW(t)
	repository := NewAuthAttemptPostgresRepository()
	a := attemptFixture(t)
	persistAttempt(t, uow, a)
	token, err := a.ReserveSend(uuid.New(), [32]byte{5}, verification.WhatsApp, attemptNow)
	if err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Save(tx, a.Binding, a, 1) }); err != nil {
		t.Fatal(err)
	}
	// Simulate process loss after committing intent, before/after the provider.
	loaded := readAttempt(t, uow, a)
	if loaded.State != attempt.Uncertain || loaded.ProviderOperation.ID != token.ID || loaded.Revision != token.Revision || loaded.ProviderReference != "" {
		t.Fatal("send intent lost")
	}
	forged := *loaded
	forged.State, forged.ProviderOperation, forged.Revision = attempt.Pending, nil, loaded.Revision+1
	if err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error {
		return repository.Save(tx, a.Binding, &forged, loaded.Revision)
	}); !errors.Is(err, personalErrors.ErrInvalidAuthAttempt) {
		t.Fatal("durable operation fingerprint removed", err)
	}
	if _, err = loaded.ReserveSend(uuid.New(), [32]byte{6}, verification.SMS, attemptNow.Add(time.Second)); !errors.Is(err, personalErrors.ErrAuthAttemptUncertain) {
		t.Fatal("implicit resend allowed", err)
	}
	rollback := errors.New("later transaction write failed")
	err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error {
		current, err := repository.GetForUpdate(tx, a.Binding)
		if err != nil {
			return err
		}
		if err = current.ReleaseUncertain(token, attemptNow.Add(time.Minute), time.Minute); err != nil {
			return err
		}
		if err = repository.Save(tx, a.Binding, current, token.Revision); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if current := readAttempt(t, uow, a); current.State != attempt.Uncertain || current.Revision != token.Revision {
		t.Fatal("repository committed own transaction")
	}
	if err = loaded.ReleaseUncertain(token, attemptNow.Add(time.Minute), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Save(tx, a.Binding, loaded, token.Revision) }); err != nil {
		t.Fatal(err)
	}
	// A late external pending outcome started from the stale pre-release copy.
	response := verification.Result{Reference: "VE-fixture", Channel: verification.WhatsApp, State: verification.Pending, CreatedAt: attemptNow, ExpiresAt: attemptNow.Add(5 * time.Minute)}
	if err = a.ApplyOutcome(token, response, attemptNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Save(tx, a.Binding, a, token.Revision) })
	if !errors.Is(err, personalErrors.ErrAuthAttemptConflict) {
		t.Fatal("late external result overwrote released revision", err)
	}
	current := readAttempt(t, uow, a)
	if current.State != attempt.Pending || current.ProviderReference != "" || current.ProviderOperation.ID != token.ID {
		t.Fatal("uncertain fingerprint lost")
	}
}

func TestAttemptPostgresConcurrentReservationOnlyOneWins(t *testing.T) {
	uow := attemptUOW(t)
	repository := NewAuthAttemptPostgresRepository()
	a := attemptFixture(t)
	persistAttempt(t, uow, a)
	start := make(chan struct{})
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		candidate := *a
		go func() {
			<-start
			_, err := candidate.ReserveSend(uuid.New(), [32]byte{5}, verification.WhatsApp, attemptNow)
			if err == nil {
				err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Save(tx, a.Binding, &candidate, 1) })
			}
			done <- err
		}()
	}
	close(start)
	wins, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-done
		if err == nil {
			wins++
		} else if errors.Is(err, personalErrors.ErrAuthAttemptConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("reservations wins=%d conflicts=%d", wins, conflicts)
	}
	current := readAttempt(t, uow, a)
	if current.State != attempt.Uncertain || current.Revision != 2 {
		t.Fatal("invalid concurrent reservation")
	}
}

func TestAttemptPostgresVerifiedBindingAndConsumeAtomic(t *testing.T) {
	authDB := profileTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	uow, err := sharedDomain.NewPersonalPostgresUnitOfWork(authDB, sharedDomain.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	owner := profileFixture(t, authDB)
	other := profileFixture(t, authDB)
	repository := NewAuthAttemptPostgresRepository()
	a := attemptFixture(t)
	a.Purpose = attempt.Recovery
	persistAttempt(t, uow, a)
	send, err := a.ReserveSend(uuid.New(), [32]byte{5}, verification.WhatsApp, attemptNow)
	if err != nil {
		t.Fatal(err)
	}
	response := verification.Result{Reference: "VE-fixture", Channel: verification.WhatsApp, State: verification.Pending, CreatedAt: attemptNow, ExpiresAt: attemptNow.Add(5 * time.Minute)}
	if err = a.ApplyOutcome(send, response, attemptNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	check, err := a.ReserveCheck(uuid.New(), [32]byte{6}, attemptNow.Add(2*time.Second), 3)
	if err != nil {
		t.Fatal(err)
	}
	response.State = verification.Approved
	if err = a.ApplyOutcome(check, response, attemptNow.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Save(tx, a.Binding, a, 1) }); err != nil {
		t.Fatal(err)
	}
	if a.AccountID != nil || a.State != attempt.Verified {
		t.Fatal("OTP created account")
	}
	previous := a.Revision
	if err = a.BindAccount(owner, attemptNow.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Save(tx, a.Binding, a, previous) }); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []*uuid.UUID{nil, &other} {
		proposal := *a
		proposal.AccountID = replacement
		proposal.Revision++
		err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Save(tx, a.Binding, &proposal, a.Revision) })
		if !errors.Is(err, personalErrors.ErrInvalidAuthAttempt) {
			t.Fatal("bound account changed", err)
		}
	}
	previous = a.Revision
	result := uuid.New()
	if err = a.Consume(result, attemptNow.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Save(tx, a.Binding, a, previous) }); err != nil {
		t.Fatal(err)
	}
	current := readAttempt(t, uow, a)
	if current.State != attempt.Consumed || *current.AccountID != owner || *current.ResultReference != result || current.ProposedAccountID == owner {
		t.Fatal("recovery identity or result lost")
	}
	// Even a hand-built valid pending entity cannot revive a stored terminal row.
	forged := *current
	forged.State = attempt.Pending
	forged.VerifiedAt = nil
	forged.ConsumedAt = nil
	forged.ResultReference = nil
	forged.Revision++
	err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error {
		return repository.Save(tx, a.Binding, &forged, current.Revision)
	})
	if !errors.Is(err, personalErrors.ErrAuthAttemptState) {
		t.Fatal("persisted terminal revived", err)
	}
}

func TestAttemptPostgresProviderExpiryAndDiagnostics(t *testing.T) {
	var diagnostics bytes.Buffer
	db := profileTestPool(t, "MANCUERNA_TEST_AUTH_DSN").Session(&gorm.Session{Logger: logger.New(log.New(&diagnostics, "", 0), logger.Config{LogLevel: logger.Info})})
	uow, err := sharedDomain.NewPersonalPostgresUnitOfWork(db, sharedDomain.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	repository := NewAuthAttemptPostgresRepository()
	broken := attemptFixture(t)
	missing := uuid.New()
	broken.AccountID = &missing
	err = uow.Command(attemptContext(broken), func(tx sharedDomain.Transaction) error { return repository.Create(tx, broken) })
	if err == nil {
		t.Fatal("fixture foreign key unexpectedly accepted")
	}
	if strings.Contains(err.Error(), missing.String()) || strings.Contains(err.Error(), broken.DestinationE164) {
		t.Fatal("database detail exposed")
	}
	a := attemptFixture(t)
	persistAttempt(t, uow, a)
	token, err := a.ReserveSend(uuid.New(), [32]byte{5}, verification.WhatsApp, attemptNow)
	if err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Save(tx, a.Binding, a, 1) }); err != nil {
		t.Fatal(err)
	}
	response := verification.Result{Reference: "VE-fixture-expired", Channel: verification.WhatsApp, State: verification.Pending, CreatedAt: attemptNow.Add(-11 * time.Minute), ExpiresAt: attemptNow.Add(-time.Minute)}
	if err = a.ApplyOutcome(token, response, attemptNow.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(attemptContext(a), func(tx sharedDomain.Transaction) error { return repository.Save(tx, a.Binding, a, token.Revision) }); err != nil {
		t.Fatal(err)
	}
	current := readAttempt(t, uow, a)
	if current.State != attempt.Expired || current.VerifiedAt != nil || !current.ExpiresAt.Equal(response.ExpiresAt) {
		t.Fatal("provider expiry lost on disk")
	}
	for _, private := range []string{a.DestinationE164, a.Candidate.Label, response.Reference} {
		if strings.Contains(diagnostics.String(), private) {
			t.Fatal("SQL diagnostics disclosed attempt data")
		}
	}
}
