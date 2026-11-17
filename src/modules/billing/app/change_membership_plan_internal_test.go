package app

// Tests del cálculo de prorrateo del UC-048:
// proporcional = round(delta × días_restantes / días_ciclo) a peso entero,
// con clamps a [0, delta] para fechas degeneradas.

import "testing"

func TestPlanChangeProration(t *testing.T) {
	cases := []struct {
		name          string
		delta         float64
		daysRemaining int
		cycleDays     int
		want          float64
	}{
		// Ejemplo de la spec: Gym $450 → Gym+Clases $700, quedan 15 de 30.
		{"ejemplo spec 15/30", 250, 15, 30, 125},
		// Redondeo a peso entero: 250 × 14/30 = 116.67 → 117.
		{"redondeo hacia arriba", 250, 14, 30, 117},
		// 250 × 13/30 = 108.33 → 108.
		{"redondeo hacia abajo", 250, 13, 30, 108},
		// Mitad exacta: 105 × 1/2 = 52.5 → 53 (round half away from zero).
		{"mitad exacta", 105, 1, 2, 53},
		// Vence hoy: días_restantes = 0 → nada que prorratear.
		{"vence hoy", 250, 0, 30, 0},
		// Cambio el mismo día del inicio: restante == ciclo → delta completa,
		// SIN redondear el delta (la diferencia completa es exacta).
		{"dia de inicio con centavos", 250.50, 30, 30, 250.50},
		// Lateral: delta 0 → $0 sugerido.
		{"lateral", 0, 15, 30, 0},
		// Centavos a mitad de ciclo: 250.50 × 15/30 = 125.25 → 125.
		{"centavos prorrateados", 250.50, 15, 30, 125},
		// Ciclo degenerado de 0 días (start == expiry por ajuste manual):
		// se sugiere la diferencia completa, nunca división entre cero.
		{"ciclo cero", 250, 1, 0, 250},
		// Restante > ciclo (membresía actual con inicio futuro por ajuste):
		// clamp a delta — la sugerencia nunca supera la diferencia completa.
		{"restante mayor al ciclo", 250, 45, 30, 250},
		// Delta negativo defensivo (el dominio ya rechazó el downgrade).
		{"delta negativo", -100, 15, 30, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := planChangeProration(c.delta, c.daysRemaining, c.cycleDays)
			if got != c.want {
				t.Errorf("planChangeProration(%v, %d, %d) = %v, want %v",
					c.delta, c.daysRemaining, c.cycleDays, got, c.want)
			}
		})
	}
}
