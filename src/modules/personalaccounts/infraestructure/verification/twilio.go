//go:build server

package verification

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	phoneVerification "github.com/cuadra/cuadra-core/src/modules/personalaccounts/domain/verification"
)

const twilioEndpoint = "https://verify.twilio.com"
const maxResponseBytes = 64 << 10

var (
	accountSID      = regexp.MustCompile(`^AC[0-9a-fA-F]{32}$`)
	apiKeySID       = regexp.MustCompile(`^SK[0-9a-fA-F]{32}$`)
	serviceSID      = regexp.MustCompile(`^VA[0-9a-fA-F]{32}$`)
	verificationSID = regexp.MustCompile(`^VE[0-9a-fA-F]{32}$`)
	e164            = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	codePattern     = regexp.MustCompile(`^[0-9]{4,10}$`)
)

type TwilioConfig struct {
	AccountSID string
	ServiceSID string
	// Username is the account SID or an API key SID; Password is its secret.
	Username string `json:"-"`
	Password string `json:"-"`
	// ServiceValidity must be verified against the isolated Verify service.
	// A zero value cannot silently become Twilio's documented default.
	ServiceValidity time.Duration
	RequestTimeout  time.Duration
}

func (TwilioConfig) String() string     { return "Twilio Verify configuration (credentials redacted)" }
func (c TwilioConfig) GoString() string { return c.String() }

type Twilio struct {
	config   TwilioConfig
	client   *http.Client
	endpoint string
}

func (*Twilio) String() string     { return "Twilio Verify provider (credentials redacted)" }
func (p *Twilio) GoString() string { return p.String() }

var _ phoneVerification.VerificationProvider = (*Twilio)(nil)

// NewTwilio configures the fixed HTTPS Verify API without performing I/O.
// Callers must enforce persistent budgets before any Start/Check operation.
func NewTwilio(config TwilioConfig) (*Twilio, error) {
	return newTwilio(config, &http.Client{Transport: http.DefaultTransport}, twilioEndpoint)
}

// Endpoint injection is private and used only by loopback HTTP contract tests.
func newTwilio(config TwilioConfig, client *http.Client, endpoint string) (*Twilio, error) {
	if !accountSID.MatchString(config.AccountSID) || !serviceSID.MatchString(config.ServiceSID) ||
		!(config.Username == config.AccountSID || apiKeySID.MatchString(config.Username)) || strings.TrimSpace(config.Password) == "" ||
		config.ServiceValidity < 2*time.Minute || config.ServiceValidity > 24*time.Hour ||
		config.RequestTimeout <= 0 || config.RequestTimeout > 30*time.Second || client == nil {
		return nil, &phoneVerification.Failure{Kind: phoneVerification.InvalidConfiguration}
	}
	copy := *client
	copy.Timeout = config.RequestTimeout
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	copy.Jar = nil
	return &Twilio{config: config, client: &copy, endpoint: endpoint}, nil
}

func (p *Twilio) Start(ctx context.Context, in phoneVerification.StartRequest) (phoneVerification.Result, error) {
	if !e164.MatchString(in.DestinationE164) || (in.Channel != phoneVerification.SMS && in.Channel != phoneVerification.WhatsApp) {
		return phoneVerification.Result{}, failure(phoneVerification.InvalidInput)
	}
	form := url.Values{"To": {in.DestinationE164}, "Channel": {string(in.Channel)}}
	result, err := p.request(ctx, http.MethodPost, "/Verifications", form, phoneVerification.Binding{DestinationE164: in.DestinationE164})
	if err != nil {
		return phoneVerification.Result{}, err
	}
	// Starting cannot act as an OTP check, even if a malformed response says
	// approved. This prevents a send response becoming an authorization path.
	if result.State == phoneVerification.Approved || result.Channel != in.Channel {
		return phoneVerification.Result{}, failure(phoneVerification.Uncertain)
	}
	return result, nil
}

func (p *Twilio) Check(ctx context.Context, in phoneVerification.CheckRequest) (phoneVerification.Result, error) {
	if !validBinding(in.Binding) || !codePattern.MatchString(in.Code) {
		return phoneVerification.Result{}, failure(phoneVerification.InvalidInput)
	}
	return p.request(ctx, http.MethodPost, "/VerificationCheck", url.Values{"VerificationSid": {in.Reference}, "Code": {in.Code}}, in.Binding)
}

func (p *Twilio) Fetch(ctx context.Context, in phoneVerification.Binding) (phoneVerification.Result, error) {
	if !validBinding(in) {
		return phoneVerification.Result{}, failure(phoneVerification.InvalidInput)
	}
	return p.request(ctx, http.MethodGet, "/Verifications/"+in.Reference, nil, in)
}

func validBinding(in phoneVerification.Binding) bool {
	return verificationSID.MatchString(in.Reference) && e164.MatchString(in.DestinationE164)
}
func failure(kind phoneVerification.FailureKind) *phoneVerification.Failure {
	return &phoneVerification.Failure{Kind: kind}
}

func (p *Twilio) request(ctx context.Context, method, path string, form url.Values, binding phoneVerification.Binding) (phoneVerification.Result, error) {
	if err := ctx.Err(); err != nil {
		return phoneVerification.Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.config.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, p.endpoint+"/v2/Services/"+p.config.ServiceSID+path, strings.NewReader(form.Encode()))
	if err != nil {
		return phoneVerification.Result{}, failure(phoneVerification.InvalidConfiguration)
	}
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.SetBasicAuth(p.config.Username, p.config.Password)
	// No automatic retries, redirects, fallback channels, status mutation, or
	// Idempotency-Key: Verify does not promise request-id deduplication here.
	res, err := p.client.Do(req)
	if err != nil {
		return phoneVerification.Result{}, requestFailure(method)
	}
	defer res.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if readErr != nil || len(body) > maxResponseBytes {
		return phoneVerification.Result{}, requestFailure(method)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return phoneVerification.Result{}, classifyResponse(method, res.StatusCode, res.Header, body)
	}
	resource, err := decodeResource(body)
	if err != nil || resource.AccountSID != p.config.AccountSID || resource.ServiceSID != p.config.ServiceSID ||
		!verificationSID.MatchString(resource.SID) || resource.To != binding.DestinationE164 ||
		(binding.Reference != "" && resource.SID != binding.Reference) {
		return phoneVerification.Result{}, requestFailure(method)
	}
	created, err := parseCreatedAt(resource.DateCreated)
	if err != nil {
		return phoneVerification.Result{}, requestFailure(method)
	}
	state, ok := providerState(resource.Status)
	channel := phoneVerification.Channel(resource.Channel)
	if !ok || (channel != phoneVerification.SMS && channel != phoneVerification.WhatsApp) {
		return phoneVerification.Result{}, requestFailure(method)
	}
	return phoneVerification.Result{Reference: resource.SID, State: state, Channel: channel, CreatedAt: created, ExpiresAt: created.Add(p.config.ServiceValidity)}, nil
}

func requestFailure(method string) *phoneVerification.Failure {
	if method == http.MethodPost {
		return failure(phoneVerification.Uncertain)
	}
	return failure(phoneVerification.Unavailable)
}

func classifyResponse(method string, status int, headers http.Header, body []byte) *phoneVerification.Failure {
	f := requestFailure(method)
	f.HTTPStatus = status
	var upstream struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(body, &upstream) == nil && upstream.Code > 0 && upstream.Code < 1000000 {
		f.ProviderCode = upstream.Code
	}
	switch {
	case status == http.StatusNotFound:
		f.Kind = phoneVerification.Missing
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		f.Kind = phoneVerification.InvalidConfiguration
	case status == http.StatusTooManyRequests:
		f.Kind = phoneVerification.RateLimited
		if f.ProviderCode == 60202 {
			f.Kind = phoneVerification.CheckLimitReached
		}
		// Expose bounded delay metadata only; the application still reserves
		// budget and decides whether an explicit attempt may be retried.
		if seconds, err := strconv.ParseInt(headers.Get("Retry-After"), 10, 32); err == nil && seconds >= 0 && seconds <= 86400 {
			f.RetryAfter = time.Duration(seconds) * time.Second
		}
	case status >= 400 && status < 500:
		f.Kind = phoneVerification.Rejected
	}
	return f
}

type twilioResource struct {
	SID         string `json:"sid"`
	ServiceSID  string `json:"service_sid"`
	AccountSID  string `json:"account_sid"`
	To          string `json:"to"`
	Channel     string `json:"channel"`
	Status      string `json:"status"`
	DateCreated string `json:"date_created"`
}

// Upstream can add fields, but duplicate keys or trailing JSON must never
// influence whether an ambiguous response is interpreted as approved.
func decodeResource(body []byte) (twilioResource, error) {
	var resource twilioResource
	dec := json.NewDecoder(strings.NewReader(string(body)))
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		return resource, io.ErrUnexpectedEOF
	}
	seen := map[string]bool{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return resource, err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return resource, io.ErrUnexpectedEOF
		}
		seen[name] = true
		var value json.RawMessage
		if err = dec.Decode(&value); err != nil {
			return resource, err
		}
	}
	if _, err = dec.Token(); err != nil {
		return resource, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return resource, io.ErrUnexpectedEOF
	}
	err = json.Unmarshal(body, &resource)
	return resource, err
}

func parseCreatedAt(value string) (time.Time, error) {
	// Documentation labels RFC 2822; official JSON examples use ISO 8601.
	for _, layout := range []string{time.RFC3339Nano, time.RFC1123Z, time.RFC1123} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, io.ErrUnexpectedEOF
}

func providerState(value string) (phoneVerification.State, bool) {
	switch value {
	case "pending":
		return phoneVerification.Pending, true
	case "approved":
		return phoneVerification.Approved, true
	case "canceled":
		return phoneVerification.Cancelled, true
	case "expired":
		return phoneVerification.Expired, true
	case "max_attempts_reached":
		return phoneVerification.AttemptsExhausted, true
	case "failed", "deleted":
		return phoneVerification.Failed, true
	default:
		return "", false
	}
}
