package models

import (
	"github.com/google/uuid"
	"time"
)

type PersonalExerciseModel struct {
	ID         uuid.UUID  `gorm:"column:id;primaryKey"`
	AccountID  uuid.UUID  `gorm:"column:account_id"`
	Name       string     `gorm:"column:name"`
	SearchKey  string     `gorm:"column:search_key"`
	LoadMode   string     `gorm:"column:load_mode"`
	Notes      string     `gorm:"column:notes"`
	Revision   int64      `gorm:"column:revision"`
	CreatedAt  time.Time  `gorm:"column:created_at"`
	UpdatedAt  time.Time  `gorm:"column:updated_at"`
	ArchivedAt *time.Time `gorm:"column:archived_at"`
	DeletedAt  *time.Time `gorm:"column:deleted_at"`
}

func (PersonalExerciseModel) TableName() string { return "mancuerna.personal_exercises" }
