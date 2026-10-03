package interfaces

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	reportsApp "github.com/cuadra/cuadra-core/src/application/reports"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
)

func TestRangeHandlerRejectsInvalidPeriodWindows(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctrl := &ReportsController{Range: reportsApp.NewRangeReport(nil, nil)}
	for _, rawURL := range []string{
		"/api/v1/reports?period=quarter",
		"/api/v1/reports?period=custom&from=2026-08-01",
		"/api/v1/reports?period=custom&from=2026-08-25&to=2026-08-24",
		"/api/v1/reports?period=custom&from=not-a-date&to=2026-08-24",
	} {
		t.Run(rawURL, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, rawURL, nil)
			c.Set(middleware.GymIDKey, uuid.New())
			ctrl.handleRange(c)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
			}
		})
	}
}

func TestPeriodExportHandlerUsesSameFailClosedWindowRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rangeUC := reportsApp.NewRangeReport(nil, nil)
	ctrl := &ReportsController{Export: &reportsApp.ExportReport{Range: rangeUC}}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet,
		"/api/v1/reports/period_summary/export?format=xlsx&period=custom&from=2026-08-25&to=2026-08-24", nil)
	c.Params = gin.Params{{Key: "type", Value: reportsApp.ReportTypePeriodSummary}}
	c.Set(middleware.GymIDKey, uuid.New())
	ctrl.handleExport(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
	}
}
