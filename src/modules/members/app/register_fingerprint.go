package app

import (
	"context"
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

// RegisterFingerprintInput backs UC-028. The capture step (talking to the
// reader, the 3-sample loop) lives in the controller / kiosk loop — by the
// time we reach the use case we already have the SDK output.
type RegisterFingerprintInput struct {
	GymID           uuid.UUID
	ActorUserID     uuid.UUID
	MemberID        uuid.UUID
	Capture         *biometric.CaptureResult
	ConsentAccepted bool // UC-028 step 1 + ADR-006 §5
}

// RegisterFingerprintOutput is what the controller turns into the JSON
// response (UC-028 step 9 — "✓ Huella registrada").
type RegisterFingerprintOutput struct {
	FingerprintID uuid.UUID
	MemberID      uuid.UUID
	QualityScore  *int
	RegisteredAt  time.Time
}

// RegisterFingerprint implements UC-028. The flow:
//
//  1. Validate consent (ADR-006 §5) and capture quality.
//  2. Confirm the member exists in this gym.
//  3. Reject if there's already an active fingerprint (DA-28.2).
//  4. Encrypt the template with the gym GMK (ADR-006 §2.4).
//  5. Insert + audit + sync queue, all inside one Command tx.
//
// The plaintext capture is zeroed before returning — defense in depth against
// memory snapshots even though Go's GC may keep copies.
type RegisterFingerprint struct {
	Members      memRepo.MemberRepository
	Fingerprints memRepo.FingerprintRepository
	GMK          bcrypto.GMKProvider
	UoW          sharedDomain.UnitOfWork
	Audit        audit.Recorder
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

func (uc *RegisterFingerprint) Execute(ctx context.Context, in RegisterFingerprintInput) (*RegisterFingerprintOutput, error) {
	if !in.ConsentAccepted {
		return nil, sharedDomain.NewBusinessError(fpDomain.ErrConsentRequired, "")
	}
	if in.Capture == nil || len(in.Capture.Bytes) == 0 {
		return nil, sharedDomain.NewValidationError(fpDomain.ErrEmptyTemplate)
	}
	if in.Capture.QualityScore != 0 && in.Capture.QualityScore < fpDomain.QualityScoreFloor {
		return nil, sharedDomain.NewBusinessError(fpDomain.ErrQualityBelowFloor, "")
	}

	now := time.Now().UTC()

	// Encrypt OUTSIDE the tx — failing here shouldn't roll back unrelated work.
	gmk, err := uc.GMK.GetGMK(ctx, in.GymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	defer bcrypto.Zero(gmk)

	encrypted, err := bcrypto.EncryptTemplate(gmk, in.Capture.Bytes)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	// Zero the plaintext we received — caller may still hold a reference, but
	// at least our copy is wiped.
	defer bcrypto.Zero(in.Capture.Bytes)

	var qualityScore *int
	if in.Capture.QualityScore > 0 {
		v := in.Capture.QualityScore
		qualityScore = &v
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

		existing, err := uc.Fingerprints.GetByMember(tx, in.MemberID)
		if err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		if existing != nil {
			return sharedDomain.NewBusinessError(fpDomain.ErrFingerprintAlreadySet, "")
		}

		format := in.Capture.Format
		if format == "" {
			format = fpDomain.FormatDP
		}

		fp, err := fpDomain.NewMemberFingerprint(
			uuid.New(), in.GymID, in.MemberID, in.ActorUserID,
			encrypted, format, qualityScore, now,
		)
		if err != nil {
			return sharedDomain.NewValidationError(err)
		}
		if _, err := uc.Fingerprints.Create(tx, fp); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}

		// Audit. Never put encrypted (let alone plaintext) bytes in changes —
		// audit_log isn't intended for biometric storage and may sync to the
		// dashboard for read.
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

		out = RegisterFingerprintOutput{
			FingerprintID: fp.ID,
			MemberID:      fp.MemberID,
			QualityScore:  fp.QualityScore,
			RegisteredAt:  fp.CreatedAt,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
