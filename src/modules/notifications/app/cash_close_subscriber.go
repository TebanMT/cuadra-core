package app

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
)

// CashCloseAlertSubscriber adapts EnqueueOwnerAlert to the
// reports.CashCloseSubscriber contract. It's the bridge that fires the
// `cash_close_diff` owner alert when a count (cierre or recount) leaves a
// non-zero discrepancy.
//
// Lives in notifications/app (not reports/) so the dependency points the
// right way: notifications can know about reports' subscriber interface
// (it's a protocol contract); reports must not know about notifications.
type CashCloseAlertSubscriber struct {
	Enqueue *EnqueueOwnerAlert
}

func NewCashCloseAlertSubscriber(enqueue *EnqueueOwnerAlert) *CashCloseAlertSubscriber {
	return &CashCloseAlertSubscriber{Enqueue: enqueue}
}

// OnCashCloseClosed enqueues an owner_alert_cash_close_diff notification
// when the discrepancy is non-zero. The owner-alert config is consulted
// inside EnqueueOwnerAlert.Execute, so a disabled toggle silently skips.
//
// Errors are swallowed (logged) on purpose — the cierre row has already
// committed by the time we get here, and the operator's HTTP response
// must not be held hostage by a downstream alert hiccup.
func (s *CashCloseAlertSubscriber) OnCashCloseClosed(ctx context.Context, evt reportsApp.CashCloseClosedEvent) {
	if s.Enqueue == nil || evt.Discrepancy == nil {
		return
	}
	cents := int64(math.Round(*evt.Discrepancy * 100))
	if cents == 0 {
		// Sub-cent float jitter is a matched count, not a difference.
		return
	}
	// Per session and result: two drawers or two sessions on the same day
	// each alert, a retry of the same count does not, and a recount that
	// changes the difference alerts again.
	suffix := evt.CloseDate.Format("2006-01-02")
	if evt.SessionID != uuid.Nil {
		suffix = fmt.Sprintf("%s:%d", evt.SessionID, cents)
	}
	in := EnqueueOwnerAlertInput{
		GymID: evt.GymID,
		Kind:  OwnerAlertCashCloseDiff,
		Vars: map[string]string{
			"diff_amount": cashDiffSummary(evt, cents),
		},
		IdempotencyKeySuffix: suffix,
	}
	if _, err := s.Enqueue.Execute(ctx, in); err != nil {
		log.Printf("[notifications/cash_close_alert] enqueue err gym=%s err=%v", evt.GymID, err)
	}
}

// cashDiffSummary fills the template's only free slot. The Meta-approved
// template has exactly {gym_name, diff_amount} and renders "…diferencia de
// ${diff_amount}. Revisa…", so the value starts with the unsigned amount and
// carries direction, counter, drawer, date and note. WhatsApp rejects
// parameters with newlines or long runs of spaces.
func cashDiffSummary(evt reportsApp.CashCloseClosedEvent, cents int64) string {
	direction := "sobrante"
	if cents < 0 {
		direction = "faltante"
		cents = -cents
	}
	parts := []string{fmt.Sprintf("%s (%s)", formatPesoCents(cents), direction)}
	if name := alertText(evt.CountedByName, 40); name != "" {
		parts = append(parts, "contó "+name)
	}
	if drawer := alertText(evt.DrawerName, 40); drawer != "" {
		parts = append(parts, drawer)
	}
	if !evt.CloseDate.IsZero() {
		day := evt.CloseDate.UTC()
		parts = append(parts, fmt.Sprintf("%d %s", day.Day(), spanishMonths[day.Month()]))
	}
	if note := alertText(evt.Reason, 80); note != "" {
		parts = append(parts, "Nota: "+note)
	}
	return strings.Join(parts, " · ")
}

func formatPesoCents(cents int64) string {
	digits := strconv.FormatInt(cents/100, 10)
	var grouped strings.Builder
	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	return fmt.Sprintf("%s.%02d", grouped.String(), cents%100)
}

func alertText(value *string, maxRunes int) string {
	if value == nil {
		return ""
	}
	text := strings.Join(strings.Fields(*value), " ")
	if utf8.RuneCountInString(text) > maxRunes {
		runes := []rune(text)
		text = strings.TrimSpace(string(runes[:maxRunes-1])) + "…"
	}
	// The template continues with ". Revisa…"; avoid "..".
	return strings.TrimRight(text, ".")
}
