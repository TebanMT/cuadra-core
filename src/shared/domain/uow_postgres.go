//go:build server

package domain

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/google/uuid"

	"gorm.io/gorm"
)

// GormTransaction is the concrete Transaction backed by a GORM *gorm.DB.
// Repositories type-assert to *GormTransaction to pull the underlying handle.
type GormTransaction struct {
	Tx    *gorm.DB
	query bool
}

func (g *GormTransaction) Execute(fn func(tx Transaction) error) error {
	return fn(g)
}

type gormUnitOfWork struct {
	db     *gorm.DB
	strict bool
}

// NewPostgresUnitOfWork wires a UnitOfWork over GORM (cloud).
func NewPostgresUnitOfWork(db *gorm.DB) UnitOfWork {
	return &gormUnitOfWork{db: db}
}

func (u *gormUnitOfWork) Begin(ctx context.Context) (Transaction, error) {
	if tenantInvalid(ctx) {
		return nil, ErrTenantScope
	}
	tx := u.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	if err := setTenant(tx, ctx); err != nil {
		_ = tx.Rollback().Error
		return nil, err
	}
	return &GormTransaction{Tx: tx}, nil
}

func (u *gormUnitOfWork) Commit(tx Transaction) error {
	return tx.(*GormTransaction).Tx.Commit().Error
}

func (u *gormUnitOfWork) Rollback(tx Transaction) error {
	return tx.(*GormTransaction).Tx.Rollback().Error
}

func (u *gormUnitOfWork) Query(ctx context.Context) (Transaction, error) {
	if !u.strict && TenantID(ctx) == uuid.Nil {
		return &GormTransaction{Tx: u.db.WithContext(ctx)}, nil
	}
	if tenantInvalid(ctx) {
		return nil, ErrTenantScope
	}
	tx := u.db.WithContext(ctx).Begin(&sql.TxOptions{ReadOnly: true})
	if tx.Error != nil {
		return nil, tx.Error
	}
	if err := setTenant(tx, ctx); err != nil {
		_ = tx.Rollback().Error
		return nil, err
	}
	return &GormTransaction{Tx: tx, query: true}, nil
}

func (u *gormUnitOfWork) ReadSnapshot(ctx context.Context, fn func(tx Transaction) error) error {
	tx := u.db.WithContext(ctx).Begin(&sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if tx.Error != nil {
		return tx.Error
	}
	if tenantInvalid(ctx) {
		_ = tx.Rollback().Error
		return ErrTenantScope
	}
	if err := setTenant(tx, ctx); err != nil {
		_ = tx.Rollback().Error
		return err
	}
	handle := &GormTransaction{Tx: tx}
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback().Error
			panic(r)
		}
	}()
	if err := fn(handle); err != nil {
		_ = tx.Rollback().Error
		return err
	}
	return tx.Commit().Error
}

func (u *gormUnitOfWork) Command(ctx context.Context, fn func(tx Transaction) error) error {
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

func setTenant(tx *gorm.DB, ctx context.Context) error {
	id := ""
	if TenantID(ctx) != uuid.Nil {
		id = TenantID(ctx).String()
	}
	return tx.Exec(`SELECT set_config('tinta.gym_id', ?, true)`, id).Error
}
func (g *GormTransaction) CloseQuery() {
	if g.query {
		_ = g.Tx.Rollback().Error
		g.query = false
	}
}

// NewTenantPostgresUnitOfWork is the server runtime. Its DB role must neither
// own tenant tables nor bypass their policies; migrations run separately.
func NewTenantPostgresUnitOfWork(db *gorm.DB) (UnitOfWork, error) {
	var schema int
	if err := db.Raw(`SELECT COALESCE(MAX(version),0) FROM _migrations`).Scan(&schema).Error; err != nil {
		return nil, err
	}
	if schema < 57 {
		return nil, fmt.Errorf("ejecuta cmd/migrate con el rol de migración antes de iniciar la API")
	}

	var unsafe bool
	err := db.Raw(`SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=current_user AND (rolsuper OR rolbypassrls OR rolcreaterole OR rolcreatedb)) OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind='r' AND EXISTS(SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attname='gym_id' AND NOT a.attisdropped) AND (pg_has_role(current_user,c.relowner,'MEMBER') OR NOT c.relrowsecurity))`).Scan(&unsafe).Error
	if err != nil {
		return nil, err
	}
	if unsafe {
		return nil, fmt.Errorf("el rol de aplicación debe ser independiente del migrador, sin propiedad de tablas ni bypass de RLS")
	}
	return &tenantUnitOfWork{gormUnitOfWork: &gormUnitOfWork{db: db, strict: true}}, nil
}

type tenantUnitOfWork struct{ *gormUnitOfWork }

func (u *tenantUnitOfWork) ResolveTenant(ctx context.Context, kind, key string) (context.Context, error) {
	var id uuid.UUID
	if err := u.db.WithContext(ctx).Raw(`SELECT COALESCE(tinta_private.resolve_tenant(?,?), '00000000-0000-0000-0000-000000000000'::uuid)`, kind, key).Row().Scan(&id); err != nil {
		return ctx, err
	}
	if id == uuid.Nil {
		return ctx, ErrTenantScope
	}
	scoped := WithTenant(ctx, id)
	if tenantInvalid(scoped) {
		return ctx, ErrTenantScope
	}
	return scoped, nil
}
func (u *tenantUnitOfWork) ForEachTenant(ctx context.Context, fn func(context.Context) (int, error)) (int, error) {
	after := uuid.Nil
	total := 0
	var firstErr error
	for {
		var ids []uuid.UUID
		if err := u.db.WithContext(ctx).Raw(`SELECT * FROM tinta_private.worker_tenants(?)`, after).Scan(&ids).Error; err != nil {
			return total, err
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			n, err := fn(WithTenant(ctx, id))
			total += n
			if firstErr == nil {
				firstErr = err
			}
			after = id
		}
		if len(ids) < 100 {
			return total, firstErr
		}
	}
}

// TenantDB supports the few infrastructure stores which run outside the UoW.
// The caller always closes the transaction; no session-level SET survives it.
func TenantDB(ctx context.Context, db *gorm.DB) (*gorm.DB, error) {
	if tenantInvalid(ctx) {
		return nil, ErrTenantScope
	}
	tx := db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, tx.Error
	}
	if err := setTenant(tx, ctx); err != nil {
		_ = tx.Rollback().Error
		return nil, err
	}
	return tx, nil
}
func ResolveDBTenant(ctx context.Context, db *gorm.DB, kind, key string) (context.Context, error) {
	u := tenantUnitOfWork{gormUnitOfWork: &gormUnitOfWork{db: db}}
	return u.ResolveTenant(ctx, kind, key)
}
