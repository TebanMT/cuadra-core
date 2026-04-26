//go:build server

// Package email implements the EmailProvider interface. The cloud-side
// production path uses Resend's REST API (a thin HTTP client — no SDK to
// avoid an extra dep). MVP usage is small: password reset (UC-004), transfer
// OTP (UC-010), and owner security alerts (UC-040 when WhatsApp is down).
package email

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	notiErrors "github.com/cuadra/cuadra-core/src/modules/notifications/domain/errors"
)

// ResendProvider posts to Resend's `/emails` endpoint with the API key
// supplied in `Authorization: Bearer ...`. Docs: https://resend.com/docs.
type ResendProvider struct {
	apiKey  string
	from    string
	httpc   *http.Client
	baseURL string
}

// ResendOptions wires the provider. APIKey + From are required; BaseURL
// defaults to "https://api.resend.com" — overridable in tests.
type ResendOptions struct {
	APIKey  string
	From    string
	BaseURL string
	Timeout time.Duration
}

func NewResendProvider(opts ResendOptions) (*ResendProvider, error) {
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, fmt.Errorf("resend: api key missing")
	}
	if strings.TrimSpace(opts.From) == "" {
		return nil, fmt.Errorf("resend: from address missing")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	base := opts.BaseURL
	if base == "" {
		base = "https://api.resend.com"
	}
	return &ResendProvider{
		apiKey:  opts.APIKey,
		from:    opts.From,
		httpc:   &http.Client{Timeout: timeout},
		baseURL: strings.TrimRight(base, "/"),
	}, nil
}

type resendCreateRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html,omitempty"`
	Text    string   `json:"text,omitempty"`
	Tags    []tag    `json:"tags,omitempty"`
}

type tag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resendCreateResponse struct {
	ID string `json:"id"`
}

func (p *ResendProvider) SendTransactional(ctx context.Context, to, subject, body, tagName string) (string, error) {
	if strings.TrimSpace(to) == "" {
		return "", notiErrors.ErrRecipientUnreachable
	}
	payload := resendCreateRequest{
		From:    p.from,
		To:      []string{to},
		Subject: subject,
		Text:    body,
	}
	if tagName != "" {
		payload.Tags = []tag{{Name: "category", Value: tagName}}
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/emails", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", notiErrors.ErrSendFailedRetry, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return "", notiErrors.ErrProviderUnavailable
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("%w: resend %d %s", notiErrors.ErrSendFailedRetry, resp.StatusCode, string(raw))
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%w: resend %d %s", notiErrors.ErrSendFailedFinal, resp.StatusCode, string(raw))
	}
	var out resendCreateResponse
	if err := json.Unmarshal(raw, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("%w: invalid resend response", notiErrors.ErrSendFailedFinal)
	}
	return out.ID, nil
}
