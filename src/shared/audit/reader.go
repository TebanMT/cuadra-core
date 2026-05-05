package audit

import (
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// LogEntry is the wire-shape row returned by Reader. Mirrors what the
// cuadra-desktop AuditLog page expects (id, gym_id, entity_type, entity_id,
// action, actor_id, actor_name, changes, created_at).
type LogEntry struct {
	ID         uuid.UUID
	GymID      uuid.UUID
	EntityType string
	EntityID   *uuid.UUID
	Action     string
	ActorID    *uuid.UUID
	ActorName  string // joined from users.full_name when ActorID is not nil
	Changes    []byte // raw JSON; controller marshals it through
	CreatedAt  time.Time
}

// ListQuery filters the audit page. EntityType + ActorID are optional;
// `From` and `To` are inclusive day boundaries.
type ListQuery struct {
	GymID      uuid.UUID
	EntityType string
	ActorID    *uuid.UUID
	From       *time.Time
	To         *time.Time
	Page       int
	PageSize   int
}

// Reader exposes the read side of the audit_log table — the writer
// (Recorder) is intentionally separate so most use cases never depend on
// reads. The handler at /api/v1/audit-log uses it.
type Reader interface {
	List(tx sharedDomain.Transaction, q ListQuery) (entries []*LogEntry, total int, err error)
}
