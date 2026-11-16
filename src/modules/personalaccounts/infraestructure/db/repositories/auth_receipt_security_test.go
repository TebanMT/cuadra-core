//go:build server

package repositories

import (
	"errors"
	"strings"
	"testing"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAuthReceiptStorageDiagnosticsAreRedacted(t *testing.T) {
	private := "synthetic encrypted response and key version"
	for _, failure := range []error{&pgconn.PgError{Code: "23503", Message: private, Detail: private, ConstraintName: private}, errors.New(private)} {
		err := receiptStorageError(failure)
		if strings.Contains(err.Error(), private) {
			t.Fatal("private storage diagnostic exposed")
		}
		var underlying *pgconn.PgError
		if errors.As(err, &underlying) || errors.Is(err, failure) {
			t.Fatal("raw storage failure retained")
		}
		var classified shared.CustomError
		if !errors.As(err, &classified) || classified.ErrorCode != shared.CodeUnexpected {
			t.Fatal("storage error classification lost")
		}
	}
	if !errors.Is(receiptStorageError(&pgconn.PgError{Code: "23505", Detail: private}), personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("unique conflict no longer idempotency mismatch")
	}
}
