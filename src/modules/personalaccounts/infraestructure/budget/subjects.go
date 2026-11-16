package budget

import (
	"crypto/hmac"
	"crypto/sha256"
	"strings"

	domain "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	budgetErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
)

// Subjects holds a stable injected key. No random key, environment fallback,
// or implicit rotation can reset persistent counters after a process restart.
type Subjects struct{ key []byte }

func NewSubjects(key []byte) (*Subjects, error) {
	if len(key) < 32 {
		return nil, budgetErrors.ErrBudgetConfiguration
	}
	return &Subjects{key: append([]byte(nil), key...)}, nil
}

func (*Subjects) String() string     { return "OTP budget subject hasher (key redacted)" }
func (s *Subjects) GoString() string { return s.String() }

func (s *Subjects) Hash(scope domain.Scope, subject string) ([32]byte, error) {
	if s == nil || len(s.key) < 32 {
		return [32]byte{}, budgetErrors.ErrBudgetConfiguration
	}
	if (scope != domain.Destination && scope != domain.Origin && scope != domain.Service && scope != domain.Recovery) || strings.TrimSpace(subject) == "" || len(subject) > 4096 {
		return [32]byte{}, budgetErrors.ErrBudgetInput
	}
	h := hmac.New(sha256.New, s.key)
	_, _ = h.Write([]byte("mancuerna:otp-budget:subject:v1\x00" + string(scope) + "\x00"))
	_, _ = h.Write([]byte(subject))
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result, nil
}

var _ domain.SubjectHasher = (*Subjects)(nil)
