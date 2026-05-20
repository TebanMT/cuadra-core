// Package audit centralises the audit_log INSERT used by every mutating use
// case (CLAUDE.md "Audit log" rule). The recorder is implemented in two flavors
// (Postgres GORM, SQLite sqlx); use cases depend on the Recorder interface and
// receive the right impl from main.go.
package audit

import (
	"context"
	"time"

	"github.com/google/uuid"

	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
)

// Action enumerates the well-known action keys. Use cases can pass arbitrary
// strings — these are just the canonical ones for Sesión 1.
const (
	ActionCreate             = "create"
	ActionUpdate             = "update"
	ActionDelete             = "delete"
	ActionLogin              = "login"
	ActionLogout             = "logout"
	ActionPasswordReset      = "password_reset"
	ActionPasswordResetReq   = "password_reset_requested"
	ActionTransferOwnership  = "transfer_ownership"
	ActionToggleActive       = "toggle_active"
	ActionAdminPasswordReset = "admin_password_reset"
	ActionLoginPIN           = "login_pin"
	ActionAssignPIN          = "assign_pin"
	ActionClearPIN           = "clear_pin"
	ActionRotateOperatorPIN  = "rotate_operator_pin"
	ActionEmailVerifyReq     = "email_verify_requested"
	ActionEmailVerified      = "email_verified"
)

// Entry is the value the use case hands to Recorder.Record. Changes is an
// arbitrary JSON-marshalable payload (typically `{"before":..., "after":...}`).
type Entry struct {
	GymID       uuid.UUID
	EntityType  string
	EntityID    uuid.UUID
	Action      string
	ActorUserID *uuid.UUID
	Changes     any
	IPAddress   string
	UserAgent   string
	At          time.Time
}

// Recorder writes a single audit_log row inside the caller's transaction. The
// transaction MUST be the same UoW.Command tx that owns the primary mutation —
// otherwise the audit row could land without the change (or vice versa).
type Recorder interface {
	Record(ctx context.Context, tx sharedDomain.Transaction, entry Entry) error
}

// FromContext extracts request-scoped audit metadata (ip, user-agent) that
// middleware stashed earlier. Returns zero values if not present — fine for
// background/CLI flows.
type ctxKey int

const (
	ipKey ctxKey = iota + 1
	uaKey
)

func WithIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ipKey, ip)
}

func WithUA(ctx context.Context, ua string) context.Context {
	return context.WithValue(ctx, uaKey, ua)
}

func IPFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ipKey).(string); ok {
		return v
	}
	return ""
}

func UAFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(uaKey).(string); ok {
		return v
	}
	return ""
}
