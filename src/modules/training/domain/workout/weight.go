package workout

import (
	"regexp"
	"strings"

	trainingErrors "github.com/cuadra/cuadra-core/src/modules/training/domain/errors"
)

var weightPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,6})(\.[0-9]{1,3})?$`)

type Load struct {
	LoadMode    string
	WeightValue *string
	WeightUnit  *string
}

// CanonicalWeight is only for entity values/read projections, never request
// hashing. The caller must retain the original command text. Padding decimal
// digits is exact: no float, rounding, unit conversion or trimming occurs.
func CanonicalWeight(value string) (string, error) {
	if !weightPattern.MatchString(value) {
		return "", trainingErrors.ErrInvalidWeight
	}
	whole, fraction, _ := strings.Cut(value, ".")
	return whole + "." + fraction + strings.Repeat("0", 3-len(fraction)), nil
}

// CanonicalLoad preserves explicit zero as an external load. Returned pointers
// are detached so later changes to caller-owned input cannot change the value.
func CanonicalLoad(mode string, value, unit *string) (Load, error) {
	if mode == "none" && value == nil && unit == nil {
		return Load{LoadMode: "none"}, nil
	}
	if mode != "external" || value == nil || unit == nil || (*unit != "kg" && *unit != "lb") {
		return Load{}, trainingErrors.ErrInvalidWeight
	}
	weight, err := CanonicalWeight(*value)
	if err != nil {
		return Load{}, err
	}
	weightUnit := *unit
	return Load{LoadMode: "external", WeightValue: &weight, WeightUnit: &weightUnit}, nil
}
