//go:build server

package repositories

import (
	"encoding/hex"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/attempt"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/phonechange"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/security"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type PhoneChangeAttemptPostgresResolver struct{}

func NewPhoneChangeAttemptResolver() *PhoneChangeAttemptPostgresResolver {
	return &PhoneChangeAttemptPostgresResolver{}
}

var _ repository.PhoneChangeAttemptResolver = (*PhoneChangeAttemptPostgresResolver)(nil)

func (*PhoneChangeAttemptPostgresResolver) Resolve(h shared.Transaction, b attempt.Binding) (actor security.AccessClaims, err error) {
	defer func() {
		if err != nil {
			actor = security.AccessClaims{}
			err = phoneChangeError(err)
		}
	}()
	t, ok := h.(*shared.GormTransaction)
	if !ok || t == nil || t.Tx == nil || t.Tx.Statement == nil || b.Validate() != nil {
		return actor, shared.ErrPersonalScope
	}
	if _, ok = t.Tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return actor, shared.ErrPersonalScope
	}
	tx := t.Tx.Session(&gorm.Session{Logger: logger.Discard})
	var allowed bool
	err = tx.Raw(`SELECT coalesce(current_setting('tinta.gym_id',true),'')='' AND coalesce(current_setting('mancuerna.account_id',true),'')=''
 AND current_setting('mancuerna.attempt_id',true)=? AND current_setting('mancuerna.attempt_hash',true)=?
 AND current_setting('transaction_isolation')='read committed'
 AND pg_has_role(current_user,'mancuerna_auth','USAGE') AND NOT pg_has_role(current_user,'mancuerna_data','MEMBER') AND NOT pg_has_role(current_user,'mancuerna_identity','MEMBER')`, b.ID.String(), hex.EncodeToString(b.ContinuationHash[:])).Scan(&allowed).Error
	if err != nil {
		return actor, err
	}
	if !allowed {
		return actor, shared.ErrPersonalScope
	}
	a, err := NewAuthAttemptPostgresRepository().Get(h, b)
	if err != nil {
		return actor, err
	}
	if a.Purpose != attempt.PhoneChange || a.AccountID == nil || *a.AccountID == uuid.Nil {
		return actor, personalErrors.ErrAuthAttemptNotFound
	}
	var rows []models.AuthReceiptModel
	err = tx.Where(`account_id=? AND attempt_id=? AND purpose=? AND request_hash=?`, *a.AccountID, a.ID, phonechange.StartPurpose, a.RequestHash[:]).Limit(2).Find(&rows).Error
	if err != nil {
		return actor, err
	}
	if len(rows) != 1 {
		return actor, personalErrors.ErrAuthAttemptNotFound
	}
	r, err := receiptToDomain(rows[0])
	if err != nil {
		return actor, err
	}
	actor = security.AccessClaims{AccountID: *a.AccountID, SessionID: r.ResultReference, DeviceID: a.Candidate.ID}
	if phonechange.MatchInitiator(a, r, actor) != nil {
		return security.AccessClaims{}, personalErrors.ErrAuthAttemptNotFound
	}
	return actor, nil
}
