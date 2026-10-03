package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	repo "github.com/cuadra/cuadra-core/src/modules/checkins/domain/repository"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"net/http/httptest"
	"testing"
	"time"
)

type pagingReader struct {
	repo.CheckinRepository
	gym    uuid.UUID
	calls  int
	offset int
	limit  int
}

func (r *pagingReader) ListByGymBetweenPage(_ shared.Transaction, gym uuid.UUID, _ string, _, _ time.Time, limit, offset int) ([]repo.RecentCheckinRow, error) {
	r.gym = gym
	r.calls++
	r.offset = offset
	r.limit = limit
	rows := []repo.RecentCheckinRow{}
	for i := offset; i < 205 && len(rows) < limit; i++ {
		rows = append(rows, repo.RecentCheckinRow{ID: uuid.New(), Result: "allowed_active", CheckinAt: time.Now()})
	}
	return rows, nil
}

type pagingUoW struct{ shared.UnitOfWork }

func (pagingUoW) Query(context.Context) (shared.Transaction, error) { return nil, nil }

func TestHistoryPaginationResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gym := uuid.New()
	reader := &pagingReader{}
	controller := &CheckinController{Repo: reader, UoW: pagingUoW{}}
	for _, tc := range []struct {
		page, count int
		more        bool
	}{{1, 50, true}, {4, 50, true}, {5, 5, false}, {6, 0, false}} {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Set(middleware.GymIDKey, gym)
		ctx.Request = httptest.NewRequest("GET", fmt.Sprintf("/api/v1/checkins?from=2026-09-01&to=2026-09-28&page_size=50&page=%d", tc.page), nil)
		controller.handleListRecent(ctx)
		if response.Code != 200 {
			t.Fatalf("response: %d %s", response.Code, response.Body.String())
		}
		var payload struct {
			Data recentCheckinsResp `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Data.Items) != tc.count || payload.Data.HasMore == nil || *payload.Data.HasMore != tc.more {
			t.Fatalf("page %d: %s", tc.page, response.Body.String())
		}
		if reader.gym != gym || reader.limit != 51 || reader.offset != (tc.page-1)*50 {
			t.Fatalf("wrong page or tenant: %+v", reader)
		}
	}
}

func TestHistoryPaginationRejectsInvalidRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reader := &pagingReader{}
	controller := &CheckinController{Repo: reader, UoW: pagingUoW{}}
	for _, query := range []string{"page_size=500", "page_size=50&page=-1", "page_size=50&page=no", "page_size=0", "page_size=50&from=2026-02-30", "page_size=50&to=2026-08-01"} {
		response := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(response)
		ctx.Request = httptest.NewRequest("GET", "/api/v1/checkins?"+query+"&from=2026-09-01&to=2026-09-28", nil)
		controller.handleListRecent(ctx)
		if response.Code != 400 {
			t.Fatalf("query %s: %d", query, response.Code)
		}
	}
	if reader.calls != 0 {
		t.Fatal("invalid request queried database")
	}
}
