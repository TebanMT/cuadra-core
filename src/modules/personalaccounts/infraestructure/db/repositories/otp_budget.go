//go:build server

package repositories

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"

	budget "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	budgetErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Two-int advisory locks occupy a separate PostgreSQL key space from the
// bigint phone locks. This namespace is dedicated to OTP capacity only.
const otpBudgetLockNamespace int32 = 0x4d434250

type OTPBudgetRepository struct{}

func NewOTPBudgetRepository() *OTPBudgetRepository { return &OTPBudgetRepository{} }

var _ budget.Repository = (*OTPBudgetRepository)(nil)

func (*OTPBudgetRepository) Reserve(handle shared.Transaction, plan budget.Plan) error {
	transaction, ok := handle.(*shared.GormTransaction)
	if !ok || transaction == nil || transaction.Tx == nil || !plan.Valid() {
		return budgetErrors.ErrBudgetInput
	}
	tx := transaction.Tx
	if tx.Statement == nil {
		return budgetErrors.ErrBudgetInput
	}
	if _, active := tx.Statement.ConnPool.(gorm.TxCommitter); !active {
		return budgetErrors.ErrBudgetInput
	}
	// A snapshot retained before waiting for advisory locks could miss a
	// committed block on another window. Each statement must see commits made
	// before it starts, and this operation must never run inside a read query.
	var mode struct {
		Isolation string
		ReadOnly  string
	}
	if err := tx.Raw("SELECT current_setting('transaction_isolation') AS isolation, current_setting('transaction_read_only') AS read_only").Scan(&mode).Error; err != nil {
		return err
	}
	if mode.Isolation != "read committed" || mode.ReadOnly != "off" {
		return budgetErrors.ErrBudgetInput
	}
	dimensions := plan.Dimensions()
	// Lock all subjects before inserting any window. Sorting actual advisory
	// keys (and deduplicating collisions) avoids inversions across different
	// windows and between plans with overlapping dimensions.
	keys := make([]int32, 0, len(dimensions))
	seen := map[int32]bool{}
	for _, d := range dimensions {
		key := budgetLockKey(d)
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, key := range keys {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(?,?)`, otpBudgetLockNamespace, key).Error; err != nil {
			return err
		}
	}
	accepted := make([]budget.Counter, 0, len(dimensions))
	for _, dimension := range dimensions {
		initial := toBudgetModel(budget.Counter{Scope: dimension.Scope, SubjectHash: dimension.SubjectHash, WindowStart: dimension.WindowStart})
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&initial).Error; err != nil {
			return err
		}
		var rows []models.OTPBudgetModel
		if err := tx.Where("scope=? AND subject_hash=? AND (window_start=? OR blocked_until>?)", string(dimension.Scope), dimension.SubjectHash[:], dimension.WindowStart, plan.At()).Order("window_start ASC").Clauses(clause.Locking{Strength: "UPDATE"}).Find(&rows).Error; err != nil {
			return err
		}
		var current budget.Counter
		found := false
		for _, row := range rows {
			if row.WindowStart.Equal(dimension.WindowStart) {
				var err error
				current, err = toBudgetDomain(row)
				if err != nil {
					return err
				}
				found = true
				break
			}
		}
		if !found {
			return budgetErrors.ErrBudgetCorrupt
		}
		// Active blocks survive a fixed-window rollover; no new window can
		// bypass a still-active block on an older row of this same subject.
		for _, row := range rows {
			if row.BlockedUntil != nil && (current.BlockedUntil == nil || row.BlockedUntil.After(*current.BlockedUntil)) {
				until := *row.BlockedUntil
				current.BlockedUntil = &until
			}
		}
		if err := current.Reserve(plan.At(), plan.Action(), dimension.Limit); err != nil {
			return err
		}
		accepted = append(accepted, current)
	}
	// No dimension consumes a count until every dimension was accepted.
	for _, counter := range accepted {
		result := tx.Model(&models.OTPBudgetModel{}).Where("scope=? AND subject_hash=? AND window_start=?", string(counter.Scope), counter.SubjectHash[:], counter.WindowStart).Updates(map[string]any{"send_count": counter.SendCount, "check_count": counter.CheckCount})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return budgetErrors.ErrBudgetCorrupt
		}
	}
	return nil
}

func budgetLockKey(d budget.Dimension) int32 {
	h := sha256.New()
	_, _ = h.Write([]byte(string(d.Scope) + "\x00"))
	_, _ = h.Write(d.SubjectHash[:])
	sum := h.Sum(nil)
	return int32(binary.BigEndian.Uint32(sum[:4]))
}

func toBudgetModel(counter budget.Counter) models.OTPBudgetModel {
	return models.OTPBudgetModel{Scope: string(counter.Scope), SubjectHash: append([]byte(nil), counter.SubjectHash[:]...), WindowStart: counter.WindowStart, SendCount: counter.SendCount, CheckCount: counter.CheckCount, BlockedUntil: counter.BlockedUntil}
}
func toBudgetDomain(model models.OTPBudgetModel) (budget.Counter, error) {
	if len(model.SubjectHash) != 32 {
		return budget.Counter{}, budgetErrors.ErrBudgetCorrupt
	}
	var hash [32]byte
	copy(hash[:], model.SubjectHash)
	return budget.Counter{Scope: budget.Scope(model.Scope), SubjectHash: hash, WindowStart: model.WindowStart.UTC(), SendCount: model.SendCount, CheckCount: model.CheckCount, BlockedUntil: model.BlockedUntil}, nil
}
