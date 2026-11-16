package repositories

import (
	"encoding/json"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// permissionsWire is the JSON value written to the sync payload: always an
// array (never null) so the column's NOT NULL holds on every device.
func permissionsWire(perms []sharedDomain.Permission) []string {
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, string(p))
	}
	return out
}

// marshalPermissions encodes the column value ('[]' when empty).
func marshalPermissions(perms []sharedDomain.Permission) string {
	raw, err := json.Marshal(permissionsWire(perms))
	if err != nil {
		return "[]"
	}
	return string(raw)
}

// unmarshalPermissions decodes the column value. Unknown keys are dropped so
// a value written by a newer build never grants something this build does
// not know how to check.
func unmarshalPermissions(raw string) []sharedDomain.Permission {
	var keys []string
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &keys)
	}
	out := make([]sharedDomain.Permission, 0, len(keys))
	for _, k := range keys {
		if p := sharedDomain.Permission(k); sharedDomain.IsKnownPermission(p) {
			out = append(out, p)
		}
	}
	return out
}
