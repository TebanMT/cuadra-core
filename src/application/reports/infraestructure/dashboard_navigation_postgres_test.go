//go:build server && integration

package infraestructure_test

import (
	reportsInfra "github.com/cuadra/cuadra-core/src/application/reports/infraestructure"
	memRepo "github.com/cuadra/cuadra-core/src/modules/members/domain/repository"
	memInfra "github.com/cuadra/cuadra-core/src/modules/members/infraestructure/db/repositories"
	"github.com/google/uuid"
	"testing"
	"time"
)

func TestPostgresDashboardUnrenewedNavigationMatchesCount(t *testing.T) {
	f := setupTZFixture(t)
	today := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	plan := uuid.New()
	if err := f.db.Exec(`INSERT INTO membership_types(id,gym_id,version,created_at,updated_at,name,price,duration_days,active) VALUES(?,?,1,NOW(),NOW(),'Mensual',500,30,true)`, plan, f.gymID).Error; err != nil {
		t.Fatal(err)
	}
	seedMembership := func(member uuid.UUID, start, expiry, status string) {
		t.Helper()
		if err := f.db.Exec(`INSERT INTO memberships(id,gym_id,version,created_at,updated_at,member_id,membership_type_id,type_name_snapshot,price_snapshot,duration_days_snapshot,start_date,expiry_date,status) VALUES(?,?,1,NOW(),NOW(),?,?,'Mensual',500,30,?,?,?)`, uuid.New(), f.gymID, member, plan, start, expiry, status).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, expiry                      string
		lost, deleted, renewed, contacted bool
	}{
		{name: "Yesterday", expiry: "2026-09-27"}, {name: "Boundary", expiry: "2026-07-30"}, {name: "Contacted", expiry: "2026-09-20", contacted: true}, {name: "Too old", expiry: "2026-07-29"}, {name: "Today", expiry: "2026-09-28"}, {name: "Lost", expiry: "2026-09-27", lost: true}, {name: "Deleted", expiry: "2026-09-27", deleted: true}, {name: "Renewed", expiry: "2026-09-20", renewed: true},
	} {
		id := uuid.New()
		if err := f.db.Exec(`INSERT INTO members(id,gym_id,version,created_at,updated_at,folio,full_name,phone,created_by) VALUES(?,?,1,NOW(),NOW(),?,?,?,?)`, id, f.gymID, "SOC-"+id.String()[:8], tc.name, id.String()[:10], f.userID).Error; err != nil {
			t.Fatal(err)
		}
		seedMembership(id, "2026-06-01", tc.expiry, "replaced")
		if tc.lost {
			if err := f.db.Exec(`UPDATE members SET status='lost' WHERE id=?`, id).Error; err != nil {
				t.Fatal(err)
			}
		}
		if tc.deleted {
			if err := f.db.Exec(`UPDATE members SET deleted_at=NOW() WHERE id=?`, id).Error; err != nil {
				t.Fatal(err)
			}
		}
		if tc.contacted {
			if err := f.db.Exec(`UPDATE members SET last_contact_attempt_at=? WHERE id=?`, today, id).Error; err != nil {
				t.Fatal(err)
			}
		}
		if tc.renewed {
			seedMembership(id, "2026-09-21", "2026-10-21", "active")
		}
	}
	tx, err := f.uow.Query(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	count, err := reportsInfra.NewPostgresReader().CountExpiredRecoverable(tx, f.gymID, today, 60)
	if err != nil {
		t.Fatal(err)
	}
	repo := memInfra.NewMemberPostgresRepository()
	query := memRepo.ListQuery{GymID: f.gymID, Today: today, StatusFilter: "unrenewed", Page: 1, PageSize: 2, Sort: "name", SortAscending: true}
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
