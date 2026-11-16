package infraestructure

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/application/reports"
)

// splitBusinessDates turns a comma-joined SQL aggregate of business dates into
// a sorted, de-duplicated YYYY-MM-DD slice. It never returns nil so the wire
// format is always a JSON array.
func splitBusinessDates(joined string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range strings.Split(joined, ",") {
		day := businessDay(raw)
		if day == "" || seen[day] {
			continue
		}
		seen[day] = true
		out = append(out, day)
	}
	sort.Strings(out)
	return out
}

func businessDay(raw string) string {
	day := strings.TrimSpace(raw)
	if len(day) > 10 {
		day = day[:10]
	}
	return day
}

// parseCashDifferences decodes the JSON array both readers aggregate from the
// effective-session CTE. Money arrives in integer cents so SQLite and Postgres
// round identically. Never returns nil.
func parseCashDifferences(raw string) ([]reports.CashSessionDifference, error) {
	out := []reports.CashSessionDifference{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	var rows []struct {
		ID              string  `json:"id"`
		Date            string  `json:"date"`
		DrawerID        *string `json:"drawer_id"`
		DrawerName      *string `json:"drawer_name"`
		Sequence        *int    `json:"sequence"`
		DifferenceCents int64   `json:"difference_cents"`
		CountedByName   *string `json:"counted_by_name"`
		Reason          *string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		id, err := uuid.Parse(row.ID)
		if err != nil {
			return nil, err
		}
		item := reports.CashSessionDifference{
			SessionID:     id,
			Date:          businessDay(row.Date),
			DrawerName:    trimmedOrNil(row.DrawerName),
			Sequence:      1,
			Difference:    float64(row.DifferenceCents) / 100,
			CountedByName: trimmedOrNil(row.CountedByName),
			Reason:        trimmedOrNil(row.Reason),
		}
		if row.Sequence != nil && *row.Sequence > 0 {
			item.Sequence = *row.Sequence
		}
		if row.DrawerID != nil && *row.DrawerID != "" {
			drawerID, err := uuid.Parse(*row.DrawerID)
			if err != nil {
				return nil, err
			}
			item.DrawerID = &drawerID
		}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date > out[j].Date
		}
		if out[i].Sequence != out[j].Sequence {
			return out[i].Sequence > out[j].Sequence
		}
		return out[i].SessionID.String() < out[j].SessionID.String()
	})
	return out, nil
}

// parseCashOpeningDifferences reads the same JSON shape as difference_sessions:
// counted_by_name carries who opened the drawer and reason its opening note.
func parseCashOpeningDifferences(raw string) ([]reports.CashOpeningDifference, error) {
	rows, err := parseCashDifferences(raw)
	if err != nil {
		return nil, err
	}
	out := make([]reports.CashOpeningDifference, 0, len(rows))
	for _, row := range rows {
		out = append(out, reports.CashOpeningDifference{
			SessionID: row.SessionID, Date: row.Date, DrawerID: row.DrawerID, DrawerName: row.DrawerName,
			Difference: row.Difference, OpenedByName: row.CountedByName, Note: row.Reason,
		})
	}
	return out, nil
}

func trimmedOrNil(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
