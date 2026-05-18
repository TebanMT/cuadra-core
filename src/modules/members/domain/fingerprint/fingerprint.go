// Package fingerprint owns the MemberFingerprint aggregate (UC-028, ADR-002 §3.8).
//
// The fingerprint is the SOCIO's biometric, not the gym's, so it lives in the
// members bounded context. Other BCs (checkins, sync) get to it through the
// repository — never by reaching into another module's tables.
package fingerprint

import (
	"time"

	"github.com/google/uuid"
)

// FormatDP is the only template format the MVP produces (ADR-004 §3.1).
// Stored in `template_format` so a future ANSI-378 migration is non-breaking.
const FormatDP = "dp_uareu"

// QualityScoreFloor is what UC-028 step 4 enforces — DP samples below this are
// rejected at capture time. We re-validate in the domain to guard against an
// SDK regression silently lowering quality.
const QualityScoreFloor = 60

// MaxFingerprintsPerMember caps how many templates one enrollment stores for a
// member. UC-028 captures the SAME finger this many times and keeps each as a
// separate template; the checkin matcher (1:N over the gym gallery) then gets
// that many chances to recognize the member, which cuts false rejects at the
// door. 3 is the norm for optical readers like the U.are.U 4500.
const MaxFingerprintsPerMember = 3

// MemberFingerprint is one enrolled biometric template per member. The
// template bytes are *already encrypted* with the gym's GMK (ADR-006 §2) by
// the time they reach the constructor — domain never holds plaintext.
type MemberFingerprint struct {
	ID                uuid.UUID
	GymID             uuid.UUID
	Version           int
	MemberID          uuid.UUID
	TemplateEncrypted []byte // [version][nonce][ciphertext+tag] per ADR-006 §2.1
	TemplateFormat    string
	QualityScore      *int // SDK self-assessment 0-100; nil if SDK didn't report
	RegisteredBy      uuid.UUID
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
}

// NewMemberFingerprint validates inputs and returns a fresh aggregate ready
// for the repository. Generation of `id` is the caller's job (UUIDs are
// minted client-side per CLAUDE.md "UUIDs generated en cliente").
func NewMemberFingerprint(id, gymID, memberID, registeredBy uuid.UUID, encrypted []byte, format string, qualityScore *int, now time.Time) (*MemberFingerprint, error) {
	if id == uuid.Nil || gymID == uuid.Nil || memberID == uuid.Nil || registeredBy == uuid.Nil {
		return nil, ErrInvalidIdentifiers
	}
	if len(encrypted) == 0 {
		return nil, ErrEmptyTemplate
	}
	if format == "" {
		format = FormatDP
	}
	if qualityScore != nil && (*qualityScore < QualityScoreFloor || *qualityScore > 100) {
		return nil, ErrQualityBelowFloor
	}
	return &MemberFingerprint{
		ID:                id,
		GymID:             gymID,
		Version:           1,
		MemberID:          memberID,
		TemplateEncrypted: encrypted,
		TemplateFormat:    format,
		QualityScore:      qualityScore,
		RegisteredBy:      registeredBy,
		CreatedAt:         now,
		UpdatedAt:         now,
	}, nil
}

// SoftDelete marks the row deleted (ARCO request, re-enrollment). Bumps
// Version so the sync agent picks the change up.
func (f *MemberFingerprint) SoftDelete(now time.Time) {
	t := now
	f.DeletedAt = &t
	f.Version++
	f.UpdatedAt = now
}
