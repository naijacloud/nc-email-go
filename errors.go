package ncemail

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The sentinel errors every failure unwraps to. Callers match on these with
// errors.Is instead of type-switching over a hierarchy, which is what Go code
// around them already does for io.EOF and sql.ErrNoRows.
//
// The taxonomy is fixed by the SDK contract and is identical in every language
// binding, so a team moving from the Node SDK to this one rewrites syntax, not
// error handling.
var (
	// ErrValidation covers a 400 or 422 from the server and every check the SDK
	// makes before a request leaves the process (those carry StatusCode 0).
	ErrValidation = errors.New("ncemail: validation failed")
	// ErrAuthentication is a 401: missing, malformed, unknown or revoked key.
	ErrAuthentication = errors.New("ncemail: authentication failed")
	// ErrPermission is a 403: a test key on the live send path, an unverified
	// From domain, a paused domain, or the daily quota.
	ErrPermission = errors.New("ncemail: not permitted")
	// ErrNotFound is a 404, and also the 400 the server answers for an unknown
	// message id. See EmailsService.Get.
	ErrNotFound = errors.New("ncemail: not found")
	// ErrConflict is a 409.
	ErrConflict = errors.New("ncemail: conflict")
	// ErrRateLimit is a 429. The *APIError carries a RateLimit detail with the
	// server's Retry-After.
	ErrRateLimit = errors.New("ncemail: rate limited")
	// ErrServer is any 5xx, and also an unexpected 3xx (see refuseRedirects).
	ErrServer = errors.New("ncemail: server error")
	// ErrConnection is a socket, DNS or TLS failure, and a cancelled context.
	ErrConnection = errors.New("ncemail: connection failed")
	// ErrTimeout is a client-side deadline, a 408, or an expired context.
	ErrTimeout = errors.New("ncemail: timeout")
	// ErrWebhookVerification is returned by Verify for any signature that does
	// not check out. It never distinguishes the reason at a level an attacker
	// could probe.
	ErrWebhookVerification = errors.New("ncemail: webhook signature verification failed")
)

// APIError is the single error type this package returns.
//
// One struct rather than a tree of types: Go callers discriminate with
// errors.Is against the sentinels above, so a hierarchy would add types
// without adding capability, and every field below is useful on every failure.
type APIError struct {
	// Message is the server's message, or the SDK's own for a local failure.
	// The server may send an array of strings; those are joined with "; ".
	Message string
	// StatusCode is the HTTP status, or 0 for a failure raised before the
	// request left the SDK (validation, a bad base URL, a webhook signature).
	StatusCode int
	// ErrorLabel is the server's short label ("Forbidden", "Bad Request"),
	// from the NestJS error body's "error" field. Empty for local failures.
	ErrorLabel string
	// RequestID is the x-request-id response header, when the server set one.
	// Quote it in a support ticket; it is how the send is found in our logs.
	RequestID string
	// Body is the raw response body, truncated at 1 MiB. Kept because a
	// gateway or proxy in front of the API can answer with something that is
	// not our error shape at all, and the bytes are then the only evidence.
	Body []byte
	// RateLimit is set on a 429 and carries the server's Retry-After.
	RateLimit *RateLimitError

	// kind is the sentinel this error unwraps to.
	kind error
	// cause is the underlying transport error, when there was one.
	cause error
	// retryAfter is the parsed Retry-After header, clamped, used by the retry
	// loop. Not exported: on a 429 the same value is on RateLimit, and on
	// anything else it is an implementation detail of the backoff.
	retryAfter time.Duration
}

// Error implements the error interface. It never contains the API key: the key
// travels only in the Authorization header, and the URL a transport error
// reports has had any query string stripped at construction time.
func (e *APIError) Error() string {
	prefix := ""
	switch {
	case e.StatusCode > 0 && e.ErrorLabel != "":
		prefix = fmt.Sprintf("%d %s: ", e.StatusCode, e.ErrorLabel)
	case e.StatusCode > 0:
		prefix = fmt.Sprintf("%d: ", e.StatusCode)
	case e.ErrorLabel != "":
		prefix = e.ErrorLabel + ": "
	}

	msg := e.Message
	if msg == "" {
		msg = "request failed"
	}

	out := "ncemail: " + prefix + msg
	if e.RequestID != "" {
		out += " (request id " + e.RequestID + ")"
	}
	return out
}

// Unwrap reports the sentinel kind and, for a transport failure, the
// underlying error.
//
// It returns a slice rather than a single error so that both
// errors.Is(err, ncemail.ErrConnection) and errors.Is(err, context.Canceled)
// match the same value — a caller who cancelled deliberately should be able to
// see their own cancellation. errors.Is and errors.As understand multiple
// unwrapping (Go 1.20+); the errors.Unwrap function does not, so match with
// errors.Is rather than peeling the chain by hand.
func (e *APIError) Unwrap() []error {
	out := make([]error, 0, 3)
	if e.kind != nil {
		out = append(out, e.kind)
	}
	if e.RateLimit != nil {
		out = append(out, e.RateLimit)
	}
	if e.cause != nil {
		out = append(out, e.cause)
	}
	return out
}

// Kind returns the sentinel this error matches, for the rare caller that wants
// to switch on it rather than test with errors.Is.
func (e *APIError) Kind() error { return e.kind }

// RateLimitError is the 429 detail hanging off an *APIError. It is reachable
// both as APIError.RateLimit and through errors.As.
type RateLimitError struct {
	// RetryAfter is the server's Retry-After, clamped to 60s. Zero when the
	// server sent no usable value, in which case the SDK falls back to its own
	// jittered backoff.
	RetryAfter time.Duration
}

// Error implements the error interface.
func (e *RateLimitError) Error() string {
	if e.RetryAfter <= 0 {
		return "ncemail: rate limited"
	}
	return "ncemail: rate limited, retry after " + e.RetryAfter.String()
}

// validationError builds the error for a rule the SDK enforces itself. The
// contract fixes StatusCode at 0 for these so a caller writes one error path
// whether the rejection came from here or from the server.
func validationError(format string, args ...any) *APIError {
	return &APIError{Message: fmt.Sprintf(format, args...), kind: ErrValidation}
}

func webhookError(format string, args ...any) *APIError {
	return &APIError{Message: fmt.Sprintf(format, args...), kind: ErrWebhookVerification}
}

// nestErrorBody is NestJS's standard error shape. message is deliberately
// json.RawMessage: NestJS sends a string for a thrown exception and an array
// of strings for a failed validation pipe, and a struct field of either
// concrete type would fail to decode half the errors the API produces.
type nestErrorBody struct {
	StatusCode int             `json:"statusCode"`
	Message    json.RawMessage `json:"message"`
	Error      string          `json:"error"`
}

// errorFromResponse turns a non-2xx response into an *APIError.
//
// It must not fail: a proxy answering with HTML, a load balancer answering
// with nothing at all, and a gateway answering with a JSON shape that is not
// ours all reach this function, and in each case the caller still needs an
// error they can act on rather than a parse panic.
func errorFromResponse(resp *http.Response, body []byte) *APIError {
	err := &APIError{
		StatusCode: resp.StatusCode,
		Body:       body,
		RequestID:  resp.Header.Get("X-Request-Id"),
	}

	var parsed nestErrorBody
	if json.Unmarshal(body, &parsed) == nil {
		err.Message = decodeNestMessage(parsed.Message)
		err.ErrorLabel = parsed.Error
	}
	if err.Message == "" {
		// Fall back to the status line. An empty or non-JSON body is common
		// enough from infrastructure in front of the API that it cannot be
		// treated as exceptional.
		err.Message = strings.TrimSpace(resp.Status)
		if err.Message == "" {
			err.Message = http.StatusText(resp.StatusCode)
		}
	}

	if d, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()); ok {
		err.retryAfter = d
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		err.RateLimit = &RateLimitError{RetryAfter: err.retryAfter}
	}

	err.kind = kindForStatus(resp.StatusCode, err.Message)
	return err
}

// decodeNestMessage handles both shapes of the NestJS message field. Arrays
// are joined with "; " — the same separator in every SDK, so a support ticket
// quoting an error reads the same whichever binding produced it.
func decodeNestMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return strings.Join(list, "; ")
	}
	return ""
}

// kindForStatus maps an HTTP status to a sentinel.
//
// The 400-with-"message not found" case is a known server quirk, not a guess:
// the controller throws BadRequestException('message not found') for an id
// that does not exist instead of a NotFoundException. Matching the string is
// ugly, but a caller checking errors.Is(err, ErrNotFound) after a Get is the
// obvious thing to write, and it should work. Tracked in email-sdks/GAPS.md.
func kindForStatus(status int, message string) error {
	switch {
	case status == http.StatusBadRequest:
		if strings.Contains(strings.ToLower(message), "message not found") {
			return ErrNotFound
		}
		return ErrValidation
	case status == http.StatusUnauthorized:
		return ErrAuthentication
	case status == http.StatusForbidden:
		return ErrPermission
	case status == http.StatusNotFound:
		return ErrNotFound
	case status == http.StatusRequestTimeout:
		return ErrTimeout
	case status == http.StatusConflict:
		return ErrConflict
	case status == http.StatusUnprocessableEntity:
		return ErrValidation
	case status == http.StatusTooManyRequests:
		return ErrRateLimit
	case status >= 500:
		return ErrServer
	case status >= 400:
		// An unlisted 4xx is the caller's fault and is never retried, so it
		// lands with the other non-retryable client errors.
		return ErrValidation
	default:
		return ErrServer
	}
}

// retryableStatus decides whether a status is worth another attempt.
//
// It is deliberately a whitelist. A 403 on an unverified domain will never
// succeed no matter how long we wait, and retrying it only delays the error
// the caller needs to see. A 3xx is excluded too: following it is refused for
// security reasons, so repeating the request against a misconfigured base URL
// would just produce the same redirect three times.
func retryableStatus(status int) bool {
	switch {
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests:
		return true
	case status >= 500:
		return true
	default:
		return false
	}
}

// parseRetryAfter accepts both forms RFC 9110 allows: a delay in seconds, or
// an HTTP date. The result is clamped to retryAfterMax so a server (or
// something pretending to be one) cannot park a caller's goroutine for hours.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}

	if secs, err := strconv.ParseInt(value, 10, 64); err == nil {
		if secs < 0 {
			return 0, true
		}
		return clampRetryAfter(time.Duration(secs) * time.Second), true
	}

	if when, err := http.ParseTime(value); err == nil {
		d := when.Sub(now)
		if d < 0 {
			// A date already in the past means "retry now", not "retry never".
			return 0, true
		}
		return clampRetryAfter(d), true
	}

	return 0, false
}

func clampRetryAfter(d time.Duration) time.Duration {
	if d > retryAfterMax {
		return retryAfterMax
	}
	return d
}
