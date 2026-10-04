package ncemail

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Version is this SDK's version. It appears in the User-Agent and nowhere else.
const Version = "0.2.0"

const (
	// DefaultBaseURL is the production API. Override it with WithBaseURL or the
	// NAIJAMAIL_BASE_URL environment variable.
	DefaultBaseURL = "https://api.naijacloud.com"
	// DefaultTimeout is the per-attempt HTTP timeout. A retry gets a fresh one,
	// so three attempts can take up to three times this long unless the
	// caller's context cuts it short first.
	DefaultTimeout = 30 * time.Second
	// DefaultMaxRetries is the number of retries after the first attempt, for
	// three attempts in total.
	DefaultMaxRetries = 2

	// EnvAPIKey is the environment variable NewFromEnv reads.
	EnvAPIKey = "NAIJAMAIL_API_KEY"
	// EnvBaseURL overrides the base URL without touching code, which is how a
	// staging deploy points at a dev control plane.
	EnvBaseURL = "NAIJAMAIL_BASE_URL"
)

// Client-side sending limits, mirroring SENDING_LIMITS in the control plane.
//
// Duplicated here on purpose: a message over one of these bounds is going to
// be refused, and refusing it locally costs the caller a function return
// instead of a round trip plus a 400 from a machine they cannot see.
const (
	// MaxRecipients is the ceiling across To, CC and BCC combined.
	MaxRecipients = 50
	// MaxPayloadBytes is the largest encoded request body the API accepts.
	MaxPayloadBytes = 10 * 1024 * 1024
	// MaxCustomHeaders bounds the Headers map.
	MaxCustomHeaders = 25
	// MaxTags bounds the Tags map.
	MaxTags = 10
	// MaxTagKeyLength and MaxTagValueLength are where the server truncates. The
	// SDK rejects instead: silently shortening an analytics label produces
	// dashboards that quietly disagree with the caller's own records.
	MaxTagKeyLength   = 64
	MaxTagValueLength = 256
	// MaxIdempotencyKeyLength matches the API's documented header limit.
	MaxIdempotencyKeyLength = 255
)

const (
	keyPrefixLive = "nmail_live_"
	keyPrefixTest = "nmail_test_"
	// keyPrefixWorkspace is a workspace API key from Settings -> API keys,
	// carrying the Email send scope. One credential covers mail, deploys and
	// the platform API, so a customer who already has one does not need a
	// second. There is no test variant of it: the live/test split belongs to
	// the nmail_ family, and inventing a second spelling of one guarantee is
	// how the two drift apart.
	keyPrefixWorkspace = "nc_live_"

	backoffBaseDefault = 500 * time.Millisecond
	backoffCapDefault  = 8 * time.Second
	retryAfterMax      = 60 * time.Second

	// Bodies are read through a limit reader so a hostile or broken server
	// cannot make the SDK allocate without bound.
	maxErrorBodyBytes    = 1 << 20
	maxResponseBodyBytes = 8 << 20
)

// keyPattern is checked at construction. An empty or obviously-wrong key
// should fail where the client is built, not as a 401 in production an hour
// after the deploy that shipped it.
//
// Both credential families the API accepts are allowed. It stays an allowlist
// rather than relaxing to "any non-empty string": the check exists to catch the
// truncated paste and the wrong-variable-name deploy, and a pattern that
// accepts anything catches neither.
var keyPattern = regexp.MustCompile(`^(?:nmail_(?:live|test)|nc_live)_[A-Za-z0-9_-]{8,}$`)

// keyPrefixes is every prefix above, for redaction and for the User-Agent
// guard. Both walk this rather than naming the constants, so a family added
// later cannot be covered in one place and missed in the other.
var keyPrefixes = []string{keyPrefixLive, keyPrefixTest, keyPrefixWorkspace}

// Client is a Naijamail API client. It is safe for concurrent use, and it
// holds no package-level state: two clients with two keys in one process do
// not interfere.
//
// Build one with New or NewFromEnv and keep it — it owns an *http.Client and
// therefore a connection pool.
type Client struct {
	// Emails is the send and retrieve resource.
	Emails *EmailsService
	// Webhooks verifies inbound event signatures. It needs no credentials of
	// its own; it hangs off the client only so callers have one entry point.
	Webhooks *WebhooksService

	apiKey     string
	baseURL    *url.URL
	httpClient *http.Client
	maxRetries int
	userAgent  string

	backoffBase time.Duration
	backoffCap  time.Duration
	// sleep is a field so tests can assert the delay the retry loop computed
	// without spending it. Production always uses sleepWithContext.
	sleep func(ctx context.Context, d time.Duration) error
}

// Option configures a Client. Options are applied in order and may fail, so a
// bad base URL or a nonsense timeout surfaces from New rather than on the
// first request.
type Option func(*config) error

type config struct {
	baseURL    string
	httpClient *http.Client
	timeout    time.Duration
	timeoutSet bool
	maxRetries int
	uaSuffix   string
}

// WithBaseURL points the client at a different API host. The scheme must be
// https unless the host is localhost, 127.0.0.1 or ::1 — see New.
func WithBaseURL(raw string) Option {
	return func(c *config) error {
		if strings.TrimSpace(raw) == "" {
			return validationError("base URL cannot be empty")
		}
		c.baseURL = raw
		return nil
	}
}

// WithHTTPClient supplies the *http.Client to send with, for callers who need
// their own transport, proxy or connection pool.
//
// The client is copied, not used directly: the SDK must set CheckRedirect (see
// refuseRedirects) and mutating the caller's value would change redirect
// behaviour for every other user of that client in the process. The copy
// shares the caller's Transport, so connection pooling still works.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *config) error {
		if hc == nil {
			return validationError("HTTP client cannot be nil")
		}
		c.httpClient = hc
		return nil
	}
}

// WithTimeout sets the per-attempt HTTP timeout. It overrides the Timeout on a
// client supplied through WithHTTPClient; without it, that client keeps its own.
func WithTimeout(d time.Duration) Option {
	return func(c *config) error {
		if d < 0 {
			return validationError("timeout cannot be negative")
		}
		c.timeout = d
		c.timeoutSet = true
		return nil
	}
}

// WithMaxRetries sets how many retries follow the first attempt. Zero disables
// retrying. Retries stay safe at any value because every send carries an
// idempotency key.
func WithMaxRetries(n int) Option {
	return func(c *config) error {
		if n < 0 {
			return validationError("max retries cannot be negative")
		}
		c.maxRetries = n
		return nil
	}
}

// WithUserAgentSuffix appends an identifier for the calling application, which
// is how support tells one customer's integration from another in the logs.
func WithUserAgentSuffix(s string) Option {
	return func(c *config) error {
		c.uaSuffix = strings.TrimSpace(s)
		return nil
	}
}

// New builds a client. apiKey may be empty, in which case the key is read from
// the NAIJAMAIL_API_KEY environment variable.
//
// It fails if the key is missing or does not have the shape of a Naijamail
// key, and if the base URL is not https for a non-loopback host — a plaintext
// base URL would put a live sending credential on the wire in clear, and no
// caller ever wants that badly enough to make it a runtime warning.
func New(apiKey string, opts ...Option) (*Client, error) {
	key := strings.TrimSpace(apiKey)
	if key == "" {
		key = strings.TrimSpace(os.Getenv(EnvAPIKey))
	}
	if key == "" {
		return nil, validationError("no API key: pass one to New or set %s", EnvAPIKey)
	}
	if !keyPattern.MatchString(key) {
		// The key itself is never quoted back, here or anywhere else.
		return nil, validationError("API key does not look like a Naijamail key (expected %s…, %s… or %s…)", keyPrefixLive, keyPrefixTest, keyPrefixWorkspace)
	}

	cfg := &config{
		baseURL:    firstNonEmpty(os.Getenv(EnvBaseURL), DefaultBaseURL),
		timeout:    DefaultTimeout,
		maxRetries: DefaultMaxRetries,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(cfg); err != nil {
			return nil, err
		}
	}

	base, err := parseBaseURL(cfg.baseURL)
	if err != nil {
		return nil, err
	}

	ua, err := buildUserAgent(cfg.uaSuffix)
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{Timeout: cfg.timeout, CheckRedirect: refuseRedirects}
	if cfg.httpClient != nil {
		copied := *cfg.httpClient
		copied.CheckRedirect = refuseRedirects
		if cfg.timeoutSet {
			copied.Timeout = cfg.timeout
		}
		httpClient = &copied
	}

	c := &Client{
		apiKey:      key,
		baseURL:     base,
		httpClient:  httpClient,
		maxRetries:  cfg.maxRetries,
		userAgent:   ua,
		backoffBase: backoffBaseDefault,
		backoffCap:  backoffCapDefault,
		sleep:       sleepWithContext,
	}
	c.Emails = &EmailsService{client: c}
	c.Webhooks = &WebhooksService{}
	return c, nil
}

// NewFromEnv builds a client from the NAIJAMAIL_API_KEY environment variable.
func NewFromEnv(opts ...Option) (*Client, error) {
	return New("", opts...)
}

// String implements fmt.Stringer with the key redacted, so a client that ends
// up in a log line, a %v in an error message, or a panic dump does not take a
// live sending credential with it.
func (c *Client) String() string {
	if c == nil {
		return "<nil *ncemail.Client>"
	}
	return fmt.Sprintf("ncemail.Client{baseURL: %s, apiKey: %s, maxRetries: %d}",
		c.baseURL, redactKey(c.apiKey), c.maxRetries)
}

// GoString implements fmt.GoStringer so %#v redacts too. Without it, %#v walks
// the struct fields and prints the key verbatim.
func (c *Client) GoString() string {
	if c == nil {
		return "(*ncemail.Client)(nil)"
	}
	return fmt.Sprintf("&ncemail.Client{baseURL: %q, apiKey: %q, maxRetries: %d}",
		c.baseURL.String(), redactKey(c.apiKey), c.maxRetries)
}

// BaseURL reports the API host in use, for a caller logging their configuration.
func (c *Client) BaseURL() string { return c.baseURL.String() }

// redactKey keeps the prefix, which identifies the environment and is not
// secret, and drops everything that is.
func redactKey(key string) string {
	for _, prefix := range keyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return prefix + "***"
		}
	}
	switch {
	case key == "":
		return "<none>"
	default:
		return "***"
	}
}

// refuseRedirects hands a 3xx back to the SDK instead of following it.
//
// net/http follows redirects by default and re-sends the Authorization header
// when the redirect stays on the same host — but a DNS takeover, a
// misconfigured proxy or a compromised edge can answer with a Location on a
// host that is not ours, and a client that follows it has just posted a live
// sending key to a stranger. Refusing costs nothing: the API never redirects.
func refuseRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// parseBaseURL validates and normalises the API host.
func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, validationError("base URL is not a valid URL: %v", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, validationError("base URL must be absolute, for example %s", DefaultBaseURL)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, validationError("base URL scheme %q is not supported; use https", u.Scheme)
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, validationError("base URL must use https (plain http is allowed only for localhost, 127.0.0.1 and ::1)")
	}

	// A query or fragment on a base URL is either a mistake or an attempt to
	// smuggle a credential into a place that ends up in logs. Neither survives.
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func buildUserAgent(suffix string) (string, error) {
	ua := fmt.Sprintf("nc-email-go/%s (go/%s)", Version, runtime.Version())
	if suffix == "" {
		return ua, nil
	}
	if err := checkNoControlChars("user agent suffix", suffix); err != nil {
		return "", err
	}
	// The contract forbids the key from appearing in the User-Agent. A caller
	// building a suffix out of their configuration could paste one in by
	// accident, and the header would then be logged by every hop in between.
	for _, prefix := range keyPrefixes {
		if strings.Contains(suffix, prefix) {
			return "", validationError("user agent suffix must not contain an API key")
		}
	}
	return ua + " " + suffix, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// do runs one API call, retrying per the contract's policy.
//
// body is the already-marshalled request bytes rather than a reader, because
// each attempt needs its own: a *bytes.Reader is consumed by the first attempt
// and would hand an empty body to the second.
func (c *Client) do(ctx context.Context, method, path string, body []byte, headers map[string]string, out any) error {
	if ctx == nil {
		return validationError("a non-nil context is required")
	}

	endpoint := c.baseURL.String() + path
	var last *APIError

	attempts := c.maxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, c.retryDelay(attempt-1, last)); err != nil {
				return err
			}
		}

		apiErr, retryable := c.attempt(ctx, method, endpoint, body, headers, out)
		if apiErr == nil {
			return nil
		}
		last = apiErr
		if !retryable || attempt == attempts-1 {
			return apiErr
		}
	}

	// Unreachable: the loop always returns. Kept so the compiler is satisfied
	// without an else branch that hides the control flow above.
	return last
}

func (c *Client) attempt(ctx context.Context, method, endpoint string, body []byte, headers map[string]string, out any) (*APIError, bool) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return validationError("could not build the request: %v", err), false
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A cancelled or expired context is not worth another attempt; the
		// next backoff would return immediately anyway, and reporting it
		// straight away is what "honours cancellation" means to a caller.
		return transportError(ctx, err), ctx.Err() == nil
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		// Reached only because CheckRedirect refused to follow. Surfacing it as
		// a server error, per the contract, tells the caller their base URL is
		// pointing somewhere that is not the API.
		return &APIError{
			Message:    fmt.Sprintf("unexpected redirect to %q (redirects are not followed: a followed redirect would re-send the API key to another host)", resp.Header.Get("Location")),
			StatusCode: resp.StatusCode,
			RequestID:  resp.Header.Get("X-Request-Id"),
			kind:       ErrServer,
		}, false
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil, false
		}
		if err := decodeJSON(resp.Body, out); err != nil {
			return &APIError{
				Message:    fmt.Sprintf("could not decode the %d response: %v", resp.StatusCode, err),
				StatusCode: resp.StatusCode,
				RequestID:  resp.Header.Get("X-Request-Id"),
				kind:       ErrServer,
				cause:      err,
			}, false
		}
		return nil, false
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return errorFromResponse(resp, raw), retryableStatus(resp.StatusCode)
}

func decodeJSON(r io.Reader, out any) error {
	return json.NewDecoder(io.LimitReader(r, maxResponseBodyBytes)).Decode(out)
}

// drainAndClose reads what is left of a response body before closing it, so
// net/http can put the connection back in the pool instead of tearing it down.
// On a retry loop that difference is a new TCP and TLS handshake per attempt.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxResponseBodyBytes))
	_ = body.Close()
}

// transportError classifies a failure from http.Client.Do.
//
// The error text can include the request URL, which is safe: the key travels
// in a header, and parseBaseURL strips any query string from the base URL.
func transportError(ctx context.Context, err error) *APIError {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return &APIError{Message: "request timed out: " + err.Error(), kind: ErrTimeout, cause: err}
	case errors.Is(err, context.Canceled):
		return &APIError{Message: "request cancelled: " + err.Error(), kind: ErrConnection, cause: err}
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &APIError{Message: "request timed out: " + err.Error(), kind: ErrTimeout, cause: err}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return contextError(ctx)
	}
	return &APIError{Message: "could not reach the API: " + err.Error(), kind: ErrConnection, cause: err}
}

// retryDelay implements the contract's backoff: full jitter over an
// exponentially growing window, with Retry-After winning outright when the
// server sent one.
//
// Full jitter rather than "exponential plus a bit of noise" because the point
// is to break up a thundering herd: when a rate limit trips for many senders
// at once, backoffs that only differ slightly retry in a clump and trip it
// again.
func (c *Client) retryDelay(retry int, last *APIError) time.Duration {
	if last != nil && last.retryAfter > 0 {
		return last.retryAfter
	}

	window := c.backoffCap
	if retry < 32 {
		if scaled := c.backoffBase << uint(retry); scaled > 0 && scaled < window {
			window = scaled
		}
	}
	if window <= 0 {
		return 0
	}
	return time.Duration(mathrand.Int63n(int64(window) + 1))
}

// sleepWithContext waits, but gives up the moment the caller's context does.
// A bare time.Sleep here would make a cancelled request keep a goroutine (and
// the caller's request handler) parked for the full backoff.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return contextError(ctx)
	}
	if d <= 0 {
		return nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return contextError(ctx)
	case <-timer.C:
		return nil
	}
}

// contextError turns a context's own error into the SDK's error type, keeping
// the context error as the cause so errors.Is(err, context.Canceled) still
// matches for a caller who cancelled deliberately.
func contextError(ctx context.Context) *APIError {
	err := ctx.Err()
	if errors.Is(err, context.DeadlineExceeded) {
		return &APIError{Message: "context deadline exceeded before the request completed", kind: ErrTimeout, cause: err}
	}
	return &APIError{Message: "context cancelled before the request completed", kind: ErrConnection, cause: err}
}

// newIdempotencyKey returns a random UUIDv4.
//
// crypto/rand rather than math/rand: a predictable key lets one caller's retry
// collide with another caller's send and return them someone else's message id.
func newIdempotencyKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", validationError("could not generate an idempotency key: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant

	hexed := hex.EncodeToString(b[:])
	return strings.Join([]string{hexed[0:8], hexed[8:12], hexed[12:16], hexed[16:20], hexed[20:32]}, "-"), nil
}
