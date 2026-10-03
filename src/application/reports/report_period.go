package reports

import (
	"errors"
	"time"
)

var (
	ErrReportPeriodInvalid       = errors.New("período de reporte inválido")
	ErrCustomReportRangeRequired = errors.New("el período personalizado requiere fecha inicial y final")
	ErrReportRangeInvalid        = errors.New("la fecha inicial del reporte no puede ser posterior a la fecha final")
)

func validReportPeriod(period string) bool {
	switch period {
	case PeriodToday, PeriodWeek, PeriodMonth, PeriodLastMonth, Period3Months, PeriodYear, PeriodCustom:
		return true
	default:
		return false
	}
}

// validateReportWindow is deliberately fail-closed: financial screens and
// exports must never silently substitute or reverse the range a user chose.
func validateReportWindow(period string, from, to *time.Time) error {
	if !validReportPeriod(period) {
		return ErrReportPeriodInvalid
	}
	if period != PeriodCustom {
		return nil
	}
	if from == nil || to == nil || from.IsZero() || to.IsZero() {
		return ErrCustomReportRangeRequired
	}
	f := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	t := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)
	if f.After(t) {
		return ErrReportRangeInvalid
	}
	return nil
}
