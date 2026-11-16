package dto

import (
	"strconv"

	accountDomain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
)

// Profile matches the generated S00 read contract. It intentionally exposes
// neither account security state nor contact/authentication fields.
type Profile struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	WeightUnit  string `json:"weight_unit"`
	Revision    string `json:"revision"`
}

// ProfileUpdate is only the profile.update payload. The command dispatcher
// validates its exact JSON schema and supplies owner/base from trusted context.
type ProfileUpdate struct {
	DisplayName string `json:"display_name"`
	WeightUnit  string `json:"weight_unit"`
}

func ProfileFromDomain(account *accountDomain.Account) (Profile, error) {
	if err := account.Validate(); err != nil {
		return Profile{}, err
	}
	return Profile{ID: account.ID.String(), DisplayName: account.DisplayName, WeightUnit: string(account.WeightUnit), Revision: strconv.FormatInt(account.Revision, 10)}, nil
}

// ParseProfileRevision converts an already resolved CAS base into PostgreSQL's
// signed bigint without floating-point rounding, signs, or leading zeroes.
// Resolving after_operation_id remains the central command dispatcher's job.
func ParseProfileRevision(value string) (int64, error) {
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != value {
		return 0, personalErrors.ErrInvalidProfileRevision
	}
	return revision, nil
}
