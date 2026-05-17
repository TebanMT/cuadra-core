package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	gymRepo "github.com/cuadra/cuadra-core/src/modules/gyms/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// SubscriptionGate (sidecar-only) bloquea TODAS las acciones de un gym cuyo
// estado local indica suscripción cancelada + grace period vencida. La
// decisión de producto (riesgo 1 de la sesión de ajustes): mientras el
// sidecar no tiene internet sigue operando como siempre; cuando un sync
// exitoso baja `subscription_status='cancelled'` desde el cloud, el
// siguiente request entra acá y el dueño ve la pantalla de bloqueo.
//
// El middleware corre ANTES de cualquier AuthMiddleware de los controllers.
// Parsea el JWT manualmente (mismo TokenService que usan los controllers)
// para resolver gym_id sin depender del orden de middleware. Si el JWT
// falta/inválido cae a c.Next() y el AuthMiddleware del controller responde
// 401 — separación de concerns: este gate sólo decide "¿está bloqueado por
// no-pago?", no "¿está autenticado?".
//
// Paths bypass:
//
//   - /health                       — liveness probe
//   - /api/v1/auth/*                — login/logout/refresh/me — necesarios incluso bloqueado
//   - /api/v1/gyms/me (GET)         — la pantalla de bloqueo muestra nombre del gym
//   - /api/v1/subscriptions/me      — la pantalla de bloqueo muestra el estado
//   - /api/v1/sync/*                — introspección del agente de sync (status, manual trigger)
//   - /api/v1/uploads/local/*       — fotos cacheadas (las renderea la pantalla)
//
// Todo lo demás (members, payments, checkins, products, expenses, etc.)
// recibe 402 con un JSON estructurado que el FE detecta para forzar la
// pantalla de bloqueo.
type subscriptionGateBypass struct {
	exactGET map[string]bool
	prefixes []string
}

var sidecarSubscriptionBypass = subscriptionGateBypass{
	exactGET: map[string]bool{
		"/health":                  true,
		"/api/v1/gyms/me":          true,
		"/api/v1/subscriptions/me": true,
	},
	prefixes: []string{
		"/api/v1/auth/",
		"/api/v1/sync/",
		"/api/v1/uploads/local/",
	},
}

func (b subscriptionGateBypass) allows(method, path string) bool {
	if method == http.MethodGet && b.exactGET[path] {
		return true
	}
	for _, p := range b.prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// SubscriptionInactiveResponse es el body del 402. El FE lo lee para
// detectar "ah, me bloquearon mid-sesión" y forzar navegación a la pantalla
// de bloqueo (caso típico: el sync trajo el flip a cancelled justo después
// de que el operador hizo un cobro — el cobro falla con 402, FE redirecciona).
type SubscriptionInactiveResponse struct {
	Error              string     `json:"error"` // siempre "subscription_inactive"
	Message            string     `json:"message"`
	SubscriptionStatus string     `json:"subscription_status"`
	SubscriptionEndsAt *time.Time `json:"subscription_ends_at,omitempty"`
}

// EnforceActiveSubscription construye el middleware. Si el repo o UoW son
// nil el middleware se desactiva (no-op) — útil para tests que no quieren
// montar el dominio completo.
func EnforceActiveSubscription(tokens auth.TokenService, gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork) gin.HandlerFunc {
	if tokens == nil || gyms == nil || uow == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		if sidecarSubscriptionBypass.allows(c.Request.Method, c.Request.URL.Path) {
			c.Next()
			return
		}
		header := c.GetHeader("Authorization")
		if header == "" || !strings.HasPrefix(header, "Bearer ") {
			// Sin token llega al controller, que devolverá 401 si la ruta
			// lo requiere. No es nuestra responsabilidad.
			c.Next()
			return
		}
		claims, err := tokens.ValidateAccessToken(strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			c.Next()
			return
		}

		// Lookup local — sub-ms en SQLite local del sidecar. Si el read
		// falla (gym no existe localmente, DB error transitorio) elegimos
		// fail-open: dejamos pasar el request. Razón: el bloqueo es estricto
		// por diseño, así que un bug en el repo bloquearía al gym entero
		// hasta que alguien debug-ee. Mejor un riesgo de "operó 1 hora más
		// de lo debido" que "no pudo cobrar nada por un bug del lookup".
		tx, err := uow.Query(c.Request.Context())
		if err != nil {
			c.Next()
			return
		}
		g, err := gyms.GetByID(tx, claims.GymID)
		if err != nil || g == nil {
			c.Next()
			return
		}
		now := time.Now().UTC()
		if !g.IsAccessHardBlocked(now) {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusPaymentRequired, SubscriptionInactiveResponse{
			Error:              "subscription_inactive",
			Message:            "Tu suscripción está vencida. Activa tu plan para seguir usando Tinta.",
			SubscriptionStatus: g.SubscriptionStatus,
			SubscriptionEndsAt: g.SubscriptionEndsAt,
		})
	}
}
