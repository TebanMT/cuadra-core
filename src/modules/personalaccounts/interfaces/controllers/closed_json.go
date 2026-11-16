//go:build server

package controllers

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
)

// encoding/json matches field names case-insensitively even when unknown
// fields are disallowed. The contract's closed JSON objects require exact keys.
// Canonical request hashing has already rejected duplicate keys/trailing input.
func decodeClosedJSON(raw []byte, destination any) error {
	typeOf := reflect.TypeOf(destination)
	if typeOf == nil || typeOf.Kind() != reflect.Pointer || typeOf.Elem().Kind() != reflect.Struct {
		return personalErrors.ErrInvalidAuthAttempt
	}
	if err := exactJSONKeys(raw, typeOf.Elem()); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

func exactJSONKeys(raw []byte, shape reflect.Type) error {
	for shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	if shape == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	if shape.Kind() == reflect.Slice || shape.Kind() == reflect.Array {
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return personalErrors.ErrInvalidAuthAttempt
		}
		for _, value := range values {
			if err := exactJSONKeys(value, shape.Elem()); err != nil {
				return err
			}
		}
		return nil
	}
	if shape.Kind() != reflect.Struct {
		return nil
	}
	var properties map[string]json.RawMessage
	if json.Unmarshal(raw, &properties) != nil || properties == nil {
		return personalErrors.ErrInvalidAuthAttempt
	}
	fields := map[string]reflect.Type{}
	for i := 0; i < shape.NumField(); i++ {
		field := shape.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			fields[name] = field.Type
		}
	}
	for name, value := range properties {
		field, ok := fields[name]
		if !ok {
			return personalErrors.ErrInvalidAuthAttempt
		}
		if err := exactJSONKeys(value, field); err != nil {
			return err
		}
	}
	return nil
}
