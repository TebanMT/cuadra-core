//go:build server

// Package models holds the GORM row structs for the challenges (retos)
// bounded context. Server build only — the sidecar uses sqlx + plain
// structs in the SQLite repos. Mappers stay in the repository file
// adjacent to each method so the on-the-wire / on-disk shapes can drift
// independently if a future migration demands it.
package models

import (
	"time"

	"github.com/google/uuid"
)

type ChallengeModel struct {
	ID                    uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID                 uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	Version               int        `gorm:"not null;default:1;column:version"`
	CreatedAt             time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt             time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt             *time.Time `gorm:"column:deleted_at"`
	Name                  string     `gorm:"not null;column:name"`
	Description           string     `gorm:"column:description"`
	StartsAt              time.Time  `gorm:"not null;column:starts_at"`
	MeasurementT0Deadline time.Time  `gorm:"not null;column:measurement_t0_deadline"`
	MeasurementT1Start    time.Time  `gorm:"not null;column:measurement_t1_start"`
	EndsAt                time.Time  `gorm:"not null;column:ends_at"`
	Status                string     `gorm:"not null;column:status"`
	InscriptionFeeCents   int        `gorm:"not null;default:0;column:inscription_fee_cents"`
	InscriptionRefundable bool       `gorm:"not null;default:true;column:inscription_refundable"`
	MinWeeklyAttendance   int        `gorm:"not null;default:3;column:min_weekly_attendance"`
	AttendanceGraceWeeks  int        `gorm:"not null;default:2;column:attendance_grace_weeks"`
	StrengthCapPct        float64    `gorm:"not null;column:strength_cap_pct"`
	TieMarginIR           float64    `gorm:"not null;column:tie_margin_ir"`
	BFFloorMalePct        float64    `gorm:"not null;column:bf_floor_male_pct"`
	BFFloorFemalePct      float64    `gorm:"not null;column:bf_floor_female_pct"`
}

func (ChallengeModel) TableName() string { return "challenges" }

type CategoryModel struct {
	ID          uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID       uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	ChallengeID uuid.UUID  `gorm:"type:uuid;not null;column:challenge_id"`
	Version     int        `gorm:"not null;default:1;column:version"`
	CreatedAt   time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt   time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt   *time.Time `gorm:"column:deleted_at"`
	Name        string     `gorm:"not null;column:name"`
	SortOrder   int        `gorm:"not null;default:0;column:sort_order"`
}

func (CategoryModel) TableName() string { return "challenge_categories" }

type ParticipantModel struct {
	ID                     uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID                  uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	ChallengeID            uuid.UUID  `gorm:"type:uuid;not null;column:challenge_id"`
	MemberID               uuid.UUID  `gorm:"type:uuid;not null;column:member_id"`
	CategoryID             uuid.UUID  `gorm:"type:uuid;not null;column:category_id"`
	Version                int        `gorm:"not null;default:1;column:version"`
	CreatedAt              time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt              time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt              *time.Time `gorm:"column:deleted_at"`
	ExerciseLegs           string     `gorm:"column:exercise_legs"`
	ExercisePush           string     `gorm:"column:exercise_push"`
	ExercisePull           string     `gorm:"column:exercise_pull"`
	InscriptionFeePaid     bool       `gorm:"not null;default:false;column:inscription_fee_paid"`
	InscriptionPaidAt      *time.Time `gorm:"column:inscription_paid_at"`
	InscriptionRefundedAt  *time.Time `gorm:"column:inscription_refunded_at"`
	Status                 string     `gorm:"not null;column:status"`
	DisqualificationReason string     `gorm:"column:disqualification_reason"`
	DisqualifiedAt         *time.Time `gorm:"column:disqualified_at"`
}

func (ParticipantModel) TableName() string { return "challenge_participants" }

type MeasurementModel struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey;column:id"`
	GymID           uuid.UUID  `gorm:"type:uuid;not null;column:gym_id"`
	ParticipantID   uuid.UUID  `gorm:"type:uuid;not null;column:participant_id"`
	Version         int        `gorm:"not null;default:1;column:version"`
	CreatedAt       time.Time  `gorm:"not null;column:created_at"`
	UpdatedAt       time.Time  `gorm:"not null;column:updated_at"`
	DeletedAt       *time.Time `gorm:"column:deleted_at"`
	Moment          string     `gorm:"not null;column:moment"`
	MeasuredAt      time.Time  `gorm:"not null;column:measured_at"`
	BodyWeightKg    float64    `gorm:"not null;column:body_weight_kg"`
	BodyFatPct      float64    `gorm:"not null;column:body_fat_pct"`
	LegsWeightKg    float64    `gorm:"not null;column:legs_weight_kg"`
	LegsReps        int        `gorm:"not null;column:legs_reps"`
	PushWeightKg    float64    `gorm:"not null;column:push_weight_kg"`
	PushReps        int        `gorm:"not null;column:push_reps"`
	PullWeightKg    float64    `gorm:"not null;column:pull_weight_kg"`
	PullReps        int        `gorm:"not null;column:pull_reps"`
	Notes           string     `gorm:"column:notes"`
	CreatedByUserID uuid.UUID  `gorm:"type:uuid;not null;column:created_by_user_id"`
	SupersededAt    *time.Time `gorm:"column:superseded_at"`
	SupersededByID  *uuid.UUID `gorm:"type:uuid;column:superseded_by_id"`
}

func (MeasurementModel) TableName() string { return "challenge_measurements" }
