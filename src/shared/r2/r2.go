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
	"bytes"
	"context"
	"errors"
	"fmt"
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

	// PublicMediaBucket is a SEPARATE, PUBLIC R2 bucket for assets that must
	// be fetchable by third parties without auth — hoy las WhatsApp media
	// (banners de bienvenida que Twilio/Meta descargan anónimamente). A
	// diferencia de Bucket (privado, PII), este tiene Public Access / dominio
	// público habilitado. Sólo uploads server-side (el cloud genera + sube).
	PublicMediaBucket string
	// PublicMediaBaseURL es el prefijo público que sirve PublicMediaBucket
	// (el `https://pub-xxxx.r2.dev` administrado de R2, o un dominio propio
	// tipo `https://media.entinta.mx`). La URL pública del objeto es
	// PublicMediaBaseURL + "/" + objectKey.
	PublicMediaBaseURL string

	// PublicMediaAccessKey / PublicMediaSecretKey son las credenciales de un
	// token EXCLUSIVO del bucket público (menor privilegio: el token de fotos
	// privadas no toca este bucket y viceversa). Mismo AccountID (misma cuenta
	// Cloudflare). Si quedan vacías, el cliente cae a AccessKey/SecretKey.
	PublicMediaAccessKey string
	PublicMediaSecretKey string
}

// IsConfigured reports whether enough env vars are present to mint URLs.
// The handler checks this and returns 501 when false so the desktop
// shows a graceful "fotos no configuradas" error in dev.
func (c Config) IsConfigured() bool {
	return c.AccountID != "" && c.AccessKey != "" && c.SecretKey != "" && c.Bucket != ""
}

// IsPublicMediaConfigured reports whether the public media bucket is wired
// (bucket + public base URL + writable credentials). The write creds are
// either the exclusive public token (PublicMediaAccessKey/SecretKey) or, as a
// fallback, the main R2 creds. AccountID is shared (same Cloudflare account).
func (c Config) IsPublicMediaConfigured() bool {
	if c.AccountID == "" || c.PublicMediaBucket == "" || c.PublicMediaBaseURL == "" {
		return false
	}
	hasExclusive := c.PublicMediaAccessKey != "" && c.PublicMediaSecretKey != ""
	hasMain := c.AccessKey != "" && c.SecretKey != ""
	return hasExclusive || hasMain
}

// Client wraps the s3 client + presign helper. The presign client is
// stateful (caches signer state) so we keep it as a field. We also keep the
// raw s3 client for server-side PutObject (public media uploads).
type Client struct {
	cfg            Config
	s3c            *s3.Client
	pubMediaClient *s3.Client // separate creds for the public media bucket
	presigner      *s3.PresignClient
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
	// Public media client — exclusive token scoped to the public bucket when
	// provided (R2_PUBLIC_MEDIA_ACCESS_KEY/SECRET); falls back to the main
	// creds otherwise. Same Cloudflare account → same endpoint.
	var pubMediaClient *s3.Client
	if c.PublicMediaBucket != "" {
		ak, sk := c.PublicMediaAccessKey, c.PublicMediaSecretKey
		if ak == "" || sk == "" {
			ak, sk = c.AccessKey, c.SecretKey
		}
		pubMediaClient = s3.New(s3.Options{
			Region:       "auto",
			BaseEndpoint: aws.String(endpoint),
			Credentials:  credentials.NewStaticCredentialsProvider(ak, sk, ""),
			UsePathStyle: true,
		})
	}
	return &Client{
		cfg:            c,
		s3c:            s3c,
		pubMediaClient: pubMediaClient,
		presigner:      s3.NewPresignClient(s3c),
	}, nil
}

// PutPublicObject uploads body to the PUBLIC media bucket server-side and
// returns the object's public URL (PublicMediaBaseURL + "/" + objectKey).
// Used for WhatsApp media (welcome banners) that Twilio/Meta fetch
// anonymously. The bucket must have Public Access / a public domain enabled;
// this call only writes the object — it does not (and cannot) flip bucket
// visibility.
func (c *Client) PutPublicObject(
	ctx context.Context,
	objectKey, contentType string,
	body []byte,
) (publicURL string, err error) {
	if c == nil {
		return "", errors.New("r2: client nil")
	}
	if !c.cfg.IsPublicMediaConfigured() {
		return "", errors.New("r2: public media bucket not configured (R2_PUBLIC_MEDIA_BUCKET / R2_PUBLIC_MEDIA_BASE_URL)")
	}
	if c.pubMediaClient == nil {
		return "", errors.New("r2: public media client not initialised")
	}
	if objectKey == "" || contentType == "" || len(body) == 0 {
		return "", errors.New("r2: objectKey + contentType + body required")
	}
	_, err = c.pubMediaClient.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.cfg.PublicMediaBucket),
		Key:           aws.String(objectKey),
		Body:          bytes.NewReader(body),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(int64(len(body))),
		// Banners are immutable (object key carries a fresh UUID per send) so
		// they're safe to cache hard at the edge / in Meta's media cache.
		CacheControl: aws.String("public, max-age=31536000, immutable"),
	})
	if err != nil {
		return "", fmt.Errorf("r2 put public object: %w", err)
	}
	base := strings.TrimRight(c.cfg.PublicMediaBaseURL, "/")
	return base + "/" + strings.TrimLeft(objectKey, "/"), nil
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
