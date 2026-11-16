//go:build server

package repositories

import (
	"context"
	"errors"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/phonechange"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type PersonalAuthResultPostgresRepository struct{}

func NewPersonalAuthResultRepository() *PersonalAuthResultPostgresRepository {
	return &PersonalAuthResultPostgresRepository{}
}

var _ repository.PersonalAuthResultRepository = (*PersonalAuthResultPostgresRepository)(nil)

func (*PersonalAuthResultPostgresRepository) GetAccountResult(h shared.Transaction, accountID, op uuid.UUID) (out repository.AccountAuthResult, err error) {
	defer func() {
		if err != nil {
			out = repository.AccountAuthResult{}
			err = personalAuthResultError(err)
		}
	}()
	if accountID == uuid.Nil || op == uuid.Nil {
		return out, personalErrors.ErrReceiptNotFound
	}
	tx, err := authSessionConnection(h, accountID, false)
	if err != nil {
		return out, err
	}
	var pure bool
	if err = tx.Raw(`SELECT coalesce(current_setting('mancuerna.attempt_id',true),'')='' AND coalesce(current_setting('mancuerna.attempt_hash',true),'')=''`).Scan(&pure).Error; err != nil {
		return out, err
	}
	if !pure {
		return out, shared.ErrPersonalScope
	}
	var row models.AuthReceiptModel
	err = tx.Where(`account_id=? AND operation_id=? AND purpose IN ('phone_change_complete','recovery_key_rotate','devices_revoke')`, accountID, op).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return out, personalErrors.ErrReceiptNotFound
	}
	if err != nil {
		return out, err
	}
	out.Receipt, err = receiptToDomain(row)
	if err != nil {
		return out, err
	}
	if out.Receipt.Purpose == "devices_revoke" {
		if out.Receipt.Owner.AttemptID != uuid.Nil || out.Receipt.ResultReference == uuid.Nil {
			return out, personalErrors.ErrReceiptNotFound
		}
		// Validate only the historical identity, never its current authority or
		// credentials. Revoking self/all legitimately retires this session.
		var originalSession bool
		err = tx.Raw(`SELECT EXISTS(SELECT 1 FROM mancuerna.sessions WHERE account_id=? AND id=?)`, accountID, out.Receipt.ResultReference).Scan(&originalSession).Error
		if err != nil {
			return out, err
		}
		if !originalSession {
			return out, personalErrors.ErrReceiptNotFound
		}
	}
	if out.Receipt.Purpose == phonechange.CompletePurpose {
		if out.Receipt.Owner.AttemptID == uuid.Nil {
			return out, personalErrors.ErrReceiptNotFound
		}
		err = tx.Raw(`SELECT EXISTS(SELECT 1 FROM mancuerna.auth_challenges c WHERE c.account_id=? AND c.id=? AND c.purpose='phone_change' AND c.status='consumed' AND c.consumed_at IS NOT NULL AND c.result_reference=? AND c.provider_operation_kind='check' AND c.provider_operation_id=? AND c.provider_operation_hash=? AND c.verified_at IS NOT NULL AND c.provider_reference IS NOT NULL)`, accountID, out.Receipt.Owner.AttemptID, op, op, out.Receipt.RequestHash[:]).Scan(&out.PhoneChangeCommitted).Error
	}
	return out, err
}
func personalAuthResultError(err error) error {
	for _, allowed := range []error{personalErrors.ErrReceiptNotFound, personalErrors.ErrAuthSessionScope, shared.ErrPersonalScope, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	return shared.NewUnexpectedError(errors.New("no se pudo consultar el resultado personal"))
}
