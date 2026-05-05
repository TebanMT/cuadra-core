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
	GymName         string     `json:"gym_name,omitempty"`
	SetupCompleted  bool       `json:"setup_completed"`
	TrialEndsAt     *time.Time `json:"trial_ends_at,omitempty"`
	SubscriptionPln string     `json:"subscription_plan,omitempty"`
	UpdatedAtMs     int64      `json:"updated_at_ms"`
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

	// ClientID is the persistent UUID this sidecar advertises to the
	// cloud bootstrap (X-Cuadra-Client-ID). The sync agent sets it on
	// boot via EnsureClientID; the proxy reads the same value.
	ClientID uuid.UUID

	// DeviceLabel is what the cloud stores in sidecar_credentials.device_label
	// for support. Hostname-shaped string set by the caller.
	DeviceLabel string
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
		var data struct {
			Email string `json:"email"`
		}
		_ = json.Unmarshal(extractEnvelopeData(resp), &data)
		p.absorbAuthResponse(c.Request.Context(), data.Email, "", "", resp)
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
		return nil, 0, errors.New("CUADRA_CLOUD_URL not configured")
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

	// Persist sidecar_token if the cloud minted one.
	if tok, ok := d["sidecar_token"].(string); ok && tok != "" {
		_ = p.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
			return syncShared.SetSidecarToken(ctx, tx, tok)
		})
		if p.AgentReload != nil {
			p.AgentReload()
		}
	}

	// Cache the credential for offline login. We hash the *plaintext*
	// password locally — we don't have the cloud-side bcrypt hash. This
	// hash is local-only, never sent to cloud, and lives next to the
	// SQLite that already holds operational PII.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return
	}
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
	if gyms, ok := d["gyms"].([]any); ok && len(gyms) > 0 {
		if g0, ok := gyms[0].(map[string]any); ok {
			if v, ok := g0["name"].(string); ok {
				cached.GymName = v
			}
		}
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

	// Gym row first (users.gym_id has FK).
	if _, err := stx.Exec(ctx, `
		INSERT INTO gyms (id, gym_id, version, created_at, updated_at, name, synced_at)
		VALUES (?, ?, 1, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		cached.GymID, cached.GymID, now, now, nilIfEmpty(cached.GymName), now,
	); err != nil {
		return fmt.Errorf("mirror gym: %w", err)
	}

	// Operator user row. role is required + checked; default to 'owner'
	// when the cloud response omitted it (signup path always sends owner).
	role := cached.Role
	if role == "" {
		role = "owner"
	}
	if _, err := stx.Exec(ctx, `
		INSERT INTO users (id, gym_id, version, created_at, updated_at,
		                    email, password_hash, full_name, role, active, synced_at)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, 1, ?)
		ON CONFLICT(id) DO NOTHING`,
		cached.UserID, cached.GymID, now, now,
		cached.Email, string(passwordHash), nonEmpty(cached.FullName, cached.Email),
		role, now,
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

// silence unused import on usersApp when the file compiles standalone.
var _ = usersApp.BootstrapSidecarTokenInput{}
