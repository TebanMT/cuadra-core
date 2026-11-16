//go:build server

package repositories

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	"github.com/google/uuid"
)

var attemptNow = time.Date(2026, 10, 19, 7, 0, 0, 123000000, time.UTC)

func attemptFixture(t *testing.T) *attempt.AuthAttempt {
	t.Helper()
	a, err := attempt.New(attempt.NewAttempt{Binding: attempt.Binding{ID: uuid.New(), ContinuationHash: [32]byte{1}}, ProposedAccountID: uuid.New(), Candidate: attempt.Candidate{ID: uuid.New(), PublicKey: [32]byte{2}, Platform: "android", Label: "Fixture teléfono"}, Nonce: [32]byte{3}, Purpose: attempt.Access, DestinationE164: "+525512345678", Channel: verification.WhatsApp, RequestHash: [32]byte{4}}, attemptNow, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAuthAttemptMappingRoundTripAndCopies(t *testing.T) {
	a := attemptFixture(t)
	if _, err := a.ReserveSend(uuid.New(), [32]byte{5}, verification.WhatsApp, attemptNow); err != nil {
		t.Fatal(err)
	}
	row := attemptToModel(a)
	roundtrip, err := attemptToDomain(&row)
	if err != nil || !reflect.DeepEqual(a, roundtrip) {
		t.Fatal("mapping changed durable binding", err)
	}
	row.ContinuationHash[0] = 9
	row.Nonce[0] = 9
	row.CandidatePublicKey[0] = 9
	row.RequestHash[0] = 9
	row.ProviderOperationHash[0] = 9
	*row.ProviderOperationID = uuid.New()
	if !reflect.DeepEqual(a, roundtrip) {
		t.Fatal("model aliases domain")
	}
	row = attemptToModel(a)
	if row.CreatedAt.Location() != time.UTC || row.ExpiresAt.Location() != time.UTC || row.ProviderOperationStartedAt.Location() != time.UTC {
		t.Fatal("not UTC")
	}
	for name, change := range map[string]func(*models.AuthAttemptModel){
		"continuation": func(m *models.AuthAttemptModel) { m.ContinuationHash = m.ContinuationHash[:31] }, "nonce": func(m *models.AuthAttemptModel) { m.Nonce = append(m.Nonce, 1) }, "key": func(m *models.AuthAttemptModel) { m.CandidatePublicKey = nil }, "request": func(m *models.AuthAttemptModel) { m.RequestHash = nil },
		"operation id": func(m *models.AuthAttemptModel) { m.ProviderOperationID = nil }, "operation hash": func(m *models.AuthAttemptModel) { m.ProviderOperationHash = nil }, "operation kind": func(m *models.AuthAttemptModel) { m.ProviderOperationKind = nil }, "operation time": func(m *models.AuthAttemptModel) { m.ProviderOperationStartedAt = nil },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := attemptToModel(a)
			change(&invalid)
			if _, err := attemptToDomain(&invalid); !errors.Is(err, personalErrors.ErrInvalidAuthAttempt) {
				t.Fatal(err)
			}
		})
	}
}
