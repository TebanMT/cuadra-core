package domain

import "fmt"

// ErrorCode classifies domain errors so the HTTP layer can map them to status
// codes without leaking internals (see shared/utils.DomainErrorToHttpCode).
type ErrorCode string

const (
	CodeValidation ErrorCode = "01" // 400 — bad input
	CodeBusiness   ErrorCode = "02" // 422 — valid input, broken invariant
	CodeUnexpected ErrorCode = "00" // 500 — infra failure
)

// CustomError wraps a sentinel error with a classification + optional
// user-facing override. The Err field is the canonical reason; CustomMessage,
// when set, is what the client sees (kept in es-MX for 4xx; scrubbed for 5xx).
// Data is an optional payload the controller can surface in the response body
// (e.g. existing_member_id on a duplicate-detection error).
type CustomError struct {
	Err           error
	CustomMessage string
	ErrorCode     ErrorCode
	Data          map[string]any
}

func (e CustomError) Error() string {
	if e.CustomMessage == "" {
		return e.Err.Error()
	}
	return fmt.Sprintf("%s: %s", e.Err.Error(), e.CustomMessage)
}

func (e CustomError) Unwrap() error { return e.Err }

func NewValidationError(err error) CustomError {
	return CustomError{Err: err, ErrorCode: CodeValidation}
}

func NewValidationErrorWithMessage(err error, msg string) CustomError {
	return CustomError{Err: err, CustomMessage: msg, ErrorCode: CodeValidation}
}

func NewBusinessError(err error, customMessage string) CustomError {
	return CustomError{Err: err, CustomMessage: customMessage, ErrorCode: CodeBusiness}
}

// NewBusinessErrorWithData attaches a payload the controller can surface to
// the client alongside the standard error envelope (e.g. fingerprint-collision
// surfaces the existing member's id + name).
func NewBusinessErrorWithData(err error, customMessage string, data map[string]any) CustomError {
	return CustomError{Err: err, CustomMessage: customMessage, ErrorCode: CodeBusiness, Data: data}
}

func NewUnexpectedError(err error) CustomError {
	return CustomError{Err: err, ErrorCode: CodeUnexpected}
}
