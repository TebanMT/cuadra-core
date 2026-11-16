// Package legal records that a gym owner accepted the Terms of Service and
// the Privacy Notice published at entinta.mx/terminos and
// entinta.mx/privacidad (UC-001). The rows are evidence: who accepted which
// document, which version, when and from where.
package legal

import (
	"time"

	"github.com/google/uuid"
)

const (
	DocumentTerms   = "terms"
	DocumentPrivacy = "privacy"

	// Versions are the "Última actualización" date of each published page
	// (cuadra-landing public/terminos.html and public/privacidad.html).
	// Bump them in the same change that publishes new text, so every new
	// acceptance points to the text the owner actually saw.
	TermsVersion   = "2026-10-02"
	PrivacyVersion = "2026-05-07"
)

type Acceptance struct {
	ID         uuid.UUID
	GymID      uuid.UUID
	UserID     uuid.UUID
	Document   string
	Version    string
	AcceptedAt time.Time
	IPAddress  string
	UserAgent  string
}

// CurrentAcceptances returns one acceptance per document, at the versions in
// force today.
func CurrentAcceptances(gymID, userID uuid.UUID, at time.Time, ip, userAgent string) []Acceptance {
	docs := []struct{ doc, version string }{
		{DocumentTerms, TermsVersion},
		{DocumentPrivacy, PrivacyVersion},
	}
	out := make([]Acceptance, 0, len(docs))
	for _, d := range docs {
		out = append(out, Acceptance{
			ID:         uuid.New(),
			GymID:      gymID,
			UserID:     userID,
			Document:   d.doc,
			Version:    d.version,
			AcceptedAt: at.UTC(),
			IPAddress:  clip(ip, maxIPLen),
			UserAgent:  clip(userAgent, maxUserAgentLen),
		})
	}
	return out
}

// Same limits as the columns in migration 069. A long User-Agent must not
// make the signup fail.
const (
	maxIPLen        = 64
	maxUserAgentLen = 512
)

func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
