//go:build server

package repositories

import (
	"errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"regexp"
)

type SecurityEventPostgresRepository struct{}

func NewSecurityEventRepository() *SecurityEventPostgresRepository {
	return &SecurityEventPostgresRepository{}
}

var eventKind = regexp.MustCompile(`^[a-z][a-z0-9_]{0,79}$`)

func (*SecurityEventPostgresRepository) Append(tx shared.Transaction, e repository.SecurityEvent) error {
	if e.ID == uuid.Nil || e.AccountID == uuid.Nil || e.OperationID == uuid.Nil || !eventKind.MatchString(e.Type) || e.OccurredAt.IsZero() || (e.ActorDeviceID == uuid.Nil && e.Type != "device_authorization_requested") {
		return shared.NewValidationError(errors.New("evento de seguridad inválido"))
	}
	db, err := authSessionConnection(tx, e.AccountID, true)
	if err != nil {
		return authSessionPersistenceError(err)
	}
	var actor any = e.ActorDeviceID
	if e.ActorDeviceID == uuid.Nil {
		// A requesting installation is not an authorized actor yet. Permit this
		// one event only after its exact request/receipt has been created under
		// both account and continuation scopes in the caller's transaction.
		var allowed bool
		err = db.Raw(`SELECT EXISTS(SELECT 1 FROM mancuerna.auth_challenges c
 JOIN mancuerna.accounts owner ON owner.id=c.account_id
 JOIN mancuerna.device_authorizations a ON a.account_id=c.account_id AND a.challenge_id=c.id
 JOIN mancuerna.auth_receipts r ON r.account_id=c.account_id AND r.attempt_id=c.id AND r.result_reference=a.id
 WHERE c.account_id=? AND c.id=nullif(current_setting('mancuerna.attempt_id',true),'')::uuid
 AND c.continuation_hash=decode(coalesce(current_setting('mancuerna.attempt_hash',true),''),'hex')
 AND c.purpose='access' AND c.status='verified' AND c.verified_at IS NOT NULL AND c.expires_at>clock_timestamp()
 AND owner.status='active' AND owner.deleted_at IS NULL AND owner.phone_e164=c.destination_e164
 AND a.status='pending' AND a.expires_at>clock_timestamp()
 AND r.operation_id=? AND r.purpose='device_authorization_start' AND r.completed_at=? AND ?<=clock_timestamp())`, e.AccountID, e.OperationID, e.OccurredAt.UTC(), e.OccurredAt.UTC()).Scan(&allowed).Error
		if err != nil {
			return shared.NewUnexpectedError(errors.New("no se pudo comprobar el evento de seguridad"))
		}
		if !allowed {
			return shared.NewValidationError(errors.New("evento de seguridad inválido"))
		}
		actor = nil
	}
	if err = db.Exec(`INSERT INTO mancuerna.security_events(id,account_id,actor_device_id,event_type,request_id,occurred_at) VALUES(?,?,?,?,?,?)`, e.ID, e.AccountID, actor, e.Type, e.OperationID, e.OccurredAt.UTC()).Error; err != nil {
		return shared.NewUnexpectedError(errors.New("no se pudo guardar el evento de seguridad"))
	}
	return nil
}
