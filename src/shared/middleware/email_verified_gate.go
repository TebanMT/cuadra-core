package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// EmailVerifiedGateResponse — body del 403 cuando una ruta exige correo
// verificado. El FE lo detecta para mostrar la pantalla "Confirma tu correo"
// en lugar del genérico "no tienes permisos". Mismo shape que PlanGateResponse
// (error code + mensaje legible) para que el manejo en api.ts sea consistente.
type EmailVerifiedGateResponse struct {
	Error   string `json:"error"` // siempre "email_not_verified"
	Message string `json:"message"`
}

// RequireEmailVerified corre DESPUÉS de AuthMiddleware (lee user_id de las
// claims) y aborta con 403 si el dueño todavía no ha confirmado su correo.
// Pensado para el wizard de setup — el dueño NO puede meter datos del gym
// hasta probar que es dueño del correo.
//
// Fail-open en infra (repo/UoW nil, query falla): preferimos un riesgo de
// "pasó sin verificar por bug del lookup" a "wizard entero bloqueado por
// DB caída". El riesgo es bajo porque este gate sólo aplica al wizard
// (3 endpoints), no a rutas calientes.
//
// Fail-closed cuando el lookup sí responde y el user no tiene
// EmailVerifiedAt: ese es el caso que queremos cazar.
func RequireEmailVerified(users userRepo.UserRepository, uow sharedDomain.UnitOfWork) gin.HandlerFunc {
	if users == nil || uow == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		userID, ok := GetUserID(c)
		if !ok {
			// Sin claims AuthMiddleware ya habrá devuelto 401; no es nuestra
			// decisión.
			c.Next()
			return
		}
		tx, err := uow.Query(c.Request.Context())
		if err != nil {
			c.Next()
			return
		}
		u, err := users.GetByID(tx, userID)
		if err != nil || u == nil {
			c.Next()
			return
		}
		if u.IsEmailVerified() {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusForbidden, EmailVerifiedGateResponse{
			Error:   "email_not_verified",
			Message: "Confirma tu correo electrónico antes de continuar con la configuración.",
		})
	}
}
