//go:build sidecar

package crypto

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// EscrowHTTPClient implementa GMKEscrowClient contra POST /api/v1/sync/gmk.
// Autentica con el sk_live_* del pareo (la misma credencial del sync); el
// gym sale del token en el cloud — el gymID del contrato sólo participa en
// logs/errores.
type EscrowHTTPClient struct {
	CloudURL string
	// Token entrega la credencial vigente (leída de sync_state, la misma
	// fuente que el sync agent usa al boot). "" = sidecar aún sin parear.
	Token func(ctx context.Context) (string, error)
	HTTP  *http.Client
}

func NewEscrowHTTPClient(cloudURL string, token func(ctx context.Context) (string, error)) *EscrowHTTPClient {
	return &EscrowHTTPClient{
		CloudURL: cloudURL,
		Token:    token,
		HTTP:     &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *EscrowHTTPClient) Ensure(ctx context.Context, gymID uuid.UUID, localGMK []byte) ([]byte, error) {
	token, err := c.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("credencial del pareo: %w", err)
	}
	if token == "" {
		return nil, fmt.Errorf("sidecar sin credencial de pareo todavía (gym %s)", gymID)
	}

	payload := map[string]string{}
	if localGMK != nil {
		payload["gmk"] = base64.StdEncoding.EncodeToString(localGMK)
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.CloudURL+"/api/v1/sync/gmk", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			GMK string `json:"gmk"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("respuesta del vault ilegible: %w", err)
		}
		gmk, err := base64.StdEncoding.DecodeString(out.GMK)
		if err != nil || len(gmk) != GMKSize {
			return nil, fmt.Errorf("el vault devolvió una GMK inválida (gym %s)", gymID)
		}
		return gmk, nil
	case http.StatusNotFound:
		return nil, ErrNoEscrowedGMK
	case http.StatusServiceUnavailable:
		// SMK no configurada en el cloud: escrow apagado. Se reporta como
		// error normal — el provider conserva su llave local si la tiene.
		return nil, fmt.Errorf("escrow de GMK deshabilitado en el cloud (503)")
	default:
		return nil, fmt.Errorf("vault de GMK respondió %d: %s", resp.StatusCode, string(raw))
	}
}
