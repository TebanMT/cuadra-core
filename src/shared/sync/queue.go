// Package sync hosts the sidecar-side write helpers for sync_queue (ADR-001
// §3.2). The full sync agent (push/pull/full) lives outside Sesión 1 — for
// now we only enqueue snapshots so a future agent can drain them.
package sync

// Operation is the wire-level operation kind. Sidecar repos only ever write
// "upsert" or "delete" — see ADR-001 §3.2.
type Operation string

const (
	OpUpsert Operation = "upsert"
	OpDelete Operation = "delete"
)

// SqliteQueue writes pending mutations into the local sync_queue. It satisfies
// sharedDomain.SyncQueueWriter (build-tagged) and is wired by the sidecar's UoW.
type SqliteQueue struct{}

func NewSqliteQueue() *SqliteQueue { return &SqliteQueue{} }
