package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
	notiDomain "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	notiRepo "github.com/cuadra/cuadra-core/src/modules/notifications/domain/repository"
	subDomain "github.com/cuadra/cuadra-core/src/modules/subscriptions/domain"
	userDomain "github.com/cuadra/cuadra-core/src/modules/users/domain/user"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Tests para el cron de renovación OXXO + el job de cancel post-vencimiento.
// Cubren los casos que el spec/funder llamó out:
//   - candidatos filtrados (annual, active, oxxo)
//   - idempotencia: dos ticks el mismo día no duplican
//   - cancel post-vencimiento sólo si pasó la gracia
//   - tarjeta-anual NO entra al cron (Stripe Subscription la renueva sola)

// ── Fakes específicos ───────────────────────────────────────────────────

type fakeOXXOReader struct {
	renewalCandidates []OXXORenewalCandidate
	expiredCandidates []OXXORenewalCandidate

	lastWindowStart, lastWindowEnd time.Time
	lastBefore                     time.Time
}

func (r *fakeOXXOReader) FindRenewalCandidates(_ sharedDomain.Transaction, windowStart, windowEnd time.Time) ([]OXXORenewalCandidate, error) {
	r.lastWindowStart = windowStart
	r.lastWindowEnd = windowEnd
	out := []OXXORenewalCandidate{}
	for _, c := range r.renewalCandidates {
		if !c.SubscriptionEndsAt.Before(windowStart) && !c.SubscriptionEndsAt.After(windowEnd) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *fakeOXXOReader) FindExpiredOXXO(_ sharedDomain.Transaction, before time.Time) ([]OXXORenewalCandidate, error) {
	r.lastBefore = before
	out := []OXXORenewalCandidate{}
	for _, c := range r.expiredCandidates {
		if c.SubscriptionEndsAt.Before(before) {
			out = append(out, c)
		}
	}
	return out, nil
}

// fakeNotifications: solo lo que necesita el use case (GetByIdempotencyKey +
// Create). El resto satisface la interfaz por completitud.
type fakeNotifications struct {
	byKey  map[string]*notiDomain.Notification
	rows   []*notiDomain.Notification
	failOn string
}

func (r *fakeNotifications) Create(_ sharedDomain.Transaction, n *notiDomain.Notification) (*notiDomain.Notification, error) {
	if r.failOn == "create" {
		return nil, errors.New("forced create failure")
	}
	if r.byKey == nil {
		r.byKey = map[string]*notiDomain.Notification{}
	}
	if n.IdempotencyKey != nil {
		r.byKey[*n.IdempotencyKey] = n
	}
	r.rows = append(r.rows, n)
	return n, nil
}

func (r *fakeNotifications) Update(_ sharedDomain.Transaction, n *notiDomain.Notification) (*notiDomain.Notification, error) {
	return n, nil
}
func (r *fakeNotifications) GetByID(_ sharedDomain.Transaction, id uuid.UUID) (*notiDomain.Notification, error) {
	for _, n := range r.rows {
		if n.ID == id {
			return n, nil
		}
	}
	return nil, errors.New("not found")
}
func (r *fakeNotifications) GetByIdempotencyKey(_ sharedDomain.Transaction, _ uuid.UUID, key string) (*notiDomain.Notification, error) {
	if r.byKey == nil {
		return nil, nil
	}
	return r.byKey[key], nil
}
func (r *fakeNotifications) GetByProviderMessageID(_ sharedDomain.Transaction, _ string) (*notiDomain.Notification, error) {
	return nil, nil
}
func (r *fakeNotifications) LeasePending(_ sharedDomain.Transaction, _ time.Time, _ int) ([]*notiDomain.Notification, error) {
	return nil, nil
}
func (r *fakeNotifications) ListByGym(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _, _ int) ([]*notiDomain.Notification, int, error) {
	return nil, 0, nil
}
func (r *fakeNotifications) ChannelStats(_ sharedDomain.Transaction, _ uuid.UUID, _ string, _ time.Time) (notiRepo.NotificationStats, error) {
	return notiRepo.NotificationStats{}, nil
}
func (r *fakeNotifications) LastError(_ sharedDomain.Transaction, _ uuid.UUID, _ string) (string, *time.Time, error) {
	return "", nil, nil
}

// fakeCheckout — siempre devuelve un URL canónico. Útil para verificar que
// el use case persiste el voucher_url en el payload de la notificación.
type fakeCheckout struct {
	calls   int
	lastIn  StartCheckoutInput
	failOn  int // si > 0, falla en la N-ésima llamada
	urlBase string
}

func (c *fakeCheckout) Execute(_ context.Context, in StartCheckoutInput) (StartCheckoutOutput, error) {
	c.calls++
	c.lastIn = in
	if c.failOn > 0 && c.calls == c.failOn {
		return StartCheckoutOutput{}, errors.New("stripe boom")
	}
	base := c.urlBase
	if base == "" {
		base = "https://checkout.stripe.com/c/oxxo/"
	}
	return StartCheckoutOutput{URL: base + in.GymID.String(), SessionID: "cs_" + in.GymID.String()}, nil
}

// fakeRecorder — captura llamadas a RecordEvent (cancel post-vencimiento).
type fakeRecorder struct {
	calls    []RecordEventInput
	applyAll bool
}

func (r *fakeRecorder) Execute(_ context.Context, in RecordEventInput) (RecordEventOutput, error) {
	r.calls = append(r.calls, in)
	return RecordEventOutput{EventID: uuid.New(), Applied: r.applyAll}, nil
}

// ── Helpers ─────────────────────────────────────────────────────────────

func newOXXOReminderUC(t *testing.T, reader OXXORenewalReader, notifs *fakeNotifications, users *fakeUsers, checkout CheckoutStarter, now time.Time) *RunOXXORenewalReminders {
	t.Helper()
	return &RunOXXORenewalReminders{
		Reader:        reader,
		Notifications: notifs,
		Gyms:          &fakeGyms{byID: map[uuid.UUID]*gymDomain.Gym{}},
		Users:         users,
		Checkout:      checkout,
		UoW:           fakeUoW{},
		NowFunc:       func() time.Time { return now },
	}
}

func newOwnerUser(gymID uuid.UUID, phone, email string) *userDomain.User {
	var phonePtr *string
	if strings.TrimSpace(phone) != "" {
		p := phone
		phonePtr = &p
	}
	return &userDomain.User{
		ID:     uuid.New(),
		GymID:  gymID,
		Role:   "owner",
		Active: true,
		Email:  email,
		Phone:  phonePtr,
	}
}

// ── Tests: candidate filtering ──────────────────────────────────────────

func TestOXXOReminder_TickEnqueuesAtAllStages(t *testing.T) {
	// Tres gyms: uno en cada ventana 30d/14d/3d, todos OXXO+annual. Tick los
	// captura a los tres. Quarto gym fuera de ventana no recibe.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	gym30 := uuid.New()
	gym14 := uuid.New()
	gym3 := uuid.New()
	gymOutOfWindow := uuid.New()

	reader := &fakeOXXOReader{
		renewalCandidates: []OXXORenewalCandidate{
			{GymID: gym30, GymName: "Gym 30d", SubscriptionEndsAt: now.Add(30 * 24 * time.Hour), GymWhatsAppReady: true},
			{GymID: gym14, GymName: "Gym 14d", SubscriptionEndsAt: now.Add(14 * 24 * time.Hour), GymWhatsAppReady: true},
			{GymID: gym3, GymName: "Gym 3d", SubscriptionEndsAt: now.Add(3 * 24 * time.Hour), GymWhatsAppReady: true},
			{GymID: gymOutOfWindow, GymName: "Gym lejos", SubscriptionEndsAt: now.Add(60 * 24 * time.Hour), GymWhatsAppReady: true},
		},
	}
	users := &fakeUsers{byGym: map[uuid.UUID][]*userDomain.User{
		gym30:          {newOwnerUser(gym30, "+5215512345678", "a@x.com")},
		gym14:          {newOwnerUser(gym14, "+5215512345678", "b@x.com")},
		gym3:           {newOwnerUser(gym3, "+5215512345678", "c@x.com")},
		gymOutOfWindow: {newOwnerUser(gymOutOfWindow, "+5215512345678", "d@x.com")},
	}}
	notifs := &fakeNotifications{}
	checkout := &fakeCheckout{}
	uc := newOXXOReminderUC(t, reader, notifs, users, checkout, now)

	n, err := uc.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("Tick err: %v", err)
	}
	if n != 3 {
		t.Errorf("enqueued=%d, want 3 (uno por gym en ventana)", n)
	}
	if len(notifs.rows) != 3 {
		t.Errorf("rows=%d, want 3", len(notifs.rows))
	}
	// Cada notificación debe llevar un voucher_url generado por StartCheckout.
	for _, row := range notifs.rows {
		if row.Payload["voucher_url"] == "" {
			t.Errorf("notificación sin voucher_url: %+v", row.Payload)
		}
		if row.Channel != notiDomain.ChannelWhatsApp {
			t.Errorf("channel=%q, want whatsapp (gym connected)", row.Channel)
		}
	}
}

func TestOXXOReminder_TickIsIdempotent(t *testing.T) {
	// Correr Tick dos veces el mismo día no genera dos notificaciones por gym.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	gymID := uuid.New()
	reader := &fakeOXXOReader{
		renewalCandidates: []OXXORenewalCandidate{
			{GymID: gymID, GymName: "Gym X", SubscriptionEndsAt: now.Add(14 * 24 * time.Hour), GymWhatsAppReady: true},
		},
	}
	users := &fakeUsers{byGym: map[uuid.UUID][]*userDomain.User{
		gymID: {newOwnerUser(gymID, "+5215512345678", "a@x.com")},
	}}
	notifs := &fakeNotifications{}
	checkout := &fakeCheckout{}
	uc := newOXXOReminderUC(t, reader, notifs, users, checkout, now)

	if _, err := uc.Tick(context.Background(), now); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if _, err := uc.Tick(context.Background(), now); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(notifs.rows) != 1 {
		t.Errorf("rows=%d después de 2 ticks, want 1 (idempotente)", len(notifs.rows))
	}
}

func TestOXXOReminder_FallbacksToEmailWhenWhatsAppNotReady(t *testing.T) {
	// Gym sin WhatsApp conectado: el use case elige email como canal primario
	// (no espera a que el dispatcher trate y caiga al fallback).
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	gymID := uuid.New()
	reader := &fakeOXXOReader{
		renewalCandidates: []OXXORenewalCandidate{
			{GymID: gymID, GymName: "Gym X", SubscriptionEndsAt: now.Add(3 * 24 * time.Hour), GymWhatsAppReady: false},
		},
	}
	users := &fakeUsers{byGym: map[uuid.UUID][]*userDomain.User{
		gymID: {newOwnerUser(gymID, "+5215512345678", "owner@x.com")},
	}}
	notifs := &fakeNotifications{}
	checkout := &fakeCheckout{}
	uc := newOXXOReminderUC(t, reader, notifs, users, checkout, now)

	if _, err := uc.Tick(context.Background(), now); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(notifs.rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(notifs.rows))
	}
	if notifs.rows[0].Channel != notiDomain.ChannelEmail {
		t.Errorf("channel=%q, want email (gym sin whatsapp)", notifs.rows[0].Channel)
	}
	if notifs.rows[0].RecipientAddress != "owner@x.com" {
		t.Errorf("recipient=%q, want owner@x.com", notifs.rows[0].RecipientAddress)
	}
}

func TestOXXOReminder_NoOwnerSkipsGracefully(t *testing.T) {
	// Gym sin owner user activo (edge: signup huérfano, transfer mal hecho).
	// El use case loguea y sigue — no rompe el tick para otros gyms.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	gymID := uuid.New()
	reader := &fakeOXXOReader{
		renewalCandidates: []OXXORenewalCandidate{
			{GymID: gymID, GymName: "Gym X", SubscriptionEndsAt: now.Add(14 * 24 * time.Hour), GymWhatsAppReady: true},
		},
	}
	users := &fakeUsers{byGym: map[uuid.UUID][]*userDomain.User{
		gymID: {}, // sin owner
	}}
	notifs := &fakeNotifications{}
	checkout := &fakeCheckout{}
	uc := newOXXOReminderUC(t, reader, notifs, users, checkout, now)

	n, err := uc.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 0 {
		t.Errorf("enqueued=%d, want 0 (skip por no-owner)", n)
	}
	if checkout.calls != 0 {
		t.Errorf("checkout.calls=%d, want 0 (no debió pedir link sin owner)", checkout.calls)
	}
}

func TestOXXOReminder_CardAnnualGymsNotInReader(t *testing.T) {
	// El reader es responsable de filtrar fuera a los gyms que pagaron tarjeta
	// (su último activated/renewed no tiene payment_method=oxxo). Aquí
	// simulamos eso: el fake reader devuelve la lista vacía aunque le pidas
	// el día exacto del vencimiento de un gym imaginario de tarjeta.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	reader := &fakeOXXOReader{renewalCandidates: []OXXORenewalCandidate{}}
	users := &fakeUsers{byGym: map[uuid.UUID][]*userDomain.User{}}
	notifs := &fakeNotifications{}
	checkout := &fakeCheckout{}
	uc := newOXXOReminderUC(t, reader, notifs, users, checkout, now)

	n, err := uc.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if n != 0 {
		t.Errorf("enqueued=%d, want 0", n)
	}
}

func TestOXXOReminder_CheckoutFailureDoesNotPersistRow(t *testing.T) {
	// Si Stripe falla generando el link, NO debemos crear la notificación
	// (su voucher_url quedaría vacío y no tiene sentido enviarla). El próximo
	// tick reintenta — la idempotency key todavía no existe.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	gymID := uuid.New()
	reader := &fakeOXXOReader{
		renewalCandidates: []OXXORenewalCandidate{
			{GymID: gymID, GymName: "Gym X", SubscriptionEndsAt: now.Add(14 * 24 * time.Hour), GymWhatsAppReady: true},
		},
	}
	users := &fakeUsers{byGym: map[uuid.UUID][]*userDomain.User{
		gymID: {newOwnerUser(gymID, "+5215512345678", "a@x.com")},
	}}
	notifs := &fakeNotifications{}
	checkout := &fakeCheckout{failOn: 1}
	uc := newOXXOReminderUC(t, reader, notifs, users, checkout, now)

	n, err := uc.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("tick err: %v", err)
	}
	if n != 0 {
		t.Errorf("enqueued=%d, want 0 (checkout falló)", n)
	}
	if len(notifs.rows) != 0 {
		t.Errorf("rows=%d, want 0 (no persistir si no hay link)", len(notifs.rows))
	}
}

// ── Tests: cancel post-vencimiento ──────────────────────────────────────

func TestCancelExpiredOXXO_OnlyAfterGrace(t *testing.T) {
	// Dos gyms: uno -8d (pasó la gracia de 7d), otro -3d (dentro de gracia).
	// El fake reader hace el filtro por `before` que recibió del use case
	// (now - 7d), así que el primero entra y el segundo no.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	gymBeyondGrace := uuid.New()
	gymInGrace := uuid.New()
	reader := &fakeOXXOReader{
		expiredCandidates: []OXXORenewalCandidate{
			{GymID: gymBeyondGrace, SubscriptionEndsAt: now.Add(-8 * 24 * time.Hour)},
			{GymID: gymInGrace, SubscriptionEndsAt: now.Add(-3 * 24 * time.Hour)},
		},
	}
	recorder := &fakeRecorder{applyAll: true}
	uc := &CancelExpiredOXXO{
		Reader:   reader,
		Recorder: recorder,
		UoW:      fakeUoW{},
		NowFunc:  func() time.Time { return now },
		Grace:    7 * 24 * time.Hour,
	}

	cancelled, err := uc.Tick(context.Background(), now)
	if err != nil {
		t.Fatalf("Tick err: %v", err)
	}
	if cancelled != 1 {
		t.Errorf("cancelled=%d, want 1 (sólo el de -8d entra)", cancelled)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("recorder.calls=%d, want 1", len(recorder.calls))
	}
	call := recorder.calls[0]
	if call.GymID != gymBeyondGrace {
		t.Errorf("cancelled gym mismatch")
	}
	if call.Type != subDomain.EventCancelled {
		t.Errorf("type=%q, want cancelled", call.Type)
	}
	if call.Provider != subDomain.ProviderManual {
		t.Errorf("provider=%q, want manual", call.Provider)
	}
	if !strings.HasPrefix(call.ExternalID, "oxxo-expiry-cancel:") {
		t.Errorf("external_id=%q, want prefix oxxo-expiry-cancel:", call.ExternalID)
	}
}

func TestCancelExpiredOXXO_DeterministicExternalID(t *testing.T) {
	// Dos ticks consecutivos deben generar el mismo external_id para el mismo
	// (gym, ends_at). RecordEvent se encarga del idempotent replay vía la
	// (provider, external_id) uniqueness; aquí sólo validamos la cadena.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	gymID := uuid.New()
	endsAt := now.Add(-10 * 24 * time.Hour)
	reader := &fakeOXXOReader{
		expiredCandidates: []OXXORenewalCandidate{
			{GymID: gymID, SubscriptionEndsAt: endsAt},
		},
	}
	recorder := &fakeRecorder{applyAll: true}
	uc := &CancelExpiredOXXO{
		Reader:   reader,
		Recorder: recorder,
		UoW:      fakeUoW{},
		NowFunc:  func() time.Time { return now },
		Grace:    7 * 24 * time.Hour,
	}

	if _, err := uc.Tick(context.Background(), now); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := uc.Tick(context.Background(), now); err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(recorder.calls) != 2 {
		t.Fatalf("recorder.calls=%d, want 2 (ambos ticks llaman)", len(recorder.calls))
	}
	if recorder.calls[0].ExternalID != recorder.calls[1].ExternalID {
		t.Errorf("external_id no determinístico: %q vs %q", recorder.calls[0].ExternalID, recorder.calls[1].ExternalID)
	}
	wantSuffix := endsAt.UTC().Format("2006-01-02")
	if !strings.HasSuffix(recorder.calls[0].ExternalID, wantSuffix) {
		t.Errorf("external_id=%q, want suffix %q", recorder.calls[0].ExternalID, wantSuffix)
	}
}

func TestOXXOReminder_StageWindowAlignment(t *testing.T) {
	// Verifica que el reader recibe la ventana ±12h centrada en now+DaysBefore.
	// Sin esto, un gym justo a 14d 11h podría caer fuera del bucket "14d".
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	reader := &fakeOXXOReader{}
	users := &fakeUsers{byGym: map[uuid.UUID][]*userDomain.User{}}
	notifs := &fakeNotifications{}
	checkout := &fakeCheckout{}
	uc := newOXXOReminderUC(t, reader, notifs, users, checkout, now)

	if _, err := uc.Tick(context.Background(), now); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// El último stage que corrió es DaysBefore=0 (día 0). Ventana esperada
	// [now - 12h, now + 12h].
	wantStart := now.Add(-12 * time.Hour)
	wantEnd := now.Add(12 * time.Hour)
	if !reader.lastWindowStart.Equal(wantStart) {
		t.Errorf("windowStart=%v, want %v", reader.lastWindowStart, wantStart)
	}
	if !reader.lastWindowEnd.Equal(wantEnd) {
		t.Errorf("windowEnd=%v, want %v", reader.lastWindowEnd, wantEnd)
	}
}
