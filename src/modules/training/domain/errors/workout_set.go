package errors

import "errors"

var ErrInvalidWorkoutSet = errors.New("invalid personal workout set")
var ErrWorkoutSetNotFound = errors.New("personal workout set unavailable")
var ErrWorkoutRevisionConflict = errors.New("personal workout revision changed")
