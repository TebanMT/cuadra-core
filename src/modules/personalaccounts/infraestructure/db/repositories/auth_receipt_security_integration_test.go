//go:build server && integration

package repositories

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/receipt"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAuthReceiptPostgresSQLDiagnosticsCannotExposeResponse(t *testing.T) {
	var diagnostics bytes.Buffer
	db := profileTestPool(t, "MANCUERNA_TEST_AUTH_DSN").Session(&gorm.Session{Logger: logger.New(log.New(&diagnostics, "", 0), logger.Config{LogLevel: logger.Info})})
	uow, err := shared.NewPersonalPostgresUnitOfWork(db, shared.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	a := attemptFixture(t)
	persistAttempt(t, uow, a)
	repository := NewAuthReceiptPostgresRepository()
	responseMarker := "synthetic-ciphertext-must-not-be-logged"
	keyMarker := "synthetic-receipt-key-marker"
	broken, err := receipt.New(uuid.New(), receipt.Owner{AttemptID: a.ID, AccountID: uuid.New()}, "receipt_security_test", [32]byte{1}, attemptNow)
	if err != nil {
		t.Fatal(err)
	}
	if err = broken.SetResponse(security.Envelope{Ciphertext: []byte(responseMarker), KeyVersion: keyMarker}, attemptNow.Add(time.Minute), uuid.New()); err != nil {
		t.Fatal(err)
	}
	err = uow.Command(attemptContext(a), func(tx shared.Transaction) error { return repository.Create(tx, broken) })
	if err == nil {
		t.Fatal("fixture missing-account foreign key accepted")
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) || strings.Contains(err.Error(), responseMarker) || strings.Contains(err.Error(), keyMarker) {
		t.Fatal("raw PostgreSQL failed row escaped repository")
	}
	r, err := receipt.New(uuid.New(), receipt.Owner{AttemptID: a.ID}, "receipt_security_test", [32]byte{2}, attemptNow)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.SetResponse(security.Envelope{Ciphertext: []byte(responseMarker), KeyVersion: keyMarker}, attemptNow.Add(time.Minute), a.ID); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(attemptContext(a), func(tx shared.Transaction) error { return repository.Create(tx, r) }); err != nil {
		t.Fatal(err)
	}
	if err = uow.Command(attemptContext(a), func(tx shared.Transaction) error { return repository.Create(tx, r) }); !errors.Is(err, personalErrors.ErrIdempotencyMismatch) {
		t.Fatal("duplicate receipt lost stable classification", err)
	}
	if err = uow.Command(attemptContext(a), func(tx shared.Transaction) error {
		loaded, err := repository.Get(tx, r.Owner, r.OperationID)
		if err != nil {
			return err
		}
		return repository.SaveResponse(tx, loaded)
	}); err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{responseMarker, keyMarker} {
		if strings.Contains(diagnostics.String(), private) {
			t.Fatal("GORM SQL logger disclosed protected receipt content")
		}
	}
}
