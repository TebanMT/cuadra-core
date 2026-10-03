//go:build sidecar

package infraestructure_test

import (
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	memInfra "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/repositories"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestSQLiteDashboardUnrenewedNavigationMatchesCount(t *testing.T) {
	f := setupReaderDB(t)
	today := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, expiry                      string
		lost, deleted, renewed, contacted bool
	}{
		{name: "Yesterday", expiry: "2026-09-27"},
		{name: "Boundary", expiry: "2026-07-30"},
		{name: "Contacted", expiry: "2026-09-20", contacted: true},
		{name: "Too old", expiry: "2026-07-29"},
		{name: "Today", expiry: "2026-09-28"},
		{name: "Lost", expiry: "2026-09-27", lost: true},
		{name: "Deleted", expiry: "2026-09-27", deleted: true},
		{name: "Renewed", expiry: "2026-09-20", renewed: true},
	} {
		id := f.seedMember(t, tc.name)
		f.seedMembership(t, id, "2026-06-01", tc.expiry, "replaced")
		if tc.lost {
			f.db.MustExec(`UPDATE members SET status='lost' WHERE id=?`, id)
		}
		if tc.deleted {
			f.db.MustExec(`UPDATE members SET deleted_at=? WHERE id=?`, today.UnixMilli(), id)
		}
		if tc.contacted {
			f.db.MustExec(`UPDATE members SET last_contact_attempt_at=? WHERE id=?`, today.UnixMilli(), id)
		}
		if tc.renewed {
			f.seedMembership(t, id, "2026-09-21", "2026-10-21", "active")
		}
	}
	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	count, err := reportsInfra.NewSQLiteReader().CountExpiredRecoverable(tx, f.gymID, today, 60)
	if err != nil {
		t.Fatal(err)
	}
	repo := memInfra.NewMemberSQLiteRepository()
	query := memRepo.ListQuery{GymID: f.gymID, Today: today, StatusFilter: "unrenewed", PageSize: 2, Page: 1, Sort: "name", SortAscending: true}
	first, total, err := repo.List(tx, query)
	if err != nil {
		t.Fatal(err)
	}
	query.Page = 2
	second, total2, err := repo.List(tx, query)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || total != count || total2 != count || len(first) != 2 || len(second) != 1 {
		t.Fatalf("count=%d total=%d/%d pages=%d/%d", count, total, total2, len(first), len(second))
	}
	if first[0].Member.FullName != "Boundary" || first[1].Member.FullName != "Contacted" || second[0].Member.FullName != "Yesterday" {
		t.Fatal("wrong members in linked list")
	}
	query.GymID = uuid.New()
	query.Page = 1
	_, total, err = repo.List(tx, query)
	if err != nil || total != 0 {
		t.Fatalf("tenant filter total=%d err=%v", total, err)
	}
}
