package utils

import (
	"errors"
	"log"

	"github.com/gin-gonic/gin"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// genericServerErrorMessage masks any 5xx detail. The real error is logged
// server-side; the wire payload stays generic so we don't leak DB hosts,
// connection strings, or stack traces.
const genericServerErrorMessage = "Ocurrió un error inesperado. Por favor intenta de nuevo más tarde."

var statusMessages = map[int]string{
	200: "OK",
	201: "CREATED",
	204: "NO_CONTENT",
	400: "BAD_REQUEST",
	401: "UNAUTHORIZED",
	403: "FORBIDDEN",
	404: "NOT_FOUND",
	409: "CONFLICT",
	422: "UNPROCESSABLE_ENTITY",
	429: "TOO_MANY_REQUESTS",
	500: "INTERNAL_SERVER_ERROR",
}

type Response[T any] struct {
	StatusCode int     `json:"status_code"`
	Message    string  `json:"message"`
	Data       T       `json:"data"`
	Exception  *string `json:"exception,omitempty"`
}

func JsonResponse[T any](c *gin.Context, statusCode int, data T) {
	c.JSON(statusCode, Response[T]{
		StatusCode: statusCode,
		Message:    statusMessages[statusCode],
		Data:       data,
	})
}

// ErrorResponse standardises error wire payloads. Logs the real error
// server-side; for 5xx (and any UnexpectedError regardless of mapped status)
// the wire message is scrubbed.
func ErrorResponse(c *gin.Context, statusCode int, err error) {
	log.Printf(
		"[ERROR] %s %s status=%d client=%s: %v",
		c.Request.Method,
		c.Request.URL.Path,
		statusCode,
		c.ClientIP(),
		err,
	)
	msg := err.Error()
	if statusCode >= 500 || isUnexpected(err) {
		msg = genericServerErrorMessage
	}
	c.JSON(statusCode, Response[any]{
		StatusCode: statusCode,
		Message:    statusMessages[statusCode],
		Data:       nil,
		Exception:  &msg,
	})
}

func isUnexpected(err error) bool {
	var ce sharedDomain.CustomError
	if errors.As(err, &ce) {
		return ce.ErrorCode == sharedDomain.CodeUnexpected
	}
	return false
}

// DomainErrorToHttpCode translates a domain-error code to its HTTP status.
// Anything we don't recognize ends up as 500.
func DomainErrorToHttpCode(err error) int {
	var ce sharedDomain.CustomError
	if errors.As(err, &ce) {
		switch ce.ErrorCode {
		case sharedDomain.CodeValidation:
			return 400
		case sharedDomain.CodeBusiness:
			return 422
		case sharedDomain.CodeUnexpected:
			return 500
		}
	}
	return 500
}
