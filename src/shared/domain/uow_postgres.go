//go:build server

package domain

import (
	"context"

	"gorm.io/gorm"
)

// GormTransaction is the concrete Transaction backed by a GORM *gorm.DB.
// Repositories type-assert to *GormTransaction to pull the underlying handle.
type GormTransaction struct {
	Tx *gorm.DB
}

func (g *GormTransaction) Execute(fn func(tx Transaction) error) error {
	return fn(g)
}

type gormUnitOfWork struct {
	db *gorm.DB
}

// NewPostgresUnitOfWork wires a UnitOfWork over GORM (cloud).
func NewPostgresUnitOfWork(db *gorm.DB) UnitOfWork {
	return &gormUnitOfWork{db: db}
}

func (u *gormUnitOfWork) Begin(ctx context.Context) (Transaction, error) {
	tx := u.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return nil, tx.Error
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
	return &GormTransaction{Tx: u.db.WithContext(ctx)}, nil
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
