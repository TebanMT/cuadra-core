package domain

import "context"

// Transaction is a database-transaction handle. It is intentionally opaque —
// callers (use cases, repositories) treat it as a value to be plumbed through.
// The concrete kind (GORM tx, sqlx tx, sync-queue-aware wrapper) is decided in
// infrastructure and resolved with a type assertion at the repository edge.
//
// Execute exists so a Transaction can be used in callback style when the
// repository wants nested operations on the same handle.
type Transaction interface {
	Execute(fn func(tx Transaction) error) error
}

// UnitOfWork brokers transactions. Two entry points:
//
//   - Command: write path. Auto begin / commit / rollback. Use this for any
//     mutation. The fn runs inside a single transaction; returning an error
//     rolls back, panicking does the same and re-panics.
//   - Query: read path. Returns a non-transactional handle (still satisfies
//     Transaction so repos work uniformly).
type UnitOfWork interface {
	Begin(ctx context.Context) (Transaction, error)
	Commit(tx Transaction) error
	Rollback(tx Transaction) error
	Command(ctx context.Context, fn func(tx Transaction) error) error
	Query(ctx context.Context) (Transaction, error)
}
