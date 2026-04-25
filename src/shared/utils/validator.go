package utils

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/go-playground/validator/v10"
)

var validate = validator.New()

func init() {
	validate.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "-" || name == "" {
			return fld.Name
		}
		return name
	})
}

// ValidateRequest applies struct-level `validate:` tags. Returns the first
// failure with a friendly message; cheaper than collecting them all and
// matches Kash's behaviour.
func ValidateRequest(req any) error {
	if err := validate.Struct(req); err != nil {
		var fe validator.ValidationErrors
		if errors.As(err, &fe) && len(fe) > 0 {
			return errors.New(messageFor(fe[0]))
		}
		return err
	}
	return nil
}

func messageFor(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return fe.Field() + ": este campo es obligatorio"
	case "email":
		return fe.Field() + ": debe ser un correo válido"
	case "min":
		return fmt.Sprintf("%s: mínimo %s caracteres", fe.Field(), fe.Param())
	case "max":
		return fmt.Sprintf("%s: máximo %s caracteres", fe.Field(), fe.Param())
	case "uuid":
		return fe.Field() + ": debe ser un UUID válido"
	case "oneof":
		return fmt.Sprintf("%s: debe ser uno de [%s]", fe.Field(), fe.Param())
	default:
		return fe.Field() + ": formato inválido"
	}
}
