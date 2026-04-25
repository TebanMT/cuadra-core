//go:build sidecar

package audit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

type sqliteRecorder struct{}

func NewSQLiteRecorder() Recorder { return &sqliteRecorder{} }

func (sqliteRecorder) Record(ctx context.Context, tx sharedDomain.Transaction, entry Entry) error {
	stx := tx.(*sharedDomain.SqlxTransaction)
	now := entry.At
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nowMs := now.UnixMilli()

	var changesStr *string
	if entry.Changes != nil {
		b, err := json.Marshal(entry.Changes)
		if err != nil {
			return err
		}
		s := string(b)
		changesStr = &s
	}
	var actor *string
	if entry.ActorUserID != nil {
		s := entry.ActorUserID.String()
		actor = &s
	}
	var ip, ua *string
	if entry.IPAddress != "" {
		s := entry.IPAddress
		ip = &s
	}
	if entry.UserAgent != "" {
		s := entry.UserAgent
		ua = &s
	}

	id := uuid.New().String()
	_, err := stx.Exec(ctx, `
		INSERT INTO audit_log (
		    id, gym_id, version, created_at, updated_at,
		    entity_type, entity_id, action, actor_user_id, changes, ip_address, user_agent
		) VALUES (?,?,1,?,?,?,?,?,?,?,?,?)`,
		id, entry.GymID.String(), nowMs, nowMs,
		entry.EntityType, entry.EntityID.String(), entry.Action, actor, changesStr, ip, ua,
	)
	if err != nil {
		return err
	}
	if entry.Changes != nil && stx.Queue != nil {
		// Audit rows are sync'd cliente→cloud (ADR-002 §3.16). Encode the row
		// as the snapshot payload so the sync batch is self-contained.
		payload, _ := json.Marshal(map[string]any{
			"id":            id,
			"gym_id":        entry.GymID.String(),
			"entity_type":   entry.EntityType,
			"entity_id":     entry.EntityID.String(),
			"action":        entry.Action,
			"actor_user_id": actor,
			"changes":       changesStr,
			"created_at":    nowMs,
			"updated_at":    nowMs,
		})
		_ = stx.EnqueueSync(ctx, "audit_log", id, "upsert", payload, 1)
	}
	return nil
}
