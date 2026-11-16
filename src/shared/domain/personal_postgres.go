//go:build server

package domain

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type personalUnitOfWork struct {
	db   *gorm.DB
	role PersonalRole
}

// NewPersonalPostgresUnitOfWork verifies the restricted runtime connection.
// The caller owns this pool; do not obtain it through the Tinta singleton.
func NewPersonalPostgresUnitOfWork(db *gorm.DB, role PersonalRole) (UnitOfWork, error) {
	if db == nil || (role != PersonalDataRole && role != PersonalAuthRole) {
		return nil, fmt.Errorf("configuración de pool personal inválida")
	}
	if err := validatePersonalPostgresRole(db, role); err != nil {
		return nil, err
	}
	return &personalUnitOfWork{db: db, role: role}, nil
}

func (u *personalUnitOfWork) open(ctx context.Context, options *sql.TxOptions) (*GormTransaction, error) {
	if err := validatePersonalScope(ctx, u.role); err != nil {
		return nil, err
	}
	tx := u.db.WithContext(ctx).Begin(options)
	if tx.Error != nil {
		return nil, tx.Error
	}
	scope := personalContext(ctx)
	account, attempt, hash := "", "", ""
	if scope.AccountID != uuid.Nil {
		account = scope.AccountID.String()
	}
	if scope.AttemptID != uuid.Nil {
		attempt = scope.AttemptID.String()
		hash = hex.EncodeToString(scope.AttemptHash[:])
	}
	if err := tx.Exec(`SELECT set_config('tinta.gym_id', '', true), set_config('mancuerna.account_id', ?, true), set_config('mancuerna.attempt_id', ?, true), set_config('mancuerna.attempt_hash', ?, true)`, account, attempt, hash).Error; err != nil {
		_ = tx.Rollback().Error
		return nil, err
	}
	return &GormTransaction{Tx: tx, query: options != nil && options.ReadOnly}, nil
}

// Begin serializes account mutations with sync and revocation using their
// shared stream row. An absent stream is an integrity failure, not a lock.
// Initial signup uses the separately scoped attempt before an account exists.
func (u *personalUnitOfWork) Begin(ctx context.Context) (Transaction, error) {
	tx, err := u.open(ctx, nil)
	if err != nil {
		return nil, err
	}
	if id := PersonalAccountID(ctx); id != uuid.Nil {
		var locked uuid.UUID
		// Touch the tuple without advancing the journal. A repeatable-read
		// snapshot that began before this mutation must fail serialization if
		// it waited for this lock, rather than read stale session authority.
		err = tx.Tx.Raw(`UPDATE mancuerna.sync_streams SET last_sequence=last_sequence WHERE account_id=? RETURNING account_id`, id).Row().Scan(&locked)
		if err != nil {
			_ = tx.Tx.Rollback().Error
			if err == sql.ErrNoRows {
				return nil, ErrPersonalStreamMissing
			}
			return nil, err
		}
	}
	return tx, nil
}

func (u *personalUnitOfWork) Commit(tx Transaction) error {
	return tx.(*GormTransaction).Tx.Commit().Error
}
func (u *personalUnitOfWork) Rollback(tx Transaction) error {
	return tx.(*GormTransaction).Tx.Rollback().Error
}
func (u *personalUnitOfWork) Query(ctx context.Context) (Transaction, error) {
	return u.open(ctx, &sql.TxOptions{ReadOnly: true})
}

func (u *personalUnitOfWork) Command(ctx context.Context, fn func(Transaction) error) error {
	tx, err := u.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			_ = u.Rollback(tx)
			panic(r)
		}
	}()
	if err := fn(tx); err != nil {
		_ = u.Rollback(tx)
		return err
	}
	return u.Commit(tx)
}

// PreparedCommand is available only to the isolated auth pool and a pure
// authenticated account scope. It never accepts an attempt capability. The
// normal Command still acquires the account mutex before invoking its callback.
func (u *personalUnitOfWork) PreparedCommand(ctx context.Context, prepare, apply func(Transaction) error) error {
	if ctx == nil || u.role != PersonalAuthRole || prepare == nil || apply == nil {
		return ErrPersonalScope
	}
	if err := validatePersonalScope(ctx, PersonalDataRole); err != nil {
		return err
	}
	tx, err := u.open(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			_ = u.Rollback(tx)
			panic(r)
		}
	}()
	if err = prepare(tx); err == nil {
		var locked uuid.UUID
		err = tx.Tx.Raw(`UPDATE mancuerna.sync_streams SET last_sequence=last_sequence WHERE account_id=? RETURNING account_id`, PersonalAccountID(ctx)).Row().Scan(&locked)
		if err == sql.ErrNoRows {
			err = ErrPersonalStreamMissing
		}
	}
	if err == nil {
		err = apply(tx)
	}
	if err != nil {
		_ = u.Rollback(tx)
		return err
	}
	return u.Commit(tx)
}

func (u *personalUnitOfWork) ReadSnapshot(ctx context.Context, fn func(Transaction) error) error {
	tx, err := u.open(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			_ = u.Rollback(tx)
			panic(r)
		}
	}()
	if err := fn(tx); err != nil {
		_ = u.Rollback(tx)
		return err
	}
	return u.Commit(tx)
}

// SnapshotCommand is for immutable personal sync materializations only.
// An account write that wins the mutex changes its MVCC tuple even when it
// only revokes credentials. PostgreSQL then aborts a stale waiting RR view;
// the caller retries the whole operation with its original ID.
func (u *personalUnitOfWork) SnapshotCommand(ctx context.Context, fn func(Transaction) error) error {
	if u.role != PersonalDataRole || fn == nil {
		return ErrPersonalScope
	}
	if err := validatePersonalScope(ctx, PersonalDataRole); err != nil {
		return err
	}
	tx, err := u.open(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer func() {
		if r := recover(); r != nil {
			_ = u.Rollback(tx)
			panic(r)
		}
	}()
	var locked uuid.UUID
	err = tx.Tx.Raw(`UPDATE mancuerna.sync_streams SET last_sequence=last_sequence WHERE account_id=? RETURNING account_id`, PersonalAccountID(ctx)).Row().Scan(&locked)
	if err == sql.ErrNoRows {
		err = ErrPersonalStreamMissing
	}
	if err == nil {
		err = fn(tx)
	}
	if err != nil {
		_ = u.Rollback(tx)
		return err
	}
	return u.Commit(tx)
}

func validatePersonalPostgresRole(db *gorm.DB, role PersonalRole) error {
	var version int
	if err := db.Raw(`SELECT COALESCE(max(version),0) FROM mancuerna.schema_versions`).Scan(&version).Error; err != nil || version < 1 {
		return fmt.Errorf("ejecuta la migración personal antes de iniciar Mancuerna")
	}
	other := PersonalAuthRole
	if role == PersonalAuthRole {
		other = PersonalDataRole
	}
	var unsafe bool
	// Membership is checked as well as effective privileges: a login must not
	// be able to SET ROLE into an owner, the other pool, or a privileged role.
	err := db.Raw(`SELECT
 NOT pg_has_role(current_user, ?, 'USAGE')
 OR pg_has_role(current_user, ?, 'MEMBER')
 OR pg_has_role(current_user, 'mancuerna_identity', 'MEMBER')
 OR EXISTS(SELECT 1 FROM pg_roles r WHERE pg_has_role(current_user,r.oid,'MEMBER') AND (r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole OR r.rolreplication))
 OR has_database_privilege(current_user,current_database(),'CREATE')
 OR EXISTS(SELECT 1 FROM pg_namespace n WHERE n.nspname IN ('public','tinta_private','mancuerna','mancuerna_private') AND (has_schema_privilege(current_user,n.oid,'CREATE') OR pg_has_role(current_user,n.nspowner,'MEMBER')))
 OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname IN ('public','mancuerna') AND c.relkind IN ('r','p') AND pg_has_role(current_user,c.relowner,'MEMBER'))
 OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p','v','m','f') AND (has_table_privilege(current_user,c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege(current_user,c.oid,'SELECT,INSERT,UPDATE,REFERENCES')))
 OR EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='tinta_private' AND has_function_privilege(current_user,p.oid,'EXECUTE'))
 OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='mancuerna' AND c.relkind IN ('r','p') AND has_table_privilege(current_user,c.oid,'DELETE,TRUNCATE,REFERENCES,TRIGGER'))
 OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='mancuerna' AND c.relkind IN ('r','p') AND (c.relname='accounts' OR EXISTS(SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attname='account_id' AND NOT a.attisdropped)) AND (NOT c.relrowsecurity OR NOT c.relforcerowsecurity OR NOT EXISTS(SELECT 1 FROM pg_policy p WHERE p.polrelid=c.oid)))
 OR NOT has_function_privilege(current_user,'mancuerna_private.session_active(uuid,uuid)','EXECUTE')`, string(role), string(other)).Scan(&unsafe).Error
	if err != nil {
		return fmt.Errorf("no fue posible comprobar el aislamiento personal: %w", err)
	}
	if unsafe {
		return fmt.Errorf("el pool personal requiere un rol aislado, sin propiedad, privilegios cruzados ni bypass de RLS")
	}
	if role == PersonalDataRole {
		err = db.Raw(`SELECT has_function_privilege(current_user,'mancuerna_private.resolve_identity(text,text)','EXECUTE')
 OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='mancuerna' AND c.relname IN ('sessions','session_rotations','auth_challenges','device_authorizations','device_proofs','recovery_keys','vault_keys','auth_receipts','otp_budgets','security_events') AND (has_table_privilege(current_user,c.oid,'SELECT,INSERT,UPDATE') OR has_any_column_privilege(current_user,c.oid,'SELECT,INSERT,UPDATE')))
 OR has_table_privilege(current_user,'mancuerna.accounts','INSERT')
 OR EXISTS(SELECT 1 FROM pg_attribute a WHERE a.attrelid='mancuerna.accounts'::regclass AND a.attnum>0 AND NOT a.attisdropped AND a.attname NOT IN ('display_name','weight_unit','revision','updated_at') AND has_column_privilege(current_user,a.attrelid,a.attnum,'UPDATE'))
 OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='mancuerna' AND c.relname IN ('schema_versions','devices','catalog_exercises') AND (has_table_privilege(current_user,c.oid,'INSERT,UPDATE') OR has_any_column_privilege(current_user,c.oid,'INSERT,UPDATE')))`).Scan(&unsafe).Error
	} else {
		err = db.Raw(`SELECT NOT has_function_privilege(current_user,'mancuerna_private.resolve_identity(text,text)','EXECUTE') OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='mancuerna' AND c.relname IN ('personal_exercises','routines','routine_drafts','routine_versions','routine_days','routine_exercises','workouts','workout_exercises','workout_sets','operation_receipts','record_revisions','sync_changes','sync_conflicts','sync_snapshots','sync_snapshot_pages') AND (has_table_privilege(current_user,c.oid,'SELECT,INSERT,UPDATE') OR has_any_column_privilege(current_user,c.oid,'SELECT,INSERT,UPDATE')))`).Scan(&unsafe).Error
	}
	if err != nil {
		return fmt.Errorf("no fue posible comprobar los permisos del pool personal: %w", err)
	}
	if unsafe {
		return fmt.Errorf("el pool personal tiene permisos incompatibles con su función")
	}
	return nil
}
