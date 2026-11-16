//go:build server

package repositories

import (
	"errors"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	"github.com/cuadra/cuadra-core/src/modules/training/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"reflect"
)

type WorkoutSetPostgresRepository struct{}

func NewWorkoutSetPostgresRepository() *WorkoutSetPostgresRepository {
	return &WorkoutSetPostgresRepository{}
}

var _ repository.WorkoutSetRepository = (*WorkoutSetPostgresRepository)(nil)

func setPersistenceError(err error) error {
	for _, e := range []error{trainingErrors.ErrInvalidWorkoutSet, trainingErrors.ErrWorkoutSetNotFound, trainingErrors.ErrWorkoutRevisionConflict} {
		if errors.Is(err, e) {
			return e
		}
	}
	return workoutPersistenceError(err)
}
func (*WorkoutSetPostgresRepository) CountIDs(handle shared.Transaction, owner, workoutID uuid.UUID) (n int64, err error) {
	defer func() {
		err = setPersistenceError(err)
		if err != nil {
			n = 0
		}
	}()
	if !protocol.ValidID(workoutID) {
		return 0, trainingErrors.ErrInvalidWorkoutSet
	}
	tx, err := trainingDataConnection(handle, owner, false)
	if err != nil {
		return 0, err
	}
	err = tx.Raw("SELECT count(*) FROM mancuerna.workout_sets WHERE account_id=? AND workout_id=?", owner, workoutID).Row().Scan(&n)
	return n, err
}
func (r *WorkoutSetPostgresRepository) CreateAndAdvance(handle shared.Transaction, owner uuid.UUID, s *workout.Set, next *workout.Workout, expected int64) (created bool, err error) {
	defer func() {
		err = setPersistenceError(err)
		if err != nil {
			created = false
		}
	}()
	if s.ValidateCreation(owner) != nil || next.Validate() != nil || next.AccountID != owner || expected < 1 {
		return false, trainingErrors.ErrInvalidWorkoutSet
	}
	tx, err := trainingDataConnection(handle, owner, true)
	if err != nil {
		return false, err
	}
	current, err := NewWorkoutPostgresRepository().GetByID(handle, owner, s.WorkoutID)
	if err != nil {
		return false, err
	}
	if current.Revision != expected {
		return false, trainingErrors.ErrWorkoutRevisionConflict
	}
	want, err := current.WithNewSet(s, next.UpdatedAt)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(want.DTO(), next.DTO()) {
		return false, trainingErrors.ErrInvalidWorkoutSet
	}
	n, err := r.CountIDs(handle, owner, current.ID)
	if err != nil {
		return false, err
	}
	if n >= workout.MaxSetIDs {
		return false, trainingErrors.ErrTrainingLimit
	}
	row := setToModel(s)
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected != 1 {
		return false, nil
	}
	result = tx.Exec("UPDATE mancuerna.workouts SET revision=?,updated_at=?,needs_review=? WHERE account_id=? AND id=? AND revision=? AND state=? AND deleted_at IS NULL AND snapshot_sealed", next.Revision, next.UpdatedAt, next.NeedsReview, owner, current.ID, expected, current.State)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected != 1 {
		return false, trainingErrors.ErrWorkoutRevisionConflict
	}
	return true, nil
}
func (*WorkoutSetPostgresRepository) GetByID(handle shared.Transaction, owner, id uuid.UUID) (out *workout.Set, err error) {
	defer func() {
		err = setPersistenceError(err)
		if err != nil {
			out = nil
		}
	}()
	if !protocol.ValidID(id) {
		return nil, trainingErrors.ErrWorkoutSetNotFound
	}
	tx, err := trainingDataConnection(handle, owner, false)
	if err != nil {
		return nil, err
	}
	var row models.WorkoutSetModel
	err = tx.Where("account_id=? AND id=?", owner, id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, trainingErrors.ErrWorkoutSetNotFound
	}
	if err != nil {
		return nil, err
	}
	out, err = setToDomain(row)
	if err != nil {
		return nil, trainingErrors.ErrTrainingPersistence
	}
	return out, nil
}
func setToModel(s *workout.Set) models.WorkoutSetModel {
	return models.WorkoutSetModel{ID: s.ID, AccountID: s.AccountID, WorkoutID: s.WorkoutID, WorkoutExerciseID: s.WorkoutExerciseID, OriginDeviceID: s.OriginDeviceID, PositionHint: s.PositionHint, LoadMode: s.Load.LoadMode, WeightValue: s.Load.WeightValue, WeightUnit: s.Load.WeightUnit, Repetitions: s.Repetitions, Revision: s.Revision, CapturedAt: s.CapturedAt, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, DeletedAt: s.DeletedAt}
}
func setToDomain(r models.WorkoutSetModel) (*workout.Set, error) {
	load, err := workout.CanonicalLoad(r.LoadMode, r.WeightValue, r.WeightUnit)
	if err != nil {
		return nil, err
	}
	s := &workout.Set{ID: r.ID, AccountID: r.AccountID, WorkoutID: r.WorkoutID, WorkoutExerciseID: r.WorkoutExerciseID, OriginDeviceID: r.OriginDeviceID, PositionHint: r.PositionHint, Repetitions: r.Repetitions, Load: load, Revision: r.Revision, CapturedAt: r.CapturedAt.UTC(), CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(), DeletedAt: r.DeletedAt}
	return s, s.Validate()
}
