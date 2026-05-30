// Package r2 wraps the S3-compatible Cloudflare R2 client. We use it for
// blob storage que el cloud es canónico sobre — fotos de socios hoy,
// logos de gym eventualmente. El sidecar nunca tiene credenciales R2;
// le pide al cloud un presigned URL (PUT para subir, GET para bajar)
// y habla con R2 directamente (offline-first flow descrito en
// challenges-mvp.md / docs/uploads.md).
//
// Bucket privado: el bucket NO debe tener Public Access habilitado.
// Las fotos son PII de socios; si un URL público se filtra (logs,
// captura, etc.) es accesible para siempre. Por eso tanto upload como
// download usan URLs firmadas con TTL corto.
//
// Build tag: server only — only the cloud has the R2 credentials.
package r2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Config carries the env-var-driven settings. Empty AccountID/Bucket
// effectively disables the client — NewClient returns an error and the
// presign endpoint 501s. This lets dev work without R2 wired up.
type Config struct {
	AccountID string // 32-char hex from Cloudflare account page
	AccessKey string
	SecretKey string
	Bucket    string
	// PublicBaseURL — DEPRECATED. Quedó como vestigio del diseño con
	// bucket público. Ahora el bucket es privado y todo va por URL
	// firmada (PresignUpload / PresignDownload). Conservamos el campo
	// vacío para no romper la firma de NewClient si alguien lo pasa
	// desde main.go; no se usa internamente.
	PublicBaseURL string
}

// IsConfigured reports whether enough env vars are present to mint URLs.
// The handler checks this and returns 501 when false so the desktop
// shows a graceful "fotos no configuradas" error in dev.
func (c Config) IsConfigured() bool {
	return c.AccountID != "" && c.AccessKey != "" && c.SecretKey != "" && c.Bucket != ""
}

// Client wraps the s3 client + presign helper. The presign client is
// stateful (caches signer state) so we keep it as a field.
type Client struct {
	cfg       Config
	s3c       *s3.Client
	presigner *s3.PresignClient
}

// NewClient bootstraps the s3 client against R2's S3-compatible
// endpoint. R2's API URL is fixed to:
//
//	https://<account_id>.r2.cloudflarestorage.com
//
// auto region is the R2 convention (Cloudflare ignores region but the
// SDK requires one).
func NewClient(c Config) (*Client, error) {
	if !c.IsConfigured() {
		return nil, errors.New("r2: missing one of R2_ACCOUNT_ID/R2_ACCESS_KEY/R2_SECRET_KEY/R2_BUCKET")
	}
	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", c.AccountID)
	s3c := s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(endpoint),
		Credentials: credentials.NewStaticCredentialsProvider(
			c.AccessKey, c.SecretKey, "",
		),
		// R2 requires path-style addressing — virtual-hosted style breaks
		// because the bucket name doesn't appear as a subdomain of the
		// R2 endpoint.
		UsePathStyle: true,
	})
	return &Client{
		cfg:       c,
		s3c:       s3c,
		presigner: s3.NewPresignClient(s3c),
	}, nil
}

// PresignUpload mints a presigned PUT URL the caller can use to upload
// the object body directly to R2. Returns only the upload URL — el
// caller ya conoce el `objectKey` (lo generó). El bucket es privado
// así que NO hay public URL permanente; las lecturas posteriores se
// hacen con PresignDownload.
//
// `objectKey` should be deterministic and scoped (we use
// gyms/<gym_id>/members/<member_id>.<ext> from the caller). Content-Type
// is bound into the signature so the uploader has to send the same
// value in the PUT request.
func (c *Client) PresignUpload(
	ctx context.Context,
	objectKey, contentType string,
	ttl time.Duration,
) (uploadURL string, err error) {
	if c == nil {
		return "", errors.New("r2: client nil")
	}
	if objectKey == "" || contentType == "" {
		return "", errors.New("r2: objectKey + contentType required")
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	req, err := c.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.cfg.Bucket),
		Key:         aws.String(objectKey),
		ContentType: aws.String(contentType),
	}, func(o *s3.PresignOptions) {
		o.Expires = ttl
	})
	if err != nil {
		return "", fmt.Errorf("r2 presign put: %w", err)
	}
	return req.URL, nil
}

// PutObject uploads a reader directly to R2 (server-side, no presign).
// Used for server-generated assets (welcome banners, etc.).
// Returns the public URL if PublicBaseURL is configured.
func (c *Client) PutObject(ctx context.Context, key, contentType string, body io.Reader, size int64) error {
	if c == nil {
		return errors.New("r2: client nil")
	}
	_, err := c.s3c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.cfg.Bucket),
		Key:           aws.String(key),
		ContentType:   aws.String(contentType),
		Body:          body,
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return fmt.Errorf("r2 put object: %w", err)
	}
	return nil
}

// PublicURL builds the public URL for a key using PublicBaseURL.
func (c *Client) PublicURL(key string) string {
	if c == nil || c.cfg.PublicBaseURL == "" {
		return ""
	}
	return strings.TrimRight(c.cfg.PublicBaseURL, "/") + "/" + key
}

// PresignDownload mintea una GET firmada para leer un objeto privado.
// El TTL corto (5-15min) limita la ventana en que un link filtrado es
// útil. El caller (sidecar download task) pide esta URL fresh en cada
// intento; no la cachea.
func (c *Client) PresignDownload(
	ctx context.Context,
	objectKey string,
	ttl time.Duration,
) (downloadURL string, err error) {
	if c == nil {
		return "", errors.New("r2: client nil")
	}
	if objectKey == "" {
		return "", errors.New("r2: objectKey required")
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	req, err := c.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.cfg.Bucket),
		Key:    aws.String(objectKey),
	}, func(o *s3.PresignOptions) {
		o.Expires = ttl
	})
	if err != nil {
		return "", fmt.Errorf("r2 presign get: %w", err)
	}
	return req.URL, nil
}
