//go:build sidecar

package db

import (
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
)

// reconcileSQLitePaymentCashDestination handles the specific pre-release
// state where v43's column exists but its migration marker does not. The SQL
// migration remains unchanged for normal upgrades. Never ignore arbitrary
// duplicate-column errors or mark a different schema as successfully migrated.
func reconcileSQLitePaymentCashDestination(db *sqlx.DB) (bool, error) {
	tx, err := db.Beginx()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var columns []struct {
		Type         string
		Required     int
		DefaultValue string `db:"default_value"`
	}
	if err := tx.Select(&columns, `SELECT type,"notnull" AS required,COALESCE(dflt_value,'') AS default_value
		FROM pragma_table_info('payments') WHERE name='cash_destination'`); err != nil {
		return false, err
	}
	if len(columns) == 0 {
		return false, nil
	}
	var tableSQL string
	if err := tx.Get(&tableSQL, `SELECT sql FROM sqlite_master WHERE type='table' AND name='payments'`); err != nil {
		return false, err
	}
	column := columns[0]
	compactSQL := strings.Join(strings.Fields(strings.ToLower(tableSQL)), "")
	if !strings.EqualFold(column.Type, "TEXT") || column.Required != 1 || column.DefaultValue != "'cash_drawer'" ||
		!strings.Contains(compactSQL, "check(cash_destinationin('cash_drawer','gym_fund'))") {
		return false, fmt.Errorf("payments.cash_destination has an incompatible definition; migration 43 was not recorded")
	}
	var invalid int
	if err := tx.Get(&invalid, `SELECT COUNT(*) FROM payments
		WHERE cash_destination IS NULL OR cash_destination NOT IN ('cash_drawer','gym_fund')`); err != nil {
		return false, err
	}
	if invalid != 0 {
		return false, fmt.Errorf("payments.cash_destination has invalid values; migration 43 was not recorded")
	}
	if _, err := tx.Exec(`INSERT INTO _migrations(version,name,applied_at)
		VALUES(43,'043_payment_cash_destination',CAST(strftime('%s','now') AS INTEGER)*1000)`); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
