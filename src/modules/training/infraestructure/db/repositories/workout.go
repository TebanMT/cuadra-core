//go:build server

package repositories

import (
	"errors"
	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/repository"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/routine"
	"github.com/cuadra/cuadra-core/src/modules/training/domain/workout"
	"github.com/cuadra/cuadra-core/src/modules/training/infraestructure/db/models"
	"github.com/cuadra/cuadra-core/src/modules/training/infraestructure/timezone"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"sort"
)

type WorkoutPostgresRepository struct{}

func NewWorkoutPostgresRepository() *WorkoutPostgresRepository { return &WorkoutPostgresRepository{} }

var _ repository.WorkoutRepository = (*WorkoutPostgresRepository)(nil)

func workoutPersistenceError(err error) error {
	for _, known := range []error{trainingErrors.ErrInvalidWorkout, trainingErrors.ErrWorkoutNotFound} {
		if errors.Is(err, known) {
			return known
		}
	}
	return trainingPersistenceError(err)
}
func (r *WorkoutPostgresRepository) Create(handle shared.Transaction, owner uuid.UUID, w *workout.Workout) (created bool, err error) {
	defer func() {
		err = workoutPersistenceError(err)
		if err != nil {
			created = false
		}
	}()
	if w.ValidateCreation(owner) != nil || !timezone.NewIANA().Valid(w.Timezone) {
		return false, trainingErrors.ErrInvalidWorkout
	}
	tx, err := trainingDataConnection(handle, owner, true)
	if err != nil {
		return false, err
	}
	version, err := NewRoutinePublicationPostgresRepository().GetVersion(handle, owner, w.RoutineVersionID)
	if err != nil {
		return false, err
	}
	if !w.MatchesVersion(version) {
		return false, trainingErrors.ErrInvalidWorkout
	}
	const savepoint = "mancuerna_workout_start"
	if err = tx.Exec("SAVEPOINT " + savepoint).Error; err != nil {
		return false, err
	}
	collision := func() (bool, error) {
		if err := tx.Exec("ROLLBACK TO SAVEPOINT " + savepoint).Error; err != nil {
			return false, err
		}
		return false, tx.Exec("RELEASE SAVEPOINT " + savepoint).Error
	}
	root, children := workoutToModels(w)
	result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&root)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected != 1 {
		return collision()
	}
	sort.Slice(children, func(i, j int) bool { return children[i].ID.String() < children[j].ID.String() })
	result = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&children)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected != int64(len(children)) {
		return collision()
	}
	result = tx.Exec("UPDATE mancuerna.workouts SET snapshot_sealed=true WHERE account_id=? AND id=? AND NOT snapshot_sealed", owner, w.ID)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected != 1 {
		return false, trainingErrors.ErrTrainingPersistence
	}
	if err = tx.Exec("RELEASE SAVEPOINT " + savepoint).Error; err != nil {
		return false, err
	}
	return true, nil
}
func (r *WorkoutPostgresRepository) GetByID(handle shared.Transaction, owner, id uuid.UUID) (out *workout.Workout, err error) {
	defer func() {
		err = workoutPersistenceError(err)
		if err != nil {
			out = nil
		}
	}()
	if !protocol.ValidID(id) {
		return nil, trainingErrors.ErrWorkoutNotFound
	}
	tx, err := trainingDataConnection(handle, owner, false)
	if err != nil {
		return nil, err
	}
	var root models.WorkoutModel
	err = tx.Where("account_id=? AND id=? AND snapshot_sealed", owner, id).Take(&root).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, trainingErrors.ErrWorkoutNotFound
	}
	if err != nil {
		return nil, err
	}
	var rows []models.WorkoutExerciseModel
	if err = tx.Where("account_id=? AND workout_id=?", owner, id).Order("position ASC,id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out, err = workoutToDomain(root, rows)
	if err != nil {
		return nil, trainingErrors.ErrTrainingPersistence
	}
	return out, nil
}
func workoutToModels(w *workout.Workout) (models.WorkoutModel, []models.WorkoutExerciseModel) {
	root := models.WorkoutModel{ID: w.ID, AccountID: w.AccountID, RoutineVersionID: w.RoutineVersionID, RoutineDayID: w.RoutineDayID, RoutineTitleSnapshot: w.RoutineTitleSnapshot, DayLabelSnapshot: w.DayLabelSnapshot, State: w.State, StartedAt: w.StartedAt, FinishedAt: w.FinishedAt, Timezone: w.Timezone, Revision: w.Revision, FinalizedRevision: w.FinalizedRevision, NeedsReview: w.NeedsReview, OriginDeviceID: w.OriginDeviceID, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt, DeletedAt: w.DeletedAt}
	children := make([]models.WorkoutExerciseModel, len(w.Exercises))
	for i, e := range w.Exercises {
		row := models.WorkoutExerciseModel{ID: e.ID, AccountID: w.AccountID, WorkoutID: w.ID, RoutineVersionID: w.RoutineVersionID, RoutineDayID: w.RoutineDayID, SourceExerciseID: e.SourceExerciseID, NameSnapshot: e.NameSnapshot, LoadModeSnapshot: e.LoadModeSnapshot, Position: e.Position, PrescribedSets: e.PrescribedSets, PrescribedReps: e.PrescribedReps, RestSeconds: e.RestSeconds, Notes: e.Notes}
		ref := uuid.MustParse(e.Reference.ID)
		if e.Reference.Source == "personal" {
			row.PersonalExerciseID = &ref
		} else {
			row.CatalogExerciseID = &ref
		}
		children[i] = row
	}
	return root, children
}
func workoutToDomain(r models.WorkoutModel, rows []models.WorkoutExerciseModel) (*workout.Workout, error) {
	w := &workout.Workout{ID: r.ID, AccountID: r.AccountID, RoutineVersionID: r.RoutineVersionID, RoutineDayID: r.RoutineDayID, OriginDeviceID: r.OriginDeviceID, RoutineTitleSnapshot: r.RoutineTitleSnapshot, DayLabelSnapshot: r.DayLabelSnapshot, State: r.State, Timezone: r.Timezone, StartedAt: r.StartedAt.UTC(), CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(), FinishedAt: r.FinishedAt, DeletedAt: r.DeletedAt, Revision: r.Revision, FinalizedRevision: r.FinalizedRevision, NeedsReview: r.NeedsReview, Exercises: make([]workout.Exercise, len(rows))}
	for i, e := range rows {
		if e.AccountID != r.AccountID || e.WorkoutID != r.ID || e.RoutineVersionID != r.RoutineVersionID || e.RoutineDayID != r.RoutineDayID || (e.CatalogExerciseID == nil) == (e.PersonalExerciseID == nil) {
			return nil, trainingErrors.ErrTrainingPersistence
		}
		ref := routine.ExerciseRef{}
		if e.PersonalExerciseID != nil {
			ref.Source = "personal"
			ref.ID = e.PersonalExerciseID.String()
		} else {
			ref.Source = "catalog"
			ref.ID = e.CatalogExerciseID.String()
		}
		w.Exercises[i] = workout.Exercise{ID: e.ID, SourceExerciseID: e.SourceExerciseID, Reference: ref, NameSnapshot: e.NameSnapshot, LoadModeSnapshot: e.LoadModeSnapshot, Position: e.Position, PrescribedSets: e.PrescribedSets, PrescribedReps: e.PrescribedReps, RestSeconds: e.RestSeconds, Notes: e.Notes}
	}
	return w, w.Validate()
}
