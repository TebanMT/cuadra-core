package app

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"

	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	fpDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// RegisterFingerprintInput backs UC-028. The capture step (the multi-sample
// loop talking to the reader) lives in the controller / FE — by the time we
// reach the use case we already have the SDK output for each sample.
type RegisterFingerprintInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	MemberID    uuid.UUID
	// Captures holds 1..MaxFingerprintsPerMember samples of the SAME finger.
	// Each is stored as its own template so the checkin matcher gets that
	// many chances to recognize the member.
	Captures        []*biometric.CaptureResult
	ConsentAccepted bool // UC-028 step 1 + ADR-006 §5
}

// RegisterFingerprintOutput is what the controller turns into the JSON
// response (UC-028 step 9 — "✓ Huella registrada").
type RegisterFingerprintOutput struct {
	FingerprintIDs []uuid.UUID
	MemberID       uuid.UUID
	QualityScore   *int // best (highest) self-assessed quality across captures
	RegisteredAt   time.Time
}

// FingerprintMatcher is the narrow 1:N surface RegisterFingerprint needs for
// the pre-enrollment collision check. The sidecar wires the BiometricHub
// (which asks the tinta-bio helper to identify against its cached gallery);
// cloud builds leave it nil and the check is skipped, same as before.
type FingerprintMatcher interface {
	// IdentifyFMD returns the member that owns a template matching the
	// probe FMD, or uuid.Nil when nothing matches. biometric.ErrNotAvailable
	// means the engine isn't ready — the caller treats it as "skip check".
	IdentifyFMD(ctx context.Context, fmd string) (uuid.UUID, error)
}

// RegisterFingerprint implements UC-028. The flow:
//
//  1. Validate consent (ADR-006 §5) and every capture's quality.
//  2. Pre-enrollment 1:N collision check (Matcher wired): if the enrollment
//     FMD matches an existing template of ANOTHER member, surface
//     ErrFingerprintCollision with the existing member's id+name so the
//     operator can investigate. Runs OUTSIDE the write tx — engine calls
//     don't belong inside Postgres/SQLite txs.
//  3. Confirm the member exists in this gym.
//  4. Reject if the member already has any active fingerprint — re-enrollment
//     is delete-first.
//  5. Encrypt every capture's template with the gym GMK (ADR-006 §2.4).
//  6. Insert all N templates + audit + sync queue, atomically in one Command
//     tx — the member gets all N or none.
//
// The plaintext captures are zeroed before returning — defense in depth
// against memory snapshots even though Go's GC may keep copies.
type RegisterFingerprint struct {
	Members      memRepo.MemberRepository
	Fingerprints memRepo.FingerprintRepository
	GMK          bcrypto.GMKProvider
	// Matcher is optional. When nil (cloud builds) or it reports
	// biometric.ErrNotAvailable (engine down), the collision check is
	// skipped and a warning is logged. Production enrollment runs on the
	// sidecar where the hub is always wired.
	Matcher FingerprintMatcher
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
}

func NewRegisterFingerprint(
	members memRepo.MemberRepository,
	fingerprints memRepo.FingerprintRepository,
	gmk bcrypto.GMKProvider,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *RegisterFingerprint {
	return &RegisterFingerprint{
		Members: members, Fingerprints: fingerprints, GMK: gmk,
		UoW: uow, Audit: recorder,
	}
}

// WithMatcher wires the 1:N matcher used for the pre-enrollment collision
// check. Returns the same pointer for chaining at DI time.
func (uc *RegisterFingerprint) WithMatcher(m FingerprintMatcher) *RegisterFingerprint {
	uc.Matcher = m
	return uc
}

func (uc *RegisterFingerprint) Execute(ctx context.Context, in RegisterFingerprintInput) (*RegisterFingerprintOutput, error) {
	if !in.ConsentAccepted {
		return nil, sharedDomain.NewBusinessError(fpDomain.ErrConsentRequired, "")
	}
	if len(in.Captures) == 0 {
		return nil, sharedDomain.NewValidationError(fpDomain.ErrEmptyTemplate)
	}
	if len(in.Captures) > fpDomain.MaxFingerprintsPerMember {
		return nil, sharedDomain.NewValidationError(fpDomain.ErrTooManyCaptures)
	}
	for _, capture := range in.Captures {
		if capture == nil || len(capture.Bytes) == 0 {
			return nil, sharedDomain.NewValidationError(fpDomain.ErrEmptyTemplate)
		}
		if capture.QualityScore != 0 && capture.QualityScore < fpDomain.QualityScoreFloor {
			return nil, sharedDomain.NewBusinessError(fpDomain.ErrQualityBelowFloor, "")
		}
	}

	now := time.Now().UTC()

	// Pre-enrollment 1:N collision check — keep it outside the write tx so
	// the SDK call doesn't hold a DB transaction open. Also gives us a clean
	// place to look up the existing member's name for the error payload.
	if err := uc.checkCollision(ctx, in); err != nil {
		return nil, err
	}

	// Encrypt OUTSIDE the tx — failing here shouldn't roll back unrelated work.
	gmk, err := uc.GMK.GetGMK(ctx, in.GymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	defer bcrypto.Zero(gmk)

	encrypted := make([][]byte, len(in.Captures))
	for i, capture := range in.Captures {
		enc, err := bcrypto.EncryptTemplate(gmk, capture.Bytes)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		encrypted[i] = enc
		// Zero the plaintext we received — caller may still hold a reference,
		// but at least our copy is wiped.
		defer bcrypto.Zero(capture.Bytes)
	}

	var out RegisterFingerprintOutput
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		m, err := uc.Members.GetByID(tx, in.MemberID)
		if err != nil {
			return err
		}
		if m.GymID != in.GymID {
			return sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
		}

		existing, err := uc.Fingerprints.ListByMember(tx, in.MemberID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if len(existing) > 0 {
			return sharedDomain.NewBusinessError(fpDomain.ErrFingerprintAlreadySet, "")
		}

		ids := make([]uuid.UUID, 0, len(in.Captures))
		for i, capture := range in.Captures {
			format := capture.Format
			if format == "" {
				format = fpDomain.FormatDP
			}
			var qualityScore *int
			if capture.QualityScore > 0 {
				v := capture.QualityScore
				qualityScore = &v
			}

			fp, err := fpDomain.NewMemberFingerprint(
				uuid.New(), in.GymID, in.MemberID, in.ActorUserID,
				encrypted[i], format, qualityScore, now,
			)
			if err != nil {
				return sharedDomain.NewValidationError(err)
			}
			if _, err := uc.Fingerprints.Create(tx, fp); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			ids = append(ids, fp.ID)

			// Audit. Never put encrypted (let alone plaintext) bytes in
			// changes — audit_log isn't intended for biometric storage and
			// may sync to the dashboard for read.
			_ = uc.Audit.Record(ctx, tx, audit.Entry{
				GymID:       in.GymID,
				EntityType:  "member_fingerprints",
				EntityID:    fp.ID,
				Action:      audit.ActionCreate,
				ActorUserID: &in.ActorUserID,
				Changes: map[string]any{
					"member_id":         fp.MemberID.String(),
					"template_format":   fp.TemplateFormat,
					"quality_score":     fp.QualityScore,
					"consent_accepted":  in.ConsentAccepted,
					"template_size_b64": len(fp.TemplateEncrypted), // size only, never bytes
				},
				IPAddress: audit.IPFromContext(ctx),
				UserAgent: audit.UAFromContext(ctx),
				At:        now,
			})
		}

		out = RegisterFingerprintOutput{
			FingerprintIDs: ids,
			MemberID:       in.MemberID,
			QualityScore:   bestQuality(in.Captures),
			RegisteredAt:   now,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// bestQuality returns the highest self-assessed quality across the captures,
// or nil when none of them carried a score.
func bestQuality(captures []*biometric.CaptureResult) *int {
	best := 0
	for _, c := range captures {
		if c.QualityScore > best {
			best = c.QualityScore
		}
	}
	if best == 0 {
		return nil
	}
	return &best
}

// checkCollision runs the 1:N pre-enrollment match via the Matcher (the
// biometric hub → tinta-bio gallery). Returns nil when there is no match,
// the Matcher is absent, the engine reports ErrNotAvailable, or the match is
// the target member themselves (future re-enroll flow must not self-collide).
// Returns a BusinessError(ErrFingerprintCollision) with `existing_member_id`
// + `existing_member_name` in Data when the probe belongs to ANOTHER member.
//
// The candidate set is the helper's per-gym gallery (today's only
// multi-tenant boundary). When multi-gym membership lands later, the
// candidate set widens — but that is out of scope here: do not add
// abstractions for it now.
func (uc *RegisterFingerprint) checkCollision(ctx context.Context, in RegisterFingerprintInput) error {
	if uc.Matcher == nil {
		log.Printf("[fingerprint] collision check skipped: no Matcher wired (member=%s gym=%s)", in.MemberID, in.GymID)
		return nil
	}

	// All captures are the same finger — the first is a fine representative
	// for the 1:N collision probe. (The session flow passes exactly one:
	// the enrollment FMD combined by the helper.)
	existingID, err := uc.Matcher.IdentifyFMD(ctx, string(in.Captures[0].Bytes))
	if err != nil {
		if errors.Is(err, biometric.ErrNotAvailable) {
			log.Printf("[fingerprint] collision check skipped: engine not available (member=%s gym=%s)", in.MemberID, in.GymID)
			return nil
		}
		return sharedDomain.NewUnexpectedError(err)
	}
	if existingID == uuid.Nil || existingID == in.MemberID {
		return nil
	}

	// Resolve the existing member's name so the operator gets a meaningful
	// modal ("Esta huella ya está registrada para X"). A separate Query tx
	// is fine — we're already outside the write path.
	nameTx, err := uc.UoW.Query(ctx)
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	names, err := uc.Members.GetNamesByIDs(nameTx, []uuid.UUID{existingID})
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	existingName := names[existingID]

	return sharedDomain.NewBusinessErrorWithData(
		fpDomain.ErrFingerprintCollision,
		"",
		map[string]any{
			"existing_member_id":   existingID.String(),
			"existing_member_name": existingName,
		},
	)
}
