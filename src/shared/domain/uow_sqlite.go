//go:build sidecar

package domain

import (
	"context"
	"database/sql"

	"github.com/jmoiron/sqlx"
)

// SqlxTransaction is the concrete Transaction backed by sqlx (sidecar / SQLite).
// When `Tx` is non-nil we are inside an explicit BEGIN; when nil we run
// statements directly off `DB` (read path).
//
// Both paths satisfy a small "execer" surface (Get/Select/NamedExec/Exec/...)
// via the helpers in this file; repositories use those instead of branching
// on Tx vs DB themselves.
type SqlxTransaction struct {
	DB    *sqlx.DB
	Tx    *sqlx.Tx
	Queue SyncQueueWriter
}

func (s *SqlxTransaction) Execute(fn func(tx Transaction) error) error {
	return fn(s)
}

// Ext returns the active sqlx handle (Tx if set, else DB). Repos call this.
func (s *SqlxTransaction) Ext() sqlx.ExtContext {
	if s.Tx != nil {
		return s.Tx
	}
	return s.DB
}

// Preparer for prepared statements (rare, but kept for parity).
func (s *SqlxTransaction) Preparer() sqlx.PreparerContext {
	if s.Tx != nil {
		return s.Tx
	}
	return s.DB
}

// Exec runs a statement on the active handle.
func (s *SqlxTransaction) Exec(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if s.Tx != nil {
		return s.Tx.ExecContext(ctx, query, args...)
	}
	return s.DB.ExecContext(ctx, query, args...)
}

func (s *SqlxTransaction) Get(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	if s.Tx != nil {
		return s.Tx.GetContext(ctx, dest, query, args...)
	}
	return s.DB.GetContext(ctx, dest, query, args...)
}

func (s *SqlxTransaction) Select(ctx context.Context, dest interface{}, query string, args ...interface{}) error {
	if s.Tx != nil {
		return s.Tx.SelectContext(ctx, dest, query, args...)
	}
	return s.DB.SelectContext(ctx, dest, query, args...)
}

func (s *SqlxTransaction) NamedExec(ctx context.Context, query string, arg interface{}) (sql.Result, error) {
	if s.Tx != nil {
		return s.Tx.NamedExecContext(ctx, query, arg)
	}
	return s.DB.NamedExecContext(ctx, query, arg)
}

// EnqueueSync — sidecar write-through. Repositories that mutate sync'd entities
// call this inside Command() so the sync agent picks them up later. Cloud
// builds compile this code out via the build tag.
func (s *SqlxTransaction) EnqueueSync(ctx context.Context, entityType, entityID, operation string, payload []byte, clientVersion int) error {
	if s.Queue == nil {
		return nil
	}
	return s.Queue.Enqueue(ctx, s, entityType, entityID, operation, payload, clientVersion)
}

// SyncQueueWriter abstracts the local sync_queue insert so we can swap mocks in
// tests and keep the dependency direction healthy.
type SyncQueueWriter interface {
	Enqueue(ctx context.Context, tx Transaction, entityType, entityID, operation string, payload []byte, clientVersion int) error
}

type sqlxUnitOfWork struct {
	db    *sqlx.DB
	queue SyncQueueWriter
}

// NewSQLiteUnitOfWork wires a UnitOfWork over sqlx with optional sync_queue
// write-through. Pass nil queue for tests / cloud-style flows.
func NewSQLiteUnitOfWork(db *sqlx.DB, queue SyncQueueWriter) UnitOfWork {
	return &sqlxUnitOfWork{db: db, queue: queue}
}

func (u *sqlxUnitOfWork) Begin(ctx context.Context) (Transaction, error) {
	tx, err := u.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &SqlxTransaction{DB: u.db, Tx: tx, Queue: u.queue}, nil
}

func (u *sqlxUnitOfWork) Commit(tx Transaction) error {
	return tx.(*SqlxTransaction).Tx.Commit()
}

func (u *sqlxUnitOfWork) Rollback(tx Transaction) error {
	return tx.(*SqlxTransaction).Tx.Rollback()
}

func (u *sqlxUnitOfWork) Query(ctx context.Context) (Transaction, error) {
	return &SqlxTransaction{DB: u.db, Tx: nil, Queue: u.queue}, nil
}

func (u *sqlxUnitOfWork) Command(ctx context.Context, fn func(tx Transaction) error) error {
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
