package app

import (
	"time"

	"github.com/google/uuid"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// MemberNumberConfig is the gyms-side implementation of the members BC seam
// members/app.MemberNumberDigitsStore (satisfied structurally — no import of
// the members package, so no cycle). It reads/grows the per-gym member-number
// length stored in the gym's KioskSettings JSON (ADR-010 §2.1). Wired in
// cmd/server + cmd/sidecar so the asignador honors the configured length and
// can bump it monotonically when the space fills.
type MemberNumberConfig struct {
	Gyms gymRepo.GymRepository
}

func NewMemberNumberConfig(gyms gymRepo.GymRepository) *MemberNumberConfig {
	return &MemberNumberConfig{Gyms: gyms}
}

// Digits returns the gym's configured member-number length (default 4).
func (c *MemberNumberConfig) Digits(tx sharedDomain.Transaction, gymID uuid.UUID) (int, error) {
	g, err := c.Gyms.GetByID(tx, gymID)
	if err != nil {
		return 0, err
	}
	return g.MemberNumberDigits(), nil
}

// Grow raises the gym's member-number length to `digits` (monotonic — gym
// domain ignores a non-increase) and persists within the caller's tx so the
// bump rides the same transaction as the member that triggered it.
func (c *MemberNumberConfig) Grow(tx sharedDomain.Transaction, gymID uuid.UUID, digits int) error {
	g, err := c.Gyms.GetByID(tx, gymID)
	if err != nil {
		return err
	}
	if g.GrowMemberNumberDigits(digits, time.Now().UTC()) {
		if _, err := c.Gyms.Update(tx, g); err != nil {
			return err
		}
	}
	return nil
}
