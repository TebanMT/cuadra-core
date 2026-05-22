package reports_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cuadra/cuadra-core/src/application/reports"
)

// TestGenderReport_ComposesReaderResults — el use case debe pasar los
// resultados del Reader tal cual y aplicar el default de 30 días cuando
// el caller no especifica ventana.
func TestGenderReport_ComposesReaderResults(t *testing.T) {
	reader := &fakeReader{
		genderComposition: reports.GenderCompositionRow{
			Hombre: 7, Mujer: 11, NoEspecificado: 2, Total: 20,
		},
		genderByHour: []reports.AttendanceByGenderHourRow{
			{Hour: 6, Hombre: 3, Mujer: 1, NoEspecificado: 0},
			{Hour: 18, Hombre: 8, Mujer: 12, NoEspecificado: 1},
		},
	}
	uc := reports.NewGenderReport(reader, fakeUoW{})

	out, err := uc.Execute(context.Background(), reports.GenderReportInput{
		GymID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.DaysBack != 30 {
		t.Errorf("default DaysBack = %d, want 30", out.DaysBack)
	}
	if out.Composition.Total != 20 || out.Composition.Mujer != 11 {
		t.Errorf("composition = %+v, want pass-through", out.Composition)
	}
	if len(out.ByHour) != 2 || out.ByHour[1].Hour != 18 || out.ByHour[1].Mujer != 12 {
		t.Errorf("by_hour = %+v, want pass-through", out.ByHour)
	}
}

func TestGenderReport_CustomDaysBack(t *testing.T) {
	reader := &fakeReader{}
	uc := reports.NewGenderReport(reader, fakeUoW{})

	out, err := uc.Execute(context.Background(), reports.GenderReportInput{
		GymID:    uuid.New(),
		DaysBack: 90,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.DaysBack != 90 {
		t.Errorf("DaysBack = %d, want 90", out.DaysBack)
	}
}
