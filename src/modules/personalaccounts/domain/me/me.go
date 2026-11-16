// Package me contains the narrow authenticated account view and configured
// capabilities. It never carries an unmasked phone or credential.
package me

import (
	"strings"
	"unicode/utf8"

	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/account"
	personalErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
)

type Snapshot struct {
	Profile          account.Account
	PhoneMasked      string
	SecurityVersion  int64
	RecoveryPrepared bool
}

func (s *Snapshot) Validate() error {
	if s == nil || s.Profile.Validate() != nil || s.Profile.DeletedAt != nil ||
		(s.Profile.Status != account.Active && s.Profile.Status != account.DeletionPending) ||
		s.SecurityVersion < 1 || !strings.HasPrefix(s.PhoneMasked, "•••• ") || len(s.PhoneMasked) != len("•••• ")+4 {
		return personalErrors.ErrInvalidMe
	}
	for _, c := range strings.TrimPrefix(s.PhoneMasked, "•••• ") {
		if c < '0' || c > '9' {
			return personalErrors.ErrInvalidMe
		}
	}
	return nil
}

// Every field is supplied by deployment policy. These protocol limits do not
// promise that a feature is implemented; Features is the explicit allowlist.
type Capabilities struct {
	ProtocolVersion        int
	MinimumProtocolVersion int
	MinimumAppVersion      string
	MaxPushOperations      int
	MaxRequestBytes        int
	MaxGroupBytes          int
	Features               []string
	PolicyVersion          *string
}

func (c Capabilities) Validate() error {
	if c.ProtocolVersion != 1 || c.MinimumProtocolVersion != 1 || c.MaxPushOperations != 50 ||
		c.MaxRequestBytes != 2097152 || c.MaxGroupBytes != 1048576 ||
		!validText(c.MinimumAppVersion, 30) || c.Features == nil || len(c.Features) > 3 ||
		(c.PolicyVersion != nil && !validText(*c.PolicyVersion, 80)) {
		return personalErrors.ErrInvalidSecurityConfiguration
	}
	seen := map[string]bool{}
	for _, feature := range c.Features {
		if (feature != "own_routines" && feature != "workouts" && feature != "personal_sync") || seen[feature] {
			return personalErrors.ErrInvalidSecurityConfiguration
		}
		seen[feature] = true
	}
	return nil
}

func (c Capabilities) Clone() Capabilities {
	c.Features = append([]string{}, c.Features...)
	if c.PolicyVersion != nil {
		value := *c.PolicyVersion
		c.PolicyVersion = &value
	}
	return c
}

func validText(value string, max int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= max && !strings.ContainsRune(value, 0)
}
