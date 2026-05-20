package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	userErrors "github.com/cuadra/cuadra-core/src/modules/users/domain/errors"
	userRepo "github.com/cuadra/cuadra-core/src/modules/users/domain/repository"
	"github.com/cuadra/cuadra-core/src/shared/audit"
	sharedDomain "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/email"
)

// VerifyTokenTTL = 24 hours. Email verification is a less hot-path than
// password reset (which uses 1h), so we give the owner a full day to
// click the link before it expires.
const VerifyTokenTTL = 24 * time.Hour

// RequestEmailVerification is UC-013 step 1. Mints a one-time token for
// the authenticated user, persists its hash, and sends the verification
// link by email. Idempotent semantics: if the user is already verified
// it returns ErrEmailAlreadyVerified so the caller can hide the banner
// / disable the "resend" button.
//
// The dashboard wires this from POST /auth/resend-verification. Signup
// (UC-001) fires it once automatically — errors there are swallowed so a
// flaky outbound email never blocks account creation.
type RequestEmailVerification struct {
	Users   userRepo.UserRepository
	Tokens  userRepo.EmailVerificationRepository
	UoW     sharedDomain.UnitOfWork
	Sender  email.Sender
	Audit   audit.Recorder
	NowFunc func() time.Time
	// BaseURL is the dashboard origin (DASHBOARD_BASE_URL). The link in
	// the email is built as BaseURL + /auth/verify-email?token=...
	BaseURL string
	// From is the transactional sender address (e.g. noreply@entinta.mx).
	// Used only when the Sender impl ignores its own configured From — the
	// Resend sender uses its constructor value and ignores this. Kept as a
	// constructor knob so the canonical From lives in main.go.
	From string
}

func NewRequestEmailVerification(
	users userRepo.UserRepository,
	tokens userRepo.EmailVerificationRepository,
	uow sharedDomain.UnitOfWork,
	sender email.Sender,
	recorder audit.Recorder,
	baseURL string,
) *RequestEmailVerification {
	return &RequestEmailVerification{
		Users: users, Tokens: tokens, UoW: uow,
		Sender: sender, Audit: recorder, BaseURL: baseURL,
		NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

func (uc *RequestEmailVerification) Execute(ctx context.Context, userID uuid.UUID) error {
	now := uc.NowFunc()
	plain, err := generateVerifyToken()
	if err != nil {
		return sharedDomain.NewUnexpectedError(err)
	}
	var (
		sendTo  string
		fullName string
	)
	err = uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		u, err := uc.Users.GetByID(tx, userID)
		if err != nil {
			return err
		}
		if u.IsEmailVerified() {
			return sharedDomain.NewBusinessError(userErrors.ErrEmailAlreadyVerified, "")
		}
		if strings.TrimSpace(u.Email) == "" {
			// Operators may legitimately have no email. The dashboard
			// shouldn't surface "resend" for them, but if we get the
			// call we treat it as a validation error rather than a
			// silent no-op so the caller knows nothing went out.
			return sharedDomain.NewValidationError(userErrors.ErrInvalidEmail)
		}
		rec := userRepo.EmailVerificationRecord{
			ID:         uuid.New(),
			UserID:     u.ID,
			PlainToken: plain,
			ExpiresAt:  now.Add(VerifyTokenTTL),
		}
		if err := uc.Tokens.Save(tx, rec); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       u.GymID,
			EntityType:  "users",
			EntityID:    u.ID,
			Action:      audit.ActionEmailVerifyReq,
			ActorUserID: &u.ID,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		sendTo = u.Email
		fullName = u.FullName
		return nil
	})
	if err != nil {
		return err
	}
	if sendTo == "" || uc.Sender == nil {
		return nil
	}
	link := uc.BaseURL + "/auth/verify-email?token=" + plain
	subject := "Verifica tu correo en Tinta"
	body := buildVerifyEmailBody(fullName, link)
	// Send is best-effort once the token is persisted: the dashboard's
	// "Reenviar" button gives the user a clean retry path if delivery
	// failed for a transient reason. We log the underlying cause so ops
	// can tell a real outbound failure from a successful queue.
	if err := uc.Sender.Send(ctx, email.Message{
		To:      sendTo,
		Subject: subject,
		Body:    body,
		Tag:     "email_verification",
	}); err != nil {
		log.Printf("[verify] send to %s failed: %v", sendTo, err)
	}
	return nil
}

func buildVerifyEmailBody(fullName, link string) string {
	greeting := "Hola"
	if name := strings.TrimSpace(strings.SplitN(fullName, " ", 2)[0]); name != "" {
		greeting = "Hola, " + name
	}
	var b strings.Builder
	b.WriteString(greeting + ",\n\n")
	b.WriteString("Confirma tu correo electrónico en Tinta haciendo clic en este enlace:\n\n")
	b.WriteString(link)
	b.WriteString("\n\nEste enlace expira en 24 horas. Si no creaste una cuenta en Tinta, ignora este mensaje.\n\n— El equipo de Tinta")
	return b.String()
}

// ConfirmEmailVerification is UC-013 step 2 (POST /auth/verify-email).
// Single-use: the token is invalidated on first successful redemption,
// and replaying the same link returns ErrInvalidVerifyToken.
type ConfirmEmailVerification struct {
	Users   userRepo.UserRepository
	Tokens  userRepo.EmailVerificationRepository
	UoW     sharedDomain.UnitOfWork
	Audit   audit.Recorder
	NowFunc func() time.Time
}

func NewConfirmEmailVerification(
	users userRepo.UserRepository,
	tokens userRepo.EmailVerificationRepository,
	uow sharedDomain.UnitOfWork,
	recorder audit.Recorder,
) *ConfirmEmailVerification {
	return &ConfirmEmailVerification{
		Users: users, Tokens: tokens, UoW: uow,
		Audit: recorder, NowFunc: func() time.Time { return time.Now().UTC() },
	}
}

// Execute consumes the verification token and returns the verified
// user's ID. Callers (handleVerifyEmail) use the ID to mint a fresh
// session so the owner doesn't have to log in again right after
// clicking the link from their email client. uuid.Nil is never
// returned alongside a nil error — a successful verify always yields
// an ID, including the idempotent already-verified branch.
func (uc *ConfirmEmailVerification) Execute(ctx context.Context, token string) (uuid.UUID, error) {
	if strings.TrimSpace(token) == "" {
		return uuid.Nil, sharedDomain.NewBusinessError(userErrors.ErrInvalidVerifyToken, "")
	}
	now := uc.NowFunc()
	var verifiedID uuid.UUID
	err := uc.UoW.Command(ctx, func(tx sharedDomain.Transaction) error {
		userID, err := uc.Tokens.Consume(tx, token, now)
		if err != nil {
			return err
		}
		u, err := uc.Users.GetByID(tx, userID)
		if err != nil {
			return err
		}
		verifiedID = u.ID
		if u.IsEmailVerified() {
			// Token was valid but the user is already verified — treat
			// as idempotent success so a double-click on the email link
			// doesn't surface a confusing error.
			return nil
		}
		u.MarkEmailVerified(now)
		if _, err := uc.Users.Update(tx, u); err != nil {
			return sharedDomain.NewUnexpectedError(err)
		}
		_ = uc.Audit.Record(ctx, tx, audit.Entry{
			GymID:       u.GymID,
			EntityType:  "users",
			EntityID:    u.ID,
			Action:      audit.ActionEmailVerified,
			ActorUserID: &u.ID,
			IPAddress:   audit.IPFromContext(ctx),
			UserAgent:   audit.UAFromContext(ctx),
			At:          now,
		})
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return verifiedID, nil
}

// generateVerifyToken returns a 32-char hex string (128 bits of
// entropy). Same shape as the password-reset token.
func generateVerifyToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
