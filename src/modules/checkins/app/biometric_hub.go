//go:build sidecar

package app

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	chkErrors "github.com/cuadra/cuadra-core/src/modules/checkins/domain/errors"
	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	memErrors "github.com/cuadra/cuadra-core/src/modules/members/domain/errors"
	fpDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/accesswebhook"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	bcrypto "github.com/cuadra/cuadra-core/src/shared/biometric/crypto"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// EnrollSamplesRequired is how many dedazos one enroll session accumulates
// before asking the helper to combine them into the enrollment FMD.
//
// CUATRO, no tres: FingerJet (dpfj_create_enrollment_fmd, vía
// Enrollment.CreateEnrollmentFmd) necesita 4 FMDs de pre-enroll para armar
// el template — el sample oficial del SDK (UareUSampleCSharp/Enrollment.cs)
// dispara el combine hasta `count >= 4`. Con 3 el SDK rechaza el set y CADA
// enroll moría en enrollment_invalid tras el 3er dedazo (mordió en la
// validación real del gym, 26-jul-2026). El 3 histórico venía del flujo
// NBIS, que combinaba distinto.
const EnrollSamplesRequired = 4

// EnrollSessionTTL bounds an enroll session: si el socio no completa los
// dedazos a tiempo, la sesión expira sola y el kiosko vuelve a identificar.
const EnrollSessionTTL = 60 * time.Second

// BioEngine is the command surface the hub needs from the tinta-bio process
// manager. *biometric.Engine implements it; tests wire biometric.MockEngine
// or a scripted fake.
type BioEngine interface {
	SetGallery(ctx context.Context, epoch string, candidates []biometric.GalleryCandidate) error
	Identify(ctx context.Context, probeFMD string, farDivisor, max int) (matches []string, galleryEpoch string, err error)
	EnrollCombine(ctx context.Context, fmds []string) (string, error)
	Alive() bool
	Connected() bool
	Info() biometric.ReaderInfo
}

// BiometricHub is the sidecar's biometric state machine — the successor of
// the old KioskLoop now that the flow is inverted (los dedazos LLEGAN del
// helper; el FE sólo escucha). It owns:
//
//   - the 1:N gallery: decrypt-all-with-GMK → helper `gallery` command,
//     stamped with an epoch; re-sent on boot, enroll, baja de huella, cambio
//     de gym activo y cada restart del helper. Valida el epoch que devuelve
//     cada identify y re-manda + reintenta en la carrera enroll-vs-identify.
//   - sample routing: default = check-in (identify → resolver socio → UC-029
//     → evento); con sesión de enroll activa los samples se acumulan y NO se
//     identifican.
//   - enroll sessions (una a la vez — hay un solo lector): timeout, cancel,
//     colisión (vía RegisterFingerprint.Matcher, que apunta de vuelta aquí),
//     persistencia cifrada + sync, y galería nueva al helper.
//
// It implements biometric.Handler (events del engine, serializados) and
// memApp.FingerprintMatcher (collision check del use case).
type BiometricHub struct {
	Engine       BioEngine
	Checkin      *CheckinByFingerprint
	Register     *memApp.RegisterFingerprint
	Members      memRepo.MemberRepository
	Fingerprints memRepo.FingerprintRepository
	GMK          bcrypto.GMKProvider
	UoW          sharedDomain.UnitOfWork
	Events       *BioBroadcaster
	// Webhook fires on allowed check-ins (torniquete/cerradura), mismo wire
	// que los endpoints HTTP. Optional; nil → no-op.
	Webhook accesswebhook.Dispatcher

	// FarDivisor tunes identify's target FAR (1/divisor). Defaults to the
	// helper's own default (100k).
	FarDivisor int
	// EnrollTTL / RequiredSamples are overridable for tests.
	EnrollTTL       time.Duration
	RequiredSamples int
	// RefreshEvery is the safety-net gallery rebuild cadence (covers rows
	// that arrive via sync pull from cloud/otro sidecar, socios toggled
	// inactive, etc.). Started by Run; 0 → default 5m.
	RefreshEvery time.Duration

	mu          sync.Mutex
	gymID       uuid.UUID
	epoch       string
	refToMember map[string]uuid.UUID
	session     *enrollSession

	// refreshMu serializes gallery rebuilds so two concurrent refreshes
	// can't interleave epochs between hub and helper.
	refreshMu sync.Mutex
}

type enrollSession struct {
	ID       uuid.UUID
	GymID    uuid.UUID
	MemberID uuid.UUID
	ActorID  uuid.UUID
	Consent  bool
	FMDs     []string
	Deadline time.Time
	// finalizing blocks timeout/cancel while the enroll command + persist
	// run — the outcome (completed/failed) closes the session itself.
	finalizing bool
	timer      *time.Timer
}

func NewBiometricHub(
	engine BioEngine,
	checkin *CheckinByFingerprint,
	register *memApp.RegisterFingerprint,
	members memRepo.MemberRepository,
	fingerprints memRepo.FingerprintRepository,
	gmk bcrypto.GMKProvider,
	uow sharedDomain.UnitOfWork,
	events *BioBroadcaster,
) *BiometricHub {
	return &BiometricHub{
		Engine:          engine,
		Checkin:         checkin,
		Register:        register,
		Members:         members,
		Fingerprints:    fingerprints,
		GMK:             gmk,
		UoW:             uow,
		Events:          events,
		FarDivisor:      biometric.DefaultFarDivisor,
		EnrollTTL:       EnrollSessionTTL,
		RequiredSamples: EnrollSamplesRequired,
		RefreshEvery:    5 * time.Minute,
		refToMember:     map[string]uuid.UUID{},
	}
}

// WithWebhook wires the access-granted dispatcher. Builder-style.
func (h *BiometricHub) WithWebhook(d accesswebhook.Dispatcher) *BiometricHub {
	if d != nil {
		h.Webhook = d
	}
	return h
}

// Run starts the periodic safety-net gallery refresh (cambios que llegan por
// sync pull no tienen hook local). Blocks until ctx fires; llamar en goroutine.
func (h *BiometricHub) Run(ctx context.Context) {
	every := h.RefreshEvery
	if every <= 0 {
		every = 5 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := h.RefreshGallery(ctx); err != nil {
				log.Printf("[biometric] refresh periódico de galería falló: %v", err)
			}
		}
	}
}

// SetActiveGym follows the operator's session (mismo hook OnActiveGymChanged
// que usaba el matcher NBIS). uuid.Nil = logout: limpia galería y cancela la
// sesión de enroll si la hubiera. Synchronous — el refresh es local (SQLite +
// AES) y milisegundos incluso con cientos de socios.
func (h *BiometricHub) SetActiveGym(gymID uuid.UUID) {
	h.mu.Lock()
	changed := h.gymID != gymID
	h.gymID = gymID
	var sess *enrollSession
	if changed {
		sess = h.session
	}
	h.mu.Unlock()
	if sess != nil {
		h.failEnrollSession(sess, EnrollFailCancelled, "cambio de gym activo")
	}
	if !changed {
		return
	}
	if err := h.RefreshGallery(context.Background()); err != nil {
		log.Printf("[biometric] refresh de galería tras cambio de gym falló: %v", err)
	}
}

// NotifyFingerprintsChanged is the hook for local mutations fuera del hub
// (DELETE /members/:id/fingerprint, POST base64). Rebuilds + re-sends.
func (h *BiometricHub) NotifyFingerprintsChanged() {
	if err := h.RefreshGallery(context.Background()); err != nil {
		log.Printf("[biometric] refresh de galería tras cambio de huellas falló: %v", err)
	}
}

// Available is what /biometric/status y /checkins/methods reportan:
// helper vivo + lector conectado.
func (h *BiometricHub) Available() bool {
	return h.Engine.Alive() && h.Engine.Connected()
}

// Enrolling reports whether an enroll session is open (snapshot para status).
func (h *BiometricHub) Enrolling() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.session != nil
}

// Subscribe proxies the broadcaster for the SSE handler.
func (h *BiometricHub) Subscribe() (<-chan BioEvent, func()) {
	return h.Events.Subscribe()
}

// ─────────────────────────── gallery ───────────────────────────

// RefreshGallery rebuilds the helper's 1:N cache from member_fingerprints:
// decrypt every active template del gym activo con la GMK y mandar `gallery`
// con un epoch nuevo. Con gym Nil (logout) manda galería vacía. Si el helper
// no está vivo, no hace nada — HandleHelperUp la re-manda al volver.
func (h *BiometricHub) RefreshGallery(ctx context.Context) error {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()

	if !h.Engine.Alive() {
		return nil
	}
	h.mu.Lock()
	gymID := h.gymID
	h.mu.Unlock()

	candidates := []biometric.GalleryCandidate{}
	refMap := map[string]uuid.UUID{}
	if gymID != uuid.Nil {
		tx, err := h.UoW.Query(ctx)
		if err != nil {
			return err
		}
		rows, err := h.Fingerprints.ListByGym(tx, gymID)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			gmk, err := h.GMK.GetGMK(ctx, gymID)
			if err != nil {
				return err
			}
			defer bcrypto.Zero(gmk)
			for _, fp := range rows {
				plain, derr := bcrypto.DecryptTemplate(gmk, fp.TemplateEncrypted)
				if derr != nil {
					// One bad blob shouldn't drop the whole gallery — skip and log.
					log.Printf("[biometric] galería: descifrado falló fp=%s member=%s: %v", fp.ID, fp.MemberID, derr)
					continue
				}
				ref := fp.ID.String()
				candidates = append(candidates, biometric.GalleryCandidate{Ref: ref, FMD: string(plain)})
				refMap[ref] = fp.MemberID
				bcrypto.Zero(plain)
			}
		}
	}

	epoch := uuid.NewString()
	if err := h.Engine.SetGallery(ctx, epoch, candidates); err != nil {
		return err
	}
	h.mu.Lock()
	h.epoch = epoch
	h.refToMember = refMap
	h.mu.Unlock()
	log.Printf("[biometric] galería enviada epoch=%s size=%d gym=%s", epoch, len(candidates), gymID)
	return nil
}

// identify runs the probe against the helper y valida el epoch: si el helper
// contesta con una galería distinta a la nuestra (carrera enroll-vs-identify
// o helper recién reiniciado), re-manda la galería y reintenta UNA vez.
func (h *BiometricHub) identify(ctx context.Context, fmd string) ([]string, error) {
	for attempt := 0; ; attempt++ {
		refs, helperEpoch, err := h.Engine.Identify(ctx, fmd, h.FarDivisor, 1)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		current := h.epoch
		h.mu.Unlock()
		if helperEpoch == current {
			return refs, nil
		}
		if attempt >= 1 {
			return nil, errors.New("galería desincronizada con el motor biométrico tras reintento")
		}
		log.Printf("[biometric] identify con epoch viejo (helper=%s hub=%s) — re-mandando galería", helperEpoch, current)
		if err := h.RefreshGallery(ctx); err != nil {
			return nil, err
		}
	}
}

// resolveRef maps a gallery ref (member_fingerprints.id) back to the socio.
// Un ref desconocido con epoch correcto implica mapa viejo → un refresh y
// segundo intento; si persiste, se trata como no-match.
func (h *BiometricHub) resolveRef(ctx context.Context, ref string) (uuid.UUID, bool) {
	h.mu.Lock()
	id, ok := h.refToMember[ref]
	h.mu.Unlock()
	if ok {
		return id, true
	}
	if err := h.RefreshGallery(ctx); err != nil {
		return uuid.Nil, false
	}
	h.mu.Lock()
	id, ok = h.refToMember[ref]
	h.mu.Unlock()
	return id, ok
}

// IdentifyFMD implements memApp.FingerprintMatcher — the pre-enrollment
// collision probe that RegisterFingerprint runs. Devuelve el socio dueño del
// template que matchea, o Nil si nadie.
func (h *BiometricHub) IdentifyFMD(ctx context.Context, fmd string) (uuid.UUID, error) {
	if !h.Engine.Alive() {
		return uuid.Nil, biometric.ErrNotAvailable
	}
	refs, err := h.identify(ctx, fmd)
	if err != nil {
		return uuid.Nil, err
	}
	if len(refs) == 0 {
		return uuid.Nil, nil
	}
	id, ok := h.resolveRef(ctx, refs[0])
	if !ok {
		return uuid.Nil, nil
	}
	return id, nil
}

// ─────────────────────────── engine events ───────────────────────────

// HandleHelperUp — helper (re)spawned with an empty gallery: re-send ours.
func (h *BiometricHub) HandleHelperUp() {
	if err := h.RefreshGallery(context.Background()); err != nil {
		log.Printf("[biometric] refresh de galería tras arranque del helper falló: %v", err)
	}
}

// HandleHelperDown — el proceso murió; el engine lo revive con backoff. La
// sesión de enroll (si hay) sobrevive: los FMDs ya acumulados viven en Go y
// siguen siendo válidos cuando el helper vuelva.
func (h *BiometricHub) HandleHelperDown(reason string) {
	log.Printf("[biometric] helper caído (%s)", reason)
	h.Events.Publish(BioEvent{Type: BioReaderDisconnected})
}

// HandleReaderState — hot-plug del lector, directo al FE.
func (h *BiometricHub) HandleReaderState(connected bool, name, serial string) {
	typ := BioReaderDisconnected
	if connected {
		typ = BioReaderConnected
	}
	h.Events.Publish(BioEvent{Type: typ, ReaderName: name, ReaderSerial: serial})
}

// HandleSampleRejected — hubo dedazo pero calidad/extracción falló: feedback
// "vuelve a apoyar el dedo" sin registrar nada.
func (h *BiometricHub) HandleSampleRejected(code, quality string) {
	h.Events.Publish(BioEvent{Type: BioSampleRejected, Code: code, Quality: quality})
}

// HandleSample is the state machine's entry point: enroll session activa →
// acumular; si no → identificar (check-in, el modo default).
func (h *BiometricHub) HandleSample(fmd, quality string) {
	h.mu.Lock()
	sess := h.session
	gymID := h.gymID
	h.mu.Unlock()

	if sess != nil {
		h.enrollSample(sess, fmd)
		return
	}
	if gymID == uuid.Nil {
		return // sin operador logueado no hay galería ni checkin que registrar
	}
	h.checkinSample(gymID, fmd)
}

// ─────────────────────────── check-in mode ───────────────────────────

func (h *BiometricHub) checkinSample(gymID uuid.UUID, fmd string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	h.Events.Publish(BioEvent{Type: BioCheckinAttempt})

	// Galería vacía conocida (ya se envió al helper y no hay enrolados):
	// contestar no_match SIN llamar al motor. Identify del SDK con 0
	// candidatos no devuelve "sin matches" — devuelve DP_INVALID_PARAMETER,
	// y ese error salía al FE como checkin_error "lector no disponible" en
	// vez del "nadie tiene huella registrada" honesto (mordió en la
	// validación real, gym recién instalado sin enrolados).
	h.mu.Lock()
	galleryLoaded := h.epoch != ""
	galleryEmpty := len(h.refToMember) == 0
	h.mu.Unlock()
	if galleryLoaded && galleryEmpty {
		h.Events.Publish(BioEvent{Type: BioCheckinNoMatch, Message: chkErrors.ErrFingerprintNotEnrolled.Error()})
		return
	}

	refs, err := h.identify(ctx, fmd)
	if err != nil {
		log.Printf("[biometric] identify falló: %v", err)
		h.Events.Publish(BioEvent{Type: BioCheckinError, Message: chkErrors.ErrReaderNotAvailable.Error()})
		return
	}
	if len(refs) == 0 {
		h.mu.Lock()
		empty := len(h.refToMember) == 0
		h.mu.Unlock()
		msg := chkErrors.ErrNoFingerprintMatch.Error()
		if empty {
			msg = chkErrors.ErrFingerprintNotEnrolled.Error()
		}
		h.Events.Publish(BioEvent{Type: BioCheckinNoMatch, Message: msg})
		return
	}
	memberID, ok := h.resolveRef(ctx, refs[0])
	if !ok {
		h.Events.Publish(BioEvent{Type: BioCheckinNoMatch, Message: chkErrors.ErrNoFingerprintMatch.Error()})
		return
	}

	view, err := h.Checkin.Execute(ctx, CheckinByFingerprintInput{GymID: gymID, MemberID: memberID})
	if err != nil {
		h.Events.Publish(BioEvent{Type: BioCheckinError, Message: err.Error()})
		return
	}
	h.Events.Publish(BioEvent{Type: BioCheckinResult, Checkin: view})
	h.fireWebhook(ctx, gymID, view)
}

// fireWebhook mirrors CheckinController.dispatchAccessWebhook para el path
// sin HTTP (el dedazo llega por el helper, no por un request).
func (h *BiometricHub) fireWebhook(ctx context.Context, gymID uuid.UUID, view *CheckinView) {
	if h.Webhook == nil || view == nil {
		return
	}
	if !accesswebhook.ResultIsAllowed(string(view.AccessStatus)) {
		return
	}
	h.Webhook.OnAccessAllowed(ctx, accesswebhook.Event{
		Schema:     "cuadra.access.allowed/v1",
		GymID:      gymID,
		MemberID:   view.MemberID,
		MemberName: view.MemberName,
		Method:     view.Method,
		Result:     string(view.AccessStatus),
		OccurredAt: view.Timestamp,
	})
}

// ─────────────────────────── enroll mode ───────────────────────────

// StartEnrollInput abre una sesión de enroll (POST /biometric/enroll/start).
type StartEnrollInput struct {
	GymID           uuid.UUID
	ActorUserID     uuid.UUID
	MemberID        uuid.UUID
	ConsentAccepted bool
}

type StartEnrollOutput struct {
	SessionID       uuid.UUID
	MemberID        uuid.UUID
	ExpiresAt       time.Time
	RequiredSamples int
}

// StartEnroll validates fail-fast (consent, lector, socio, huella previa,
// sesión duplicada) y abre la sesión. Los siguientes RequiredSamples dedazos
// se acumulan; el resultado viaja por SSE, no por esta respuesta.
func (h *BiometricHub) StartEnroll(ctx context.Context, in StartEnrollInput) (*StartEnrollOutput, error) {
	if !in.ConsentAccepted {
		return nil, sharedDomain.NewBusinessError(fpDomain.ErrConsentRequired, "")
	}
	if !h.Available() {
		return nil, sharedDomain.NewBusinessError(chkErrors.ErrReaderNotAvailable, "")
	}
	h.mu.Lock()
	activeGym := h.gymID
	busy := h.session != nil
	h.mu.Unlock()
	if busy {
		return nil, sharedDomain.NewBusinessError(chkErrors.ErrEnrollSessionActive, "")
	}
	if activeGym == uuid.Nil || activeGym != in.GymID {
		return nil, sharedDomain.NewBusinessError(chkErrors.ErrReaderNotAvailable, "el motor biométrico no tiene gym activo")
	}

	// Fail fast on member problems — mejor un 4xx aquí que 3 dedazos y un
	// evento de error. RegisterFingerprint re-valida todo dentro de su tx.
	tx, err := h.UoW.Query(ctx)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	m, err := h.Members.GetByID(tx, in.MemberID)
	if err != nil {
		return nil, err
	}
	if m.GymID != in.GymID {
		return nil, sharedDomain.NewBusinessError(memErrors.ErrCrossGym, "")
	}
	existing, err := h.Fingerprints.ListByMember(tx, in.MemberID)
	if err != nil {
		return nil, sharedDomain.NewUnexpectedError(err)
	}
	if len(existing) > 0 {
		return nil, sharedDomain.NewBusinessError(fpDomain.ErrFingerprintAlreadySet, "")
	}

	sess := &enrollSession{
		ID:       uuid.New(),
		GymID:    in.GymID,
		MemberID: in.MemberID,
		ActorID:  in.ActorUserID,
		Consent:  in.ConsentAccepted,
		Deadline: time.Now().UTC().Add(h.EnrollTTL),
	}
	h.mu.Lock()
	if h.session != nil {
		h.mu.Unlock()
		return nil, sharedDomain.NewBusinessError(chkErrors.ErrEnrollSessionActive, "")
	}
	h.session = sess
	sess.timer = time.AfterFunc(h.EnrollTTL, func() { h.expireEnrollSession(sess) })
	h.mu.Unlock()

	h.Events.Publish(BioEvent{Type: BioEnrollStarted, Enroll: h.enrollState(sess)})
	return &StartEnrollOutput{
		SessionID:       sess.ID,
		MemberID:        sess.MemberID,
		ExpiresAt:       sess.Deadline,
		RequiredSamples: h.RequiredSamples,
	}, nil
}

// CancelEnroll aborts the active session (POST /biometric/enroll/cancel).
func (h *BiometricHub) CancelEnroll(_ context.Context) error {
	h.mu.Lock()
	sess := h.session
	if sess == nil || sess.finalizing {
		h.mu.Unlock()
		return sharedDomain.NewBusinessError(chkErrors.ErrEnrollSessionNotFound, "")
	}
	h.mu.Unlock()
	h.failEnrollSession(sess, EnrollFailCancelled, "cancelado por el operador")
	return nil
}

// enrollSample accumulates one dedazo into the session; on the Nth it runs
// the finalize pipeline (combine → colisión → persistir → galería).
func (h *BiometricHub) enrollSample(sess *enrollSession, fmd string) {
	h.mu.Lock()
	if h.session != sess || sess.finalizing {
		h.mu.Unlock()
		return
	}
	sess.FMDs = append(sess.FMDs, fmd)
	captured := len(sess.FMDs)
	ready := captured >= h.RequiredSamples
	if ready {
		sess.finalizing = true
	}
	h.mu.Unlock()

	h.Events.Publish(BioEvent{Type: BioEnrollProgress, Enroll: h.enrollState(sess)})
	if ready {
		h.finalizeEnroll(sess)
	}
}

func (h *BiometricHub) finalizeEnroll(sess *enrollSession) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	enrollFMD, err := h.Engine.EnrollCombine(ctx, sess.FMDs)
	if err != nil {
		// Set inválido (o helper caído a media combinación): la sesión se
		// CIERRA. Invariante: todo enroll_failed es terminal — el FE suelta
		// su session_id al recibirlo (sessionRef=null) y el reintento abre
		// sesión nueva. La versión anterior dejaba la sesión viva "para
		// otros dedazos", pero nadie la escuchaba ya y la sesión zombie se
		// tragaba los dedazos de CHECK-IN hasta que venciera el TTL (mordió
		// en la validación real: check-in mudo tras un enroll fallido).
		code := EnrollFailEngine
		var cmdErr *biometric.CommandError
		if errors.As(err, &cmdErr) {
			code = EnrollFailInvalidSet
		}
		log.Printf("[biometric] enroll combine falló (session=%s): %v", sess.ID, err)
		h.closeEnrollSession(sess)
		h.Events.Publish(BioEvent{
			Type: BioEnrollFailed, Code: code,
			Message: "no se pudo combinar las capturas; vuelve a intentarlo",
			Enroll:  h.enrollState(sess),
		})
		return
	}

	out, err := h.Register.Execute(ctx, memApp.RegisterFingerprintInput{
		GymID:           sess.GymID,
		ActorUserID:     sess.ActorID,
		MemberID:        sess.MemberID,
		ConsentAccepted: sess.Consent,
		Captures: []*biometric.CaptureResult{{
			Bytes:  []byte(enrollFMD),
			Format: biometric.FormatFMD,
		}},
	})
	if err != nil {
		h.closeEnrollSession(sess)
		state := h.enrollState(sess)
		if errors.Is(err, fpDomain.ErrFingerprintCollision) {
			var ce sharedDomain.CustomError
			if errors.As(err, &ce) && len(ce.Data) > 0 {
				state.Data = ce.Data
			}
			h.Events.Publish(BioEvent{
				Type: BioEnrollFailed, Code: EnrollFailCollision,
				Message: fpDomain.ErrFingerprintCollision.Error(),
				Enroll:  state,
			})
			return
		}
		h.Events.Publish(BioEvent{
			Type: BioEnrollFailed, Code: EnrollFailInternal,
			Message: err.Error(),
			Enroll:  state,
		})
		return
	}

	h.closeEnrollSession(sess)
	// Galería nueva ANTES de anunciar el éxito: el siguiente dedazo del
	// mismo socio ya debe identificar.
	if err := h.RefreshGallery(ctx); err != nil {
		log.Printf("[biometric] refresh de galería tras enroll falló: %v", err)
	}
	state := h.enrollState(sess)
	state.FingerprintIDs = out.FingerprintIDs
	h.Events.Publish(BioEvent{Type: BioEnrollCompleted, Enroll: state})
}

// expireEnrollSession is the TTL timer callback.
func (h *BiometricHub) expireEnrollSession(sess *enrollSession) {
	h.mu.Lock()
	stale := h.session != sess || sess.finalizing
	h.mu.Unlock()
	if stale {
		return
	}
	h.failEnrollSession(sess, EnrollFailTimeout, "se acabó el tiempo para capturar la huella")
}

// failEnrollSession closes the session (if still current) and publishes the
// failure event with the given code.
func (h *BiometricHub) failEnrollSession(sess *enrollSession, code, message string) {
	h.mu.Lock()
	if h.session != sess {
		h.mu.Unlock()
		return
	}
	h.session = nil
	if sess.timer != nil {
		sess.timer.Stop()
	}
	h.mu.Unlock()
	h.Events.Publish(BioEvent{
		Type: BioEnrollFailed, Code: code, Message: message,
		Enroll: h.enrollState(sess),
	})
}

func (h *BiometricHub) closeEnrollSession(sess *enrollSession) {
	h.mu.Lock()
	if h.session == sess {
		h.session = nil
	}
	if sess.timer != nil {
		sess.timer.Stop()
	}
	h.mu.Unlock()
}

// enrollState snapshots the session for event payloads.
func (h *BiometricHub) enrollState(sess *enrollSession) *BioEnrollState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &BioEnrollState{
		SessionID: sess.ID,
		MemberID:  sess.MemberID,
		Captured:  len(sess.FMDs),
		Required:  h.RequiredSamples,
		ExpiresAt: sess.Deadline,
	}
}
