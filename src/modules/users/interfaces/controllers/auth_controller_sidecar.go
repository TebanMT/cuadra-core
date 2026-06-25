//go:build sidecar

package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	usersApp "github.com/cuadra/cuadra-core/src/modules/users/app"
	"github.com/cuadra/cuadra-core/src/shared/auth"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	syncShared "github.com/cuadra/cuadra-core/src/shared/sync"
)

// Sidecar-only sync_state key (ADR-008 §3.4) for the offline-login
// credential cache. The sidecar_token key lives in the sync package
// because the agent reads it on every tick.
const keyCachedLogin = "cached_login"

// cachedLoginRow is the JSON payload stored under sync_state[keyCachedLogin]
// after a successful online login. It carries enough to mint a local JWT
// and rebuild the loginResp shape from cache when the cloud is offline.
type cachedLoginRow struct {
	Email           string     `json:"email"`
	PasswordHash    string     `json:"password_hash"`
	UserID          string     `json:"user_id"`
	GymID           string     `json:"gym_id"`
	Role            string     `json:"role"`
	FullName        string     `json:"full_name,omitempty"`
	Phone           string     `json:"phone,omitempty"`
	GymName         string     `json:"gym_name,omitempty"`
	SetupCompleted  bool       `json:"setup_completed"`
	TrialEndsAt     *time.Time `json:"trial_ends_at,omitempty"`
	SubscriptionPln string     `json:"subscription_plan,omitempty"`
	UpdatedAtMs     int64      `json:"updated_at_ms"`
	// PinHash bcrypt del PIN del user. Viene del cloud (loginResp /
	// redeemInstallerResp) y se mirrorea al sqlite local para que el
	// flujo login-pin offline funcione sin esperar el primer sync pull.
	PinHash string `json:"pin_hash,omitempty"`
}

// SidecarAuthProxy reimplements /api/v1/auth/* on the sidecar build as a
// thin forwarder to the cloud, with an offline-login fallback against a
// bcrypt cache populated on the most recent online login (ADR-008 §3.4).
//
// The proxy intentionally does NOT touch domain repositories — the
// cloud's /auth response is the source of truth for identity. The local
// `users` and `gyms` SQLite tables get refreshed by the sync agent's pull
// cycle, not by this proxy.
type SidecarAuthProxy struct {
	CloudURL    string
	HTTPClient  *http.Client
	UoW         sharedDomain.UnitOfWork
	LocalTokens auth.TokenService

	// AgentReload (optional) fires after the proxy persists a new
	// sidecar_token so the long-running agent can re-read its credential
	// without waiting for the next tick.
	AgentReload func()

	// OnSidecarTokenChanged (optional) hands the freshly-minted plaintext
	// sidecar_token directly to the agent so its in-memory cache hot-swaps.
	// Necessary when a mid-day re-login mints a new credential because the
	// previous one was revoked: persisting to sync_state alone is not
	// enough — the agent only re-reads on bootstrap. Without this hook the
	// agent keeps presenting the old (rejected-by-cloud) token until the
	// sidecar restarts.
	OnSidecarTokenChanged func(token string)

	// OnActiveGymChanged (optional) tells process-wide stateful subsystems
	// which gym the operator just signed in to. Today only the biometric
	// matcher consumes it (NBIS matcher needs the per-gym GMK to decrypt
	// the gallery in Identify). Fires on every successful login (cloud or
	// offline, password or PIN, redeem-installer) and on refresh; uuid.Nil
	// means "no operator signed in" so subsystems can clear state.
	OnActiveGymChanged func(gymID uuid.UUID)

	// ClientID is the persistent UUID this sidecar advertises to the
	// cloud bootstrap (X-Cuadra-Client-ID). The sync agent sets it on
	// boot via EnsureClientID; the proxy reads the same value.
	ClientID uuid.UUID

	// DeviceLabel is what the cloud stores in sidecar_credentials.device_label
	// for support. Hostname-shaped string set by the caller.
	DeviceLabel string

	// LoginPIN backs POST /api/v1/auth/login-pin. Offline by construction:
	// validates against local SQLite users.pin_hash. Optional — when nil,
	// the route 501s (older builds of the sidecar before the auth refactor).
	LoginPIN *usersApp.LoginPIN

	// ListOperators backs GET /api/v1/auth/operators. Reads local SQLite.
	// Optional — when nil the route 501s.
	ListOperators *usersApp.ListOperatorsForLogin

	// LoginGymID is the gym this sidecar is paired to. The operators-list
	// endpoint scopes by this id so the public route can't be coerced to
	// dump arbitrary gyms. Set at boot from the cached_login row (mirrored
	// from cloud on installer redemption); zero until first online login.
	LoginGymID func() uuid.UUID
}

func NewSidecarAuthProxy(cfg SidecarAuthProxy) *SidecarAuthProxy {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	c := cfg
	return &c
}

func (p *SidecarAuthProxy) RegisterRoutes(r *gin.Engine) {
	g := r.Group("/api/v1/auth")
	g.POST("/signup", p.handleSignup)
	g.POST("/login", p.handleLogin)
	g.POST("/refresh", p.handleRefresh)
	g.POST("/logout", p.handleLogout)
	g.POST("/forgot-password", p.handleForgotPassword)
	g.POST("/reset-password", p.handleResetPassword)
	// Verify-password is offline-first: we compare the operator's password
	// against cached_login.password_hash so the override-checkin flow keeps
	// working without internet (the only consumer today). Sits under the same
	// /auth/* group; AuthMiddleware is applied per-handler when available
	// (cmd/sidecar/main.go uses LocalTokens for verification of the JWT subj).
	g.POST("/verify-password", p.handleVerifyPassword)
	// Installer bootstrap: forwards the one-time code to cloud, stashes the
	// returned sidecar_token + cached login locally, and resigns the JWTs
	// with the sidecar's secret so subsequent local requests validate.
	g.POST("/redeem-installer", p.handleRedeemInstaller)
	// Pairing status: tells the desktop whether this machine already has
	// a cached_login row, i.e. has been paired with a gym (regardless of
	// whether the operator currently has a session). The desktop uses
	// this on cold-start to decide between Welcome (fresh laptop) and
	// Login (paired but logged out). No auth required — the only thing
	// it leaks is the email/gym_name of an already-paired account, which
	// the operator would see on the login screen anyway.
	g.GET("/pairing-status", p.handlePairingStatus)
	// PIN-based auth surface. Both routes need to live with the rest of
	// /auth/* on the sidecar because:
	//   - login-pin is the primary credential at the reception kiosk and
	//     must work offline (validates against local SQLite, no cloud
	//     round-trip).
	//   - operators is the unauthenticated read the login grid renders to
	//     decide which avatars to show; also offline.
	g.POST("/login-pin", p.handleLoginPIN)
	g.GET("/operators", p.handleListOperators)
	// Refresh de identidad — el dueño aprieta "Volver a leer datos del
	// gym" desde Configuración. Va al cloud (saltándose el GET /auth/me
	// local que lee SQLite), re-mirrorea los datos en local, y devuelve
	// la respuesta fresca. Útil cuando el mirror inicial quedó con datos
	// viejos (bug histórico de absorb, o sync que aún no completa).
	g.POST("/me/refresh", p.handleRefreshIdentity)
}

// ── Handlers ─────────────────────────────────────────────────────────────

func (p *SidecarAuthProxy) handleSignup(c *gin.Context) {
	body, err := c.GetRawData()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	resp, status, err := p.forward(c.Request.Context(), "POST", "/api/v1/auth/signup", body, p.bootstrapHeaders())
	if err != nil {
		// Cloud unreachable — signup REQUIRES internet (ADR-008 §3.4).
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": "auth.signup.errors.offline",
			"hint":  "Necesitas internet para crear una cuenta nueva.",
		})
		return
	}
	if status == http.StatusOK || status == http.StatusCreated {
		// Stash sidecar_token + cache the credential. Email/password come
		// from the original request body, response carries the rest.
		var reqBody struct {
			Email    string `json:"email"`
			Password string `json:"password"`
			FullName string `json:"full_name"`
		}
		_ = json.Unmarshal(body, &reqBody)
		p.absorbAuthResponse(c.Request.Context(), reqBody.Email, reqBody.Password, reqBody.FullName, resp)
	}
	p.relayAndResign(c, status, resp)
}

func (p *SidecarAuthProxy) handleLogin(c *gin.Context) {
	body, err := c.GetRawData()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	var reqBody struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(body, &reqBody); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	resp, status, err := p.forward(c.Request.Context(), "POST", "/api/v1/auth/login", body, p.bootstrapHeaders())
	if err == nil {
		// Cloud answered. Cache only on success — a 4xx from cloud means
		// the operator typed the wrong password and we shouldn't trust
		// the local cache for it either.
		if status == http.StatusOK {
			p.absorbAuthResponse(c.Request.Context(), reqBody.Email, reqBody.Password, "", resp)
		}
		p.relayAndResign(c, status, resp)
		return
	}

	// Cloud unreachable — try offline.
	cached, ok := p.loadCachedLogin(c.Request.Context())
	if !ok || !strings.EqualFold(cached.Email, reqBody.Email) {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": "auth.login.errors.offline_no_cache",
			"hint":  "Sin internet y esta cuenta no tiene credenciales guardadas localmente.",
		})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(cached.PasswordHash), []byte(reqBody.Password)); err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "auth.login.errors.invalid_credentials"})
		return
	}
	// Re-mirror the local gym/user rows from cached_login on every
	// offline login. The mirror upserts with COALESCE on
	// setup_completed_at — fills nulls without overwriting a real
	// timestamp. Costs ~2 SQL upserts per offline login but pays back
	// in two cases:
	//   - First login after fixing this code path on a desktop where
	//     the gym row was inserted with NULL setup_completed_at.
	//   - User completed setup on web after the desktop's last sync;
	//     cached_login eventually catches up and the next login pulls
	//     the bool into the local row.
	_ = p.UoW.Command(c.Request.Context(), func(tx sharedDomain.Transaction) error {
		return mirrorCloudIdentity(c.Request.Context(), tx, cached, []byte(cached.PasswordHash))
	})

	userID, _ := uuid.Parse(cached.UserID)
	gymID, _ := uuid.Parse(cached.GymID)
	access, err := p.LocalTokens.GenerateAccessToken(userID, gymID, cached.Role)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	refresh, err := p.LocalTokens.GenerateRefreshToken(userID, gymID, cached.Role)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if p.OnActiveGymChanged != nil {
		p.OnActiveGymChanged(gymID)
	}
	c.JSON(http.StatusOK, gin.H{
		"status_code": http.StatusOK,
		"data": gin.H{
			"user_id":           cached.UserID,
			"gym_id":            cached.GymID,
			"role":              cached.Role,
			"access_token":      access,
			"refresh_token":     refresh,
			"setup_completed":   cached.SetupCompleted,
			"trial_ends_at":     cached.TrialEndsAt,
			"subscription_plan": cached.SubscriptionPln,
			"gyms":              []gin.H{{"id": cached.GymID, "name": cached.GymName}},
			// sidecar_token deliberately omitted — offline path can't
			// rotate it; the existing one stays valid.
		},
	})
}

// handleRefresh mints a fresh local JWT pair from cached_login. Cuadra
// desktop is a kiosko — checkins must keep working all day, so this
// endpoint is intentionally generous: as long as the operator has logged
// in once on this laptop (cached_login present), refresh always succeeds.
//
// Cloud is NOT consulted: today the cloud has no /auth/refresh handler,
// and even when it does we still want the sidecar to be the trust anchor
// for desktop sessions. The sidecar's local JWT_SECRET signs these
// tokens; the same middleware verifies them on the next request, so the
// loop closes without a network round-trip.
//
// Failure modes:
//   - cached_login missing (fresh laptop, never logged in): 401 — the
//     desktop has no cached identity to mint from. Operator must run
//     /auth/login at least once with internet.
//   - JWT signing failure (local crypto error, vanishingly rare): 500.
func (p *SidecarAuthProxy) handleRefresh(c *gin.Context) {
	cached, ok := p.loadCachedLogin(c.Request.Context())
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "auth.refresh.errors.no_cache",
			"hint":  "Necesitas iniciar sesión otra vez con internet.",
		})
		return
	}
	userID, err := uuid.Parse(cached.UserID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "cached user_id invalid"})
		return
	}
	gymID, err := uuid.Parse(cached.GymID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "cached gym_id invalid"})
		return
	}
	role := cached.Role
	if role == "" {
		role = "owner"
	}
	access, err := p.LocalTokens.GenerateAccessToken(userID, gymID, role)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	refresh, err := p.LocalTokens.GenerateRefreshToken(userID, gymID, role)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if p.OnActiveGymChanged != nil {
		p.OnActiveGymChanged(gymID)
	}
	c.JSON(http.StatusOK, gin.H{
		"status_code": http.StatusOK,
		"data": gin.H{
			"access_token":  access,
			"refresh_token": refresh,
		},
	})
}

func (p *SidecarAuthProxy) handleLogout(c *gin.Context) {
	body, _ := c.GetRawData()
	resp, status, err := p.forward(c.Request.Context(), "POST", "/api/v1/auth/logout", body, nil)
	if err != nil {
		// Best-effort: cloud unreachable still counts as a successful
		// local logout. The user's JWT lives in the desktop's storage,
		// not here, and the sync agent keeps running on its long-lived
		// sidecar_token (ADR-008 §3.4 logout column).
		c.Status(http.StatusNoContent)
		return
	}
	relayResponse(c, status, resp)
}

// handleRefreshIdentity — POST /api/v1/auth/me/refresh (sidecar).
// El dueño aprieta "Volver a leer datos del gym" desde Configuración
// porque ve datos viejos en el desktop (ej. nombre del gym vacío
// porque el mirror inicial tuvo un bug, o el primer pull del sync
// agent todavía no completa). Vamos al cloud con el sk_live_* del
// sidecar — los JWTs del operador son sidecar-firmados, así que el
// cloud no los acepta. El sk_live_* identifica al "bootstrap user"
// del pareo, que en fase 1 = el dueño (único role con acceso a esta
// UI). El handler trae el shape estándar de /me, re-mirrorea local,
// y devuelve la respuesta fresca.
func (p *SidecarAuthProxy) handleRefreshIdentity(c *gin.Context) {
	// Cargamos el sk_live_* del sync_state. Si no existe, el sidecar
	// nunca terminó su pareo — bounce con 412 para que la UI mande al
	// dueño al wizard de instalación.
	var sidecarToken string
	if tx, err := p.UoW.Query(c.Request.Context()); err == nil {
		if st, err := syncShared.ReadState(c.Request.Context(), tx); err == nil {
			sidecarToken = st.SidecarToken
		}
	}
	if sidecarToken == "" {
		c.AbortWithStatusJSON(http.StatusPreconditionFailed, gin.H{
			"error": "auth.refresh_identity.errors.not_paired",
			"hint":  "Esta laptop no está vinculada con la nube — re-pareo necesario.",
		})
		return
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+sidecarToken)
	resp, status, err := p.forward(c.Request.Context(), "GET", "/api/v1/auth/me", nil, hdr)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": "auth.refresh_identity.errors.offline",
			"hint":  "Necesitas internet para volver a leer los datos del gym.",
		})
		return
	}
	if status != http.StatusOK {
		relayResponse(c, status, resp)
		return
	}

	// Parseamos el shape de /auth/me (meResponse) — user + gym anidados.
	var env struct {
		Data struct {
			User struct {
				UserID   string  `json:"user_id"`
				FullName string  `json:"full_name"`
				Email    string  `json:"email"`
				Phone    *string `json:"phone"`
				Role     string  `json:"role"`
			} `json:"user"`
			Gym struct {
				GymID            string     `json:"gym_id"`
				Name             string     `json:"name"`
				SetupCompleted   bool       `json:"setup_completed"`
				TrialEndsAt      *time.Time `json:"trial_ends_at"`
				SubscriptionPlan string     `json:"subscription_plan"`
			} `json:"gym"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "cloud response unparseable"})
		return
	}

	// Cargamos el cached_login para preservar el password_hash (cloud
	// no lo devuelve; queda intacto para que el verify-password offline
	// siga funcionando).
	prior, ok := p.loadCachedLogin(c.Request.Context())
	if !ok {
		// Edge case: sidecar tiene sk_live_* pero no cached_login. Raro
		// (signup-direct sin redeem-installer) — armamos un cached row
		// nuevo sin password_hash. El mirror lo deja como string vacío;
		// el offline-login no va a poder validar contraseña, pero el
		// resto sigue.
		prior = cachedLoginRow{}
	}
	updated := prior
	updated.UserID = env.Data.User.UserID
	updated.GymID = env.Data.Gym.GymID
	updated.Email = env.Data.User.Email
	updated.FullName = env.Data.User.FullName
	if env.Data.User.Phone != nil {
		updated.Phone = *env.Data.User.Phone
	} else {
		updated.Phone = ""
	}
	updated.Role = env.Data.User.Role
	updated.GymName = env.Data.Gym.Name
	updated.SetupCompleted = env.Data.Gym.SetupCompleted
	updated.TrialEndsAt = env.Data.Gym.TrialEndsAt
	updated.SubscriptionPln = env.Data.Gym.SubscriptionPlan
	updated.UpdatedAtMs = time.Now().UTC().UnixMilli()

	// Persist + re-mirror.
	payload, err := json.Marshal(updated)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "marshal cached"})
		return
	}
	_ = p.UoW.Command(c.Request.Context(), func(tx sharedDomain.Transaction) error {
		if err := setCachedLogin(c.Request.Context(), tx, string(payload)); err != nil {
			return err
		}
		return mirrorCloudIdentity(c.Request.Context(), tx, updated, []byte(updated.PasswordHash))
	})

	// Devolvemos la respuesta del cloud tal cual — el FE espera la
	// misma shape que GET /auth/me. El front actualiza su auth store
	// con esos datos.
	relayResponse(c, status, resp)
}

func (p *SidecarAuthProxy) handleRedeemInstaller(c *gin.Context) {
	body, err := c.GetRawData()
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	resp, status, err := p.forward(c.Request.Context(), "POST", "/api/v1/auth/redeem-installer", body, p.bootstrapHeaders())
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": "auth.installer.errors.offline",
			"hint":  "Necesitas internet para canjear el código de instalación.",
		})
		return
	}
	if status == http.StatusOK {
		// The redeem response carries the same shape as a login (user/gym
		// identity + sidecar_token), so we can absorb + cache the same way.
		// Email/password aren't part of the redeem flow — the operator will
		// have to log in once with credentials to seed the offline cache.
		// Email + full_name vienen en el envelope.data; los desempacamos
		// como hint, pero absorbAuthResponse igual relee del data map por
		// si el shape evoluciona.
		var data struct {
			Email    string `json:"email"`
			FullName string `json:"full_name"`
		}
		_ = json.Unmarshal(extractEnvelopeData(resp), &data)
		p.absorbAuthResponse(c.Request.Context(), data.Email, "", data.FullName, resp)
	}
	p.relayAndResign(c, status, resp)
}

// extractEnvelopeData pulls the `data` object out of a {status_code,data,...}
// response so absorbAuthResponse / relayAndResign can read individual
// fields by name without re-implementing the parser.
func extractEnvelopeData(resp []byte) []byte {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		return resp
	}
	return env.Data
}

// handleForgotPassword forwards to cloud. The recovery channel selection
// (email vs. WhatsApp once fase 2 lights it up) happens cloud-side via
// the recovery.Registry — see src/shared/recovery and pending ADR-014.
// The sidecar doesn't need a local registry because:
//   - Recovery requires internet by definition (the cloud is authoritative
//     for users and is the only place that can mint the reset token /
//     enqueue WhatsApp messages via Twilio).
//   - Routing the selection through cloud means the same user gets the
//     same channel regardless of which desktop they hit forgot-password
//     from. Sidecar-local channel selection would risk drift.
//
// Wiring WhatsApp in fase 2 means swapping the cloud-side
// EmailOnlyRegistry for TieredRegistry — no sidecar change needed.
func (p *SidecarAuthProxy) handleForgotPassword(c *gin.Context) {
	body, _ := c.GetRawData()
	resp, status, err := p.forward(c.Request.Context(), "POST", "/api/v1/auth/forgot-password", body, nil)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": "auth.forgot_password.errors.offline",
			"hint":  "Necesitas internet para recuperar tu contraseña.",
		})
		return
	}
	relayResponse(c, status, resp)
}

// handleVerifyPassword compares the supplied password against the offline
// bcrypt cache so the override-checkin flow keeps working without internet.
// Returns 401 when no cache exists (operator must log in once with internet).
func (p *SidecarAuthProxy) handleVerifyPassword(c *gin.Context) {
	cached, ok := p.loadCachedLogin(c.Request.Context())
	if !ok {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "auth.verify.errors.no_cache",
		})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Password == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(cached.PasswordHash), []byte(req.Password)); err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "auth.verify.errors.invalid_credentials",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status_code": http.StatusOK, "data": gin.H{"ok": true}})
}

func (p *SidecarAuthProxy) handleResetPassword(c *gin.Context) {
	body, _ := c.GetRawData()
	resp, status, err := p.forward(c.Request.Context(), "POST", "/api/v1/auth/reset-password", body, nil)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": "auth.reset_password.errors.offline",
			"hint":  "Necesitas internet para restablecer tu contraseña.",
		})
		return
	}
	relayResponse(c, status, resp)
}

// ── Helpers ──────────────────────────────────────────────────────────────

func (p *SidecarAuthProxy) bootstrapHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Cuadra-Client", "sidecar")
	if p.ClientID != uuid.Nil {
		h.Set("X-Cuadra-Client-ID", p.ClientID.String())
	}
	if p.DeviceLabel != "" {
		h.Set("X-Cuadra-Device-Label", p.DeviceLabel)
	}
	return h
}

// forward proxies a single request to the cloud and returns (body, status,
// transportErr). When transportErr is non-nil the cloud was unreachable
// (network failure / timeout) and the caller decides whether to fall back
// to cache or surface a 503. When transportErr is nil but status is non-2xx
// the cloud answered authoritatively (e.g. 401 invalid credentials) and
// the caller relays it verbatim.
func (p *SidecarAuthProxy) forward(ctx context.Context, method, path string, body []byte, extraHeaders http.Header) ([]byte, int, error) {
	if p.CloudURL == "" {
		return nil, 0, errors.New("TINTA_CLOUD_URL not configured")
	}
	url := strings.TrimRight(p.CloudURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return out, resp.StatusCode, nil
}

func relayResponse(c *gin.Context, status int, body []byte) {
	contentType := "application/json"
	c.Data(status, contentType, body)
}

// relayAndResign parses the cloud's auth response envelope, swaps the
// cloud-signed JWTs for ones signed by the sidecar's JWT_SECRET, and
// writes the patched body. Decouples the sidecar's auth gate from the
// cloud's signing key — they no longer have to share the same secret.
// On any parsing/signing failure, falls back to the original payload (the
// desktop just ends up with a token its sidecar can't validate; the next
// /auth/me call will fail and trigger a re-login).
func (p *SidecarAuthProxy) relayAndResign(c *gin.Context, status int, body []byte) {
	if status >= 300 || p.LocalTokens == nil {
		relayResponse(c, status, body)
		return
	}
	var env struct {
		StatusCode int            `json:"status_code"`
		Message    string         `json:"message"`
		Data       map[string]any `json:"data"`
		Exception  *string        `json:"exception,omitempty"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Data == nil {
		relayResponse(c, status, body)
		return
	}
	userIDStr, _ := env.Data["user_id"].(string)
	gymIDStr, _ := env.Data["gym_id"].(string)
	userID, errA := uuid.Parse(userIDStr)
	gymID, errB := uuid.Parse(gymIDStr)
	if errA != nil || errB != nil {
		relayResponse(c, status, body)
		return
	}
	role, _ := env.Data["role"].(string)
	if role == "" {
		// Signup response doesn't carry role explicitly; owner is the only
		// role minted by signup so default safely.
		role = "owner"
	}
	access, err := p.LocalTokens.GenerateAccessToken(userID, gymID, role)
	if err != nil {
		relayResponse(c, status, body)
		return
	}
	refresh, err := p.LocalTokens.GenerateRefreshToken(userID, gymID, role)
	if err != nil {
		relayResponse(c, status, body)
		return
	}
	env.Data["access_token"] = access
	env.Data["refresh_token"] = refresh
	out, err := json.Marshal(env)
	if err != nil {
		relayResponse(c, status, body)
		return
	}
	c.Data(status, "application/json", out)
}

// absorbAuthResponse extracts sidecar_token + identity fields from a
// successful cloud auth response and stores them locally. Always best-
// effort — failures don't fail the operator's login.
func (p *SidecarAuthProxy) absorbAuthResponse(ctx context.Context, email, password, fullName string, resp []byte) {
	// The cloud's response envelope is {status_code, data: {…}}; we look
	// for fields under data.
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(resp, &env); err != nil || env.Data == nil {
		return
	}
	d := env.Data

	// Persist sidecar_token if the cloud minted one. Two hooks fire in
	// order: (1) hand the token to the agent's in-memory cache so the very
	// next sync attempt uses it (no need to wait for a restart), (2) nudge
	// the agent loop to run immediately.
	if tok, ok := d["sidecar_token"].(string); ok && tok != "" {
		_ = p.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
			return syncShared.SetSidecarToken(ctx, tx, tok)
		})
		if p.OnSidecarTokenChanged != nil {
			p.OnSidecarTokenChanged(tok)
		}
		if p.AgentReload != nil {
			p.AgentReload()
		}
	}

	// Cache the credential for offline login. We hash the *plaintext*
	// password locally — we don't have the cloud-side bcrypt hash. This
	// hash is local-only, never sent to cloud, and lives next to the
	// SQLite that already holds operational PII.
	//
	// Usamos auth.HashPassword (mismo bcryptCost global que el cloud) en vez
	// de bcrypt.DefaultCost (10): así la credencial offline no queda más débil
	// que la del cloud, y si alguna vez se sincronizara (vía mirror/enqueue) no
	// introduce un downgrade de costo. bcrypt es cost-agnóstico al verificar,
	// así que los hashes cost=10 ya cacheados siguen funcionando.
	hashStr, err := auth.HashPassword(password)
	if err != nil {
		return
	}
	hash := []byte(hashStr)
	cached := cachedLoginRow{
		Email:        email,
		PasswordHash: string(hash),
		FullName:     fullName,
		UpdatedAtMs:  time.Now().UTC().UnixMilli(),
	}
	if v, ok := d["user_id"].(string); ok {
		cached.UserID = v
	}
	if v, ok := d["gym_id"].(string); ok {
		cached.GymID = v
	}
	if v, ok := d["role"].(string); ok {
		cached.Role = v
	}
	if v, ok := d["setup_completed"].(bool); ok {
		cached.SetupCompleted = v
	}
	if v, ok := d["subscription_plan"].(string); ok {
		cached.SubscriptionPln = v
	}
	if v, ok := d["trial_ends_at"].(string); ok && v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			cached.TrialEndsAt = &t
		}
	}
	// Cloud responses (loginResp / redeemInstallerResp) put `full_name`,
	// `phone`, y `gym_name` en el top-level del envelope.data. Antes
	// buscábamos gym_name dentro de un `gyms[]` array que no existe en la
	// shape actual → la fila local quedaba con name="" y full_name=email
	// (fallback). Leemos del top-level y caemos al arg de la función sólo
	// si la respuesta omitió el campo.
	if v, ok := d["full_name"].(string); ok && v != "" {
		cached.FullName = v
	}
	if v, ok := d["phone"].(string); ok {
		cached.Phone = v
	}
	if v, ok := d["pin_hash"].(string); ok && v != "" {
		cached.PinHash = v
	}
	if v, ok := d["gym_name"].(string); ok {
		cached.GymName = v
	}
	payload, err := json.Marshal(cached)
	if err != nil {
		return
	}
	_ = p.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		return setCachedLogin(ctx, tx, string(payload))
	})

	// Mirror the cloud-side identity rows into local SQLite so the desktop
	// can immediately operate on them (gym wizard, /users/me lookups, …)
	// without waiting for the next sync pull. Bypasses sync_queue on
	// purpose — these rows came FROM the cloud, pushing them back would
	// echo. The sync agent's pull will refresh them as the gym is edited
	// elsewhere.
	if cached.GymID != "" && cached.UserID != "" {
		_ = p.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
			return mirrorCloudIdentity(ctx, tx, cached, hash)
		})
	}

	if p.OnActiveGymChanged != nil {
		if gymID, err := uuid.Parse(cached.GymID); err == nil {
			p.OnActiveGymChanged(gymID)
		}
	}
}

// mirrorCloudIdentity upserts the gym + user rows into local SQLite from
// the cloud's auth response. Idempotent: if the rows already exist (sync
// pulled them earlier), the INSERT is a no-op so no version drift.
func mirrorCloudIdentity(ctx context.Context, tx sharedDomain.Transaction, cached cachedLoginRow, passwordHash []byte) error {
	stx, ok := tx.(*sharedDomain.SqlxTransaction)
	if !ok {
		return fmt.Errorf("expected sqlx transaction")
	}
	now := time.Now().UTC().UnixMilli()

	// setup_completed_at: cached_login carries a boolean from the cloud
	// (`setup_completed: bool`). The local schema stores a timestamp —
	// nil means "not setup", any value means "setup". We don't have the
	// real completion timestamp here, so we use `now` as a placeholder
	// when the cloud says setup is complete. The next sync pull replaces
	// this with the canonical timestamp from cloud, but the placeholder
	// is enough to make `IsSetupComplete()` return true immediately so
	// the operator doesn't get bounced to /auth/setup-required after
	// logging in to a gym that's already configured.
	var setupCompletedAt any
	if cached.SetupCompleted {
		setupCompletedAt = now
	}

	// Gym row first (users.gym_id has FK).
	//
	// ON CONFLICT: previous version was DO NOTHING, but that left
	// setup_completed_at = NULL forever on installs where the user
	// redeemed BEFORE finishing the wizard on web — even after they
	// later completed setup, the desktop kept seeing
	// IsSetupComplete()=false on every login until the sync agent
	// pulled an updated row.
	//
	// Now: COALESCE preserves whatever the local row already has and
	// only fills in nulls from the cloud-derived data. We never
	// overwrite a non-null setup_completed_at — sync is the only path
	// that can roll back setup status, on purpose.
	// trial_ends_at: el cloud lo manda como time.Time; SQLite lo guarda
	// como INTEGER UnixMilli. Convertimos sólo si está presente.
	var trialEndsAtMs any
	if cached.TrialEndsAt != nil {
		trialEndsAtMs = cached.TrialEndsAt.UnixMilli()
	}
	// subscription_plan tiene un CHECK constraint que rechaza ''. Si no
	// vino del cloud, dejamos el default del esquema (trial).
	var subPlan any
	if cached.SubscriptionPln != "" {
		subPlan = cached.SubscriptionPln
	} else {
		subPlan = "trial"
	}
	if _, err := stx.Exec(ctx, `
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at,
		                   name, setup_completed_at, subscription_plan,
		                   trial_ends_at, synced_at)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = COALESCE(gyms.name, excluded.name),
			setup_completed_at = COALESCE(gyms.setup_completed_at, excluded.setup_completed_at),
			subscription_plan = excluded.subscription_plan,
			trial_ends_at = COALESCE(excluded.trial_ends_at, gyms.trial_ends_at),
			synced_at = excluded.synced_at`,
		cached.GymID, cached.GymID, now, now, nilIfEmpty(cached.GymName),
		setupCompletedAt, subPlan, trialEndsAtMs, now,
	); err != nil {
		return fmt.Errorf("mirror gym: %w", err)
	}

	// Operator user row. role is required + checked; default to 'owner'
	// when the cloud response omitted it (signup path always sends owner).
	role := cached.Role
	if role == "" {
		role = "owner"
	}
	// pin_hash + pin_assigned_at: el cloud manda pin_hash en el response;
	// si está set, lo mirroreamos también para que el dueño pueda hacer
	// login-pin offline desde el primer arranque del desktop. Si no viene,
	// el row queda con pin_hash=NULL y el sync agent eventualmente lo
	// rellena al pull del row del cloud.
	var pinAssignedAt any
	if cached.PinHash != "" {
		pinAssignedAt = now
	}
	if _, err := stx.Exec(ctx, `
		INSERT INTO users (id, gym_id, version, created_at, updated_at,
		                    email, password_hash, full_name, phone, role, active,
		                    pin_hash, pin_assigned_at, synced_at)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			full_name = excluded.full_name,
			phone = COALESCE(excluded.phone, users.phone),
			pin_hash = COALESCE(excluded.pin_hash, users.pin_hash),
			pin_assigned_at = COALESCE(excluded.pin_assigned_at, users.pin_assigned_at),
			synced_at = excluded.synced_at`,
		cached.UserID, cached.GymID, now, now,
		cached.Email, string(passwordHash), nonEmpty(cached.FullName, cached.Email),
		nilIfEmpty(cached.Phone), role,
		nilIfEmpty(cached.PinHash), pinAssignedAt, now,
	); err != nil {
		return fmt.Errorf("mirror user: %w", err)
	}
	return nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nonEmpty(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}

// handlePairingStatus returns whether this sidecar has a cached_login row,
// which is the canonical "is this laptop paired with a gym?" signal. Used
// by the desktop's Welcome screen to skip the install-code flow when the
// laptop is already paired but the operator just logged out.
//
// Returns (always 200):
//
//	{ paired: bool, email?: string, gym_name?: string }
//
// Email + gym_name are included so the login screen can show a friendly
// "Continúa como X en Y" hint without an extra round-trip.
func (p *SidecarAuthProxy) handlePairingStatus(c *gin.Context) {
	cached, ok := p.loadCachedLogin(c.Request.Context())
	if !ok {
		c.JSON(http.StatusOK, gin.H{
			"status_code": http.StatusOK,
			"data":        gin.H{"paired": false},
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status_code": http.StatusOK,
		"data": gin.H{
			"paired":   true,
			"email":    cached.Email,
			"gym_name": cached.GymName,
		},
	})
}

func (p *SidecarAuthProxy) loadCachedLogin(ctx context.Context) (cachedLoginRow, bool) {
	var raw string
	err := p.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		stx, ok := tx.(*sharedDomain.SqlxTransaction)
		if !ok {
			return fmt.Errorf("expected sqlx transaction")
		}
		return stx.Get(ctx, &raw, `SELECT value FROM sync_state WHERE key = ?`, keyCachedLogin)
	})
	if err != nil || raw == "" {
		return cachedLoginRow{}, false
	}
	var out cachedLoginRow
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return cachedLoginRow{}, false
	}
	return out, true
}

// setCachedLogin persists the cached_login JSON via sqlx. Equivalent of
// agent_state.SetKV but inlined so the proxy doesn't need to leak the
// `cached_login` key into the sync package.
func setCachedLogin(ctx context.Context, tx sharedDomain.Transaction, value string) error {
	stx, ok := tx.(*sharedDomain.SqlxTransaction)
	if !ok {
		return fmt.Errorf("expected sqlx transaction")
	}
	_, err := stx.Exec(ctx, `
		INSERT INTO sync_state(key, value, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		keyCachedLogin, value, time.Now().UTC().UnixMilli(),
	)
	return err
}

// handleLoginPIN — POST /api/v1/auth/login-pin (no auth).
// Body: { user_id, pin }. Validates against local SQLite users.pin_hash and
// returns the same envelope shape as /auth/login so the desktop's
// post-login flow (setSession, redirects to setup-required / dashboard)
// reuses the existing code path.
//
// Online behavior matches offline behavior — the sidecar does not consult
// the cloud here. Cloud is the source of truth for the PIN itself, but
// since the projector sync keeps the local users row fresh, "online" PIN
// login would just be a slower path to the same answer.
func (p *SidecarAuthProxy) handleLoginPIN(c *gin.Context) {
	if p.LoginPIN == nil {
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
			"error": "auth.login_pin.errors.not_configured",
		})
		return
	}
	var req struct {
		UserID string `json:"user_id"`
		PIN    string `json:"pin"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	userID, err := uuid.Parse(req.UserID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid user_id"})
		return
	}
	out, err := p.LoginPIN.Execute(c.Request.Context(), usersApp.LoginPINInput{
		UserID: userID,
		PIN:    req.PIN,
	})
	if err != nil {
		// Genericise: PIN-set checks become "invalid PIN" so we never tell
		// a kiosk-level attacker whether a given user has a PIN configured.
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "auth.login_pin.errors.invalid_credentials",
		})
		return
	}
	if p.OnActiveGymChanged != nil {
		p.OnActiveGymChanged(out.GymID)
	}
	c.JSON(http.StatusOK, gin.H{
		"status_code": http.StatusOK,
		"data": gin.H{
			"user_id":           out.UserID,
			"gym_id":            out.GymID,
			"role":              out.Role,
			"full_name":         out.FullName,
			"email":             out.Email,
			"gym_name":          out.GymName,
			"access_token":      out.AccessToken,
			"refresh_token":     out.RefreshToken,
			"setup_completed":   out.SetupCompleted,
			"trial_ends_at":     out.TrialEndsAt,
			"subscription_plan": out.SubscriptionPlan,
		},
	})
}

// handleListOperators — GET /api/v1/auth/operators (no auth).
// Renders the avatars-and-PIN grid on the desktop's /auth/login. Scope is
// the gym this sidecar is paired to (LoginGymID), so the public surface
// can't be coerced into dumping other tenants.
func (p *SidecarAuthProxy) handleListOperators(c *gin.Context) {
	if p.ListOperators == nil || p.LoginGymID == nil {
		c.AbortWithStatusJSON(http.StatusNotImplemented, gin.H{
			"error": "auth.operators.errors.not_configured",
		})
		return
	}
	gymID := p.LoginGymID()
	if gymID == uuid.Nil {
		// No paired gym yet (fresh laptop, redeem not done) — answer with
		// an empty list so the FE can show its "no operadores" state
		// without errorring out.
		c.JSON(http.StatusOK, gin.H{
			"status_code": http.StatusOK,
			"data":        gin.H{"operators": []any{}},
		})
		return
	}
	ops, err := p.ListOperators.Execute(c.Request.Context(), gymID)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status_code": http.StatusOK,
		"data":        gin.H{"operators": ops},
	})
}

// silence unused import on usersApp when the file compiles standalone.
var _ = usersApp.BootstrapSidecarTokenInput{}
