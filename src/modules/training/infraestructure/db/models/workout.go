//go:build server

package models

import (
	"github.com/google/uuid"
	"time"
)

type WorkoutModel struct {
	ID                   uuid.UUID  `gorm:"column:id;primaryKey"`
	AccountID            uuid.UUID  `gorm:"column:account_id"`
	RoutineVersionID     uuid.UUID  `gorm:"column:routine_version_id"`
	RoutineDayID         uuid.UUID  `gorm:"column:routine_day_id"`
	RoutineTitleSnapshot string     `gorm:"column:routine_title_snapshot"`
	DayLabelSnapshot     string     `gorm:"column:day_label_snapshot"`
	State                string     `gorm:"column:state"`
	StartedAt            time.Time  `gorm:"column:started_at"`
	FinishedAt           *time.Time `gorm:"column:finished_at"`
	Timezone             string     `gorm:"column:timezone"`
	Revision             int64      `gorm:"column:revision"`
	FinalizedRevision    *int64     `gorm:"column:finalized_revision"`
	NeedsReview          bool       `gorm:"column:needs_review"`
	SnapshotSealed       bool       `gorm:"column:snapshot_sealed"`
	OriginDeviceID       uuid.UUID  `gorm:"column:origin_device_id"`
	CreatedAt            time.Time  `gorm:"column:created_at"`
	UpdatedAt            time.Time  `gorm:"column:updated_at"`
	DeletedAt            *time.Time `gorm:"column:deleted_at"`
}

func (WorkoutModel) TableName() string { return "mancuerna.workouts" }

type WorkoutExerciseModel struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	AccountID          uuid.UUID  `gorm:"column:account_id"`
	WorkoutID          uuid.UUID  `gorm:"column:workout_id"`
	RoutineVersionID   uuid.UUID  `gorm:"column:routine_version_id"`
	RoutineDayID       uuid.UUID  `gorm:"column:routine_day_id"`
	SourceExerciseID   uuid.UUID  `gorm:"column:source_exercise_id"`
	CatalogExerciseID  *uuid.UUID `gorm:"column:catalog_exercise_id"`
	PersonalExerciseID *uuid.UUID `gorm:"column:personal_exercise_id"`
	NameSnapshot       string     `gorm:"column:name_snapshot"`
	LoadModeSnapshot   string     `gorm:"column:load_mode_snapshot"`
	Position           int        `gorm:"column:position"`
	PrescribedSets     int        `gorm:"column:prescribed_sets"`
	PrescribedReps     string     `gorm:"column:prescribed_reps"`
	RestSeconds        *int       `gorm:"column:rest_seconds"`
	Notes              string     `gorm:"column:notes"`
}

func (WorkoutExerciseModel) TableName() string { return "mancuerna.workout_exercises" }
