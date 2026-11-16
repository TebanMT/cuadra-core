//go:build server && integration

package repositories

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	budget "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/budget"
	budgetErrors "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/errors"
	hashing "github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/budget"
	"github.com/cuadra/cuadra-core/src/modules/personalaccounts/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func budgetTestPool(t *testing.T, key string) *gorm.DB {
	t.Helper()
	dsn := os.Getenv(key)
	if dsn == "" {
		t.Skip(key + " required for isolated PostgreSQL budget tests")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid isolated database configuration")
	}
	local := func(host string) bool {
		if filepath.IsAbs(host) || host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	if !local(config.Host) {
		t.Fatal("budget tests require local PostgreSQL")
	}
	for _, fallback := range config.Fallbacks {
		if !local(fallback.Host) {
			t.Fatal("budget tests require local PostgreSQL fallbacks")
		}
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal("isolated PostgreSQL unavailable")
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(8)
	pool.SetMaxIdleConns(8)
	t.Cleanup(func() { pool.Close() })
	return db
}
func budgetTestUoW(t *testing.T, db *gorm.DB) shared.UnitOfWork {
	t.Helper()
	uow, err := shared.NewPersonalPostgresUnitOfWork(db, shared.PersonalAuthRole)
	if err != nil {
		t.Fatal(err)
	}
	return uow
}
func budgetTestSubjects(t *testing.T) map[budget.Scope][32]byte {
	t.Helper()
	h, err := hashing.NewSubjects([]byte("synthetic-budget-key-32-bytes-minimum"))
	if err != nil {
		t.Fatal(err)
	}
	subjects := map[budget.Scope][32]byte{}
	for _, scope := range []budget.Scope{budget.Destination, budget.Origin, budget.Service} {
		hash, err := h.Hash(scope, uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		subjects[scope] = hash
	}
	return subjects
}
func budgetTestRules(t *testing.T, maxSend, maxCheck int, window time.Duration) budget.Rules {
	t.Helper()
	r, err := budget.NewRules(map[budget.Scope]budget.Limit{budget.Destination: {Window: window, MaxSend: maxSend, MaxCheck: maxCheck}, budget.Origin: {Window: window, MaxSend: maxSend, MaxCheck: maxCheck}, budget.Service: {Window: window, MaxSend: maxSend, MaxCheck: maxCheck}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func budgetTestPlan(t *testing.T, r budget.Rules, at time.Time, action budget.Action, subjects map[budget.Scope][32]byte) budget.Plan {
	t.Helper()
	p, err := r.Plan(at, action, subjects)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func budgetTestContext() context.Context {
	return shared.WithPersonalAttempt(context.Background(), uuid.New(), sha256.Sum256([]byte(uuid.NewString())))
}
func reserveBudget(uow shared.UnitOfWork, repo *OTPBudgetRepository, p budget.Plan) error {
	ctx, cancel := context.WithTimeout(budgetTestContext(), 10*time.Second)
	defer cancel()
	return uow.Command(ctx, func(tx shared.Transaction) error { return repo.Reserve(tx, p) })
}
func budgetRows(t *testing.T, owner *gorm.DB, d budget.Dimension) []models.OTPBudgetModel {
	t.Helper()
	var rows []models.OTPBudgetModel
	if err := owner.Where("scope=? AND subject_hash=?", string(d.Scope), d.SubjectHash[:]).Order("window_start").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}
func assertBudgetCounts(t *testing.T, owner *gorm.DB, p budget.Plan, sends, checks int) {
	t.Helper()
	for _, d := range p.Dimensions() {
		rows := budgetRows(t, owner, d)
		if len(rows) != 1 || rows[0].SendCount != sends || rows[0].CheckCount != checks {
			t.Fatalf("unexpected %s capacity: rows=%d", d.Scope, len(rows))
		}
	}
}

func TestOTPBudgetPostgresConcurrentInstancesRespectGlobalLimit(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	authA := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	authB := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	uows := []shared.UnitOfWork{budgetTestUoW(t, authA), budgetTestUoW(t, authB)}
	repos := []*OTPBudgetRepository{NewOTPBudgetRepository(), NewOTPBudgetRepository()}
	subjects := budgetTestSubjects(t)
	rules := budgetTestRules(t, 5, 10, time.Minute)
	p := budgetTestPlan(t, rules, time.Date(2026, 10, 19, 0, 0, 20, 0, time.UTC), budget.Send, subjects)
	var accepted, limited atomic.Int32
	var wg sync.WaitGroup
	unexpected := make(chan error, 20)
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			err := reserveBudget(uows[index%2], repos[index%2], p)
			switch {
			case err == nil:
				accepted.Add(1)
			case errors.Is(err, budgetErrors.ErrBudgetExceeded):
				limited.Add(1)
			default:
				unexpected <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(unexpected)
	for err := range unexpected {
		t.Fatal(err)
	}
	if accepted.Load() != 5 || limited.Load() != 15 {
		t.Fatalf("capacity exceeded: accepted=%d limited=%d", accepted.Load(), limited.Load())
	}
	assertBudgetCounts(t, owner, p, 5, 0)
	// A third repository and newly constructed UoW have no process-local
	// counters to reset; the same persisted subjects stay exhausted.
	restarted := budgetTestUoW(t, budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN"))
	if err := reserveBudget(restarted, NewOTPBudgetRepository(), p); !errors.Is(err, budgetErrors.ErrBudgetExceeded) {
		t.Fatalf("restart reset capacity: %v", err)
	}
	assertBudgetCounts(t, owner, p, 5, 0)
}

func TestOTPBudgetPostgresSharesChannelsSeparatesChecksAndRollsBack(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	auth := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	uow := budgetTestUoW(t, auth)
	repo := NewOTPBudgetRepository()
	rules := budgetTestRules(t, 2, 2, time.Minute)
	subjects := budgetTestSubjects(t)
	now := time.Date(2026, 10, 19, 1, 0, 20, 0, time.UTC)
	send := budgetTestPlan(t, rules, now, budget.Send, subjects)
	for _, channel := range []string{"whatsapp", "sms"} {
		if err := reserveBudget(uow, repo, send); err != nil {
			t.Fatalf("%s explicit reservation failed: %v", channel, err)
		}
	}
	if err := reserveBudget(uow, repo, send); !errors.Is(err, budgetErrors.ErrBudgetExceeded) {
		t.Fatal("channel fallback gained new capacity")
	}
	check := budgetTestPlan(t, rules, now, budget.Check, subjects)
	for i := 0; i < 2; i++ {
		if err := reserveBudget(uow, repo, check); err != nil {
			t.Fatal(err)
		}
	}
	if err := reserveBudget(uow, repo, check); !errors.Is(err, budgetErrors.ErrBudgetExceeded) {
		t.Fatal(err)
	}
	assertBudgetCounts(t, owner, send, 2, 2)
	rollbackPlan := budgetTestPlan(t, rules, now, budget.Send, budgetTestSubjects(t))
	sentinel := errors.New("synthetic later attempt failure")
	err := uow.Command(budgetTestContext(), func(tx shared.Transaction) error {
		if err := repo.Reserve(tx, rollbackPlan); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	for _, d := range rollbackPlan.Dimensions() {
		if len(budgetRows(t, owner, d)) != 0 {
			t.Fatal("attempt rollback retained budget effects")
		}
	}
	if err := repo.Reserve(&shared.GormTransaction{Tx: auth}, send); !errors.Is(err, budgetErrors.ErrBudgetInput) {
		t.Fatal("repository accepted a nontransactional handle")
	}
}

func TestOTPBudgetPostgresRejectingOneDimensionDoesNotConsumeOthers(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	auth := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	uow := budgetTestUoW(t, auth)
	p := budgetTestPlan(t, budgetTestRules(t, 1, 2, time.Minute), time.Date(2026, 10, 19, 2, 0, 20, 0, time.UTC), budget.Send, budgetTestSubjects(t))
	for _, d := range p.Dimensions() {
		if d.Scope == budget.Origin {
			m := toBudgetModel(budget.Counter{Scope: d.Scope, SubjectHash: d.SubjectHash, WindowStart: d.WindowStart, SendCount: 1})
			if err := owner.Create(&m).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := reserveBudget(uow, NewOTPBudgetRepository(), p); !errors.Is(err, budgetErrors.ErrBudgetExceeded) {
		t.Fatal(err)
	}
	for _, d := range p.Dimensions() {
		rows := budgetRows(t, owner, d)
		if d.Scope == budget.Origin {
			if len(rows) != 1 || rows[0].SendCount != 1 {
				t.Fatal("rejecting dimension changed")
			}
		} else if len(rows) != 0 {
			t.Fatal("earlier empty dimension was not rolled back")
		}
	}
}

func TestOTPBudgetPostgresFixedWindowAndCrossWindowBlock(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	auth := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	uow := budgetTestUoW(t, auth)
	repo := NewOTPBudgetRepository()
	rules := budgetTestRules(t, 1, 2, time.Minute)
	subjects := budgetTestSubjects(t)
	boundary := time.Date(2026, 10, 19, 3, 1, 0, 0, time.UTC)
	before := budgetTestPlan(t, rules, boundary.Add(-time.Nanosecond), budget.Send, subjects)
	after := budgetTestPlan(t, rules, boundary, budget.Send, subjects)
	if err := reserveBudget(uow, repo, before); err != nil {
		t.Fatal(err)
	}
	until := boundary.Add(20 * time.Second)
	for _, d := range before.Dimensions() {
		if d.Scope == budget.Destination {
			if err := owner.Model(&models.OTPBudgetModel{}).Where("scope=? AND subject_hash=? AND window_start=?", string(d.Scope), d.SubjectHash[:], d.WindowStart).Update("blocked_until", until).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := reserveBudget(uow, repo, after); !errors.Is(err, budgetErrors.ErrBudgetBlocked) {
		t.Fatalf("new window bypassed old active block: %v", err)
	}
	exactEnd := budgetTestPlan(t, rules, until, budget.Send, subjects)
	if err := reserveBudget(uow, repo, exactEnd); err != nil {
		t.Fatal(err)
	}
	for _, d := range before.Dimensions() {
		rows := budgetRows(t, owner, d)
		if len(rows) != 2 || rows[0].SendCount != 1 || rows[1].SendCount != 1 {
			t.Fatal("window reset or block expiry lost counts")
		}
	}
}

func TestOTPBudgetPostgresDifferentWindowsSerializeBeforeReadingBlock(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	authA := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	authB := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	uowA, uowB := budgetTestUoW(t, authA), budgetTestUoW(t, authB)
	repo := NewOTPBudgetRepository()
	rules := budgetTestRules(t, 2, 2, time.Minute)
	subjects := budgetTestSubjects(t)
	boundary := time.Date(2026, 10, 19, 4, 1, 0, 0, time.UTC)
	oldPlan := budgetTestPlan(t, rules, boundary.Add(-time.Second), budget.Send, subjects)
	newPlan := budgetTestPlan(t, rules, boundary.Add(time.Second), budget.Send, subjects)
	ctx, cancel := context.WithTimeout(budgetTestContext(), 10*time.Second)
	defer cancel()
	held, err := uowA.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer uowA.Rollback(held)
	if err = repo.Reserve(held, oldPlan); err != nil {
		t.Fatal(err)
	}
	// Simulate a future policy decision setting a block in the same capacity
	// transaction. Every writer must share the repository's advisory protocol.
	d := oldPlan.Dimensions()[0]
	if err = held.(*shared.GormTransaction).Tx.Model(&models.OTPBudgetModel{}).Where("scope=? AND subject_hash=? AND window_start=?", string(d.Scope), d.SubjectHash[:], d.WindowStart).Update("blocked_until", boundary.Add(time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	// A different-window contender cannot slip past an uncommitted block.
	waitCtx, stop := context.WithTimeout(budgetTestContext(), 150*time.Millisecond)
	err = uowB.Command(waitCtx, func(tx shared.Transaction) error { return repo.Reserve(tx, newPlan) })
	waitErr := waitCtx.Err()
	stop()
	if err == nil || errors.Is(err, budgetErrors.ErrBudgetBlocked) || !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("contender did not wait for earlier transaction: %v", err)
	}
	if err = uowA.Commit(held); err != nil {
		t.Fatal(err)
	}
	if err = reserveBudget(uowB, repo, newPlan); !errors.Is(err, budgetErrors.ErrBudgetBlocked) {
		t.Fatalf("committed old-window block was bypassed: %v", err)
	}
	for _, dimension := range oldPlan.Dimensions() {
		rows := budgetRows(t, owner, dimension)
		if len(rows) != 1 || rows[0].SendCount != 1 {
			t.Fatal("contending window consumed capacity")
		}
	}
}

func TestOTPBudgetPostgresRejectsRetainedSnapshotsAndReadOnly(t *testing.T) {
	owner := budgetTestPool(t, "MANCUERNA_TEST_OWNER_DSN")
	auth := budgetTestPool(t, "MANCUERNA_TEST_AUTH_DSN")
	p := budgetTestPlan(t, budgetTestRules(t, 2, 2, time.Minute), time.Date(2026, 10, 19, 5, 0, 20, 0, time.UTC), budget.Send, budgetTestSubjects(t))
	for _, options := range []sql.TxOptions{
		{Isolation: sql.LevelRepeatableRead},
		{Isolation: sql.LevelSerializable},
		{Isolation: sql.LevelReadCommitted, ReadOnly: true},
	} {
		tx := auth.Begin(&options)
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		err := NewOTPBudgetRepository().Reserve(&shared.GormTransaction{Tx: tx}, p)
		rollback := tx.Rollback().Error
		if !errors.Is(err, budgetErrors.ErrBudgetInput) || rollback != nil {
			t.Fatalf("unsafe transaction mode accepted or left unusable: reserve=%v rollback=%v", err, rollback)
		}
	}
	for _, d := range p.Dimensions() {
		if len(budgetRows(t, owner, d)) != 0 {
			t.Fatal("rejected transaction changed capacity")
		}
	}
}
