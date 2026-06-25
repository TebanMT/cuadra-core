package gym_test

import (
	"testing"
	"time"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
)

// ADR-009 §2.1: sólo un gym Plus que completó UC-037 envía desde su propio
// número. Standard, trial y Plus-sin-conectar caen al número maestro de Tinta.
// UsesOwnWhatsAppNumber es la decisión que usa el dispatcher para resolver el
// `from`.
func TestUsesOwnWhatsAppNumber_OnlyConnectedPlus(t *testing.T) {
	cases := []struct {
		name      string
		plan      string
		connected bool
		want      bool
	}{
		{"trial sin conectar", gymDomain.PlanTrial, false, false},
		{"standard sin conectar", gymDomain.PlanStandardMonthly, false, false},
		{"standard conectado (no aplica: sigue maestro)", gymDomain.PlanStandardMonthly, true, false},
		{"plus sin conectar (default maestro)", gymDomain.PlanPlusMonthly, false, false},
		{"plus mensual conectado", gymDomain.PlanPlusMonthly, true, true},
		{"plus anual conectado", gymDomain.PlanPlusAnnual, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := &gymDomain.Gym{SubscriptionPlan: c.plan}
			if c.connected {
				if err := g.ConnectWhatsApp("+524421112233", nil, time.Now().UTC()); err != nil {
					t.Fatalf("connect: %v", err)
				}
			}
			if got := g.UsesOwnWhatsAppNumber(); got != c.want {
				t.Errorf("UsesOwnWhatsAppNumber()=%v, want %v", got, c.want)
			}
		})
	}
}
