//go:build server

package audit

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// auditLogModel is local to this package — audit rows are infrastructure plumbing,
// no domain entity owns them.
type auditLogModel struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey"`
	GymID       uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version     int        `gorm:"not null;default:1"`
	CreatedAt   time.Time  `gorm:"not null"`
	UpdatedAt   time.Time  `gorm:"not null"`
	EntityType  string     `gorm:"not null"`
	EntityID    uuid.UUID  `gorm:"type:uuid;not null"`
	Action      string     `gorm:"not null"`
	ActorUserID *uuid.UUID `gorm:"type:uuid"`
	Changes     []byte     `gorm:"type:jsonb"`
	IPAddress   *string    `gorm:"type:inet"`
	UserAgent   *string
}

func (auditLogModel) TableName() string { return "audit_log" }

type postgresRecorder struct{}

// NewPostgresRecorder returns the cloud impl. Sidecar uses NewSQLiteRecorder.
func NewPostgresRecorder() Recorder { return &postgresRecorder{} }

func (postgresRecorder) Record(ctx context.Context, tx sharedDomain.Transaction, entry Entry) error {
	gormTx := tx.(*sharedDomain.GormTransaction).Tx
	now := entry.At
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var payload []byte
	if entry.Changes != nil {
		b, err := json.Marshal(entry.Changes)
		if err != nil {
			return err
		}
		payload = b
	}
	row := auditLogModel{
		ID:          uuid.New(),
		GymID:       entry.GymID,
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
		EntityType:  entry.EntityType,
		EntityID:    entry.EntityID,
		Action:      entry.Action,
		ActorUserID: entry.ActorUserID,
		Changes:     payload,
	}
	if entry.IPAddress != "" {
		ip := entry.IPAddress
		row.IPAddress = &ip
	}
	if entry.UserAgent != "" {
		ua := entry.UserAgent
		row.UserAgent = &ua
	}
	return gormTx.WithContext(ctx).Create(&row).Error
}
