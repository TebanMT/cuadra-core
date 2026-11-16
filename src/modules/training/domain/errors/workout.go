package errors

import "errors"

var ErrInvalidWorkout = errors.New("invalid personal workout")
var ErrWorkoutNotFound = errors.New("personal workout unavailable")
