package middleware

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	gymDomain "github.com/cuadra/cuadra-core/src/modules/gyms/domain/gym"
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

// trialReadOnlyWrites son las únicas escrituras de una prueba vencida
// (IsTrialReadOnly, UC-002): registrar la entrada de un socio por búsqueda o
// por número. La huella se registra dentro del sidecar y no pasa por aquí.
// "Permitir de todos modos" (/checkins/override) queda fuera a propósito:
// dejaría entrar a socios vencidos sin cobrarles.
var trialReadOnlyWrites = map[string]bool{
	"/api/v1/checkins/manual": true,
	"/api/v1/checkins/number": true,
}

func trialReadOnlyAllows(method, path string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	case http.MethodPost:
		return trialReadOnlyWrites[path]
	}
	return false
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

// SubscriptionInactiveResponse es el body del 402. Error vale
// "subscription_inactive" (bloqueo total) o "subscription_read_only" (prueba
// vencida: el FE pasa a solo lectura sin sacar al operador de la pantalla). El FE lo lee para
// detectar "ah, me bloquearon mid-sesión" y forzar navegación a la pantalla
// de bloqueo (caso típico: el sync trajo el flip a cancelled justo después
// de que el operador hizo un cobro — el cobro falla con 402, FE redirecciona).
type SubscriptionInactiveResponse struct {
	Error              string     `json:"error"` // subscription_inactive | subscription_read_only
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
		g := gymFromBearer(c, tokens, gyms, uow)
		if g == nil {
			c.Next()
			return
		}
		now := time.Now().UTC()
		if g.IsTrialReadOnly(now) {
			if trialReadOnlyAllows(c.Request.Method, c.Request.URL.Path) {
				c.Next()
				return
			}
			abortTrialReadOnly(c, g)
			return
		}
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

// EnforceTrialReadOnly (cloud) aplica en la API del cloud la misma solo
// lectura de una prueba terminada (Gym.IsTrialReadOnly, UC-002) que el
// sidecar aplica en recepción. Sin esto el dueño seguía cobrando, dando de
// alta y editando desde el dashboard con la prueba vencida.
//
// No aplica el bloqueo total de un plan de pago cancelado: el dashboard es
// justo donde el dueño paga para salir de él.
//
// Va a nivel engine. Las lecturas pasan sin tocar la base; solo una
// escritura con JWT de usuario cuesta el lookup del gym.
func EnforceTrialReadOnly(tokens auth.TokenService, gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork) gin.HandlerFunc {
	if tokens == nil || gyms == nil || uow == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		method, path := c.Request.Method, c.Request.URL.Path
		if trialReadOnlyAllows(method, path) || cloudTrialReadOnlyBypass.allows(method, path) || cloudTrialReadOnlyWrites[method+" "+c.FullPath()] {
			c.Next()
			return
		}
		g := gymFromBearer(c, tokens, gyms, uow)
		if g == nil || !g.IsTrialReadOnly(time.Now().UTC()) {
			c.Next()
			return
		}
		abortTrialReadOnly(c, g)
	}
}

// cloudTrialReadOnlyBypass son las rutas del cloud que una prueba en solo
// lectura siempre puede usar, con cualquier método.
var cloudTrialReadOnlyBypass = subscriptionGateBypass{
	prefixes: []string{
		"/api/v1/auth/",          // sesión, perfil propio, PIN, instalar una recepción
		"/api/v1/sync/",          // las recepciones suben las entradas que registran
		"/api/v1/subscriptions/", // activar el plan: checkout y portal de pago
		"/api/v1/webhooks/",      // Stripe, Mercado Pago y Twilio
		"/api/v1/admin/",         // releases, con su propio secreto
		"/api/mancuerna/",        // otro producto con su propia auth
	},
}

// cloudTrialReadOnlyWrites son escrituras que la prueba terminada no frena,
// por ruta de Gin: ver fotos (presign-get es una lectura por POST) y las
// acciones de seguridad sobre operadores. Un dueño nunca debe quedarse sin
// poder quitarle el acceso a alguien por no haber pagado.
var cloudTrialReadOnlyWrites = map[string]bool{
	"POST /api/v1/uploads/presign-get":      true,
	"PATCH /api/v1/users/:id/active":        true,
	"POST /api/v1/users/:id/reset-password": true,
	"POST /api/v1/users/:id/rotate-pin":     true,
}

// gymFromBearer resuelve el gym del JWT de usuario. Devuelve nil (el gate
// deja pasar) si no hay token válido —el AuthMiddleware del controller
// responde 401— o si el lookup falla: fail-open a propósito, un bug del
// repo no debe dejar a un gym sin poder operar. El lookup corre con el
// tenant del token para que RLS lo permita en el cloud.
func gymFromBearer(c *gin.Context, tokens auth.TokenService, gyms gymRepo.GymRepository, uow sharedDomain.UnitOfWork) *gymDomain.Gym {
	header := c.GetHeader("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return nil
	}
	claims, err := tokens.ValidateAccessToken(strings.TrimPrefix(header, "Bearer "))
	if err != nil {
		return nil
	}
	tx, err := uow.Query(sharedDomain.WithTenant(c.Request.Context(), claims.GymID))
	defer sharedDomain.CloseQuery(tx)
	if err != nil {
		return nil
	}
	g, err := gyms.GetByID(tx, claims.GymID)
	if err != nil {
		return nil
	}
	return g
}

func abortTrialReadOnly(c *gin.Context, g *gymDomain.Gym) {
	c.AbortWithStatusJSON(http.StatusPaymentRequired, SubscriptionInactiveResponse{
		Error:              "subscription_read_only",
		Message:            "Tu prueba terminó. Puedes consultar y registrar entradas; para cobrar o hacer cambios, activa tu plan.",
		SubscriptionStatus: g.SubscriptionStatus,
		SubscriptionEndsAt: g.SubscriptionEndsAt,
	})
}
