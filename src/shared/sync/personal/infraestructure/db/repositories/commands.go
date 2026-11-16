//go:build server

package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	crypto "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/security"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/cuadra/cuadra-core/src/shared/sync/personal/domain/repository"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Commands struct{}

func NewCommands() *Commands { return &Commands{} }

var _ repository.Commands = (*Commands)(nil)

// This guard deliberately accepts only the data role. It never resolves an
// identity or reads authentication secrets. All writes share the account lock.
func dataConnection(handle shared.Transaction, accountID uuid.UUID, write bool) (*gorm.DB, error) {
	transaction, ok := handle.(*shared.GormTransaction)
	if !ok || transaction == nil || transaction.Tx == nil || transaction.Tx.Statement == nil || !protocol.ValidID(accountID) {
		return nil, protocol.ErrCommandScope
	}
	if _, ok = transaction.Tx.Statement.ConnPool.(gorm.TxCommitter); !ok {
		return nil, protocol.ErrCommandScope
	}
	tx := transaction.Tx.Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent), SkipDefaultTransaction: true})
	var scope struct {
		Account, Gym, Attempt, AttemptHash, Isolation, ReadOnly string
		DataOnly                                                bool
	}
	err := tx.Raw(`SELECT coalesce(current_setting('mancuerna.account_id',true),'') AS account,
 coalesce(current_setting('tinta.gym_id',true),'') AS gym,coalesce(current_setting('mancuerna.attempt_id',true),'') AS attempt,
 coalesce(current_setting('mancuerna.attempt_hash',true),'') AS attempt_hash,current_setting('transaction_isolation') AS isolation,
 current_setting('transaction_read_only') AS read_only,(pg_has_role(current_user,'mancuerna_data','USAGE')
 AND NOT pg_has_role(current_user,'mancuerna_auth','MEMBER') AND NOT pg_has_role(current_user,'mancuerna_identity','MEMBER')) AS data_only`).Scan(&scope).Error
	if err != nil {
		return nil, err
	}
	if scope.Account != accountID.String() || scope.Gym != "" || scope.Attempt != "" || scope.AttemptHash != "" || !scope.DataOnly || scope.Isolation != "read committed" || (write && scope.ReadOnly != "off") {
		return nil, protocol.ErrCommandScope
	}
	if write {
		var locked uuid.UUID
		if err = tx.Raw("SELECT account_id FROM mancuerna.sync_streams WHERE account_id=? FOR UPDATE", accountID).Row().Scan(&locked); err != nil {
			return nil, err
		}
	}
	return tx, nil
}
func (*Commands) RequireActor(handle shared.Transaction, actor protocol.Actor, origin uuid.UUID) (err error) {
	defer func() { err = persistenceError(err) }()
	if !protocol.ValidID(actor.SessionID) || !protocol.ValidID(actor.DeviceID) || !protocol.ValidID(origin) {
		return protocol.ErrCommandInvalid
	}
	tx, err := dataConnection(handle, actor.AccountID, true)
	if err != nil {
		return err
	}
	var active bool
	if err = tx.Raw("SELECT mancuerna_private.session_active(?,?)", actor.SessionID, actor.DeviceID).Scan(&active).Error; err != nil {
		return err
	}
	if !active {
		return protocol.ErrCommandActor
	}
	// Origin is provenance, not authority. A revoked/deleted originating device
	// can be preserved when the same account recovers its offline operations.
	if err = tx.Raw("SELECT EXISTS(SELECT 1 FROM mancuerna.devices WHERE account_id=? AND id=?)", actor.AccountID, origin).Scan(&active).Error; err != nil {
		return err
	}
	if !active {
		return protocol.ErrCommandOrigin
	}
	return nil
}
func (*Commands) GetReceipt(handle shared.Transaction, accountID, operationID uuid.UUID) (out *protocol.Receipt, err error) {
	defer func() {
		err = persistenceError(err)
		if err != nil {
			out = nil
		}
	}()
	tx, err := dataConnection(handle, accountID, false)
	if err != nil {
		return nil, err
	}
	var hash, raw []byte
	var captured sql.NullString
	r := &protocol.Receipt{}
	err = tx.Raw(`SELECT operation_id,account_id,actor_device_id,origin_device_id,request_hash,command_type,result,committed_at,captured_at
 FROM mancuerna.operation_receipts WHERE account_id=? AND operation_id=?`, accountID, operationID).Row().Scan(&r.OperationID, &r.AccountID, &r.ActorDeviceID, &r.OriginDeviceID, &hash, &r.CommandType, &raw, &r.CommittedAt, &captured)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, protocol.ErrOperationMissing
	}
	if err != nil {
		return nil, err
	}
	if len(hash) != 32 || json.Unmarshal(raw, &r.Result) != nil || r.Result.OperationID != operationID.String() {
		return nil, protocol.ErrCommandPersistence
	}
	copy(r.RequestHash[:], hash)
	if captured.Valid {
		if _, err := protocol.ParseCapturedAt(captured.String); err != nil {
			return nil, protocol.ErrCommandPersistence
		}
		r.CapturedAt = captured.String
	}
	return r, nil
}
func (*Commands) SaveConflict(handle shared.Transaction, c protocol.Conflict) (err error) {
	defer func() { err = persistenceError(err) }()
	if !protocol.ValidID(c.ID) || !protocol.ValidID(c.OperationID) || !protocol.ValidID(c.EntityID) || c.BaseRevision < 0 || c.CurrentRevision < 0 || c.CreatedAt.IsZero() || !json.Valid(c.Proposal) || len(c.Proposal) > 1048576 {
		return protocol.ErrCommandInvalid
	}
	tx, err := dataConnection(handle, c.AccountID, true)
	if err != nil {
		return err
	}
	return tx.Exec(`INSERT INTO mancuerna.sync_conflicts(id,account_id,operation_id,entity_type,entity_id,base_revision,current_revision,proposal,reason,state,revision,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?::jsonb,?,'open',1,?,?)`, c.ID, c.AccountID, c.OperationID, c.EntityType, c.EntityID, c.BaseRevision, c.CurrentRevision, string(c.Proposal), c.Reason, c.CreatedAt, c.CreatedAt).Error
}

type checkedChanges struct {
	previous, next int64
	changes        []protocol.Change
	canonical      []byte
}

// Reuse the audited stateless codec at the infrastructure boundary. The domain
// port remains independent of concrete cryptography and persistence packages.
func checkChanges(tx *gorm.DB, accountID, operationID uuid.UUID, changes []protocol.Change) (checkedChanges, error) {
	if !protocol.ValidID(operationID) || len(changes) < 1 || len(changes) > 1004 {
		return checkedChanges{}, protocol.ErrCommandInvalid
	}
	var checked checkedChanges
	if err := tx.Raw("SELECT last_sequence FROM mancuerna.sync_streams WHERE account_id=?", accountID).Row().Scan(&checked.previous); err != nil {
		return checkedChanges{}, err
	}
	if checked.previous < 0 || checked.previous == math.MaxInt64 {
		return checkedChanges{}, protocol.ErrCommandPersistence
	}
	checked.next = checked.previous + 1
	checked.changes = make([]protocol.Change, len(changes))
	seen := make(map[string]bool, len(changes))
	codec := crypto.JSONCanonicalizer{}
	for i, change := range changes {
		id, parseErr := uuid.Parse(change.EntityID)
		revision, revErr := protocol.ParseRevision(change.Revision)
		if parseErr != nil || !protocol.ValidID(id) || id.String() != change.EntityID || revErr != nil || revision < 1 {
			return checkedChanges{}, protocol.ErrCommandInvalid
		}
		switch change.EntityType {
		case "profile", "personal_exercise", "routine", "routine_draft", "routine_version", "workout", "workout_set", "conflict":
		default:
			return checkedChanges{}, protocol.ErrCommandInvalid
		}
		key := change.EntityType + ":" + change.EntityID
		if seen[key] {
			return checkedChanges{}, protocol.ErrCommandInvalid
		}
		seen[key] = true
		data, err := codec.Canonicalize(change.Data)
		if err != nil {
			return checkedChanges{}, protocol.ErrCommandInvalid
		}
		change.Data = data
		checked.changes[i] = change
	}
	encoded, err := json.Marshal(checked.changes)
	if err != nil {
		return checkedChanges{}, protocol.ErrCommandInvalid
	}
	checked.canonical, err = codec.Canonicalize(encoded)
	if err != nil {
		return checkedChanges{}, protocol.ErrCommandInvalid
	}
	group, err := json.Marshal(protocol.ChangeGroup{Sequence: strconv.FormatInt(checked.next, 10), OperationID: operationID.String(), Changes: checked.changes})
	if err != nil {
		return checkedChanges{}, protocol.ErrCommandInvalid
	}
	canonicalGroup, err := codec.Canonicalize(group)
	if err != nil {
		return checkedChanges{}, protocol.ErrCommandInvalid
	}
	if len(canonicalGroup) > protocol.PullPageBytes {
		return checkedChanges{}, protocol.ErrCommandLimit
	}
	// PostgreSQL's JSONB text includes extra whitespace. Preserve its conservative
	// CHECK as a second bound, but detect it before any receipt or record write.
	var fits bool
	if err = tx.Raw("SELECT octet_length((?::jsonb)::text)<=?", string(checked.canonical), protocol.PullPageBytes).Row().Scan(&fits); err != nil {
		return checkedChanges{}, err
	}
	if !fits {
		return checkedChanges{}, protocol.ErrCommandLimit
	}
	return checked, nil
}

func (*Commands) CheckChanges(handle shared.Transaction, accountID, operationID uuid.UUID, changes []protocol.Change) (err error) {
	defer func() { err = persistenceError(err) }()
	tx, err := dataConnection(handle, accountID, true)
	if err != nil {
		return err
	}
	_, err = checkChanges(tx, accountID, operationID, changes)
	return err
}

func (*Commands) Commit(handle shared.Transaction, r protocol.Receipt, changes []protocol.Change) (err error) {
	defer func() { err = persistenceError(err) }()
	if _, err := protocol.ParseCapturedAt(r.CapturedAt); err != nil {
		return protocol.ErrCommandInvalid
	}
	if !protocol.ValidID(r.OperationID) || !protocol.ValidID(r.ActorDeviceID) || !protocol.ValidID(r.OriginDeviceID) || r.RequestHash == [32]byte{} || r.CommandType == "" || r.CommittedAt.IsZero() || r.Result.OperationID != r.OperationID.String() || len(changes) > 1004 {
		return protocol.ErrCommandInvalid
	}
	switch r.Result.Outcome {
	case "accepted", "accepted_with_review", "conflict", "dependency_blocked", "rejected":
	default:
		return protocol.ErrCommandInvalid
	}
	if (r.Result.Outcome == "accepted" || r.Result.Outcome == "accepted_with_review" || r.Result.Outcome == "conflict") && len(changes) == 0 {
		return protocol.ErrCommandInvalid
	}
	tx, err := dataConnection(handle, r.AccountID, true)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(r.Result)
	if err != nil {
		return protocol.ErrCommandInvalid
	}
	var checked checkedChanges
	if len(changes) > 0 {
		checked, err = checkChanges(tx, r.AccountID, r.OperationID, changes)
		if err != nil {
			return err
		}
	}
	if err = tx.Exec(`INSERT INTO mancuerna.operation_receipts(operation_id,account_id,actor_device_id,origin_device_id,request_hash,command_type,outcome,result,committed_at,captured_at)
 VALUES(?,?,?,?,?,?,?,?::jsonb,?,?)`, r.OperationID, r.AccountID, r.ActorDeviceID, r.OriginDeviceID, r.RequestHash[:], r.CommandType, r.Result.Outcome, string(encoded), r.CommittedAt, r.CapturedAt).Error; err != nil {
		return err
	}
	if len(changes) == 0 {
		return nil
	}
	for _, change := range checked.changes {
		id := uuid.MustParse(change.EntityID)
		revision, _ := protocol.ParseRevision(change.Revision)
		if err = tx.Exec(`INSERT INTO mancuerna.record_revisions(account_id,entity_type,entity_id,revision,operation_id,canonical,created_at) VALUES(?,?,?,?,?,?::jsonb,?)`, r.AccountID, change.EntityType, id, revision, r.OperationID, string(change.Data), r.CommittedAt).Error; err != nil {
			return err
		}
	}
	var sequence int64
	if err = tx.Raw("UPDATE mancuerna.sync_streams SET last_sequence=? WHERE account_id=? AND last_sequence=? RETURNING last_sequence", checked.next, r.AccountID, checked.previous).Row().Scan(&sequence); err != nil {
		return err
	}
	if sequence != checked.next {
		return protocol.ErrCommandPersistence
	}
	return tx.Exec("INSERT INTO mancuerna.sync_changes(account_id,sequence,operation_id,changes,created_at) VALUES(?,?,?,?::jsonb,?)", r.AccountID, strconv.FormatInt(sequence, 10), r.OperationID, string(checked.canonical), r.CommittedAt).Error
}
func persistenceError(err error) error {
	if err == nil {
		return nil
	}
	for _, allowed := range []error{protocol.ErrCommandInvalid, protocol.ErrCommandLimit, protocol.ErrCommandScope, protocol.ErrCommandActor, protocol.ErrCommandOrigin, protocol.ErrOperationMissing, protocol.ErrOperationMismatch, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, allowed) {
			return allowed
		}
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && (pg.ConstraintName == "operation_receipts_pkey" || pg.ConstraintName == "operation_receipts_account_id_operation_id_key") {
		return protocol.ErrOperationMismatch
	}
	return protocol.ErrCommandPersistence
}
