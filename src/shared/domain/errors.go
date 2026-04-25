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
type CustomError struct {
	Err           error
	CustomMessage string
	ErrorCode     ErrorCode
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

func NewUnexpectedError(err error) CustomError {
	return CustomError{Err: err, ErrorCode: CodeUnexpected}
}
