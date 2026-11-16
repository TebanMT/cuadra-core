//go:build server

package models

import (
	"github.com/google/uuid"
	"time"
)

type WorkoutSetModel struct {
	ID                uuid.UUID  `gorm:"column:id;primaryKey"`
	AccountID         uuid.UUID  `gorm:"column:account_id"`
	WorkoutID         uuid.UUID  `gorm:"column:workout_id"`
	WorkoutExerciseID uuid.UUID  `gorm:"column:workout_exercise_id"`
	OriginDeviceID    uuid.UUID  `gorm:"column:origin_device_id"`
	PositionHint      int        `gorm:"column:position_hint"`
	LoadMode          string     `gorm:"column:load_mode"`
	WeightValue       *string    `gorm:"column:weight_value;type:numeric(10,3)"`
	WeightUnit        *string    `gorm:"column:weight_unit"`
	Repetitions       int        `gorm:"column:repetitions"`
	Revision          int64      `gorm:"column:revision"`
	CapturedAt        time.Time  `gorm:"column:captured_at"`
	CreatedAt         time.Time  `gorm:"column:created_at"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
	DeletedAt         *time.Time `gorm:"column:deleted_at"`
}

func (WorkoutSetModel) TableName() string { return "mancuerna.workout_sets" }
