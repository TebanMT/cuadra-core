package models

import (
	"github.com/google/uuid"
	"time"
)

type RoutineVersionModel struct {
	ID                  uuid.UUID `gorm:"column:id;primaryKey"`
	AccountID           uuid.UUID `gorm:"column:account_id"`
	RoutineID           uuid.UUID `gorm:"column:routine_id"`
	VersionNumber       int       `gorm:"column:version_number"`
	TitleSnapshot       string    `gorm:"column:title_snapshot"`
	ContentHash         []byte    `gorm:"column:content_hash"`
	FormatVersion       int       `gorm:"column:format_version"`
	SourceDraftRevision int64     `gorm:"column:source_draft_revision"`
	Sealed              bool      `gorm:"column:sealed"`
	ActorDeviceID       uuid.UUID `gorm:"column:actor_device_id"`
	PublishedAt         time.Time `gorm:"column:published_at"`
}

func (RoutineVersionModel) TableName() string { return "mancuerna.routine_versions" }

type RoutineDayModel struct {
	ID        uuid.UUID `gorm:"column:id;primaryKey"`
	AccountID uuid.UUID `gorm:"column:account_id"`
	VersionID uuid.UUID `gorm:"column:version_id"`
	Label     string    `gorm:"column:label"`
	Position  int       `gorm:"column:position"`
}

func (RoutineDayModel) TableName() string { return "mancuerna.routine_days" }

type RoutineExerciseModel struct {
	ID                 uuid.UUID  `gorm:"column:id;primaryKey"`
	AccountID          uuid.UUID  `gorm:"column:account_id"`
	VersionID          uuid.UUID  `gorm:"column:version_id"`
	DayID              uuid.UUID  `gorm:"column:day_id"`
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

func (RoutineExerciseModel) TableName() string { return "mancuerna.routine_exercises" }
