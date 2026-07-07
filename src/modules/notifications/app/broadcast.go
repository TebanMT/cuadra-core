package app

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/tz"
)

// BroadcastFilter selects the audience. MVP supports the two filters listed
// in UC-041 (DA-41.1) — anything more sophisticated lands in V1.0+.
type BroadcastFilter string

const (
	BroadcastFilterAllActive    BroadcastFilter = "all_active"
	BroadcastFilterExpiringSoon BroadcastFilter = "expiring_soon" // por vencer
	BroadcastFilterExpired      BroadcastFilter = "expired"       // vencidos (recuperar)
	BroadcastFilterInactive     BroadcastFilter = "inactive"      // sin venir 21+ días
)

// BroadcastInput backs UC-041 (POST /api/v1/broadcasts). Owner authors a
// freeform Spanish message; we render it through the `broadcast_freeform`
// template so the WhatsApp side stays inside an approved skeleton.
type BroadcastInput struct {
	GymID       uuid.UUID
	ActorUserID uuid.UUID
	Filter      BroadcastFilter
	Message     string

	// MemberIDs — selección explícita de socios (UC-041 opción C). Cuando no
	// está vacía, GANA sobre Filter: la audiencia se resuelve por estos IDs
	// (validados contra el gym). El FE la siembra desde un filtro y deja al
	// dueño quitar/agregar. Vacía = se usa Filter (flujo por segmento).
	MemberIDs []uuid.UUID

	// Confirmed is the DA-41.2 step ("Enviar a 87 socios?"). When false the
	// use case returns a preview without enqueuing.
	Confirmed bool
}

type BroadcastOutput struct {
	Preview     bool
	AudienceN   int
	EnqueuedN   int
	BroadcastID uuid.UUID

	// Límites efectivos del plan, para que el FE pinte "te queda 1 de 2 este
	// mes" y el tope de socios sin re-derivarlos. MonthlyLimit==0 = ilimitado.
	IsPlus       bool
	AudienceCap  int
	MonthlyLimit int
	MonthlyUsed  int

	// AudiencePreview — lista (id+nombre) de la audiencia resuelta, sólo en
	// preview (!Confirmed). El FE la usa para SEMBRAR la selección de socios
	// (opción C: el filtro precarga, el dueño ajusta). Sin teléfonos en el wire.
	AudiencePreview []AudienceMemberView
}

// AudienceMemberView es la proyección mínima que viaja al FE para la selección.
type AudienceMemberView struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// MaxAudience is a guardrail against accidental "all members" pushes when
// the filter is broken. ADR-007 §4.6 — Tier 1 numbers cap at 1000
// conversations/24h, so 500 is a safe MVP ceiling (techo en Plus).
const MaxAudience = 500

// Límites del plan Standard para el envío masivo (2C). El envío entra en
// Standard pero acotado, porque el costo de cada mensaje lo ABSORBE Tinta (sale
// por el número maestro). Plus levanta ambos topes (MaxAudience + sin tope
// mensual).
const (
	// 150 = techo del segmento objetivo (gyms de barrio 30–150 socios), para
	// que "mándale a todos mis activos" funcione en todo el segmento. El
	// control de costo real es el tope mensual, no el tamaño del envío.
	standardAudienceCap  = 150 // socios por envío
	standardMonthlyLimit = 2   // envíos por mes
)

// Drip del envío masivo: en vez de encolar todo con scheduled_for=now (que el
// dispatcher dispararía en ráfaga), escalonamos el scheduled_for de cada
// mensaje. Beneficio doble: (1) ráfaga suave hacia Meta (~60/min, lejos del
// límite del número maestro), (2) QoS — lo transaccional (recibos/OTP,
// scheduled=now) se libera ANTES que el broadcast (scheduled en el futuro),
// porque LeasePending ordena por scheduled_for ASC.
const (
	broadcastDripLead   = 5 * time.Second // arranca unos segundos después → transaccional primero
	broadcastDripPerMsg = 1 * time.Second // ~60 mensajes/min
)

type Broadcast struct {
	Notifications notiRepo.NotificationRepository
	Members       memRepo.MemberRepository
	Gyms          gymRepo.GymRepository
	UoW           sharedDomain.UnitOfWork
	Audit         audit.Recorder
	// AuditReader cuenta los envíos del mes (cada send deja un audit
	// EntityType "broadcasts"). nil = sin tope mensual (tests).
	AuditReader audit.Reader
}

func NewBroadcast(
	notifications notiRepo.NotificationRepository,
	members memRepo.MemberRepository,
	gyms gymRepo.GymRepository,
	auditReader audit.Reader,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *Broadcast {
	return &Broadcast{
		Notifications: notifications,
		Members:       members,
		Gyms:          gyms,
		AuditReader:   auditReader,
		UoW:           uow,
		Audit:         recorder,
	}
}

func (uc *Broadcast) Execute(ctx context.Context, in BroadcastInput) (*BroadcastOutput, error) {
	msg := strings.TrimSpace(in.Message)
	// El mensaje sólo es obligatorio al CONFIRMAR; el preview (conteo de
	// audiencia + límites del plan) corre antes de que el dueño escriba.
	if in.Confirmed && msg == "" {
		return nil, sharedDomain.NewValidationError(notiErrors.ErrBroadcastMessage)
	}
	now := time.Now().UTC()

	out := BroadcastOutput{Preview: !in.Confirmed, BroadcastID: uuid.New()}
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		gym, err := uc.Gyms.GetByID(tx, in.GymID)
		if err != nil {
			return err
		}
		// ADR-009: el sender se resuelve en el dispatcher (número maestro de
		// Tinta para Standard/Plus-sin-conectar, número propio para Plus
		// conectado). Sin gate de conexión aquí.
		//
		// Límites por plan (2C): en Standard el costo de cada mensaje lo absorbe
		// Tinta (número maestro), así que acotamos. Plus levanta ambos topes.
		isPlus := gymDomain.CanAccessPlusFeatures(gym.SubscriptionPlan)
		out.IsPlus = isPlus
		out.AudienceCap = standardAudienceCap
		out.MonthlyLimit = standardMonthlyLimit
		if isPlus {
			out.AudienceCap = MaxAudience
			out.MonthlyLimit = 0 // ilimitado
		}

		// Envíos ya hechos este mes (sólo importa cuando hay tope = Standard).
		// La frontera de mes es la del CALENDARIO DEL GYM: con la frontera
		// UTC, el tope se reseteaba a las 6 PM locales del último día del
		// mes (CDMX) y un gym Standard podía colar un tercer envío.
		if uc.AuditReader != nil && out.MonthlyLimit > 0 {
			localToday := tz.LocalToday(gym.Timezone, now)
			monthStart := time.Date(localToday.Year(), localToday.Month(), 1, 0, 0, 0, 0, time.UTC)
			if _, used, lerr := uc.AuditReader.List(tx, audit.ListQuery{
				GymID:      in.GymID,
				EntityType: "broadcasts",
				From:       &monthStart,
				To:         &now,
				Page:       1,
				PageSize:   1,
			}); lerr == nil {
				out.MonthlyUsed = used
			}
		}

		audience, err := uc.resolveAudience(tx, in.GymID, in.Filter, in.MemberIDs, now)
		if err != nil {
			return err
		}
		out.AudienceN = len(audience)

		// Preview: devuelve conteo + LISTA (id+nombre) + límites SIN abortar,
		// para que el FE pinte el aviso ("150 socios — el tope es 100"), siembre
		// la selección de socios y deshabilite Enviar según corresponda.
		if !in.Confirmed {
			out.AudiencePreview = make([]AudienceMemberView, 0, len(audience))
			for _, m := range audience {
				out.AudiencePreview = append(out.AudiencePreview, AudienceMemberView{ID: m.ID, Name: m.FullName})
			}
			return nil
		}

		// Confirmado: aquí sí se enforce.
		if len(audience) == 0 {
			return sharedDomain.NewBusinessError(notiErrors.ErrBroadcastEmpty, "")
		}
		if len(audience) > out.AudienceCap {
			return sharedDomain.NewBusinessError(notiErrors.ErrBroadcastTooBig, "")
		}
		if out.MonthlyLimit > 0 && out.MonthlyUsed >= out.MonthlyLimit {
			return sharedDomain.NewBusinessError(notiErrors.ErrBroadcastMonthlyLimit, "")
		}

		gymName := ""
		if gym.Name != nil {
			gymName = *gym.Name
		}
		for i, m := range audience {
			vars := map[string]string{
				"member_first_name": firstName(m.FullName),
				"gym_name":          gymName,
				"message":           msg,
			}
			// Drip: escalona el scheduled_for para no rafaguear (ver consts).
			scheduledFor := now.Add(broadcastDripLead + time.Duration(i)*broadcastDripPerMsg)
			n, err := notiDomain.New(
				uuid.New(), in.GymID, m.ID,
				notiDomain.ChannelWhatsApp,
				"broadcast_freeform",
				notiDomain.RecipientMember,
				m.Phone,
				vars,
				scheduledFor, now,
				nil, // broadcasts intentionally non-idempotent — owner re-sends are real intent
			)
			if err != nil {
				continue
			}
			if _, err := uc.Notifications.Create(tx, n); err != nil {
				return sharedDomain.NewUnexpectedError(err)
			}
			out.EnqueuedN++
		}

		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       in.GymID,
			EntityType:  "broadcasts",
			EntityID:    out.BroadcastID,
			Action:      "broadcast_send",
			ActorUserID: &in.ActorUserID,
			Changes: map[string]any{
				"filter":     string(in.Filter),
				"audience_n": out.AudienceN,
				"enqueued_n": out.EnqueuedN,
				"message":    msg,
			},
			IPAddress: audit.IPFromContext(ctx),
			UserAgent: audit.UAFromContext(ctx),
			At:        now,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// audienceMember is a tiny projection — only the fields the broadcast
// renders need.
type audienceMember struct {
	ID       uuid.UUID
	FullName string
	Phone    string
}

// resolveAudience determina la audiencia. Si hay MemberIDs (selección manual,
// opción C) gana sobre el filtro: los resuelve por IDs validados contra el gym.
// Si no, traduce el filtro al StatusFilter del repo de socios. En ambos casos
// proyecta a audienceMember y descarta los que no tienen teléfono.
func (uc *Broadcast) resolveAudience(tx sharedDomain.Transaction, gymID uuid.UUID, filter BroadcastFilter, memberIDs []uuid.UUID, now time.Time) ([]audienceMember, error) {
	// Selección manual: GetContactsByIDs ya filtra por gym_id (anti
	// cross-tenant) y omite borrados; aquí sólo descartamos sin teléfono.
	if len(memberIDs) > 0 {
		contacts, err := uc.Members.GetContactsByIDs(tx, gymID, memberIDs)
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		out := make([]audienceMember, 0, len(contacts))
		for _, c := range contacts {
			if strings.TrimSpace(c.Phone) == "" {
				continue
			}
			out = append(out, audienceMember{ID: c.ID, FullName: c.FullName, Phone: c.Phone})
		}
		return out, nil
	}

	var status string
	switch filter {
	case "", BroadcastFilterAllActive:
		status = "active"
	case BroadcastFilterExpiringSoon:
		status = "expiring_soon"
	case BroadcastFilterExpired:
		status = "expired"
	case BroadcastFilterInactive:
		status = "inactive"
	default:
		return nil, sharedDomain.NewValidationError(notiErrors.ErrInvalidVariables)
	}

	// OJO: el repo de socios recorta page_size>200 a 25 (clamp de la lista
	// paginada). Por eso NO pedimos MaxAudience+1 de un jalón — paginamos en
	// bloques de 200 hasta agotar el segmento o pasar el techo (MaxAudience).
	// El cap del plan (100/500) se valida después sobre len(audience).
	const pageSize = 200
	out := make([]audienceMember, 0)
	for page := 1; ; page++ {
		rows, _, err := uc.Members.List(tx, memRepo.ListQuery{
			GymID:        gymID,
			StatusFilter: status,
			Page:         page,
			PageSize:     pageSize,
			Today:        now,
		})
		if err != nil {
			return nil, sharedDomain.NewUnexpectedError(err)
		}
		for _, r := range rows {
			if r == nil || r.Member == nil {
				continue
			}
			if strings.TrimSpace(r.Member.Phone) == "" {
				continue
			}
			out = append(out, audienceMember{ID: r.Member.ID, FullName: r.Member.FullName, Phone: r.Member.Phone})
		}
		// Última página, o ya pasamos el techo (no tiene caso traer más: el
		// cap lo va a rechazar de todas formas).
		if len(rows) < pageSize || len(out) > MaxAudience {
			break
		}
	}
	return out, nil
}
