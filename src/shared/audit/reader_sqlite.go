//go:build sidecar

package audit

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type sqliteReader struct{}

func NewSQLiteReader() Reader { return &sqliteReader{} }

func (sqliteReader) List(tx sharedDomain.Transaction, q ListQuery) ([]*LogEntry, int, error) {
	stx := tx.(*sharedDomain.SqlxTransaction)
	page := q.Page
	if page < 1 {
		page = 1
	}
	pageSize := q.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}

	args := []any{q.GymID.String()}
	where := "a.gym_id = ?"
	if q.EntityType != "" {
		where += " AND a.entity_type = ?"
		args = append(args, q.EntityType)
	}
	if q.ActorID != nil {
		where += " AND a.actor_user_id = ?"
		args = append(args, q.ActorID.String())
	}
	if q.From != nil {
		where += " AND a.created_at >= ?"
		args = append(args, q.From.UTC().UnixMilli())
	}
	if q.To != nil {
		end := q.To.UTC().Add(24 * time.Hour).UnixMilli()
		where += " AND a.created_at < ?"
		args = append(args, end)
	}

	var total int
	if err := stx.Get(context.Background(), &total,
		`SELECT COUNT(1) FROM audit_log AS a WHERE `+where, args...); err != nil {
		return nil, 0, err
	}

	type row struct {
		ID          string         `db:"id"`
		GymID       string         `db:"gym_id"`
		EntityType  string         `db:"entity_type"`
		EntityID    string         `db:"entity_id"`
		Action      string         `db:"action"`
		ActorUserID sql.NullString `db:"actor_user_id"`
		ActorName   sql.NullString `db:"actor_name"`
		Changes     []byte         `db:"changes"`
		CreatedAt   int64          `db:"created_at"`
	}
	listArgs := append([]any{}, args...)
	listArgs = append(listArgs, pageSize, (page-1)*pageSize)
	var rows []row
	err := stx.Select(context.Background(), &rows,
		`SELECT a.id, a.gym_id, a.entity_type, a.entity_id, a.action,
		        a.actor_user_id, u.full_name AS actor_name,
		        a.changes, a.created_at
		 FROM audit_log AS a
		 LEFT JOIN users u ON u.id = a.actor_user_id
		 WHERE `+where+`
		 ORDER BY a.created_at DESC
		 LIMIT ? OFFSET ?`, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*LogEntry, 0, len(rows))
	for _, r := range rows {
		id, _ := uuid.Parse(r.ID)
		gid, _ := uuid.Parse(r.GymID)
		entry := &LogEntry{
			ID:         id,
			GymID:      gid,
			EntityType: r.EntityType,
			Action:     r.Action,
			Changes:    r.Changes,
			CreatedAt:  time.UnixMilli(r.CreatedAt).UTC(),
		}
		if r.EntityID != "" {
			eid, err := uuid.Parse(r.EntityID)
			if err == nil && eid != uuid.Nil {
				entry.EntityID = &eid
			}
		}
		if r.ActorUserID.Valid {
			aid, err := uuid.Parse(r.ActorUserID.String)
			if err == nil {
				entry.ActorID = &aid
			}
		}
		if r.ActorName.Valid {
			entry.ActorName = r.ActorName.String
		}
		out = append(out, entry)
	}
	return out, total, nil
}
