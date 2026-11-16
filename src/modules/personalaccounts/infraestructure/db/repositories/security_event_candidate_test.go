//go:build server

package repositories

import (
	"errors"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
)

func TestSecurityEventRequiresAuthorizedActorExceptExactCandidateRequest(t *testing.T) {
	for _, kind := range []string{"signup", "device_authorization_approved", "device_authorization_denied", "vault_created", "device_authorization_requested_extra"} {
		err := NewSecurityEventRepository().Append(nil, repository.SecurityEvent{ID: uuid.New(), AccountID: uuid.New(), OperationID: uuid.New(), Type: kind, OccurredAt: time.Now()})
		var classified shared.CustomError
		if !errors.As(err, &classified) || classified.ErrorCode != shared.CodeValidation {
			t.Fatal("nil actor reached persistence for protected event")
		}
	}
}
