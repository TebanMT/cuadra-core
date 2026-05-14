//go:build sidecar

package controllers

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	memApp "github.com/cuadra/cuadra-core/src/modules/members/app"
	fpDomain "github.com/cuadra/cuadra-core/src/modules/members/domain/fingerprint"
	"github.com/cuadra/cuadra-core/src/shared/biometric"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/utils"
)

// FingerprintSessionController is the sidecar-only async enrollment surface
// the desktop UI consumes. Flow:
//
//	POST /api/v1/members/:id/fingerprint/start  → {session_id, captures_total}
//	GET  /api/v1/members/:id/fingerprint/progress?session_id=…
//	      → {status, captures_done, captures_total, last_quality?, error?}
//
// On /start, the controller spawns a goroutine that drives bioReader.Capture
// blocking-style; when the SDK returns bytes, we run RegisterFingerprint and
// transition the session to "success". Failures (capture error, register
// error) flip the session to "failed" with a stable error code the FE maps
// to a Spanish hint.
//
// Sessions live in memory and self-expire after 5 minutes — enrollments are
// short, and losing a session on restart only forces the operator to retry.
type FingerprintSessionController struct {
	Reader   biometric.Reader
	Register *memApp.RegisterFingerprint
	Tokens   interface{} // not used directly; routes inherit AuthMiddleware

	mu       sync.Mutex
	sessions map[string]*fingerprintSession
}

type fingerprintSession struct {
	MemberID      uuid.UUID
	Status        string // "waiting" | "capturing" | "success" | "failed"
	CapturesDone  int
	CapturesTotal int
	LastQuality   *int
	ErrorCode     string
	// Collision payload — populated only when ErrorCode == "collision". Carries
	// the existing member's identity so the FE can offer "Ver perfil de X".
	CollisionMemberID   string
	CollisionMemberName string
	StartedAt           time.Time
}

func NewFingerprintSessionController(reader biometric.Reader, register *memApp.RegisterFingerprint) *FingerprintSessionController {
	return &FingerprintSessionController{
		Reader: reader, Register: register, sessions: map[string]*fingerprintSession{},
	}
}

func (c *FingerprintSessionController) RegisterRoutes(r *gin.Engine) {
	api := r.Group("/api/v1")
	// AuthMiddleware is mounted by the sibling FingerprintController (single
	// /fingerprint endpoint). The session routes piggy-back on the same auth.
	api.POST("/members/:id/fingerprint/start", c.handleStart)
	api.GET("/members/:id/fingerprint/progress", c.handleProgress)
}

func (c *FingerprintSessionController) handleStart(ctx *gin.Context) {
	gymID, _ := middleware.GetGymID(ctx)
	actor, _ := middleware.GetUserID(ctx)
	memberID, ok := parseUUIDParam(ctx, "id")
	if !ok {
		return
	}
	sessionID := uuid.NewString()
	sess := &fingerprintSession{
		MemberID:      memberID,
		Status:        "waiting",
		CapturesTotal: 1, // MVP: single-capture enrollment
		StartedAt:     time.Now().UTC(),
	}
	c.mu.Lock()
	c.sessions[sessionID] = sess
	// Garbage-collect expired sessions opportunistically (cheap walk).
	for id, s := range c.sessions {
		if time.Since(s.StartedAt) > 5*time.Minute {
			delete(c.sessions, id)
		}
	}
	c.mu.Unlock()
	go c.driveCapture(sessionID, gymID, actor, memberID)
	utils.JsonResponse(ctx, http.StatusCreated, gin.H{
		"session_id":     sessionID,
		"captures_total": sess.CapturesTotal,
	})
}

func (c *FingerprintSessionController) handleProgress(ctx *gin.Context) {
	id := ctx.Query("session_id")
	c.mu.Lock()
	sess, ok := c.sessions[id]
	c.mu.Unlock()
	if !ok {
		utils.JsonResponse(ctx, http.StatusOK, gin.H{
			"status": "failed", "captures_done": 0, "captures_total": 1,
			"error": "session_unknown",
		})
		return
	}
	resp := gin.H{
		"status":         sess.Status,
		"captures_done":  sess.CapturesDone,
		"captures_total": sess.CapturesTotal,
	}
	if sess.LastQuality != nil {
		resp["last_quality"] = *sess.LastQuality
	}
	if sess.ErrorCode != "" {
		resp["error"] = sess.ErrorCode
	}
	if sess.CollisionMemberID != "" {
		resp["existing_member_id"] = sess.CollisionMemberID
		resp["existing_member_name"] = sess.CollisionMemberName
	}
	utils.JsonResponse(ctx, http.StatusOK, resp)
}

// driveCapture is the goroutine spawned by /start. It blocks on the SDK's
// Capture() until the operator presses a finger, then runs RegisterFingerprint.
// Any failure flips the session to "failed" with a code the FE maps to a hint.
func (c *FingerprintSessionController) driveCapture(sessionID string, gymID, actor, memberID uuid.UUID) {
	c.update(sessionID, func(s *fingerprintSession) { s.Status = "capturing" })

	if c.Reader == nil {
		c.update(sessionID, func(s *fingerprintSession) {
			s.Status, s.ErrorCode = "failed", "biometric_unavailable"
		})
		return
	}
	if !c.Reader.Available(context.Background()) {
		c.update(sessionID, func(s *fingerprintSession) {
			s.Status, s.ErrorCode = "failed", "reader_unavailable"
		})
		return
	}
	cap, err := c.Reader.Capture(context.Background())
	if err != nil || cap == nil || len(cap.Bytes) == 0 {
		c.update(sessionID, func(s *fingerprintSession) {
			s.Status, s.ErrorCode = "failed", "capture_failed"
		})
		return
	}
	q := cap.QualityScore
	c.update(sessionID, func(s *fingerprintSession) {
		s.LastQuality = &q
	})
	if c.Register == nil {
		c.update(sessionID, func(s *fingerprintSession) {
			s.Status, s.ErrorCode = "failed", "register_unavailable"
		})
		return
	}
	if _, err := c.Register.Execute(context.Background(), memApp.RegisterFingerprintInput{
		GymID:           gymID,
		ActorUserID:     actor,
		MemberID:        memberID,
		Capture:         cap,
		ConsentAccepted: true,
	}); err != nil {
		if errors.Is(err, fpDomain.ErrFingerprintCollision) {
			id, name := collisionData(err)
			c.update(sessionID, func(s *fingerprintSession) {
				s.Status, s.ErrorCode = "failed", "collision"
				s.CollisionMemberID, s.CollisionMemberName = id, name
			})
			return
		}
		c.update(sessionID, func(s *fingerprintSession) {
			s.Status, s.ErrorCode = "failed", classifyRegisterErr(err)
		})
		return
	}
	c.update(sessionID, func(s *fingerprintSession) {
		s.Status = "success"
		s.CapturesDone = s.CapturesTotal
	})
}

func (c *FingerprintSessionController) update(sessionID string, mut func(*fingerprintSession)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[sessionID]
	if !ok {
		return
	}
	mut(s)
}

// collisionData pulls the existing-member payload out of a CustomError wrapping
// ErrFingerprintCollision. Returns empty strings when the error lacks data —
// the FE can still surface a generic "huella duplicada" hint.
func collisionData(err error) (id, name string) {
	var ce sharedDomain.CustomError
	if !errors.As(err, &ce) {
		return "", ""
	}
	if v, ok := ce.Data["existing_member_id"].(string); ok {
		id = v
	}
	if v, ok := ce.Data["existing_member_name"].(string); ok {
		name = v
	}
	return id, name
}

// classifyRegisterErr maps the use case error to the codes the FE recognises
// (READER_ERROR_CODES / CAPTURE_ERROR_CODES). Keeping it narrow — anything
// unrecognised flows through as "capture_failed".
func classifyRegisterErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch msg {
	case "reader_unavailable", "biometric_unavailable":
		return "reader_unavailable"
	case "low_quality", "quality_too_low":
		return "low_quality"
	}
	return "capture_failed"
}
