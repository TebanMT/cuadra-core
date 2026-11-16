//go:build server

package repositories

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	deviceDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/device"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	sessionDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/session"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAuthSessionMappingCopiesCredentialBytes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	d, _ := deviceDomain.NewAuthorizedDevice(uuid.New(), attempt.Candidate{ID: uuid.New(), PublicKey: [32]byte{1}, Platform: "ios", Label: "Synthetic"}, deviceDomain.Signup, now)
	model := deviceToModel(d)
	copy, err := deviceToDomain(model)
	if err != nil || copy.PublicKey != d.PublicKey || copy.ID != d.ID {
		t.Fatal("device mapping lost candidate")
	}
	model.PublicKey[0] = 9
	if copy.PublicKey[0] != 1 || d.PublicKey[0] != 1 {
		t.Fatal("device key mapping aliased source")
	}
	s, _ := sessionDomain.NewSession(d.AccountID, d.ID, uuid.New(), [32]byte{2}, uuid.New(), [32]byte{3}, now)
	row := sessionToModel(s)
	mapped, err := sessionToDomain(row)
	if err != nil || mapped.RefreshHash != s.RefreshHash || mapped.RevocationTicketHash != s.RevocationTicketHash || !mapped.ExpiresAt.Equal(s.ExpiresAt) {
		t.Fatal("session mapping lost verifiers or expiry")
	}
	row.RefreshHash[0] = 9
	if mapped.RefreshHash[0] != 2 || s.RefreshHash[0] != 2 {
		t.Fatal("session hash mapping aliased source")
	}
	row.RefreshHash = row.RefreshHash[:31]
	if _, err := sessionToDomain(row); !errors.Is(err, personalErrors.ErrInvalidSession) {
		t.Fatal("invalid stored verifier silently padded")
	}
}

func TestAuthPersistenceErrorsNeverRetainProviderOrSQLDetail(t *testing.T) {
	private := "private destination +525500000099 and credential verifier"
	upstream := &pgconn.PgError{Code: "23514", Message: private, Detail: private, Where: private}
	for _, sanitize := range []func(error) error{accessIdentityPersistenceError, authSessionPersistenceError, recoveryPersistenceError} {
		err := sanitize(upstream)
		for cause := err; cause != nil; cause = errors.Unwrap(cause) {
			if strings.Contains(cause.Error(), private) || errors.Is(cause, upstream) {
				t.Fatal("sanitized error retained upstream details")
			}
		}
	}
	if !errors.Is(accessIdentityPersistenceError(personalErrors.ErrAccessIdentityScope), personalErrors.ErrAccessIdentityScope) || !errors.Is(authSessionPersistenceError(personalErrors.ErrAuthObjectNotFound), personalErrors.ErrAuthObjectNotFound) {
		t.Fatal("sanitization lost safe error identity")
	}
}
