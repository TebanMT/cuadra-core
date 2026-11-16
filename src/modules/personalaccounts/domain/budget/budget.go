// Package budget defines fixed-window OTP capacity. Limits count operations;
// they are not a monetary authorization or a substitute for provider billing.
package budget

import (
	"bytes"
	"sort"
	"time"

	budgetErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
)

type Scope string

const (
	Destination Scope = "destination"
	Origin      Scope = "origin"
	Service     Scope = "service"
	Recovery    Scope = "recovery"
)

type Action string

const (
	Send  Action = "send"
	Check Action = "check"
)

type Limit struct {
	Window   time.Duration
	MaxSend  int
	MaxCheck int
}

func (l Limit) Valid() bool {
	return l.Window >= time.Second && l.Window%time.Second == 0 && l.MaxSend > 0 && l.MaxCheck > 0 && l.MaxSend <= 2147483647 && l.MaxCheck <= 2147483647
}

type Rules struct{ limits map[Scope]Limit }

// NewRules requires all three shared scopes, without commercial defaults.
// Channel is intentionally absent: WhatsApp and SMS reserve the same budget.
func NewRules(limits map[Scope]Limit) (Rules, error) {
	if len(limits) != 3 {
		return Rules{}, budgetErrors.ErrBudgetConfiguration
	}
	copy := make(map[Scope]Limit, 3)
	for _, scope := range []Scope{Destination, Origin, Service} {
		limit, ok := limits[scope]
		if !ok || !limit.Valid() {
			return Rules{}, budgetErrors.ErrBudgetConfiguration
		}
		copy[scope] = limit
	}
	return Rules{limits: copy}, nil
}

// Recovery capacity is a separate policy; it never replaces the three scopes
// required to send or check a phone verification.
func NewRecoveryRules(limit Limit) (Rules, error) {
	if !limit.Valid() {
		return Rules{}, budgetErrors.ErrBudgetConfiguration
	}
	return Rules{limits: map[Scope]Limit{Recovery: limit}}, nil
}

type Dimension struct {
	Scope       Scope
	SubjectHash [32]byte
	WindowStart time.Time
	Limit       Limit
}

type Plan struct {
	at         time.Time
	action     Action
	dimensions []Dimension
}

// Plan must use a trusted server clock and canonical, already hashed subjects.
// Its immutable copy prevents later config mutations changing a reservation.
func (r Rules) Plan(at time.Time, action Action, subjects map[Scope][32]byte) (Plan, error) {
	if len(r.limits) == 0 || len(subjects) != len(r.limits) {
		return Plan{}, budgetErrors.ErrBudgetConfiguration
	}
	if at.IsZero() || (action != Send && action != Check) {
		return Plan{}, budgetErrors.ErrBudgetInput
	}
	p := Plan{at: at.UTC(), action: action}
	for scope, limit := range r.limits {
		hash, ok := subjects[scope]
		if !ok || hash == [32]byte{} || !limit.Valid() {
			return Plan{}, budgetErrors.ErrBudgetInput
		}
		p.dimensions = append(p.dimensions, Dimension{Scope: scope, SubjectHash: hash, WindowStart: WindowStart(at, limit.Window), Limit: limit})
	}
	sort.Slice(p.dimensions, func(i, j int) bool {
		a, b := p.dimensions[i], p.dimensions[j]
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		return bytes.Compare(a.SubjectHash[:], b.SubjectHash[:]) < 0
	})
	return p, nil
}

func (p Plan) At() time.Time           { return p.at }
func (p Plan) Action() Action          { return p.action }
func (p Plan) Dimensions() []Dimension { return append([]Dimension(nil), p.dimensions...) }
func (p Plan) Valid() bool {
	return !p.at.IsZero() && (p.action == Send || p.action == Check) && (len(p.dimensions) == 1 || len(p.dimensions) == 3)
}

// WindowStart anchors fixed windows to the Unix epoch, independent of the
// machine's timezone and daylight saving. Rules allow whole seconds only.
func WindowStart(at time.Time, window time.Duration) time.Time {
	seconds := int64(window / time.Second)
	if seconds <= 0 {
		return time.Time{}
	}
	u := at.Unix()
	q := u / seconds
	if u < 0 && u%seconds != 0 {
		q--
	}
	return time.Unix(q*seconds, 0).UTC()
}

type Counter struct {
	Scope        Scope
	SubjectHash  [32]byte
	WindowStart  time.Time
	SendCount    int
	CheckCount   int
	BlockedUntil *time.Time
}

// Reserve mutates only this in-memory candidate. The repository evaluates
// every dimension before writing any accepted counter to PostgreSQL.
func (c *Counter) Reserve(at time.Time, action Action, limit Limit) error {
	if !limit.Valid() {
		return budgetErrors.ErrBudgetConfiguration
	}
	if at.IsZero() || (action != Send && action != Check) || !c.WindowStart.Equal(WindowStart(at, limit.Window)) {
		return budgetErrors.ErrBudgetInput
	}
	if c.SendCount < 0 || c.CheckCount < 0 {
		return budgetErrors.ErrBudgetCorrupt
	}
	if c.BlockedUntil != nil && at.Before(*c.BlockedUntil) {
		return budgetErrors.ErrBudgetBlocked
	}
	if action == Send {
		if c.SendCount >= limit.MaxSend {
			return budgetErrors.ErrBudgetExceeded
		}
		c.SendCount++
	} else {
		if c.CheckCount >= limit.MaxCheck {
			return budgetErrors.ErrBudgetExceeded
		}
		c.CheckCount++
	}
	return nil
}

type Repository interface {
	// Reserve uses only the supplied transaction. The caller must propagate
	// any error so the future attempt and all budget dimensions roll back.
	Reserve(shared.Transaction, Plan) error
}

type SubjectHasher interface {
	Hash(Scope, string) ([32]byte, error)
}
