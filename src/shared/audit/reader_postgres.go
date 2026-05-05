//go:build server

package audit

import (
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type postgresReader struct{}

func NewPostgresReader() Reader { return &postgresReader{} }

func (postgresReader) List(tx sharedDomain.Transaction, q ListQuery) ([]*LogEntry, int, error) {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	page := q.Page
	if page < 1 {
		page = 1
	}
	pageSize := q.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}

	base := gormTx.Table("audit_log AS a").
		Where("a.gym_id = ?", q.GymID)
	if q.EntityType != "" {
		base = base.Where("a.entity_type = ?", q.EntityType)
	}
	if q.ActorID != nil {
		base = base.Where("a.actor_user_id = ?", *q.ActorID)
	}
	if q.From != nil {
		base = base.Where("a.created_at >= ?", q.From.UTC())
	}
	if q.To != nil {
		// `To` is inclusive — extend by 24h so the boundary day is fully covered.
		end := q.To.UTC().Add(24 * time.Hour)
		base = base.Where("a.created_at < ?", end)
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	type row struct {
		ID          uuid.UUID
		GymID       uuid.UUID
		EntityType  string
		EntityID    uuid.UUID
		Action      string
		ActorUserID *uuid.UUID
		ActorName   *string
		Changes     []byte
		CreatedAt   time.Time
	}
	var rows []row
	err := base.
		Select(`a.id, a.gym_id, a.entity_type, a.entity_id, a.action,
		        a.actor_user_id, COALESCE(u.full_name, u.email) AS actor_name,
		        a.changes, a.created_at`).
		Joins(`LEFT JOIN users u ON u.id = a.actor_user_id`).
		Order("a.created_at DESC").
		Limit(pageSize).Offset((page - 1) * pageSize).
		Scan(&rows).Error
	if err != nil {
		return nil, 0, err
	}
	out := make([]*LogEntry, 0, len(rows))
	for _, r := range rows {
		entry := &LogEntry{
			ID:         r.ID,
			GymID:      r.GymID,
			EntityType: r.EntityType,
			Action:     r.Action,
			Changes:    r.Changes,
			CreatedAt:  r.CreatedAt,
		}
		if r.EntityID != uuid.Nil {
			eid := r.EntityID
			entry.EntityID = &eid
		}
		entry.ActorID = r.ActorUserID
		if r.ActorName != nil {
			entry.ActorName = *r.ActorName
		}
		out = append(out, entry)
	}
	return out, int(total), nil
}
