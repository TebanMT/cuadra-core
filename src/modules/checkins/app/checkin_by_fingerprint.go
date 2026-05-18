//go:build sidecar

package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	chkRepo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// FingerprintMatchThresholdDefault is what UC-029 uses when the gym hasn't
// overridden it via gyms.kiosk_settings (UC-029 DA-29.4). It is a raw
// bozorth3 score, NOT a 0-1 ratio — Identify compares the matcher's integer
// score directly against it. bozorth3 scores the same finger ~55 and
// unrelated fingers 3-7 (reader validation 2026-05-16); 40 is the operational
// floor that admits same-finger variation while rejecting strangers. Lower =
// more permissive, higher = stricter.
const FingerprintMatchThresholdDefault = 40

// CheckinByFingerprintInput. The Capture is a *plaintext* template that the
// caller (kiosko loop or manual flow) just got from biometric.Reader.Capture.
// Threshold 0 falls back to the default.
type CheckinByFingerprintInput struct {
	GymID     uuid.UUID
	Capture   *biometric.CaptureResult
	Threshold float64
	Now       time.Time
}

// CheckinByFingerprint runs the UC-029 flow:
//
//  1. Load active fingerprint blobs for the gym (via members service).
//  2. Ask the Reader to identify the capture against the candidate set.
//  3. On hit, evaluate access status + record checkin (no operator).
//  4. On miss, return ErrNoFingerprintMatch — caller surfaces "no
//     identificado, pasa con recepción".
//
// Both reader operations and the DB tx happen serially: we read before opening
// the tx to keep the tx short (no SDK call inside Postgres/SQLite tx).
type CheckinByFingerprint struct {
	Members *memApp.MemberService
	Repo    chkRepo.CheckinRepository
	Reader  biometric.Reader
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
}

func NewCheckinByFingerprint(
	members *memApp.MemberService,
	repo chkRepo.CheckinRepository,
	reader biometric.Reader,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *CheckinByFingerprint {
	return &CheckinByFingerprint{
		Members: members, Repo: repo, Reader: reader,
		UoW: uow, Audit: recorder,
	}
}

func (uc *CheckinByFingerprint) Execute(ctx context.Context, in CheckinByFingerprintInput) (*CheckinView, error) {
	if uc.Reader == nil {
		return nil, sharedDomain.NewBusinessError(chkErrors.ErrReaderNotAvailable, "")
	}
	if in.Capture == nil || len(in.Capture.Bytes) == 0 {
		return nil, sharedDomain.NewValidationError(chkErrors.ErrNoFingerprintMatch)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	threshold := in.Threshold
	if threshold <= 0 {
		threshold = FingerprintMatchThresholdDefault
	}

	// Phase 1 — read the candidate set via a query tx (no writes).
	queryTx, err := uc.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	loaded, err := uc.Members.LoadFingerprintsForGym(ctx, queryTx, memApp.LoadFingerprintsForGymInput{GymID: in.GymID})
	if err != nil {
		return nil, err
	}
	if len(loaded.Templates) == 0 {
		return nil, sharedDomain.NewBusinessError(chkErrors.ErrFingerprintNotEnrolled, "")
	}
	enrolled := make([]biometric.EncryptedTemplate, 0, len(loaded.Templates))
	for _, t := range loaded.Templates {
		enrolled = append(enrolled, biometric.EncryptedTemplate{
			MemberID: t.MemberID.String(),
			Bytes:    t.Encrypted,
			Format:   t.Format,
		})
	}

	// Phase 2 — identify. The Reader handles GMK lookup + decrypt internally.
	match, err := uc.Reader.Identify(ctx, in.Capture, enrolled, threshold)
	if err != nil {
		if errors.Is(err, biometric.ErrNoMatch) {
			return nil, sharedDomain.NewBusinessError(chkErrors.ErrNoFingerprintMatch, "")
		}
		if errors.Is(err, biometric.ErrNotAvailable) {
			return nil, sharedDomain.NewBusinessError(chkErrors.ErrReaderNotAvailable, "")
		}
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	memberID, perr := uuid.Parse(match.MemberID)
	if perr != nil {
		return nil, sharedDomain.NewUnexpectedError(perr)
	}

	// Phase 3 — record the checkin in a Command tx.
	today := truncateToDay(now)
	var view *CheckinView
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		v, err := recordCheckin(ctx, tx, uc.Members, uc.Repo, uc.Audit,
			in.GymID, memberID, "fingerprint", nil, now, today)
		if err != nil {
			return err
		}
		view = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return view, nil
}
