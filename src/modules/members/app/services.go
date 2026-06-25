package app

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/modules/members/domain/access"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	memberDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/member"
	membershipDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership"
	mtDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/membership_type"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// MemberService is the cross-BC seam for `members`. Other BCs (billing in
// Sesión 3, checkins in Sesión 5, promotions en Sesión 9) call methods
// here within their own UnitOfWork transactions.
type MemberService struct {
	Members         memRepo.MemberRepository
	Memberships     memRepo.MembershipRepository
	MembershipTypes memRepo.MembershipTypeRepository
	// Fingerprints is optional — older callers (billing en Sesión 3) don't
	// need it. Sesión 5 wires it in for checkins/UC-029. Use the setter
	// WithFingerprints to attach without breaking existing constructors.
	Fingerprints memRepo.FingerprintRepository
	// Adjustments es opcional — habilita ApplyMembershipAdjustment para
	// que el flujo de promo `extra_days` registre un membership_adjustments
	// con el motivo dentro de la misma tx del cobro. Sin el setter,
	// ApplyMembershipAdjustment devuelve un error claro.
	Adjustments memRepo.MembershipAdjustmentRepository
}

func NewMemberService(members memRepo.MemberRepository, memberships memRepo.MembershipRepository,
	mtypes memRepo.MembershipTypeRepository) *MemberService {
	return &MemberService{Members: members, Memberships: memberships, MembershipTypes: mtypes}
}

// WithFingerprints attaches the fingerprint repository so checkins can call
// LoadFingerprintsForGym. Returns the same pointer so callers can chain.
func (s *MemberService) WithFingerprints(fp memRepo.FingerprintRepository) *MemberService {
	s.Fingerprints = fp
	return s
}

// WithAdjustments attaches el adjustment repo (necesario para el flujo
// de promo `extra_days`).
func (s *MemberService) WithAdjustments(ar memRepo.MembershipAdjustmentRepository) *MemberService {
	s.Adjustments = ar
	return s
}

// RenewMembershipForPaymentInput is the cross-BC input from billing/UC-018.
type RenewMembershipForPaymentInput struct {
	MemberID         uuid.UUID
	MembershipTypeID uuid.UUID
	PaymentDate      time.Time
}

type RenewMembershipForPaymentOutput struct {
	OldMembership *membershipDomain.Membership
	NewMembership *membershipDomain.Membership
	NextType      *mtDomain.MembershipType
}

// RenewMembershipForPayment runs the renewal logic of UC-018 (the half that
// lives in `members`): mark the old Membership replaced, create a new one
// with snapshot fields from the (possibly different) MembershipType, return
// both. Caller (billing) must be inside its own UnitOfWork.Command tx.
//
// NOTE: billing is wired in Sesión 3. This method is defined here so that
// the future implementation of UC-018 can stitch into a stable seam.
func (s *MemberService) RenewMembershipForPayment(ctx context.Context, tx sharedDomain.Transaction, in RenewMembershipForPaymentInput, now time.Time) (*RenewMembershipForPaymentOutput, error) {
	mt, err := s.MembershipTypes.GetByID(tx, in.MembershipTypeID)
	if err != nil {
		return nil, err
	}
	if !mt.Active {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMembershipTypeInactive, "")
	}
	current, err := s.Memberships.GetCurrentByMember(tx, in.MemberID)
	if err != nil {
		if !errors.Is(err, memErrors.ErrNoActiveMembership) {
			return nil, err
		}
		// Caso 0 — socio huérfano: no tiene membresía vigente (active/pending).
		// Pasa cuando se importó sin sociomembresia, cuando su membresía quedó
		// en 'expired'/'cancelled', o cuando nunca se le asignó una. En vez de
		// rebotar el cobro con ErrNoActiveMembership, lo RE-INSCRIBIMOS: creamos
		// una membresía nueva con el plan elegido y la activamos con este pago
		// (mismo efecto que el Caso A, pero partiendo de cero). Las membresías
		// viejas quedan como historial; el índice uq_memberships_member_active
		// no se viola porque ninguna está activa.
		member, mErr := s.Members.GetByID(tx, in.MemberID)
		if mErr != nil {
			return nil, mErr
		}
		if member.GymID != mt.GymID {
			return nil, sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
		}
		fresh := membershipDomain.NewPendingPayment(uuid.New(), mt.GymID, in.MemberID, mt, in.PaymentDate, now)
		if actErr := fresh.Activate(in.PaymentDate, now); actErr != nil {
			return nil, sharedDomain.NewBusinessError(actErr, "")
		}
		if _, cErr := s.Memberships.Create(tx, fresh); cErr != nil {
			return nil, sharedDomain.NewUnexpectedError(cErr)
		}
		return &RenewMembershipForPaymentOutput{
			OldMembership: nil,
			NewMembership: fresh,
			NextType:      mt,
		}, nil
	}
	if current.GymID != mt.GymID {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
	}

	// Caso A — Activación de pending_payment. El socio fue inscrito sin
	// primer pago; este abono (parcial o total) activa la membresía en
	// el mismo row, sin renovar ni crear una fila nueva. La idea es: el
	// plan elegido al inscribir es el que paga; si el operador cambió
	// de plan, respetamos el cambio actualizando los snapshots antes
	// de activar.
	if current.Status == membershipDomain.StatusPendingPayment {
		if current.MembershipTypeID != mt.ID {
			// Operador cambió de plan al cobrar — refrescamos snapshots.
			current.MembershipTypeID = mt.ID
			current.TypeNameSnapshot = mt.Name
			current.PriceSnapshot = mt.Price
			current.DurationDaysSnapshot = mt.DurationDays
		}
		if err := current.Activate(in.PaymentDate, now); err != nil {
			return nil, sharedDomain.NewBusinessError(err, "")
		}
		if _, err := s.Memberships.Update(tx, current); err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		return &RenewMembershipForPaymentOutput{
			OldMembership: nil, // no hubo "vieja" — la misma fila se activó.
			NewMembership: current,
			NextType:      mt,
		}, nil
	}

	// Caso B — Renovación clásica: socio con membresía activa paga el
	// siguiente ciclo. Creamos una nueva fila y marcamos la actual
	// como replaced (3-step dance descrito abajo).
	newID := uuid.New()
	next := current.Renew(newID, mt, in.PaymentDate, now)
	// Three-step dance to satisfy *both* constraints simultaneously:
	//   * uq_memberships_member_active forbids two `active` rows per member
	//     (so we can't INSERT new before vacating `current`).
	//   * memberships.replaced_by FK forbids pointing at a non-existent row
	//     (so we can't UPDATE `current.replaced_by = newID` before INSERT).
	// 1) Flip current to `replaced` without setting replaced_by → frees the
	//    partial unique index.
	// 2) INSERT new row → FK satisfied because predecessor exists.
	// 3) UPDATE current.replaced_by = newID → FK target now exists.
	current.Status = membershipDomain.StatusReplaced
	current.Version++
	current.UpdatedAt = now
	if _, err := s.Memberships.Update(tx, current); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if _, err := s.Memberships.Create(tx, next); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	current.ReplacedBy = &newID
	current.Version++
	current.UpdatedAt = now
	if _, err := s.Memberships.Update(tx, current); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return &RenewMembershipForPaymentOutput{
		OldMembership: current,
		NewMembership: next,
		NextType:      mt,
	}, nil
}

// RevertMembershipFromPaymentInput is the cross-BC input from billing/UC-022.
// Caller passes the member whose latest renewal must be undone. The members
// BC cancels the active Membership and re-activates the one it replaced (the
// previous expiry is preserved on the predecessor row, so no re-computation
// is needed).
type RevertMembershipFromPaymentInput struct {
	MemberID uuid.UUID
}

type RevertMembershipFromPaymentOutput struct {
	CancelledMembership *membershipDomain.Membership
	RestoredMembership  *membershipDomain.Membership
}

// RevertMembershipFromPayment is invoked by billing/UC-022 when the operator
// requested `revert_membership=true` on a refund. We undo the most recent
// renewal: cancel the current active membership, set its predecessor (the
// `replaced` row pointing at it) back to `active`. Returns business errors
// when there is no active membership or no predecessor to restore.
func (s *MemberService) RevertMembershipFromPayment(ctx context.Context, tx sharedDomain.Transaction, in RevertMembershipFromPaymentInput, now time.Time) (*RevertMembershipFromPaymentOutput, error) {
	current, err := s.Memberships.GetCurrentByMember(tx, in.MemberID)
	if err != nil {
		return nil, err
	}
	predecessor, err := s.Memberships.GetReplacedBy(tx, current.ID)
	if err != nil {
		return nil, err
	}
	current.Cancel(now)
	if _, err := s.Memberships.Update(tx, current); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if predecessor != nil {
		predecessor.Status = membershipDomain.StatusActive
		predecessor.ReplacedBy = nil
		predecessor.Version++
		predecessor.UpdatedAt = now
		if _, err := s.Memberships.Update(tx, predecessor); err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
	}
	return &RevertMembershipFromPaymentOutput{
		CancelledMembership: current,
		RestoredMembership:  predecessor,
	}, nil
}

// GetAccessStatusInput is consumed by checkins/Sesión 5.
type GetAccessStatusInput struct {
	GymID    uuid.UUID
	MemberID uuid.UUID
	Today    time.Time // caller passes "today in gym timezone" — domain stays UTC-pure
}

type GetAccessStatusOutput struct {
	Member            *memberDomain.Member
	CurrentMembership *membershipDomain.Membership
	Status            access.AccessStatus
}

// GetAccessStatus is a read helper. Uses the existing transaction (so checkins
// can hold a single tx for the whole "scan -> evaluate -> insert checkin" flow).
func (s *MemberService) GetAccessStatus(ctx context.Context, tx sharedDomain.Transaction, in GetAccessStatusInput) (*GetAccessStatusOutput, error) {
	mw, err := s.Members.GetWithCurrentMembership(tx, in.GymID, in.MemberID)
	if err != nil {
		return nil, err
	}
	if mw.Member == nil {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrMemberNotFound, "")
	}
	today := in.Today
	if today.IsZero() {
		now := time.Now().UTC()
		today = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	}
	st := access.New().Evaluate(mw.Member, mw.CurrentMembership, today)
	return &GetAccessStatusOutput{
		Member:            mw.Member,
		CurrentMembership: mw.CurrentMembership,
		Status:            st,
	}, nil
}

// LoadFingerprintsForGymInput is consumed by the kiosko at boot (UC-029
// step 4 needs the full candidate set in memory before Identify can run).
type LoadFingerprintsForGymInput struct {
	GymID uuid.UUID
}

// LoadFingerprintsForGymOutput carries the per-member encrypted blobs ready
// to feed biometric.Reader.Identify.
type LoadFingerprintsForGymOutput struct {
	Templates []EncryptedFingerprint
}

// EncryptedFingerprint is the cross-BC view checkins consumes. It mirrors
// biometric.EncryptedTemplate but lives in the members BC so checkins doesn't
// have to depend on the biometric package directly.
type EncryptedFingerprint struct {
	MemberID  uuid.UUID
	Encrypted []byte
	Format    string
}

// LoadFingerprintsForGym returns every active fingerprint blob in the gym.
// Caller (sidecar kiosko goroutine) decrypts on its side via the GMK kept in
// keychain.
func (s *MemberService) LoadFingerprintsForGym(ctx context.Context, tx sharedDomain.Transaction, in LoadFingerprintsForGymInput) (*LoadFingerprintsForGymOutput, error) {
	if s.Fingerprints == nil {
		return &LoadFingerprintsForGymOutput{}, nil
	}
	rows, err := s.Fingerprints.ListByGym(tx, in.GymID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	out := &LoadFingerprintsForGymOutput{Templates: make([]EncryptedFingerprint, 0, len(rows))}
	for _, fp := range rows {
		out.Templates = append(out.Templates, EncryptedFingerprint{
			MemberID:  fp.MemberID,
			Encrypted: fp.TemplateEncrypted,
			Format:    fp.TemplateFormat,
		})
	}
	return out, nil
}

// ApplyMembershipAdjustmentInput es lo que el flujo de promo extra_days
// pasa para extender el expiry de una membresía dentro de la misma tx
// del cobro. Reason debe ser >= 5 chars (validación del dominio del
// adjustment).
type ApplyMembershipAdjustmentInput struct {
	GymID        uuid.UUID
	MembershipID uuid.UUID
	Days         int
	Reason       string
	ActorUserID  uuid.UUID
}

// ApplyMembershipAdjustment extiende el expiry de una membresía + crea
// el row de adjustment, dentro de la tx del caller. Pensado para que
// billing/promotions ejecuten el efecto extra_days post-renew sin
// abrir otra UoW.Command (rollback completo si algo falla aguas
// abajo).
func (s *MemberService) ApplyMembershipAdjustment(ctx context.Context, tx sharedDomain.Transaction, in ApplyMembershipAdjustmentInput, now time.Time) error {
	if s.Adjustments == nil {
		return sharedDomain.NewUnexpectedError(memErrors.ErrAdjustmentInvalidDays)
	}
	ms, err := s.Memberships.GetByID(tx, in.MembershipID)
	if err != nil {
		return err
	}
	if ms.GymID != in.GymID {
		return sharedDomain.NewBusinessError(memErrors.ErrMembershipNotFound, "")
	}
	prev, next, err := ms.AdjustExpiry(in.Days, now)
	if err != nil {
		return sharedDomain.NewValidationError(err)
	}
	adj, err := membershipDomain.NewAdjustment(uuid.New(), in.GymID, ms.ID, in.ActorUserID,
		in.Reason, in.Days, prev, next, now)
	if err != nil {
		return sharedDomain.NewValidationError(err)
	}
	if _, err := s.Memberships.Update(tx, ms); err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	if _, err := s.Adjustments.Create(tx, adj); err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	return nil
}

// GiftMembershipInput es lo que el flujo de promo companion_memberships
// pasa por cada socio destinatario. CompanionMemberID es del socio que
// recibe la membresía gratis; MembershipTypeID es el plan referencia
// (mismo del cobro principal). PriceSnapshot se fuerza a 0.
type GiftMembershipInput struct {
	GymID             uuid.UUID
	CompanionMemberID uuid.UUID
	MembershipTypeID  uuid.UUID
	StartDate         time.Time
}

// GiftMembership materializa una membership de regalo ($0) para un
// socio destinatario de promo companion_memberships. Maneja los 3
// estados del companion sin romper el partial unique index
// `uq_memberships_member_active`:
//
//  1. Sin membresía vigente   → crea nueva $0 active.
//  2. Pending payment         → activa la existing (es regalo, sin
//     payment) y deja PriceSnapshot=0.
//  3. Active (vencida o no)   → renueva $0 con el 3-step dance:
//     marca actual replaced → INSERT nueva
//     $0 → UPDATE replaced_by. El expiry
//     nuevo sigue la regla normal de
//     Renew (extiende sobre el existente
//     si aún vigente; sino arranca hoy).
//
// El caso 3 es el realista en producción: casi cualquier socio activo
// que el dueño elige para regalo YA tiene una mensualidad. Antes
// rechazábamos con un error (mensaje raro de "teléfono duplicado") —
// ahora extendemos como cualquier otra renovación, que es la semántica
// natural del 2x1 ("le regalo un mes encima").
func (s *MemberService) GiftMembership(ctx context.Context, tx sharedDomain.Transaction, in GiftMembershipInput, now time.Time) (*membershipDomain.Membership, error) {
	// Sanity: socio existe + pertenece al gym.
	comp, err := s.Members.GetByID(tx, in.CompanionMemberID)
	if err != nil {
		return nil, err
	}
	if comp.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
	}
	mt, err := s.MembershipTypes.GetByID(tx, in.MembershipTypeID)
	if err != nil {
		return nil, err
	}
	if mt.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
	}

	current, currErr := s.Memberships.GetCurrentByMember(tx, in.CompanionMemberID)

	// Caso 1: sin membresía vigente — crear nueva $0 active.
	if currErr != nil {
		// GetCurrentByMember devuelve BusinessError(ErrNoActiveMembership)
		// cuando no hay current. Cualquier otro error es infra → propagar.
		if !errors.Is(currErr, memErrors.ErrNoActiveMembership) {
			return nil, currErr
		}
		newMs := membershipDomain.New(uuid.New(), in.GymID, in.CompanionMemberID, mt, in.StartDate, now)
		newMs.PriceSnapshot = 0
		if _, err := s.Memberships.Create(tx, newMs); err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		return newMs, nil
	}

	// Caso 2: pending_payment — activar la existing y dejar snapshot $0.
	// Es regalo: NO se crea Payment. El operador la "paga" con la promo.
	if current.Status == membershipDomain.StatusPendingPayment {
		// Snapshot a $0 + sincronizar plan si el del regalo es distinto.
		current.PriceSnapshot = 0
		if current.MembershipTypeID != mt.ID {
			current.MembershipTypeID = mt.ID
			current.TypeNameSnapshot = mt.Name
			current.DurationDaysSnapshot = mt.DurationDays
		}
		if err := current.Activate(in.StartDate, now); err != nil {
			return nil, sharedDomain.NewBusinessError(err, "")
		}
		if _, err := s.Memberships.Update(tx, current); err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		return current, nil
	}

	// Caso 3: active — renovar con snapshot $0 vía 3-step dance (mismo
	// patrón que RenewMembershipForPayment para no romper el partial
	// unique index uq_memberships_member_active).
	newID := uuid.New()
	next := current.Renew(newID, mt, in.StartDate, now)
	next.PriceSnapshot = 0
	current.Status = membershipDomain.StatusReplaced
	current.Version++
	current.UpdatedAt = now
	if _, err := s.Memberships.Update(tx, current); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if _, err := s.Memberships.Create(tx, next); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	current.ReplacedBy = &newID
	current.Version++
	current.UpdatedAt = now
	if _, err := s.Memberships.Update(tx, current); err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	return next, nil
}

// Compile-time assertion: domain types are reachable from this package without
// being used elsewhere — a deliberate seam check.
var _ = memberDomain.StatusActive
