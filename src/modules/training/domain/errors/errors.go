package errors

import "errors"

var (
	ErrInvalidExercise         = errors.New("invalid personal exercise")
	ErrExerciseNotFound        = errors.New("personal exercise unavailable")
	ErrTrainingPersistence     = errors.New("personal training persistence unavailable")
	ErrTrainingConfiguration   = errors.New("invalid personal training configuration")
	ErrInvalidRoutine          = errors.New("invalid personal routine")
	ErrRoutineNotFound         = errors.New("personal routine unavailable")
	ErrRoutineReference        = errors.New("personal routine reference unavailable")
	ErrTrainingLimit           = errors.New("personal training size limit exceeded")
	ErrRoutineRevisionConflict = errors.New("personal routine draft revision changed")
)
