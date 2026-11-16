//go:build server

package repositories

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"regexp"
	"time"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	port "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type IdentityAccessRepository struct{ phoneKey []byte }

func NewIdentityAccessRepository(phoneHMACKey []byte) (*IdentityAccessRepository, error) {
	if len(phoneHMACKey) < 32 {
		return nil, personalErrors.ErrAccessIdentityConfiguration
	}
	return &IdentityAccessRepository{phoneKey: append([]byte(nil), phoneHMACKey...)}, nil
}

func (*IdentityAccessRepository) String() string     { return "access identity repository (key redacted)" }
func (r *IdentityAccessRepository) GoString() string { return r.String() }

var _ port.IdentityAccessRepository = (*IdentityAccessRepository)(nil)
var accessPhoneFormat = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// This projection is deliberately narrower than the mutable AuthAttempt
// aggregate. Resolving identity neither consumes it nor changes its binding.
type accessChallenge struct {
	ID                uuid.UUID
	AccountID         *uuid.UUID
	ProposedAccountID uuid.UUID
	DestinationE164   string
	VerifiedAt        *time.Time
	Usable            bool
}

func (r *IdentityAccessRepository) ResolveVerifiedAccess(handle shared.Transaction, attemptID uuid.UUID) (bound port.BoundAccess, err error) {
	defer func() {
		if err != nil {
			bound = port.BoundAccess{}
			err = accessIdentityPersistenceError(err)
		}
	}()
	if r == nil || len(r.phoneKey) < 32 {
		return port.BoundAccess{}, personalErrors.ErrAccessIdentityConfiguration
	}
	transaction, ok := handle.(*shared.GormTransaction)
	if !ok || transaction == nil || transaction.Tx == nil || transaction.Tx.Statement == nil || attemptID == uuid.Nil {
		return port.BoundAccess{}, personalErrors.ErrAccessIdentityScope
	}
	if _, active := transaction.Tx.Statement.ConnPool.(gorm.TxCommitter); !active {
		return port.BoundAccess{}, personalErrors.ErrAccessIdentityScope
	}
	// Query tracing must not interpolate the phone or continuation verifier.
	// Session preserves the supplied SQL transaction; no new connection opens.
	tx := transaction.Tx.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	hash, err := accessAttemptScope(tx, attemptID)
	if err != nil {
		return port.BoundAccess{}, err
	}
	before, err := readAccessChallenge(tx, attemptID, hash, false)
	if err != nil {
		return port.BoundAccess{}, err
	}
	// The bigint advisory namespace is distinct from two-int OTP budgets.
	// Phone locks precede every account or challenge row lock.
	if err = tx.Exec("SELECT pg_advisory_xact_lock(?)", r.phoneLockKey(before.DestinationE164)).Error; err != nil {
		return port.BoundAccess{}, err
	}
	var resolved uuid.NullUUID
	if err = tx.Raw("SELECT mancuerna_private.resolve_identity('phone', ?)", before.DestinationE164).Row().Scan(&resolved); err != nil {
		return port.BoundAccess{}, err
	}
	result := port.BoundAccess{AccountID: resolved.UUID, Created: !resolved.Valid}
	if !resolved.Valid {
		if before.AccountID != nil {
			return port.BoundAccess{}, personalErrors.ErrAccessIdentityUnavailable
		}
		result.AccountID = before.ProposedAccountID
	} else if before.AccountID != nil && *before.AccountID != result.AccountID {
		return port.BoundAccess{}, personalErrors.ErrAccessIdentityUnavailable
	}
	if err = tx.Exec("SELECT set_config('mancuerna.account_id', ?, true)", result.AccountID.String()).Error; err != nil {
		return port.BoundAccess{}, err
	}
	if result.Created {
		// A collision on either proposed UUID or phone is never interpreted as
		// permission to adopt the existing account or authorize its candidate.
		var inserted uuid.UUID
		err = tx.Raw(`INSERT INTO mancuerna.accounts(id,phone_e164,phone_verified_at)
 VALUES(?,?,?) ON CONFLICT DO NOTHING RETURNING id`, result.AccountID, before.DestinationE164, before.VerifiedAt).Row().Scan(&inserted)
		if errors.Is(err, sql.ErrNoRows) {
			return port.BoundAccess{}, personalErrors.ErrAccessIdentityUnavailable
		}
		if err != nil {
			return port.BoundAccess{}, err
		}
		if err = tx.Exec("INSERT INTO mancuerna.sync_streams(account_id,epoch) VALUES(?,?)", result.AccountID, uuid.New()).Error; err != nil {
			return port.BoundAccess{}, err
		}
	}
	var locked uuid.UUID
	// This is the initial account lock for an attempt-scoped transaction.
	// Match the UoW's MVCC touch so waiting RR snapshots cannot retain old
	// authority after an identity mutation committed.
	err = tx.Raw("UPDATE mancuerna.sync_streams SET last_sequence=last_sequence WHERE account_id=? RETURNING account_id", result.AccountID).Row().Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return port.BoundAccess{}, personalErrors.ErrAccessIdentityUnavailable
	}
	if err != nil {
		return port.BoundAccess{}, err
	}
	var active bool
	if err = tx.Raw(`SELECT EXISTS(SELECT 1 FROM mancuerna.accounts WHERE id=? AND phone_e164=? AND status='active' AND deleted_at IS NULL)`, result.AccountID, before.DestinationE164).Scan(&active).Error; err != nil {
		return port.BoundAccess{}, err
	}
	if !active {
		return port.BoundAccess{}, personalErrors.ErrAccessIdentityUnavailable
	}
	after, err := readAccessChallenge(tx, attemptID, hash, true)
	if err != nil {
		return port.BoundAccess{}, err
	}
	if before.ProposedAccountID != after.ProposedAccountID || before.DestinationE164 != after.DestinationE164 || !before.VerifiedAt.Equal(*after.VerifiedAt) || (after.AccountID != nil && *after.AccountID != result.AccountID) {
		return port.BoundAccess{}, personalErrors.ErrAccessIdentityUnavailable
	}
	return result, nil
}

func accessAttemptScope(tx *gorm.DB, attemptID uuid.UUID) ([32]byte, error) {
	var scope struct {
		Gym       string
		Account   string
		Attempt   string
		Hash      string
		Isolation string
		ReadOnly  string
		AuthOnly  bool
	}
	err := tx.Raw(`SELECT coalesce(current_setting('tinta.gym_id',true),'') AS gym,
 coalesce(current_setting('mancuerna.account_id',true),'') AS account,
 coalesce(current_setting('mancuerna.attempt_id',true),'') AS attempt,
 coalesce(current_setting('mancuerna.attempt_hash',true),'') AS hash,
 current_setting('transaction_isolation') AS isolation, current_setting('transaction_read_only') AS read_only,
 (pg_has_role(current_user,'mancuerna_auth','USAGE') AND NOT pg_has_role(current_user,'mancuerna_data','MEMBER')
 AND NOT pg_has_role(current_user,'mancuerna_identity','MEMBER')) AS auth_only`).Scan(&scope).Error
	if err != nil {
		return [32]byte{}, err
	}
	hashBytes, hashErr := hex.DecodeString(scope.Hash)
	var hash [32]byte
	copy(hash[:], hashBytes)
	if scope.Gym != "" || scope.Account != "" || scope.Attempt != attemptID.String() || hashErr != nil || len(hashBytes) != 32 || hash == [32]byte{} || !scope.AuthOnly || scope.Isolation != "read committed" || scope.ReadOnly != "off" {
		return [32]byte{}, personalErrors.ErrAccessIdentityScope
	}
	return hash, nil
}

func readAccessChallenge(tx *gorm.DB, id uuid.UUID, hash [32]byte, lock bool) (accessChallenge, error) {
	query := `SELECT id,account_id,proposed_account_id,destination_e164,verified_at,
 (purpose='access' AND status='verified' AND expires_at>clock_timestamp() AND verified_at IS NOT NULL
 AND verified_at>=created_at AND verified_at<=clock_timestamp() AND verified_at<expires_at
 AND provider_reference IS NOT NULL AND provider_reference<>'' AND consumed_at IS NULL) AS usable
 FROM mancuerna.auth_challenges WHERE id=? AND continuation_hash=?`
	if lock {
		query += " FOR UPDATE"
	}
	var challenge accessChallenge
	result := tx.Raw(query, id, hash[:]).Scan(&challenge)
	if result.Error != nil {
		return accessChallenge{}, result.Error
	}
	if result.RowsAffected != 1 || !challenge.Usable || challenge.ProposedAccountID == uuid.Nil || challenge.VerifiedAt == nil || !accessPhoneFormat.MatchString(challenge.DestinationE164) {
		return accessChallenge{}, personalErrors.ErrAccessIdentityUnavailable
	}
	return challenge, nil
}

func (r *IdentityAccessRepository) phoneLockKey(phone string) int64 {
	h := hmac.New(sha256.New, r.phoneKey)
	_, _ = h.Write([]byte("mancuerna:identity:phone-lock:v1\x00" + phone))
	return int64(binary.BigEndian.Uint64(h.Sum(nil)[:8]))
}

func accessIdentityPersistenceError(err error) error {
	for _, allowed := range []error{personalErrors.ErrAccessIdentityConfiguration, personalErrors.ErrAccessIdentityScope, personalErrors.ErrAccessIdentityUnavailable, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	return shared.NewUnexpectedError(errors.New("no se pudo resolver la identidad de acceso"))
}
