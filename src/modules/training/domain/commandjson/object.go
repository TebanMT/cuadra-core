// Package commandjson validates closed training command values without normalizing them.
package commandjson

import (
	"bytes"
	"encoding/json"
	protocol "github.com/cuadra/cuadra-core/src/shared/sync/personal/domain"
	"io"
	"strconv"
	"unicode/utf8"
)

func Object(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	if len(raw) > 2097152 || !utf8.Valid(raw) {
		return nil, protocol.ErrCommandInvalid
	}
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, protocol.ErrCommandInvalid
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		key, ok := token.(string)
		if err != nil || !ok || !allowed[key] || out[key] != nil {
			return nil, protocol.ErrCommandInvalid
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, protocol.ErrCommandInvalid
		}
		out[key] = value
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') || len(out) != len(keys) {
		return nil, protocol.ErrCommandInvalid
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, protocol.ErrCommandInvalid
	}
	return out, nil
}

func Text(raw []byte, target *string) error {
	if len(raw) < 2 || raw[0] != '"' || !utf8.Valid(raw) {
		return protocol.ErrCommandInvalid
	}
	// Reject unpaired UTF-16 escapes rather than let json.Unmarshal replace them.
	for i := 1; i < len(raw)-1; i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw)-1 {
			return protocol.ErrCommandInvalid
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return protocol.ErrCommandInvalid
		}
		value, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return protocol.ErrCommandInvalid
		}
		i += 4
		if value >= 0xDC00 && value <= 0xDFFF {
			return protocol.ErrCommandInvalid
		}
		if value >= 0xD800 && value <= 0xDBFF {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return protocol.ErrCommandInvalid
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xDC00 || low > 0xDFFF {
				return protocol.ErrCommandInvalid
			}
			i += 6
		}
	}
	if json.Unmarshal(raw, target) != nil {
		return protocol.ErrCommandInvalid
	}
	return nil
}
