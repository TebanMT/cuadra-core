package models

import (
	"encoding/json"
	"github.com/google/uuid"
	"time"
)

type RoutineModel struct {
	ID                      uuid.UUID  `gorm:"column:id;primaryKey"`
	AccountID               uuid.UUID  `gorm:"column:account_id"`
	Title                   string     `gorm:"column:title"`
	Status                  string     `gorm:"column:status"`
	Revision                int64      `gorm:"column:revision"`
	CurrentVersionID        *uuid.UUID `gorm:"column:current_version_id"`
	DuplicatedFromVersionID *uuid.UUID `gorm:"column:duplicated_from_version_id"`
	CreatedAt               time.Time  `gorm:"column:created_at"`
	UpdatedAt               time.Time  `gorm:"column:updated_at"`
	DeletedAt               *time.Time `gorm:"column:deleted_at"`
}

func (RoutineModel) TableName() string { return "mancuerna.routines" }

type RoutineDraftModel struct {
	RoutineID     uuid.UUID       `gorm:"column:routine_id;primaryKey"`
	AccountID     uuid.UUID       `gorm:"column:account_id"`
	Revision      int64           `gorm:"column:revision"`
	BaseVersionID *uuid.UUID      `gorm:"column:base_version_id"`
	Content       json.RawMessage `gorm:"column:content;type:jsonb"`
	CreatedAt     time.Time       `gorm:"column:created_at"`
	UpdatedAt     time.Time       `gorm:"column:updated_at"`
	DeletedAt     *time.Time      `gorm:"column:deleted_at"`
}

func (RoutineDraftModel) TableName() string { return "mancuerna.routine_drafts" }
