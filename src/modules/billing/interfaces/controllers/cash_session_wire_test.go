package controllers

import (
	reports "github.com/cuadra/cuadra-core/src/application/reports"
	"testing"
	"time"
)

func TestCashSessionResponsePreservesMillisecondBoundaries(t *testing.T) {
	start := time.Date(2026, 9, 27, 18, 0, 0, 501000000, time.UTC)
	end := start.Add(time.Minute)
	out := cashSessionToResp(&reports.CashSessionView{OpenedAt: start, ClosedAt: &end, ReconciledAt: &end, FinishedAt: &end})
	for name, raw := range map[string]string{"opening": out.OpenedAt, "finished": *out.FinishedAt} {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || parsed.Nanosecond() != 501000000 {
			t.Fatalf("%s lost the cash boundary: %s (%v)", name, raw, err)
		}
	}
}
