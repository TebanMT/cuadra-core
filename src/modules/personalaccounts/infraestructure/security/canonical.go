package security

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/google/uuid"
)

var rawURL = base64.RawURLEncoding.Strict()

func validKeyVersion(version string) bool {
	return version != "" && len(version) <= 128 && strings.TrimSpace(version) == version && utf8.ValidString(version)
}

func validID(id uuid.UUID) bool {
	return id != uuid.Nil && id.Variant() == uuid.RFC4122 && id.Version() >= 1 && id.Version() <= 8
}

func parseID(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil || !validID(id) || id.String() != value {
		return uuid.Nil, personalErrors.ErrInvalidProof
	}
	return id, nil
}

// RequestHash reproduces the S00 security.py request envelope. operationID is
// the Idempotency-Key, and path is the exact concrete contract path. Headers
// (including X-Device-Proof) are deliberately not part of this representation.
// S00 uses JSON integers; decimal quantities and revisions are JSON strings.
func RequestHash(method, path string, operationID uuid.UUID, body []byte) ([32]byte, error) {
	var zero [32]byte
	if !validID(operationID) || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#\r\n") {
		return zero, personalErrors.ErrInvalidProof
	}
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		return zero, personalErrors.ErrInvalidProof
	}
	value, err := parseJSON(body)
	if err != nil {
		return zero, personalErrors.ErrInvalidProof
	}
	encoded, err := canonical(map[string]any{"method": method, "path": path, "operation_id": operationID.String(), "body": value})
	if err != nil {
		return zero, personalErrors.ErrInvalidProof
	}
	return sha256.Sum256(encoded), nil
}

// canonical matches json.dumps(sort_keys=True, separators=(',', ':'),
// ensure_ascii=False) from S00 for its contract value types. In particular,
// HTML characters and U+2028/U+2029 are not rewritten by Go's JSON encoder.
func canonical(value any) ([]byte, error) {
	var out bytes.Buffer
	if err := appendCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func appendCanonical(out *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(value))
	case string:
		if !utf8.ValidString(value) {
			return personalErrors.ErrInvalidProof
		}
		out.WriteByte('"')
		for _, r := range value {
			switch r {
			case '"', '\\':
				out.WriteByte('\\')
				out.WriteRune(r)
			case '\b':
				out.WriteString(`\b`)
			case '\f':
				out.WriteString(`\f`)
			case '\n':
				out.WriteString(`\n`)
			case '\r':
				out.WriteString(`\r`)
			case '\t':
				out.WriteString(`\t`)
			default:
				if r < 0x20 {
					fmt.Fprintf(out, `\u%04x`, r)
				} else {
					out.WriteRune(r)
				}
			}
		}
		out.WriteByte('"')
	case int:
		out.WriteString(strconv.Itoa(value))
	case json.Number:
		// A non-integer JSON representation has no S00 cross-language
		// canonicalization rule. Reject it instead of hashing rounded floats.
		if strings.ContainsAny(string(value), ".eE") {
			return personalErrors.ErrInvalidProof
		}
		if value == "-0" {
			out.WriteByte('0')
		} else {
			out.WriteString(string(value))
		}
	case []any:
		out.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := appendCanonical(out, key); err != nil {
				return err
			}
			out.WriteByte(':')
			if err := appendCanonical(out, value[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return personalErrors.ErrInvalidProof
	}
	return nil
}

func parseJSON(encoded []byte) (any, error) {
	if !utf8.Valid(encoded) || !validJSONUnicode(encoded) {
		return nil, personalErrors.ErrInvalidProof
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	value, err := readJSON(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, personalErrors.ErrInvalidProof
	}
	return value, nil
}

// encoding/json replaces unpaired UTF-16 escapes with U+FFFD. S00's UTF-8
// canonical representation cannot encode those inputs, so accepting that
// replacement would create a different signed message across implementations.
func validJSONUnicode(encoded []byte) bool {
	inString := false
	for i := 0; i < len(encoded); i++ {
		if encoded[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || encoded[i] != '\\' {
			continue
		}
		i++
		if i >= len(encoded) {
			return false
		}
		if encoded[i] != 'u' {
			continue
		}
		if i+4 >= len(encoded) {
			return false
		}
		unit, err := strconv.ParseUint(string(encoded[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
		if unit < 0xd800 || unit > 0xdbff {
			continue
		}
		if i+6 >= len(encoded) || encoded[i+1] != '\\' || encoded[i+2] != 'u' {
			return false
		}
		low, err := strconv.ParseUint(string(encoded[i+3:i+7]), 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}

func readJSON(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, personalErrors.ErrInvalidProof
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return token, nil
	}
	switch delimiter {
	case '{':
		value := make(map[string]any)
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := token.(string)
			if !ok {
				return nil, personalErrors.ErrInvalidProof
			}
			if _, duplicate := value[key]; duplicate {
				return nil, personalErrors.ErrInvalidProof
			}
			value[key], err = readJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, personalErrors.ErrInvalidProof
		}
		return value, nil
	case '[':
		value := make([]any, 0)
		for decoder.More() {
			item, err := readJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			value = append(value, item)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, personalErrors.ErrInvalidProof
		}
		return value, nil
	default:
		return nil, personalErrors.ErrInvalidProof
	}
}
